package gateway

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func dotIdle(t *testing.T, m *Manager) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for {
		m.mu.Lock()
		busy := m.dotBusy
		ch := m.changed
		m.mu.Unlock()
		if !busy {
			return
		}
		select {
		case <-ch:
		case <-ctx.Done():
			t.Fatal("Dot slot not released")
		}
	}
}

func TestDotEventCalibrationClaimIsolationAndLease(t *testing.T) {
	c := testConfig()
	c.Dots.Enabled = true
	h := newHarness(t, c)
	bridge := NewEvents(h.m, "test-tunnel-principal")
	secret := "whsec_" + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{42}, 32))
	var eventBodies [][]byte
	var eventMu sync.Mutex
	bridge.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(r.Body)
		want, _ := webhookSignature(secret, r.Header.Get("webhook-id"), r.Header.Get("webhook-timestamp"), b)
		if r.Header.Get("webhook-signature") != want || r.Header.Get("X-MCP-Subscription-Id") == "" {
			t.Error("event signature or subscription header missing")
		}
		var payload map[string]any
		_ = json.Unmarshal(b, &payload)
		response := []byte(`{}`)
		if payload["type"] == "verification" {
			response, _ = json.Marshal(map[string]any{"challenge": payload["challenge"]})
		} else {
			eventMu.Lock()
			eventBodies = append(eventBodies, append([]byte{}, b...))
			eventMu.Unlock()
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(response)), Header: http.Header{}}, nil
	})}
	server := NewMCP(h.m, "test")
	if err := bridge.Register(server); err != nil {
		t.Fatal(err)
	}
	st, ct := mcp.NewInMemoryTransports()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Run(ctx, st) }()
	client := mcp.NewClient(&mcp.Implementation{Name: "offline-dot", Version: "1"}, nil)
	for _, method := range []string{"events/list", "events/subscribe", "events/unsubscribe"} {
		if err := mcp.AddSendingCustomMethod[*EventParams, *EventResult](client, method); err != nil {
			t.Fatal(err)
		}
	}
	cs, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cs.Close(); cancel(); <-done }()
	list, err := mcp.CallCustomMethod[*EventParams, *EventResult](ctx, cs, "events/list", &EventParams{})
	if err != nil || len(list.Value["events"].([]any)) != 1 {
		t.Fatalf("event discovery: %v %v", list, err)
	}
	p := &EventParams{Name: "task.available", Arguments: map[string]any{"queue": "default"}}
	p.Delivery.Mode = "webhook"
	p.Delivery.URL = "https://callback.example.com/events"
	p.Delivery.Secret = secret
	sub, err := mcp.CallCustomMethod[*EventParams, *EventResult](ctx, cs, "events/subscribe", p)
	if err != nil {
		t.Fatal(err)
	}
	subID := sub.Value["id"].(string)
	subAgain, err := mcp.CallCustomMethod[*EventParams, *EventResult](ctx, cs, "events/subscribe", p)
	if err != nil || subAgain.Value["id"] != subID {
		t.Fatal("subscription identity was not deterministic", err)
	}
	r := request("not ready")
	r.Model = ModelDots
	_, err = h.m.Submit(r)
	code(t, err, "model_not_ready")
	if err = bridge.deliver(ctx); err != nil {
		t.Fatal(err)
	}
	eventMu.Lock()
	if len(eventBodies) != 1 {
		t.Fatal("calibration event not delivered")
	}
	var event struct {
		Data struct {
			RequestID string `json:"request_id"`
		} `json:"data"`
	}
	_ = json.Unmarshal(eventBodies[0], &event)
	eventMu.Unlock()
	claimArgs := map[string]any{"request_id": event.Data.RequestID, "subscription_id": subID}
	claim := callTool(t, cs, "claim_task", claimArgs, false)
	claim2 := callTool(t, cs, "claim_task", claimArgs, false)
	if claim2["turn_token"] != claim["turn_token"] {
		t.Fatal("claim retry rotated credentials")
	}
	auth := map[string]any{"request_id": claim["request_id"], "turn_token": claim["turn_token"]}
	callTool(t, cs, "start_task", auth, false)
	h.m.mu.Lock()
	nonce := h.m.tasks[event.Data.RequestID].Calibration
	h.m.mu.Unlock()
	auth["status"] = "succeeded"
	auth["final_answer"] = nonce
	callTool(t, cs, "complete_task", auth, false)
	dotIdle(t, h.m)
	if !strings.Contains(strings.Join(h.m.Models(), ","), ModelDots) {
		t.Fatal("successful event calibration did not enable dots")
	}
	fullMessages := []Message{{"system", "system context"}, {"user", "first"}, {"assistant", "prior answer"}, {"user", "next independent task"}}
	r = Request{Model: ModelDots, Messages: fullMessages, Options: Options{Workdir: t.TempDir(), SessionID: "ignored-native-session", ProjectID: "ignored-project"}}
	a := h.submit(t, r)
	b := h.submit(t, r)
	if a.Request.Options.SessionID != "" || a.Request.Options.ProjectID != "" {
		t.Fatal("Dot native routing was retained")
	}
	first := callTool(t, cs, "claim_task", map[string]any{"request_id": a.ID, "subscription_id": subID}, false)
	if len(first["messages"].([]any)) != 4 {
		t.Fatal("full messages were not delivered")
	}
	callTool(t, cs, "claim_task", map[string]any{"request_id": b.ID, "subscription_id": subID}, true)
	// A web task can still use a different global slot while the single Dot is busy.
	web := h.submit(t, request("parallel web"))
	webCall := h.next(t)
	if webCall.x.Task.ID != web.ID {
		t.Fatal("Dot was dispatched through Ego webpage execution")
	}
	dotAuth := Credentials{RequestID: a.ID, Token: first["turn_token"].(string)}
	start(t, h.m, dotAuth)
	if _, err = h.m.renewDot(dotAuth); err != nil {
		t.Fatal(err)
	}
	h.m.mu.Lock()
	task := h.m.tasks[a.ID]
	next := *task
	next.LeaseUntil = time.Now().Add(-time.Second)
	if err = h.m.persistTransitionLocked(task, next, nil); err != nil {
		h.m.mu.Unlock()
		t.Fatal(err)
	}
	h.m.mu.Unlock()
	_, err = h.m.renewDot(dotAuth)
	code(t, err, "task_inactive")
	got, _ := h.m.Get(a.ID)
	if got.Error.Code != "lease_expired" {
		t.Fatal(got)
	}
	dotIdle(t, h.m)
	_, _ = h.m.Cancel(web.ID)
	_, _ = h.m.Cancel(b.ID)
	if _, err = mcp.CallCustomMethod[*EventParams, *EventResult](ctx, cs, "events/unsubscribe", &EventParams{ID: subID}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(h.m.Models(), ","), ModelDots) {
		t.Fatal("expired subscription remained ready")
	}
}

