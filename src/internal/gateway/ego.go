package gateway

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

var scriptNames = []string{"common.js", "probe.js", "prepare-page.js", "create-conversation.js", "select-project.js", "select-model.js", "attach-plugin.js", "submit-task.js", "inspect-turn.js", "recover-completion.js", "cancel-turn.js", "dot-space.js"}

type ScriptSet struct {
	files map[string]string
	Hash  string
}

func LoadScripts(root string) (*ScriptSet, error) {
	s := &ScriptSet{files: map[string]string{}}
	var all strings.Builder
	for _, name := range scriptNames {
		b, err := os.ReadFile(filepath.Join(root, "ego_scripts", name))
		if err != nil {
			return nil, fmt.Errorf("missing Ego script %s", name)
		}
		if len(b) == 0 || len(b) > 256*1024 {
			return nil, fmt.Errorf("invalid Ego script %s", name)
		}
		s.files[name] = string(b)
		all.WriteString(name)
		all.WriteByte(0)
		all.Write(b)
		all.WriteByte(0)
	}
	s.Hash = digest(all.String())
	return s, nil
}

type limitedOutput struct {
	bytes.Buffer
	limit int
}

func (b *limitedOutput) Write(p []byte) (int, error) {
	n := len(p)
	remaining := b.limit - b.Len()
	if remaining > 0 {
		if len(p) > remaining {
			p = p[:remaining]
		}
		_, _ = b.Buffer.Write(p)
	}
	return n, nil
}

type EgoResult struct {
	OK        bool    `json:"ok"`
	Operation string  `json:"operation"`
	Binding   Binding `json:"binding"`
	State     string  `json:"state"`
	Model     string  `json:"model"`
	PluginID  string  `json:"plugin_id"`
	Version   string  `json:"version"`
	Received  bool    `json:"received"`
	ErrorCode string  `json:"error_code"`
}
type ScriptRunner interface {
	Call(context.Context, *ScriptSet, string, map[string]any) (EgoResult, error)
}
type CLIRunner struct{ AppName string }

func (r CLIRunner) Call(ctx context.Context, s *ScriptSet, op string, input map[string]any) (EgoResult, error) {
	var result EgoResult
	code, ok := s.files[op+".js"]
	if !ok {
		return result, fault("script_missing", "required Ego script is missing", 503)
	}
	b, err := json.Marshal(input)
	if err != nil {
		return result, err
	}
	// Base64 is data, never shell text or an interpolated JavaScript expression.
	marker := randomID("GPTWEB_RESULT_") + ":"
	source := "(async()=>{\n" + s.files["common.js"] + "\n" + code + "\nconst input=JSON.parse(Buffer.from(\"" + base64.StdEncoding.EncodeToString(b) + "\",\"base64\").toString(\"utf8\"));\ntry { const result=await operation(input); console.log(" + jsString(marker) + "+JSON.stringify({...result,operation:" + jsString(op) + "})); } catch(e) { console.log(" + jsString(marker) + "+JSON.stringify({ok:false,operation:" + jsString(op) + ",error_code:e.code||e.error_code||\"ego_operation_failed\"})); }\n})();\n"
	args := []string{"nodejs"}
	if r.AppName != "" {
		args = append([]string{"--ego-server-name=" + r.AppName}, args...)
	}
	cmd := exec.CommandContext(ctx, "ego-browser", args...)
	cmd.Stdin = strings.NewReader(source)
	cmd.WaitDelay = 2 * time.Second
	out := &limitedOutput{limit: 1024 * 1024}
	cmd.Stdout = out
	cmd.Stderr = out
	if err := cmd.Run(); err != nil {
		return result, fault("ego_unavailable", "ego-browser is unavailable or the operation timed out", 503)
	}
	for _, line := range strings.Split(out.String(), "\n") {
		if strings.HasPrefix(line, marker) {
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, marker)), &result); err != nil {
				return result, fault("ego_protocol_error", "invalid structured Ego result", 503)
			}
			if !result.OK {
				return result, fault(result.ErrorCode, "Ego could not verify the requested page operation", 503)
			}
			if result.Operation != op {
				return result, fault("ego_protocol_error", "Ego operation identity mismatch", 503)
			}
			return result, nil
		}
	}
	return result, fault("ego_protocol_error", "Ego returned no structured result", 503)
}
func jsString(s string) string { b, _ := json.Marshal(s); return string(b) }

