package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type executionCall struct {
	x       *Execution
	release chan struct{}
}
type testExecutor struct{ calls chan executionCall }

func (e *testExecutor) Run(ctx context.Context, x *Execution) error {
	call := executionCall{x, make(chan struct{})}
	select {
	case e.calls <- call:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-call.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (e *testExecutor) DotSpace(ctx context.Context, slot int) (Binding, error) {
	return Binding{SpaceID: slot + 1, ProfileID: "test-profile"}, nil
}
func testConfig() Config {
	var c Config
	c.Tasks.MaxConcurrency = 2
	c.Tasks.QueueCapacity = 4
	c.TaskTimeout = time.Minute
	c.Tasks.Timeout = "1m"
	c.Server.APIKey = "client-test-key"
	return c
}

type harness struct {
	m   *Manager
	s   *Store
	e   *testExecutor
	cfg Config
}

func newHarness(t *testing.T, c Config) *harness {
	t.Helper()
	s, err := OpenStore(filepath.Join(t.TempDir(), "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	e := &testExecutor{calls: make(chan executionCall, 16)}
	m, err := NewManager(s, c, slog.New(slog.NewTextHandler(io.Discard, nil)), e)
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{m, s, e, c}
	m.SetTunnelReady(true)
	m.MCPSeen()
	m.SetModelReady(ModelGPT6, true)
	m.SetModelReady(ModelSol, true)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
		defer cancel()
		_ = m.Drain(ctx)
		_ = s.Close()
	})
	return h
}
func (h *harness) submit(t *testing.T, r Request) Task {
	t.Helper()
	v, err := h.m.Submit(r)
	if err != nil {
		t.Fatal(err)
	}
	return v
}
func (h *harness) next(t *testing.T) executionCall {
	t.Helper()
	select {
	case c := <-h.e.calls:
		return c
	case <-time.After(3 * time.Second):
		t.Fatal("execution was not dispatched")
		return executionCall{}
	}
}
func request(content string) Request {
	return Request{Model: ModelGPT6, Messages: []Message{{"user", content}}}
}
func start(t *testing.T, m *Manager, c Credentials) {
	t.Helper()
	if _, err := m.StartTask(c); err != nil {
		t.Fatal(err)
	}
}
func finish(t *testing.T, m *Manager, c Credentials, answer string) {
	t.Helper()
	if _, err := m.Complete(c, Completion{Status: "succeeded", Answer: answer}); err != nil {
		t.Fatal(err)
	}
}
func code(t *testing.T, err error, want string) {
	t.Helper()
	var f *Fault
	if !errors.As(err, &f) || f.Code != want {
		t.Fatalf("wanted %s, got %v", want, err)
	}
}

func TestCompletionPersistenceConflictAndLateWaiter(t *testing.T) {
	h := newHarness(t, testConfig())
	task := h.submit(t, request("first"))
	call := h.next(t)
	start(t, h.m, call.x.Credentials)
	_, err := h.m.Complete(Credentials{task.ID, "wrong"}, Completion{Status: "succeeded", Answer: "answer"})
	code(t, err, "invalid_capability")
	finish(t, h.m, call.x.Credentials, "answer")
	// A waiter installed after completion must read persisted state, not miss a signal.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	got, err := h.m.Wait(ctx, task.ID)
	if err != nil || got.Answer != "answer" {
		t.Fatalf("late waiter: %+v %v", got, err)
	}
	persisted, err := h.s.loadTask(task.ID)
	if err != nil || persisted.Answer != "answer" || persisted.TokenHash == "" {
		t.Fatalf("completion not durable: %+v %v", persisted, err)
	}
	ack, err := h.m.Complete(call.x.Credentials, Completion{Status: "succeeded", Answer: "answer"})
	if err != nil || ack["duplicate"] != true {
		t.Fatalf("duplicate: %v %v", ack, err)
	}
	_, err = h.m.Complete(call.x.Credentials, Completion{Status: "succeeded", Answer: "different"})
	code(t, err, "completion_conflict")
	_, err = h.m.StartTask(call.x.Credentials)
	code(t, err, "task_inactive")
	close(call.release)
}
func TestGlobalSlotsSessionSerialAndHistory(t *testing.T) {
	c := testConfig()
	h := newHarness(t, c)
	r := request("one")
	a := h.submit(t, r)
	first := h.next(t)
	secondReq := Request{Model: ModelGPT6, Options: a.Request.Options, Messages: []Message{{"user", "one"}, {"assistant", "answer one"}, {"user", "two"}}}
	h.submit(t, secondReq)
	other := h.submit(t, request("independent"))
	parallel := h.next(t)
	if parallel.x.Task.ID != other.ID || parallel.x.Slot == first.x.Slot {
		t.Fatal("independent requests did not receive exclusive slots")
	}
	select {
	case <-h.e.calls:
		t.Fatal("same session executed concurrently")
	case <-time.After(30 * time.Millisecond):
	}
	start(t, h.m, first.x.Credentials)
	finish(t, h.m, first.x.Credentials, "answer one")
	select {
	case <-h.e.calls:
		t.Fatal("slot released before browser cleanup")
	case <-time.After(30 * time.Millisecond):
	}
	close(first.release)
	second := h.next(t)
	if second.x.Task.Request.Options.SessionID != a.Request.Options.SessionID {
		t.Fatal("wrong continuation")
	}
	bad := secondReq
	bad.Messages = []Message{{"user", "changed history"}}
	_, err := h.m.Submit(bad)
	code(t, err, "history_conflict")
	bad = secondReq
	bad.Options.Workdir = t.TempDir()
	_, err = h.m.Submit(bad)
	if err == nil {
		t.Fatal("continuation changed bound directory")
	}
	_, _ = h.m.Cancel(parallel.x.Task.ID)
	_, _ = h.m.Cancel(second.x.Task.ID)
}
func TestQueueLimitDeadlineAndDisconnect(t *testing.T) {
	c := testConfig()
	c.Tasks.MaxConcurrency = 1
	c.Tasks.QueueCapacity = 1
	c.TaskTimeout = 150 * time.Millisecond
	h := newHarness(t, c)
	active := h.submit(t, request("active"))
	h.next(t)
	queued := h.submit(t, request("queued"))
	_, err := h.m.Submit(request("overflow"))
	code(t, err, "queue_full")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = h.m.Wait(ctx, active.ID)
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	v, _ := h.m.Get(active.ID)
	if v.Terminal() {
		t.Fatal("client disconnect cancelled the task")
	}
	waitCtx, stop := context.WithTimeout(context.Background(), 3*time.Second)
	defer stop()
	v, err = h.m.Wait(waitCtx, queued.ID)
	if err != nil || v.State != "expired" {
		t.Fatalf("queued deadline: %s %v", v.State, err)
	}
}
func TestCancelVersusCompleteHasSingleWinner(t *testing.T) {
	h := newHarness(t, testConfig())
	task := h.submit(t, request("race"))
	call := h.next(t)
	start(t, h.m, call.x.Credentials)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _, _ = h.m.Cancel(task.ID) }()
	go func() {
		defer wg.Done()
		_, _ = h.m.Complete(call.x.Credentials, Completion{Status: "succeeded", Answer: "done"})
	}()
	wg.Wait()
	got, _ := h.m.Get(task.ID)
	if got.State != "completed" && got.State != "cancelled" {
		t.Fatal(got.State)
	}
	persisted, _ := h.s.loadTask(task.ID)
	if persisted.State != got.State {
		t.Fatal("terminal winner not durable")
	}
	_, err := h.m.StartTask(call.x.Credentials)
	code(t, err, "task_inactive")
	close(call.release)
}

func mcpSession(t *testing.T, h *harness) *mcp.ClientSession {
	t.Helper()
	server := NewMCP(h.m, "test")
	st, ct := mcp.NewInMemoryTransports()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Run(ctx, st) }()
	client := mcp.NewClient(&mcp.Implementation{Name: "offline-test", Version: "1"}, nil)
	session, err := client.Connect(ctx, ct, nil)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close(); cancel(); <-done })
	return session
}
func callTool(t *testing.T, s *mcp.ClientSession, name string, args map[string]any, wantError bool) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	res, err := s.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError != wantError {
		t.Fatalf("%s isError=%v: %+v", name, res.IsError, res.Content)
	}
	var out map[string]any
	if len(res.Content) == 0 {
		t.Fatal("empty tool response")
	}
	if err = json.Unmarshal([]byte(res.Content[0].(*mcp.TextContent).Text), &out); err != nil {
		t.Fatal(err)
	}
	return out
}
func TestMCPFilesCapabilitiesAndIdempotentPatch(t *testing.T) {
	h := newHarness(t, testConfig())
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "file.txt"), []byte("before\n"), 0644); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(outside, "secret"), []byte("outside"), 0600)
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	r := request("edit")
	r.Options.Workdir = root
	h.submit(t, r)
	call := h.next(t)
	cs := mcpSession(t, h)
	auth := map[string]any{"request_id": call.x.Task.ID, "turn_token": call.x.Credentials.Token}
	callTool(t, cs, "start_task", auth, false)
	args := map[string]any{"request_id": call.x.Task.ID, "turn_token": call.x.Credentials.Token, "path": "escape/secret"}
	callTool(t, cs, "read_file", args, true)
	args["path"] = "../secret"
	callTool(t, cs, "read_file", args, true)
	args["path"] = "file.txt"
	got := callTool(t, cs, "read_file", args, false)
	if got["content"] != "before\n" {
		t.Fatal(got)
	}
	args["operation_id"] = "edit-1"
	args["old_text"] = "before"
	args["new_text"] = "after"
	callTool(t, cs, "apply_patch", args, false)
	callTool(t, cs, "apply_patch", args, false)
	b, _ := os.ReadFile(filepath.Join(root, "file.txt"))
	if string(b) != "after\n" {
		t.Fatal("write was not applied exactly once")
	}
	args["new_text"] = "conflict"
	callTool(t, cs, "apply_patch", args, true)
	callTool(t, cs, "run_command", auth, true)
	finish(t, h.m, call.x.Credentials, "updated")
	callTool(t, cs, "read_file", map[string]any{"request_id": call.x.Task.ID, "turn_token": call.x.Credentials.Token, "path": "file.txt"}, true)
	close(call.release)
}
func TestDirectoryReplacementRevokesCapability(t *testing.T) {
	h := newHarness(t, testConfig())
	parent := t.TempDir()
	root := filepath.Join(parent, "project")
	_ = os.Mkdir(root, 0700)
	r := request("edit")
	r.Options.Workdir = root
	h.submit(t, r)
	call := h.next(t)
	start(t, h.m, call.x.Credentials)
	if err := os.Rename(root, root+"-old"); err != nil {
		t.Fatal(err)
	}
	_ = os.Mkdir(root, 0700)
	_, err := h.m.fileOperation(context.Background(), call.x.Credentials, "", "", func(root *os.Root) (any, error) {
		t.Fatal("operation reached a replacement directory")
		return nil, nil
	})
	code(t, err, "workdir_identity_changed")
}
func TestCompletionWaitsForInFlightTool(t *testing.T) {
	h := newHarness(t, testConfig())
	r := request("read")
	r.Options.Workdir = t.TempDir()
	h.submit(t, r)
	call := h.next(t)
	start(t, h.m, call.x.Credentials)
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = h.m.fileOperation(context.Background(), call.x.Credentials, "", "", func(root *os.Root) (any, error) { close(entered); <-release; return "done", nil })
	}()
	<-entered
	_, err := h.m.Complete(call.x.Credentials, Completion{Status: "succeeded", Answer: "too soon"})
	code(t, err, "tools_active")
	close(release)
	<-done
	finish(t, h.m, call.x.Credentials, "finished")
	close(call.release)
}

