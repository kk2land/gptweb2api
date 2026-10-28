package gateway

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"
)

type Execution struct {
	Task        Task
	Credentials Credentials
	Session     *Session
	Slot        int
	Manager     *Manager
}
type Executor interface {
	Run(context.Context, *Execution) error
}

type Manager struct {
	mu           sync.Mutex
	fileMu       sync.Mutex
	store        *Store
	cfg          Config
	log          *slog.Logger
	executor     Executor
	tasks        map[string]*Task
	queue        []string
	active       map[string]context.CancelFunc
	busySessions map[string]bool
	busySlots    map[int]bool
	changed      chan struct{}
	accepting    bool
	tunnelReady  bool
	mcpSeen      bool
	models       map[string]bool
	maintenance  bool
	dotBusy      bool
	dotClaims    map[string]Credentials
	ctx          context.Context
	cancel       context.CancelFunc
	wg           sync.WaitGroup
}

func NewManager(store *Store, cfg Config, log *slog.Logger, executor Executor) (*Manager, error) {
	ctx, cancel := context.WithCancel(context.Background())
	m := &Manager{store: store, cfg: cfg, log: log, executor: executor, tasks: map[string]*Task{}, active: map[string]context.CancelFunc{}, busySessions: map[string]bool{}, busySlots: map[int]bool{}, changed: make(chan struct{}), accepting: true, models: map[string]bool{}, dotClaims: map[string]Credentials{}, ctx: ctx, cancel: cancel}
	tasks, err := store.recoverTasks()
	if err != nil {
		cancel()
		return nil, err
	}
	for _, t := range tasks {
		m.tasks[t.ID] = t
		if time.Now().After(t.Deadline) {
			t.State = "expired"
			t.Error = &Fault{"task_expired", "task deadline elapsed", 504}
		} else if t.State == "queued" {
			m.queue = append(m.queue, t.ID)
			continue
		} else {
			t.State = "failed"
			t.Error = &Fault{"restart_uncertain", "gateway restarted during execution; inspect side effects and start a new session", 409}
		}
		t.Updated = time.Now()
		var session *Session
		if t.Request.Model != ModelDots {
			session, _ = store.session(t.Request.Options.SessionID)
			if session != nil {
				session.Valid = false
			}
		}
		if err := store.save(t, session); err != nil {
			cancel()
			return nil, err
		}
	}
	m.wg.Add(1)
	go m.loop()
	return m, nil
}
func (m *Manager) signalLocked() { close(m.changed); m.changed = make(chan struct{}) }
func (m *Manager) SetTunnelReady(ready bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tunnelReady = ready
	m.signalLocked()
}
func (m *Manager) MCPSeen() { m.mu.Lock(); defer m.mu.Unlock(); m.mcpSeen = true; m.signalLocked() }
func (m *Manager) SetModelReady(model string, ready bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.models[model] = ready
	m.signalLocked()
}
func (m *Manager) modelReadyLocked(model string) bool {
	return m.tunnelReady && m.mcpSeen && m.models[model] && (model != ModelDots || m.cfg.Dots.Enabled)
}
func (m *Manager) Models() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for _, v := range []string{ModelGPT6, ModelSol, ModelDots} {
		if m.accepting && m.modelReadyLocked(v) {
			out = append(out, v)
		}
	}
	return out
}
func (m *Manager) Health() map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	return map[string]any{"alive": true, "accepting": m.accepting, "tunnel_connected": m.tunnelReady, "mcp_tool_verified": m.mcpSeen, "web_models": map[string]bool{ModelGPT6: m.models[ModelGPT6], ModelSol: m.models[ModelSol]}, "dots_ready": m.modelReadyLocked(ModelDots), "active": len(m.active), "queued": len(m.queue)}
}