type EgoExecutor struct {
	Root         string
	Runner       ScriptRunner
	Log          *slog.Logger
	mu           sync.Mutex
	slots        map[int]Binding
	quarantined  map[int]bool
	profile      string
	pluginID     string
	store        *Store
	slotRequests map[int]string
	appName      string
}

func NewEgo(root string, log *slog.Logger, appName string) *EgoExecutor {
	return &EgoExecutor{Root: root, Runner: CLIRunner{AppName: appName}, Log: log, slots: map[int]Binding{}, quarantined: map[int]bool{}, slotRequests: map[int]string{}, appName: appName}
}

type egoState struct {
	Profile         string
	Slots           map[int]Binding
	Quarantined     map[int]bool
	Requests        map[int]string
	PluginID        string
	AppName         string
	LegacySpaceName string `json:"SpaceName,omitempty"`
}

func (e *EgoExecutor) Restore(store *Store) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.store = store
	var b []byte
	err := store.db.QueryRow(`SELECT payload FROM runtime_state WHERE name='ego'`).Scan(&b)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	var state egoState
	if err = json.Unmarshal(b, &state); err != nil {
		return err
	}
	if state.Slots == nil || state.Quarantined == nil || state.Requests == nil {
		return fmt.Errorf("invalid persisted Ego identity")
	}
	if state.AppName != e.appName {
		// Numeric Space/page IDs and Profile/plugin IDs are local to an app.
		// Invalidate web sessions in the same transaction as the new app identity.
		e.profile = ""
		e.pluginID = ""
		e.slots = map[int]Binding{}
		e.quarantined = map[int]bool{}
		e.slotRequests = map[int]string{}
		return e.persistLocked(true)
	}
	e.profile = state.Profile
	e.pluginID = state.PluginID
	if state.LegacySpaceName != "" {
		e.slots = map[int]Binding{}
		e.quarantined = map[int]bool{}
		e.slotRequests = map[int]string{}
		return e.saveLocked()
	}
	e.slots, e.quarantined, e.slotRequests = state.Slots, state.Quarantined, state.Requests
	for slot, binding := range e.slots {
		if binding.PageID != "" {
			e.quarantined[slot] = true
		}
	}
	return nil
}
func (e *EgoExecutor) saveLocked() error {
	return e.persistLocked(false)
}
func (e *EgoExecutor) persistLocked(invalidateSessions bool) error {
	if e.store == nil {
		return nil
	}
	b, err := json.Marshal(egoState{Profile: e.profile, Slots: e.slots, Quarantined: e.quarantined, Requests: e.slotRequests, PluginID: e.pluginID, AppName: e.appName})
	if err != nil {
		return err
	}
	tx, err := e.store.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if invalidateSessions {
		if _, err = tx.Exec(`UPDATE sessions SET payload=json_set(payload,'$.valid',json('false'))`); err != nil {
			return err
		}
	}
	if _, err = tx.Exec(`INSERT INTO runtime_state VALUES('ego',?) ON CONFLICT(name) DO UPDATE SET payload=excluded.payload`, b); err != nil {
		return err
	}
	return tx.Commit()
}
func (e *EgoExecutor) base(slot int) map[string]any {
	e.mu.Lock()
	defer e.mu.Unlock()
	return map[string]any{"slot": slot, "binding": e.slots[slot], "profile_id": e.profile, "space_name": fmt.Sprintf("gptweb2api-slot-%d", slot), "request_id": e.slotRequests[slot], "plugin_id": e.pluginID}
}
func callBound(ctx context.Context, runner ScriptRunner, s *ScriptSet, op string, input map[string]any) (EgoResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	return runner.Call(ctx, s, op, input)
}