func TestAPIRejectsUnsupportedSemanticsAndStreamsFailure(t *testing.T) {
	h := newHarness(t, testConfig())
	handler := NewAPI(h.m, h.cfg.Server.APIKey)
	invoke := func(path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+h.cfg.Server.APIKey)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	w := invoke("/v1/tasks", `{"model":"web/gpt-6","messages":[{"role":"user","content":"x"}],"temperature":0}`)
	if w.Code != 400 {
		t.Fatal(w.Code)
	}
	w = invoke("/v1/tasks", `{"model":"dots","messages":[{"role":"user","content":"x"}]}`)
	if w.Code != 404 {
		t.Fatal(w.Code)
	}
	w = invoke("/v1/responses", `{}`)
	if w.Code != 404 {
		t.Fatal(w.Code)
	}
	r := httptest.NewRequest("GET", "/v1/models", nil)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal("API key was not required")
	}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- invoke("/v1/chat/completions", `{"model":"web/gpt-6","messages":[{"role":"user","content":"x"}],"stream":true}`)
	}()
	call := h.next(t)
	_, _ = h.m.Cancel(call.x.Task.ID)
	w = <-done
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"error"`) || strings.Contains(w.Body.String(), `"finish_reason":"stop"`) || !strings.Contains(w.Body.String(), "[DONE]") {
		t.Fatal(w.Body.String())
	}
	if w.Header().Get("X-Request-ID") == "" {
		t.Fatal("missing queryable request identifier")
	}
}
func TestReadinessNeverAdvertisesUnverifiedModels(t *testing.T) {
	h := newHarness(t, testConfig())
	h.m.SetTunnelReady(false)
	if len(h.m.Models()) != 0 {
		t.Fatal("offline models advertised")
	}
	_, err := h.m.Submit(request("x"))
	code(t, err, "model_not_ready")
	h.m.SetTunnelReady(true)
	h.m.SetModelReady(ModelGPT6, false)
	if len(h.m.Models()) != 1 || h.m.Models()[0] != ModelSol {
		t.Fatal(h.m.Models())
	}
}

func TestCrashRecoveryPreservesQueueAndFailsUncertainExecution(t *testing.T) {
	s, err := OpenStore(filepath.Join(t.TempDir(), "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now()
	for _, state := range []string{"queued", "running"} {
		session := &Session{ID: "sess_" + state, Model: ModelGPT6, Valid: true}
		task := &Task{ID: "req_" + state, State: state, Created: now, Updated: now, Deadline: now.Add(time.Minute), Request: Request{Model: ModelGPT6, Options: Options{SessionID: session.ID}, Messages: []Message{{"user", "x"}}}}
		if err = s.save(task, session); err != nil {
			t.Fatal(err)
		}
	}
	e := &testExecutor{calls: make(chan executionCall, 4)}
	m, err := NewManager(s, testConfig(), slog.New(slog.NewTextHandler(io.Discard, nil)), e)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
		defer cancel()
		_ = m.Drain(ctx)
	}()
	running, _ := m.Get("req_running")
	queued, _ := m.Get("req_queued")
	if running.State != "failed" || running.Error.Code != "restart_uncertain" || queued.State != "queued" {
		t.Fatalf("%+v %+v", running, queued)
	}
	ss, _ := s.session("sess_running")
	if ss.Valid {
		t.Fatal("uncertain session remained usable")
	}
}

var _ http.Handler
