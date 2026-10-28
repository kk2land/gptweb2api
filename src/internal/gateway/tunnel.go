package gateway

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	tunnelclient "github.com/openai/tunnel-client"
)

// These messages are verified against the pinned SDK poller.go. Its Ready
// channel records the first successful poll only; subsequent outages must
// revoke readiness as well. Unknown/new SDK messages do not imply recovery.
type tunnelWriter struct {
	mu      sync.Mutex
	pending []byte
	out     io.Writer
	manager *Manager
}

func (w *tunnelWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.pending = append(w.pending, p...)
	for {
		idx := bytes.IndexByte(w.pending, '\n')
		if idx < 0 {
			break
		}
		line := w.pending[:idx+1]
		msg := string(line)
		if strings.Contains(msg, `msg="poll failed; backing off"`) || strings.Contains(msg, `msg="poll timed out; backing off"`) || strings.Contains(msg, `msg="poller stopped"`) {
			w.manager.SetTunnelReady(false)
		}
		if strings.Contains(msg, `msg="poller recovered; polling operational"`) {
			w.manager.SetTunnelReady(true)
		}
		if _, err := w.out.Write(line); err != nil {
			return 0, err
		}
		w.pending = w.pending[idx+1:]
	}
	if len(w.pending) > 65536 {
		w.pending = []byte("[oversized SDK log record omitted]\n")
	}
	return len(p), nil
}

func RunTunnel(ctx context.Context, cfg Config, m *Manager, server *mcp.Server, logs io.Writer) error {
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverCtx, stopServer := context.WithCancel(context.Background())
	defer stopServer()
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.Run(serverCtx, serverTransport) }()
	client, err := tunnelclient.New(tunnelclient.Config{TunnelID: cfg.Tunnel.ID, APIKey: cfg.Tunnel.APIKey, LogLevel: slog.LevelInfo, LogWriter: &tunnelWriter{out: logs, manager: m}}, clientTransport)
	if err != nil {
		return errors.New("cannot construct the pinned Tunnel SDK; check tunnel identity")
	}
	startCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	err = client.Start(startCtx)
	cancel()
	if err != nil {
		return errors.New("Tunnel SDK startup failed")
	}
	defer func() {
		m.SetTunnelReady(false)
		stopCtx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		_ = client.Stop(stopCtx)
	}()
	go func() {
		if client.WaitUntilReady(serverCtx) == nil {
			m.SetTunnelReady(true)
		}
	}()
	select {
	case <-ctx.Done():
		return nil
	case <-client.Done():
		return errors.New("Tunnel SDK stopped unexpectedly")
	case <-serverDone:
		return errors.New("in-memory MCP server stopped unexpectedly")
	}
}
