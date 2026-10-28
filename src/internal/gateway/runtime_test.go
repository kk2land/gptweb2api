package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	tunnelclient "github.com/openai/tunnel-client"
	"github.com/openai/tunnel-client/testsupport/mocktunnelservice"
)

func TestConfigStrictAndSecretSafe(t *testing.T) {
	b, err := os.ReadFile("../../../config/mcp-gateway.example.toml")
	if err != nil {
		t.Fatal(err)
	}
	good := strings.NewReplacer("REPLACE_WITH_API_KEY", "client-local-test", "REPLACE_WITH_TUNNEL_ID", "tunnel_localtestaaaaaaaaaaaaaaaaaaaaaaa", "REPLACE_WITH_RUNTIME_API_KEY", "runtime-local-test").Replace(string(b))
	for _, test := range []struct {
		name, text string
		valid      bool
	}{
		{"valid", good, true}, {"placeholders", string(b), false}, {"unknown", good + "\nunknown_field=1\n", false}, {"public listen", strings.Replace(good, "127.0.0.1", "0.0.0.0", 1), false}, {"zero concurrency", strings.Replace(good, "max_concurrency = 2", "max_concurrency = 0", 1), false}, {"invalid duration", strings.Replace(good, `timeout = "30m"`, `timeout = "never"`, 1), false}, {"wrong field type", strings.Replace(good, `api_key = "client-local-test"`, `api_key = 123`, 1), false}, {"parse secret", strings.Replace(good, `"runtime-local-test"`, `runtime-secret-do-not-print`, 1), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			_ = os.Mkdir(filepath.Join(root, "config"), 0700)
			_ = os.WriteFile(filepath.Join(root, "config/mcp-gateway.toml"), []byte(test.text), 0600)
			_, err := LoadConfig(root)
			if (err == nil) != test.valid {
				t.Fatalf("valid=%v error=%v", test.valid, err)
			}
			if err != nil && (strings.Contains(err.Error(), "runtime-secret") || strings.Contains(err.Error(), "runtime-local-test") || strings.Contains(err.Error(), "client-local-test")) {
				t.Fatal("credential appeared in error")
			}
		})
	}
}
func TestLogRotationRedactionAndBoundedRecords(t *testing.T) {
	p := filepath.Join(t.TempDir(), "gateway.log")
	l, err := OpenLog(p, 1, 2, "secret-split-across-writes")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = l.Write([]byte("key=secret-split-"))
	_, _ = l.Write([]byte("across-writes\n"))
	b, _ := os.ReadFile(p)
	if bytes.Contains(b, []byte("secret-split")) || !bytes.Contains(b, []byte("[REDACTED]")) {
		t.Fatal(string(b))
	}
	for i := 0; i < 120; i++ {
		_, err = l.Write([]byte(strings.Repeat("x", 32000) + "\n"))
		if err != nil {
			t.Fatal(err)
		}
	}
	_, _ = l.Write([]byte(strings.Repeat("y", 200000) + "\n"))
	if err = l.Close(); err != nil {
		t.Fatal(err)
	}
	files, _ := filepath.Glob(p + "*")
	if len(files) != 3 {
		t.Fatal(files)
	}
	for _, f := range files {
		st, _ := os.Stat(f)
		if st.Size() > 1024*1024 || st.Mode().Perm() != 0600 {
			t.Fatal("log size or permissions", st)
		}
	}
}
func TestPinnedSDKForwardsRealTaskToolsInMemory(t *testing.T) {
	h := newHarness(t, testConfig())
	task := h.submit(t, request("SDK round trip"))
	call := h.next(t)
	tunnelID := "tunnel_sdktestaaaaaaaaaaaaaaaaaaaaaaaaa"
	makeCommand := func(id, name string, args any, check func(map[string]any)) mocktunnelservice.CommandResponse {
		payload, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": "tools/call", "params": map[string]any{"name": name, "arguments": args}})
		return mocktunnelservice.CommandResponse{Command: mocktunnelservice.NewCommand(id, payload, nil), ExpectedResponses: []mocktunnelservice.ExpectedResponse{{RequestID: id, Assert: func(tb testing.TB, r mocktunnelservice.ReceivedResponse) {
			var response struct {
				Result struct {
					IsError bool `json:"isError"`
					Content []struct {
						Text string `json:"text"`
					} `json:"content"`
				} `json:"result"`
			}
			if r.ResponseCode != 200 || json.Unmarshal(r.JSONResponse, &response) != nil || response.Result.IsError || len(response.Result.Content) != 1 {
				tb.Fatalf("unexpected tool response: %s", r.JSONResponse)
			}
			var out map[string]any
			if err := json.Unmarshal([]byte(response.Result.Content[0].Text), &out); err != nil {
				tb.Fatal(err)
			}
			check(out)
		}}}}
	}
	auth := map[string]any{"request_id": task.ID, "turn_token": call.x.Credentials.Token}
	complete := map[string]any{"request_id": task.ID, "turn_token": call.x.Credentials.Token, "status": "succeeded", "final_answer": "full answer from SDK"}
	control := mocktunnelservice.NewMockTunnelService(mocktunnelservice.WithAPIKey("offline-runtime-key"), mocktunnelservice.WithTunnelID(tunnelID), mocktunnelservice.WithInitializationPhaseCommandsWithoutSessionHeaders(), mocktunnelservice.WithCommandResponses(
		makeCommand("begin", "start_task", auth, func(out map[string]any) {
			if out["request_id"] != task.ID {
				t.Error(out)
			}
		}),
		makeCommand("finish", "complete_task", complete, func(out map[string]any) {
			if out["accepted"] != true {
				t.Error(out)
			}
		}),
	))
	control.Start(t)
	st, ct := mcp.NewInMemoryTransports()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- NewMCP(h.m, "sdk-test").Run(ctx, st) }()
	client, err := tunnelclient.New(tunnelclient.Config{TunnelID: tunnelID, APIKey: "offline-runtime-key", ControlPlaneBaseURL: control.BaseURL().String(), PollTimeout: 10 * time.Millisecond, LogWriter: io.Discard}, ct)
	if err != nil {
		t.Fatal(err)
	}
	startCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	if err = client.Start(startCtx); err != nil {
		t.Fatal(err)
	}
	defer func() {
		stopCtx, stop := context.WithTimeout(context.Background(), 3*time.Second)
		defer stop()
		if err := client.Stop(stopCtx); err != nil {
			t.Error(err)
		}
		cancel()
		<-done
	}()
	if err = client.WaitUntilReady(startCtx); err != nil {
		t.Fatal(err)
	}
	if err = control.WaitUntilIdle(startCtx); err != nil {
		t.Fatal(err)
	}
	got, err := h.m.Get(task.ID)
	if err != nil || got.State != "completed" || got.Answer != "full answer from SDK" {
		t.Fatalf("task result: %+v %v", got, err)
	}
	close(call.release)
}
func TestTunnelOutageRevokesReadiness(t *testing.T) {
	h := newHarness(t, testConfig())
	var out bytes.Buffer
	w := &tunnelWriter{out: &out, manager: h.m}
	_, _ = w.Write([]byte("level=WARN msg=\"poll failed; "))
	_, _ = w.Write([]byte("backing off\" error=offline\n"))
	if len(h.m.Models()) != 0 {
		t.Fatal("first-ready flag hid a later outage")
	}
	_, _ = w.Write([]byte("level=INFO msg=\"poller recovered; polling operational\"\n"))
	if len(h.m.Models()) == 0 {
		t.Fatal("verified recovery not reflected")
	}
}