// Probe never sends a user task. Only models whose actual picker label and
// installed plugin identity are observed are advertised by the API.
func (e *EgoExecutor) Probe(ctx context.Context, m *Manager) error {
	s, err := LoadScripts(e.Root)
	if err != nil {
		return err
	}
	// The caller reserved maintenance while no task is active. Reconcile
	// quarantined pages before navigating or enabling any model again.
	for _, slot := range e.Diagnostics() {
		input := e.base(slot)
		if _, err = callBound(ctx, e.Runner, s, "cancel-turn", input); err != nil {
			return err
		}
		e.mu.Lock()
		e.quarantined[slot] = false
		err = e.saveLocked()
		e.mu.Unlock()
		if err != nil {
			return err
		}
	}
	if _, err = e.provision(ctx, s, 0, true); err != nil {
		return err
	}
	input := e.base(0)
	res, err := callBound(ctx, e.Runner, s, "create-conversation", input)
	if err != nil {
		return err
	}
	e.mu.Lock()
	e.slots[0] = res.Binding
	e.profile = res.Binding.ProfileID
	err = e.saveLocked()
	e.mu.Unlock()
	if err != nil {
		return err
	}
	input["binding"] = res.Binding
	res, err = callBound(ctx, e.Runner, s, "probe", input)
	if err != nil {
		return err
	}
	e.Log.Info("Ego probe verified", "version", res.Version, "script_hash", s.Hash)
	for _, model := range []string{ModelGPT6, ModelSol} {
		input["model"] = model
		_, modelErr := callBound(ctx, e.Runner, s, "select-model", input)
		if modelErr == nil {
			var pluginResult EgoResult
			pluginResult, modelErr = callBound(ctx, e.Runner, s, "attach-plugin", input)
			if modelErr == nil {
				e.mu.Lock()
				if pluginResult.PluginID == "" || e.pluginID != "" && pluginResult.PluginID != e.pluginID {
					modelErr = fault("plugin_identity_mismatch", "plugin identity differs from the persisted identity", 503)
				} else {
					e.pluginID = pluginResult.PluginID
					modelErr = e.saveLocked()
					input["plugin_id"] = e.pluginID
				}
				e.mu.Unlock()
			}
		}
		m.SetModelReady(model, modelErr == nil)
		if modelErr != nil {
			e.Log.Warn("web model probe failed", "model", model, "error_code", safeErrorCode(modelErr))
		}
	}
	return nil
}