func TestCallbackRejectsNonPublicAddresses(t *testing.T) {
	for _, raw := range []string{"127.0.0.1", "10.1.2.3", "100.64.1.1", "169.254.169.254", "192.168.1.1", "::1", "::ffff:127.0.0.1", "fc00::1", "fe80::1", "2001:db8::1", "224.1.1.1"} {
		if publicIP(netip.MustParseAddr(raw)) {
			t.Errorf("accepted %s", raw)
		}
	}
	for _, raw := range []string{"8.8.8.8", "2606:4700:4700::1111"} {
		if !publicIP(netip.MustParseAddr(raw)) {
			t.Errorf("rejected public %s", raw)
		}
	}
	for _, raw := range []string{"http://example.com/callback", "https://127.0.0.1/x", "https://user:password@example.com/x", "https://example.com/#fragment"} {
		if validateCallback(raw) == nil {
			t.Error("accepted invalid callback", raw)
		}
	}
	for _, secret := range []string{"secret", "whsec_eA==", "whsec_%%%"} {
		if _, err := signingKey(secret); err == nil {
			t.Fatal("invalid signing secret accepted")
		}
	}
}

func TestEventOutboxRetainsExactBodyAndIDAcrossRetry(t *testing.T) {
	c := testConfig()
	c.Dots.Enabled = true
	h := newHarness(t, c)
	bridge := NewEvents(h.m, "principal")
	secret := "whsec_" + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
	var bodies [][]byte
	var ids []string
	bridge.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, b)
		ids = append(ids, r.Header.Get("webhook-id"))
		status := 503
		if len(bodies) > 1 {
			status = 200
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
	})}
	s := Subscription{ID: "sub_test", Owner: "principal", URL: "https://callback.example.com/events", Secret: secret, Expires: time.Now().Add(time.Hour)}
	b, _ := json.Marshal(s)
	_, err := h.s.db.Exec(`INSERT INTO subscriptions VALUES(?,?,?)`, s.ID, s.Expires.UnixMilli(), b)
	if err != nil {
		t.Fatal(err)
	}
	if err = h.m.ensureDotCalibration(); err != nil {
		t.Fatal(err)
	}
	if err = bridge.deliver(context.Background()); err == nil {
		t.Fatal("HTTP 503 treated as delivery success")
	}
	_, _ = h.s.db.Exec(`UPDATE event_outbox SET next_attempt=0`)
	if err = bridge.deliver(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 2 || !bytes.Equal(bodies[0], bodies[1]) || ids[0] != ids[1] {
		t.Fatal("retry changed event identity or signed bytes")
	}
}