func TestPinnedSDKForwardsMCP2EventDiscovery(t *testing.T) {
	c := testConfig()
	c.Dots.Enabled = true
	h := newHarness(t, c)
	server := NewMCP(h.m, "event-sdk-test")
	bridge := NewEvents(h.m, "principal")
	if err := bridge.Register(server); err != nil {
		t.Fatal(err)
	}
	meta := map[string]any{mcp.MetaKeyProtocolVersion: "2026-07-28", mcp.MetaKeyClientInfo: map[string]any{"name": "test-dot", "version": "1"}, mcp.MetaKeyClientCapabilities: map[string]any{}}
	command := func(id, method string) mocktunnelservice.CommandResponse {
		payload, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": map[string]any{"_meta": meta}})
		return mocktunnelservice.CommandResponse{Command: mocktunnelservice.NewCommand(id, payload, nil), ExpectedResponses: []mocktunnelservice.ExpectedResponse{{RequestID: id, Assert: func(tb testing.TB, r mocktunnelservice.ReceivedResponse) {
			var response map[string]any
			if json.Unmarshal(r.JSONResponse, &response) != nil || response["error"] != nil {
				tb.Fatalf("event RPC failed: %s", r.JSONResponse)
			}
			result, ok := response["result"].(map[string]any)
			if !ok {
				tb.Fatal("missing result")
				return
			}
			if method == "server/discover" {
				caps, _ := result["capabilities"].(map[string]any)
				if caps["events"] == nil {
					tb.Error("events capability missing")
				}
			} else if events, ok := result["events"].([]any); !ok || len(events) != 1 {
				tb.Error("event catalog missing")
			}
		}}}}
	}
	id := "tunnel_sdktestaaaaaaaaaaaaaaaaaaaaaaaaa"
	control := mocktunnelservice.NewMockTunnelService(mocktunnelservice.WithAPIKey("event-test-key"), mocktunnelservice.WithTunnelID(id), mocktunnelservice.WithCommandResponses(command("discover", "server/discover"), command("events", "events/list")))
	control.Start(t)
	st, ct := mcp.NewInMemoryTransports()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Run(ctx, st) }()
	client, err := tunnelclient.New(tunnelclient.Config{TunnelID: id, APIKey: "event-test-key", ControlPlaneBaseURL: control.BaseURL().String(), PollTimeout: 10 * time.Millisecond, LogWriter: io.Discard}, ct)
	if err != nil {
		t.Fatal(err)
	}
	deadline, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	if err = client.Start(deadline); err != nil {
		t.Fatal(err)
	}
	defer func() {
		stopCtx, stop := context.WithTimeout(context.Background(), 3*time.Second)
		defer stop()
		if err := client.Stop(stopCtx); err != nil {
			t.Error(err)
		}
		cancel()
		<-done
	}()
	if err = client.WaitUntilReady(deadline); err != nil {
		t.Fatal(err)
	}
	if err = control.WaitUntilIdle(deadline); err != nil {
		t.Fatal(err)
	}
}