func (e *EgoExecutor) Run(ctx context.Context, x *Execution) (runErr error) {
	s, err := LoadScripts(e.Root)
	if err != nil {
		return err
	}
	e.mu.Lock()
	bad := e.quarantined[x.Slot]
	e.mu.Unlock()
	if bad {
		return fault("space_quarantined", "slot requires a successful cleanup probe before reuse", 503)
	}
	if _, err = e.provision(ctx, s, x.Slot, true); err != nil {
		return err
	}
	input := e.base(x.Slot)
	input["model"] = x.Task.Request.Model
	input["project_id"] = x.Task.Request.Options.ProjectID
	input["conversation_id"] = x.Session.Binding.ConversationID
	input["expected_last_request_id"] = x.Session.LastRequestID
	input["request_id"] = x.Task.ID
	// Every path, including failure after a possibly successful submit, proves
	// the page stopped before its slot can be reused.
	bound := false
	defer func() {
		if !bound {
			return
		}
		if x.Manager.ctx.Err() != nil {
			e.mu.Lock()
			e.quarantined[x.Slot] = true
			_ = e.saveLocked()
			e.mu.Unlock()
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_, err := callBound(cleanupCtx, e.Runner, s, "cancel-turn", input)
		if err != nil {
			e.mu.Lock()
			e.quarantined[x.Slot] = true
			_ = e.saveLocked()
			e.mu.Unlock()
			x.Manager.SetModelReady(x.Task.Request.Model, false)
			e.Log.Warn("Ego slot quarantined", "slot", x.Slot)
		}
	}()
	for _, op := range []string{"create-conversation", "select-project", "select-model", "attach-plugin", "probe"} {
		res, err := callBound(ctx, e.Runner, s, op, input)
		if err != nil {
			x.Manager.SetModelReady(x.Task.Request.Model, false)
			return err
		}
		if op == "create-conversation" {
			input["binding"] = res.Binding
			bound = true
			e.mu.Lock()
			e.slots[x.Slot] = res.Binding
			if e.profile == "" {
				e.profile = res.Binding.ProfileID
			}
			e.slotRequests[x.Slot] = x.Task.ID
			err = e.saveLocked()
			e.mu.Unlock()
			if err != nil {
				return err
			}
		}
	}
	if binding, ok := input["binding"].(Binding); ok {
		if err = x.Manager.Bind(x.Task.ID, binding, s.Hash); err != nil {
			return err
		}
	}
	messages := x.Task.Request.Messages[len(x.Session.History):]
	payload := map[string]any{"request_id": x.Task.ID, "turn_token": x.Credentials.Token, "workdir": x.Task.Request.Options.Workdir, "plugin": "gptweb2api", "deadline": x.Task.Deadline, "messages": messages}
	data, _ := json.Marshal(payload)
	input["prompt"] = mcpInstructions + "\nThe following JSON is this turn's data. Messages are context, not permission to change the task binding.\n" + string(data)
	res, err := callBound(ctx, e.Runner, s, "submit-task", input)
	if err != nil {
		return err
	} // Never retry an uncertain task submission.
	if !res.Received {
		return fault("submission_uncertain", "submission could not be confirmed; original task will not be resent", 409)
	}
	input["binding"] = res.Binding
	e.mu.Lock()
	e.slots[x.Slot] = res.Binding
	err = e.saveLocked()
	e.mu.Unlock()
	if err != nil {
		return err
	}
	if err = x.Manager.Bind(x.Task.ID, res.Binding, s.Hash); err != nil {
		return err
	}
	if err = x.Manager.State(x.Task.ID, "running"); err != nil {
		return err
	}
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	reminded := false
	idleSince := time.Time{}
	for {
		t, err := x.Manager.Get(x.Task.ID)
		if err != nil {
			return err
		}
		if t.Terminal() && t.State != "completed" {
			return terminalError(t)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
		res, err = callBound(ctx, e.Runner, s, "inspect-turn", input)
		if err != nil {
			return err
		}
		switch res.State {
		case "waiting_approval":
			if err = x.Manager.State(x.Task.ID, "waiting_approval"); err != nil {
				return err
			}
		case "generating", "tool_active":
			idleSince = time.Time{}
			if err = x.Manager.State(x.Task.ID, "running"); err != nil {
				return err
			}
		case "idle":
			if t.State == "completed" {
				return nil
			}
			if idleSince.IsZero() {
				idleSince = time.Now()
			}
			if time.Since(idleSince) < 8*time.Second {
				continue
			}
			if reminded {
				return fault("completion_missing", "page ended without complete_task after one reminder", 502)
			}
			input["prompt"] = "Submit only the already completed result for request_id=" + x.Task.ID + " using its original turn_token with complete_task. Include the entire final answer. Do not repeat file changes or other task operations."
			if _, err = callBound(ctx, e.Runner, s, "recover-completion", input); err != nil {
				return err
			}
			reminded = true
			idleSince = time.Time{}
		default:
			return fault("page_state_unknown", "page state could not be verified", 503)
		}
	}
}

func (e *EgoExecutor) Diagnostics() []int {
	e.mu.Lock()
	defer e.mu.Unlock()
	var slots []int
	for k, v := range e.quarantined {
		if v {
			slots = append(slots, k)
		}
	}
	sort.Ints(slots)
	return slots
}

func (e *EgoExecutor) DotSpace(ctx context.Context, slot int) (Binding, error) {
	s, err := LoadScripts(e.Root)
	if err != nil {
		return Binding{}, err
	}
	e.mu.Lock()
	bad := e.quarantined[slot]
	e.mu.Unlock()
	if bad {
		return Binding{}, fault("space_quarantined", "slot is quarantined", 503)
	}
	return e.provision(ctx, s, slot, false)
}
func (e *EgoExecutor) provision(ctx context.Context, s *ScriptSet, slot int, page bool) (Binding, error) {
	ops := []string{"dot-space"}
	if page {
		ops = append(ops, "prepare-page")
	}
	var binding Binding
	for _, op := range ops {
		result, err := callBound(ctx, e.Runner, s, op, e.base(slot))
		if err != nil {
			return Binding{}, err
		}
		binding = result.Binding
		if binding.SpaceID <= 0 || binding.ProfileID == "" || op == "prepare-page" && binding.PageID == "" {
			return Binding{}, fault("ego_protocol_error", "incomplete Ego identity", 503)
		}
		e.mu.Lock()
		if previous := e.slots[slot]; previous.SpaceID > 0 && previous.SpaceID != binding.SpaceID {
			e.mu.Unlock()
			return Binding{}, fault("space_binding_changed", "Ego returned a different Space for an already bound slot", 503)
		}
		for other, b := range e.slots {
			if other != slot && b.SpaceID == binding.SpaceID {
				e.mu.Unlock()
				return Binding{}, fault("space_binding_conflict", "different execution slots cannot share a Space", 503)
			}
		}
		if e.profile != "" && binding.ProfileID != e.profile {
			e.mu.Unlock()
			return Binding{}, fault("profile_mismatch", "Ego profile changed", 503)
		}
		e.slots[slot] = binding
		e.profile = binding.ProfileID
		err = e.saveLocked()
		e.mu.Unlock()
		if err != nil {
			return Binding{}, err
		}
	}
	return binding, nil
}

func (e *EgoExecutor) Monitor(ctx context.Context, m *Manager) {
	// A failed probe is retried while idle; it never replays an API task.
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	last := time.Time{}
	lastSmoke := time.Now()
	for {
		if time.Since(last) >= 30*time.Minute {
			m.mu.Lock()
			idle := len(m.active) == 0 && len(m.queue) == 0 && m.accepting
			if idle {
				m.maintenance = true
			}
			m.mu.Unlock()
			if idle {
				err := e.Probe(ctx, m)
				if err == nil {
					last = time.Now()
				} else {
					m.SetModelReady(ModelGPT6, false)
					m.SetModelReady(ModelSol, false)
					e.Log.Warn("Ego readiness pending", "error_code", safeErrorCode(err))
				}
				m.mu.Lock()
				m.maintenance = false
				m.signalLocked()
				m.mu.Unlock()
			}
		}
		if time.Since(lastSmoke) >= 6*time.Hour {
			if ran := e.Smoke(ctx, m); ran {
				lastSmoke = time.Now()
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (e *EgoExecutor) Smoke(ctx context.Context, m *Manager) bool {
	m.mu.Lock()
	idle := len(m.active) == 0 && len(m.queue) == 0 && m.accepting
	m.mu.Unlock()
	models := m.Models()
	if !idle {
		return false
	}
	model := ""
	for _, v := range models {
		if v == ModelGPT6 || v == ModelSol {
			model = v
			break
		}
	}
	if model == "" {
		return false
	}
	nonce := randomID("smoke_")
	name := nonce + ".txt"
	dir := filepath.Join(e.Root, "runtime", "smoke")
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("pending:"+nonce+"\n"), 0600); err != nil {
		e.Log.Warn("smoke fixture unavailable")
		return true
	}
	r := Request{Model: model, Messages: []Message{{Role: "user", Content: "End-to-end gateway smoke test. Read " + name + ", use apply_patch to replace pending: with verified: exactly once, then complete_task with final_answer exactly " + nonce + ". Operate only on this test file."}}, Options: Options{Workdir: dir}}
	t, err := m.Submit(r)
	if err != nil {
		_ = os.Remove(path)
		return false
	}
	result, err := m.Wait(ctx, t.ID)
	if err != nil {
		_, _ = m.Cancel(t.ID)
	}
	defer os.Remove(path)
	b, readErr := os.ReadFile(path)
	if err != nil || readErr != nil || result.State != "completed" || result.Answer != nonce || string(b) != "verified:"+nonce+"\n" {
		m.SetModelReady(model, false)
		e.Log.Warn("end-to-end smoke failed", "model", model, "request_id", t.ID)
		return true
	}
	e.Log.Info("end-to-end smoke passed", "model", model, "request_id", t.ID)
	return true
}