func validateRequest(r *Request) error {
	if r.Model != ModelGPT6 && r.Model != ModelSol && r.Model != ModelDots {
		return fault("model_not_found", "unknown model", 404)
	}
	if len(r.Messages) == 0 || r.Messages[len(r.Messages)-1].Role != "user" {
		return fault("invalid_messages", "messages must end with a user message", 400)
	}
	for _, msg := range r.Messages {
		if msg.Role != "user" && msg.Role != "assistant" && msg.Role != "system" && msg.Role != "developer" {
			return fault("unsupported_role", "only text user, assistant, system and developer messages are supported", 400)
		}
		if strings.TrimSpace(msg.Content) == "" {
			return fault("invalid_messages", "message content must be nonempty text", 400)
		}
	}
	if len(r.Options.SessionID) > 128 || strings.ContainsAny(r.Options.SessionID, "/\\\x00") {
		return fault("invalid_session", "invalid session_id", 400)
	}
	if r.Options.ProjectID != "" {
		for _, c := range r.Options.ProjectID {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
				return fault("invalid_project", "chatgpt_project_id must be a project identifier, not a URL", 400)
			}
		}
	}
	if r.Options.Workdir != "" {
		if !filepath.IsAbs(r.Options.Workdir) {
			return fault("invalid_workdir", "workdir must be an existing absolute directory", 400)
		}
		p, err := filepath.EvalSymlinks(filepath.Clean(r.Options.Workdir))
		if err != nil {
			return fault("invalid_workdir", "workdir is unavailable", 400)
		}
		st, err := os.Stat(p)
		if err != nil || !st.IsDir() {
			return fault("invalid_workdir", "workdir must be an existing directory", 400)
		}
		r.Options.Workdir = p
	}
	return nil
}

