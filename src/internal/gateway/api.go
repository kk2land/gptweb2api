package gateway

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type API struct {
	manager *Manager
	key     string
}

func NewAPI(m *Manager, key string) http.Handler {
	a := &API{m, key}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health/live", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]bool{"alive": true}) })
	mux.HandleFunc("GET /health/ready", a.auth(func(w http.ResponseWriter, r *http.Request) {
		code := 200
		if len(m.Models()) == 0 {
			code = 503
		}
		writeJSON(w, code, m.Health())
	}))
	mux.HandleFunc("GET /v1/models", a.auth(a.models))
	mux.HandleFunc("POST /v1/chat/completions", a.auth(a.completions))
	mux.HandleFunc("POST /v1/tasks", a.auth(a.submit))
	mux.HandleFunc("GET /v1/tasks/{id}", a.auth(a.get))
	mux.HandleFunc("POST /v1/tasks/{id}/cancel", a.auth(a.cancel))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, fault("not_found", "endpoint is not supported", 404), "")
	})
	return mux
}
func (a *API) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if token == r.Header.Get("Authorization") || subtle.ConstantTimeCompare([]byte(digest(token)), []byte(digest(a.key))) != 1 {
			writeError(w, fault("invalid_api_key", "invalid API key", 401), "")
			return
		}
		next(w, r)
	}
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func errorBody(err error, id string) map[string]any {
	f := &Fault{"internal_error", "internal gateway error", 500}
	var known *Fault
	if errors.As(err, &known) {
		f = known
	}
	e := map[string]any{"message": f.Message, "type": "gateway_error", "code": f.Code}
	if id != "" {
		e["request_id"] = id
	}
	return map[string]any{"error": e}
}
func writeError(w http.ResponseWriter, err error, id string) {
	status := 500
	var f *Fault
	if errors.As(err, &f) && f.HTTP > 0 {
		status = f.HTTP
	}
	writeJSON(w, status, errorBody(err, id))
}
func readRequest(w http.ResponseWriter, r *http.Request) (Request, error) {
	var req Request
	r.Body = http.MaxBytesReader(w, r.Body, 4*1024*1024)
	b, err := io.ReadAll(r.Body)
	if err != nil {
		return req, fault("invalid_request", "request exceeds 4 MiB or cannot be read", 400)
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(b, &fields); err != nil {
		return req, fault("invalid_request", "request must be a JSON object", 400)
	}
	for k := range fields {
		switch k {
		case "model", "messages", "stream", "x_gptweb2api":
		default:
			return req, fault("unsupported_parameter", "unsupported request parameter: "+k, 400)
		}
	}
	d := json.NewDecoder(strings.NewReader(string(b)))
	d.DisallowUnknownFields()
	if err = d.Decode(&req); err != nil {
		return req, fault("invalid_request", "only text messages and documented fields are supported", 400)
	}
	return req, nil
}
func (a *API) models(w http.ResponseWriter, r *http.Request) {
	models := []map[string]any{}
	for _, id := range a.manager.Models() {
		models = append(models, map[string]any{"id": id, "object": "model", "created": 0, "owned_by": "gptweb2api"})
	}
	writeJSON(w, 200, map[string]any{"object": "list", "data": models})
}
func taskHeaders(w http.ResponseWriter, t Task) {
	w.Header().Set("X-Request-ID", t.ID)
	w.Header().Set("Location", "/v1/tasks/"+t.ID)
	if t.Request.Model != ModelDots {
		w.Header().Set("X-Gptweb2api-Session-ID", t.Request.Options.SessionID)
	}
}
func (a *API) submit(w http.ResponseWriter, r *http.Request) {
	req, err := readRequest(w, r)
	if err != nil {
		writeError(w, err, "")
		return
	}
	t, err := a.manager.Submit(req)
	if err != nil {
		writeError(w, err, "")
		return
	}
	taskHeaders(w, t)
	writeJSON(w, 202, t)
}
func (a *API) get(w http.ResponseWriter, r *http.Request) {
	t, err := a.manager.Get(r.PathValue("id"))
	if err != nil {
		writeError(w, err, "")
		return
	}
	taskHeaders(w, t)
	writeJSON(w, 200, t)
}
func (a *API) cancel(w http.ResponseWriter, r *http.Request) {
	t, err := a.manager.Cancel(r.PathValue("id"))
	if err != nil {
		writeError(w, err, "")
		return
	}
	writeJSON(w, 200, t)
}
func (a *API) completions(w http.ResponseWriter, r *http.Request) {
	req, err := readRequest(w, r)
	if err != nil {
		writeError(w, err, "")
		return
	}
	t, err := a.manager.Submit(req)
	if err != nil {
		writeError(w, err, "")
		return
	}
	taskHeaders(w, t)
	if req.Stream {
		a.stream(w, r, t)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), min(a.manager.cfg.TaskTimeout+time.Second, 5*time.Minute))
	defer cancel()
	result, err := a.manager.Wait(ctx, t.ID)
	if err != nil {
		if r.Context().Err() == nil {
			writeError(w, fault("wait_timeout", "task continues; query Location using the request_id", 504), t.ID)
		}
		return
	}
	if result.State != "completed" {
		writeError(w, terminalError(result), t.ID)
		return
	}
	writeJSON(w, 200, completionResponse(result))
}
func terminalError(t Task) error {
	code, msg := "task_failed", "task did not complete"
	if t.Error != nil {
		code, msg = t.Error.Code, t.Error.Message
	}
	status := 502
	if t.State == "expired" {
		status = 504
	}
	if t.State == "cancelled" {
		status = 409
	}
	return fault(code, msg, status)
}
func completionResponse(t Task) map[string]any {
	return map[string]any{"id": t.ID, "object": "chat.completion", "created": t.Created.Unix(), "model": t.Request.Model, "choices": []any{map[string]any{"index": 0, "message": Message{Role: "assistant", Content: t.Answer}, "finish_reason": "stop"}}, "x_gptweb2api": map[string]any{"session_id": t.Request.Options.SessionID, "request_id": t.ID}}
}
func (a *API) stream(w http.ResponseWriter, r *http.Request, t Task) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("Cache-Control", "no-cache")
	controller := http.NewResponseController(w)
	send := func(v any) error {
		_ = controller.SetWriteDeadline(time.Now().Add(10 * time.Second))
		b, _ := json.Marshal(v)
		if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
			return err
		}
		return controller.Flush()
	}
	chunk := func(delta map[string]any, reason any) map[string]any {
		return map[string]any{"id": t.ID, "object": "chat.completion.chunk", "created": t.Created.Unix(), "model": t.Request.Model, "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": reason}}}
	}
	if err := send(chunk(map[string]any{"role": "assistant"}, nil)); err != nil {
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	type waitResult struct {
		t   Task
		err error
	}
	done := make(chan waitResult, 1)
	go func() { v, err := a.manager.Wait(ctx, t.ID); done <- waitResult{v, err} }()
	tick := time.NewTicker(15 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-tick.C:
			_ = controller.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			if controller.Flush() != nil {
				return
			}
		case result := <-done:
			if result.err != nil {
				return
			}
			if result.t.State != "completed" {
				if send(errorBody(terminalError(result.t), t.ID)) != nil {
					return
				}
			} else {
				if send(chunk(map[string]any{"content": result.t.Answer}, nil)) != nil {
					return
				}
				if send(chunk(map[string]any{}, "stop")) != nil {
					return
				}
			}
			_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
			_ = controller.Flush()
			return
		}
	}
}