func (m *Manager) Submit(r Request) (Task, error) {
	if r.Model == ModelDots {
		r.Options.SessionID = ""
		r.Options.ProjectID = ""
	}
	if err := validateRequest(&r); err != nil {
		return Task{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.accepting {
		return Task{}, fault("draining", "gateway is draining", 503)
	}
	if r.Model == ModelDots && !m.cfg.Dots.Enabled {
		return Task{}, fault("model_disabled", "dots is disabled", 404)
	}
	if !m.modelReadyLocked(r.Model) {
		return Task{}, fault("model_not_ready", "model has not passed Tunnel, MCP and backend readiness checks", 503)
	}
	// Capacity counts waiting tasks only; an immediately available slot does not
	// consume queue capacity. The scheduler holds the same lock while claiming.
	if len(m.queue) >= m.cfg.Tasks.QueueCapacity {
		return Task{}, fault("queue_full", "waiting queue is full", 429)
	}
	var session *Session
	dirID, err := directoryID(r.Options.Workdir)
	if err != nil {
		return Task{}, fault("invalid_workdir", "workdir became unavailable", 400)
	}
	if r.Model != ModelDots {
		if r.Options.SessionID != "" {
			var err error
			session, err = m.store.session(r.Options.SessionID)
			if err == sql.ErrNoRows {
				return Task{}, fault("session_not_found", "session_id was not created by this gateway", 404)
			}
			if err != nil {
				return Task{}, err
			}
			if err = checkSession(session, r); err != nil {
				return Task{}, err
			}
		} else {
			r.Options.SessionID = randomID("sess_")
			session = &Session{ID: r.Options.SessionID, Model: r.Model, Workdir: r.Options.Workdir, ProjectID: r.Options.ProjectID, Valid: true, DirectoryID: dirID}
		}
	}
	now := time.Now().UTC()
	t := &Task{ID: randomID("req_"), State: "queued", Request: r, Created: now, Updated: now, Deadline: now.Add(m.cfg.TaskTimeout), DirectoryID: dirID}
	if err := m.store.save(t, session); err != nil {
		return Task{}, err
	}
	m.tasks[t.ID] = t
	m.queue = append(m.queue, t.ID)
	m.signalLocked()
	return cloneTask(t), nil
}
func checkSession(s *Session, r Request) error {
	dirID, err := directoryID(r.Options.Workdir)
	if err != nil || dirID != s.DirectoryID {
		return fault("workdir_identity_changed", "bound directory identity changed; create a new session", 409)
	}
	if !s.Valid {
		return fault("session_uncertain", "session state is uncertain; create a new session", 409)
	}
	if s.Model != r.Model || s.Workdir != r.Options.Workdir || s.ProjectID != r.Options.ProjectID {
		return fault("session_binding_conflict", "continuation cannot change model, workdir or ChatGPT project", 409)
	}
	if len(r.Messages) <= len(s.History) || len(s.History) > 0 && !reflect.DeepEqual(r.Messages[:len(s.History)], s.History) {
		return fault("history_conflict", "messages must contain the complete unchanged session history followed by new input", 409)
	}
	return nil
}

func (m *Manager) Get(id string) (Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if t := m.tasks[id]; t != nil {
		return cloneTask(t), nil
	}
	t, err := m.store.loadTask(id)
	if err == sql.ErrNoRows {
		return Task{}, fault("task_not_found", "task not found", 404)
	}
	if err != nil {
		return Task{}, err
	}
	return cloneTask(t), nil
}
func (m *Manager) Wait(ctx context.Context, id string) (Task, error) {
	for {
		m.mu.Lock()
		t := m.tasks[id]
		if t == nil {
			var err error
			t, err = m.store.loadTask(id)
			if err != nil {
				m.mu.Unlock()
				if err == sql.ErrNoRows {
					return Task{}, fault("task_not_found", "task not found", 404)
				}
				return Task{}, err
			}
		}
		out := cloneTask(t)
		ch := m.changed
		m.mu.Unlock()
		if out.Terminal() {
			return out, nil
		}
		select {
		case <-ctx.Done():
			return out, ctx.Err()
		case <-ch:
		}
	}
}

func (m *Manager) persistTransitionLocked(t *Task, next Task, session *Session) error {
	next.Updated = time.Now().UTC()
	if err := m.store.save(&next, session); err != nil {
		return err
	}
	*t = next
	m.signalLocked()
	return nil
}
func (m *Manager) endLocked(t *Task, state, code, msg string) error {
	if t.Terminal() {
		return nil
	}
	next := *t
	next.State = state
	next.Error = &Fault{code, msg, 500}
	next.CompletedAt = time.Now()
	var session *Session
	if t.State != "queued" && t.Request.Model != ModelDots {
		session, _ = m.store.session(t.Request.Options.SessionID)
		if session != nil {
			session.Valid = false
		}
	}
	if err := m.persistTransitionLocked(t, next, session); err != nil {
		return err
	}
	if cancel := m.active[t.ID]; cancel != nil {
		cancel()
	}
	return nil
}
func (m *Manager) Cancel(id string) (Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t := m.tasks[id]
	if t == nil {
		t, err := m.store.loadTask(id)
		if err == sql.ErrNoRows {
			return Task{}, fault("task_not_found", "task not found", 404)
		}
		if err != nil {
			return Task{}, err
		}
		return cloneTask(t), nil
	}
	if err := m.endLocked(t, "cancelled", "task_cancelled", "task cancelled; already completed side effects are not undone"); err != nil {
		return Task{}, err
	}
	return cloneTask(t), nil
}

func (m *Manager) authorizeLocked(c Credentials, started bool) (*Task, error) {
	t := m.tasks[c.RequestID]
	if t == nil || t.TokenHash == "" || subtle.ConstantTimeCompare([]byte(t.TokenHash), []byte(digest(c.Token))) != 1 {
		return nil, fault("invalid_capability", "invalid task credentials", 403)
	}
	if !t.Terminal() && !time.Now().Before(t.Deadline) {
		if err := m.endLocked(t, "expired", "task_expired", "task deadline elapsed"); err != nil {
			return nil, err
		}
	}
	if !t.Terminal() && t.Request.Model == ModelDots && !t.LeaseUntil.IsZero() && !time.Now().Before(t.LeaseUntil) {
		if err := m.endLocked(t, "failed", "lease_expired", "Dot lease expired; task will not be replayed"); err != nil {
			return nil, err
		}
	}
	if t.Terminal() || t.State == "queued" {
		return nil, fault("task_inactive", "task no longer permits execution", 409)
	}
	if started && !t.Started {
		return nil, fault("task_not_started", "call start_task first", 409)
	}
	return t, nil
}
func (m *Manager) StartTask(c Credentials) (map[string]any, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, err := m.authorizeLocked(c, false)
	if err != nil {
		return nil, err
	}
	next := *t
	next.Started = true
	next.State = "running"
	if err := m.persistTransitionLocked(t, next, nil); err != nil {
		return nil, err
	}
	return map[string]any{"request_id": t.ID, "workdir": t.Request.Options.Workdir, "commands_enabled": false, "deadline": t.Deadline, "instructions": "Use this task capability for all calls. Call complete_task with the full final answer after operations finish. Reuse operation_id only for an identical retry."}, nil
}
func (m *Manager) Complete(c Credentials, result Completion) (map[string]any, error) {
	if result.Status != "succeeded" && result.Status != "failed" {
		return nil, fault("invalid_completion", "status must be succeeded or failed", 400)
	}
	if strings.TrimSpace(result.Answer) == "" || len(result.Answer) > 2*1024*1024 {
		return nil, fault("invalid_completion", "final_answer must be nonempty and at most 2 MiB", 400)
	}
	if result.Status == "failed" && (result.Error == nil || result.Error.Code == "" || result.Error.Message == "") {
		return nil, fault("invalid_completion", "failed status requires error.code and error.message", 400)
	}
	if result.Status == "succeeded" && result.Error != nil {
		return nil, fault("invalid_completion", "succeeded status cannot contain error", 400)
	}
	b, _ := json.Marshal(result)
	hash := digest(string(b))
	m.mu.Lock()
	defer m.mu.Unlock()
	t := m.tasks[c.RequestID]
	if t == nil {
		var err error
		t, err = m.store.loadTask(c.RequestID)
		if err != nil {
			return nil, fault("invalid_capability", "invalid task credentials", 403)
		}
	}
	if t.TokenHash == "" || subtle.ConstantTimeCompare([]byte(t.TokenHash), []byte(digest(c.Token))) != 1 {
		return nil, fault("invalid_capability", "invalid task credentials", 403)
	}
	if t.CompletionHash != "" {
		if time.Since(t.CompletedAt) > 5*time.Minute {
			return nil, fault("completion_expired", "completion retry window elapsed", 409)
		}
		if t.CompletionHash != hash {
			return nil, fault("completion_conflict", "a different final answer was already accepted", 409)
		}
		return map[string]any{"accepted": true, "request_id": t.ID, "state": t.State, "duplicate": true}, nil
	}
	t, err := m.authorizeLocked(c, true)
	if err != nil {
		return nil, err
	}
	if t.ActiveTools != 0 {
		return nil, fault("tools_active", "wait for managed operations before completing", 409)
	}
	if t.Calibration != "" && (result.Status != "succeeded" || result.Answer != t.Calibration) {
		_ = m.endLocked(t, "failed", "calibration_failed", "Dot event calibration returned an incorrect nonce")
		return nil, fault("calibration_failed", "complete the calibration with its exact nonce", 409)
	}
	next := *t
	next.Answer = result.Answer
	next.Error = result.Error
	next.CompletionHash = hash
	next.CompletedAt = time.Now()
	next.State = "completed"
	if result.Status == "failed" {
		next.State = "failed"
	}
	var session *Session
	if t.Request.Model != ModelDots {
		session, err = m.store.session(t.Request.Options.SessionID)
		if err != nil {
			return nil, err
		}
		session.History = append(append([]Message{}, t.Request.Messages...), Message{Role: "assistant", Content: result.Answer})
		session.LastRequestID = t.ID
		if next.State != "completed" {
			session.Valid = false
		}
	}
	if err := m.persistTransitionLocked(t, next, session); err != nil {
		return nil, err
	}
	if t.Calibration != "" {
		m.models[ModelDots] = true
	}
	return map[string]any{"accepted": true, "request_id": t.ID, "state": t.State, "duplicate": false}, nil
}

func (m *Manager) Bind(id string, b Binding, scriptHash string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	t := m.tasks[id]
	if t == nil || t.Terminal() && t.CompletionHash == "" {
		return fault("task_inactive", "task stopped before browser binding", 409)
	}
	next := *t
	next.Binding = b
	next.ScriptHash = scriptHash
	session, err := m.store.session(t.Request.Options.SessionID)
	if err != nil {
		return err
	}
	session.Binding = b
	return m.persistTransitionLocked(t, next, session)
}
func (m *Manager) State(id, state string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	t := m.tasks[id]
	if t == nil || t.Terminal() {
		return nil
	}
	if state != "running" && state != "waiting_approval" {
		return errors.New("invalid runtime state")
	}
	next := *t
	next.State = state
	return m.persistTransitionLocked(t, next, nil)
}

func (m *Manager) loop() {
	defer m.wg.Done()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	clean := time.NewTicker(time.Hour)
	defer clean.Stop()
	for {
		m.mu.Lock()
		ch := m.changed
		for _, t := range m.tasks {
			if !t.Terminal() && !time.Now().Before(t.Deadline) {
				if err := m.endLocked(t, "expired", "task_expired", "task deadline elapsed"); err != nil {
					m.log.Error("persist task expiry", "error", err)
				}
			}
			if !t.Terminal() && t.Request.Model == ModelDots && !t.LeaseUntil.IsZero() && !time.Now().Before(t.LeaseUntil) {
				if err := m.endLocked(t, "failed", "lease_expired", "Dot lease expired; task will not be replayed"); err != nil {
					m.log.Error("persist lease expiry", "error", err)
				}
			}
		}
		m.scheduleLocked()
		m.mu.Unlock()
		select {
		case <-m.ctx.Done():
			return
		case <-ch:
		case <-tick.C:
		case <-clean.C:
			m.mu.Lock()
			if err := m.store.cleanup(); err != nil {
				m.log.Error("database cleanup failed", "error", err)
			}
			for id, t := range m.tasks {
				if t.Terminal() && m.active[id] == nil && time.Since(t.Updated) > 5*time.Minute {
					delete(m.tasks, id)
				}
			}
			m.mu.Unlock()
		}
	}
}
func (m *Manager) scheduleLocked() {
	if m.maintenance {
		return
	}
	remaining := m.queue[:0]
	for _, id := range m.queue {
		t := m.tasks[id]
		if t.Terminal() {
			continue
		}
		if !time.Now().Before(t.Deadline) {
			if err := m.endLocked(t, "expired", "task_expired", "task deadline elapsed"); err != nil {
				remaining = append(remaining, id)
			}
			continue
		}
		if len(m.active) >= m.cfg.Tasks.MaxConcurrency || m.busySessions[t.Request.Options.SessionID] || !m.modelReadyLocked(t.Request.Model) {
			remaining = append(remaining, id)
			continue
		}
		if t.Request.Model == ModelDots {
			remaining = append(remaining, id)
			continue
		} // claimed only by the verified Dot path
		session, err := m.store.session(t.Request.Options.SessionID)
		if err == nil {
			err = checkSession(session, t.Request)
		}
		if err != nil {
			if saveErr := m.endLocked(t, "failed", "session_conflict", err.Error()); saveErr != nil {
				m.log.Error("persist session failure", "error", saveErr)
				remaining = append(remaining, id)
			}
			continue
		}
		token := randomID("turn_")
		next := *t
		next.State = "submitting"
		next.TokenHash = digest(token)
		if err := m.persistTransitionLocked(t, next, nil); err != nil {
			m.log.Error("claim task failed", "error", err)
			remaining = append(remaining, id)
			continue
		}
		slot := 0
		for m.busySlots[slot] {
			slot++
		}
		ctx, cancel := context.WithDeadline(m.ctx, t.Deadline)
		m.active[id] = cancel
		m.busySlots[slot] = true
		m.busySessions[t.Request.Options.SessionID] = true
		e := &Execution{Task: *t, Credentials: Credentials{id, token}, Session: session, Slot: slot, Manager: m}
		m.wg.Add(1)
		go m.execute(ctx, e)
	}
	m.queue = remaining
}
func (m *Manager) execute(ctx context.Context, e *Execution) {
	defer m.wg.Done()
	err := m.executor.Run(ctx, e)
	m.mu.Lock()
	defer m.mu.Unlock()
	t := m.tasks[e.Task.ID]
	if !t.Terminal() {
		if !time.Now().Before(t.Deadline) {
			if saveErr := m.endLocked(t, "expired", "task_expired", "task deadline elapsed"); saveErr != nil {
				m.log.Error("persist execution expiry", "error", saveErr)
			}
		} else {
			code, msg := "execution_failed", "backend ended without a completion"
			if err != nil {
				var f *Fault
				if errors.As(err, &f) {
					code, msg = f.Code, f.Message
				}
			}
			if saveErr := m.endLocked(t, "failed", code, msg); saveErr != nil {
				m.log.Error("persist execution failure", "request_id", t.ID, "error", saveErr)
			}
		}
	}
	if err != nil {
		m.log.Warn("backend stopped", "request_id", t.ID, "error_code", safeErrorCode(err))
	}
	m.active[t.ID]()
	delete(m.active, t.ID)
	delete(m.busySlots, e.Slot)
	delete(m.busySessions, t.Request.Options.SessionID)
	m.signalLocked()
}
func safeErrorCode(err error) string {
	var f *Fault
	if errors.As(err, &f) {
		return f.Code
	}
	return "internal_error"
}

func (m *Manager) BeginDrain() {
	m.mu.Lock()
	m.accepting = false
	m.signalLocked()
	m.mu.Unlock()
}
func (m *Manager) Drain(ctx context.Context) error {
	m.BeginDrain()
	for {
		m.mu.Lock()
		active := len(m.active)
		queued := false
		for _, t := range m.tasks {
			if !t.Terminal() {
				queued = true
				break
			}
		}
		ch := m.changed
		m.mu.Unlock()
		if active == 0 && !queued {
			m.cancel()
			m.wg.Wait()
			return nil
		}
		select {
		case <-ch:
		case <-ctx.Done():
			m.mu.Lock()
			for _, t := range m.tasks {
				if !t.Terminal() {
					if err := m.endLocked(t, "cancelled", "gateway_stopped", "gateway drain deadline elapsed; inspect completed side effects"); err != nil {
						m.log.Error("persist shutdown cancellation failed", "error", err)
					}
				}
			}
			m.mu.Unlock()
			m.cancel()
			m.wg.Wait()
			return ctx.Err()
		}
	}
}
