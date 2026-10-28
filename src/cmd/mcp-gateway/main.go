package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/kk2land/gptweb2api/src/internal/gateway"
)

var version = "dev"

type processIdentity struct {
	PID        int    `json:"pid"`
	Executable string `json:"executable"`
	Started    string `json:"started"`
}

var processStart = func(pid int) (string, error) {
	b, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "lstart=").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}
var processCommand = func(pid int) (string, error) {
	b, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "command=").Output()
	return strings.TrimSpace(string(b)), err
}

func rootDirectory() (string, error) {
	p, err := os.Executable()
	if err != nil {
		return "", err
	}
	p, err = filepath.EvalSymlinks(p)
	if err != nil {
		return "", err
	}
	if filepath.Base(filepath.Dir(p)) != "bin" {
		return "", errors.New("binary must be installed at <project>/bin/mcp-gateway")
	}
	return filepath.Dir(filepath.Dir(p)), nil
}
func pidFile(root string) (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(root, "runtime", "mcp-gateway.pid"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, errors.New("gateway is already running")
	}
	return f, nil
}
func claimPID(root string) (*os.File, error) {
	f, err := pidFile(root)
	if err != nil {
		return nil, err
	}
	exe, err := os.Executable()
	if err != nil {
		f.Close()
		return nil, err
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		f.Close()
		return nil, err
	}
	start, err := processStart(os.Getpid())
	if err != nil {
		f.Close()
		return nil, err
	}
	if err = f.Truncate(0); err != nil {
		f.Close()
		return nil, err
	}
	_, err = f.Seek(0, 0)
	if err == nil {
		err = json.NewEncoder(f).Encode(processIdentity{os.Getpid(), exe, start})
	}
	if err == nil {
		err = f.Sync()
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}
func control(root, action string) error {
	f, err := os.OpenFile(filepath.Join(root, "runtime", "mcp-gateway.pid"), os.O_RDWR, 0600)
	if err != nil {
		fmt.Println("stopped")
		if action == "status" {
			return errors.New("gateway is not running")
		}
		return nil
	}
	defer f.Close()
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
		fmt.Println("stopped (stale PID file ignored)")
		if action == "status" {
			return errors.New("gateway is not running")
		}
		return nil
	}
	var id processIdentity
	if err = json.NewDecoder(f).Decode(&id); err != nil {
		return errors.New("invalid PID identity; refusing to signal")
	}
	exe, _ := os.Executable()
	exe, _ = filepath.EvalSymlinks(exe)
	start, err := processStart(id.PID)
	cmd, cmdErr := processCommand(id.PID)
	if err != nil || cmdErr != nil || id.PID <= 1 || start != id.Started || id.Executable != exe || !(cmd == exe || strings.HasPrefix(cmd, exe+" ")) {
		return errors.New("PID identity mismatch; refusing to signal")
	}
	if action == "status" {
		fmt.Printf("running (pid %d)\n", id.PID)
		return nil
	}
	if action != "stop" {
		return errors.New("unsupported control action")
	}
	if err = syscall.Kill(id.PID, syscall.SIGTERM); err != nil {
		return err
	}
	// Observe the exact locked inode. Never send SIGKILL to a reused PID.
	deadline := time.NewTimer(90 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-deadline.C:
			return errors.New("gateway did not stop within 90s; inspect its log")
		case <-tick.C:
			if syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) == nil {
				fmt.Println("stopped")
				return nil
			}
		}
	}
}
func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	check := flag.Bool("check-config", false, "validate the fixed configuration and scripts without connecting")
	showVersion := flag.Bool("version", false, "print build version")
	action := flag.String("control", "", "status or stop, with PID identity verification")
	flag.Parse()
	if flag.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	if *showVersion {
		fmt.Println(version)
		return nil
	}
	root, err := rootDirectory()
	if err != nil {
		return err
	}
	if *action != "" {
		return control(root, *action)
	}
	cfg, err := gateway.LoadConfig(root)
	if err != nil {
		return err
	}
	if _, err = gateway.LoadScripts(root); err != nil {
		return err
	}
	if *check {
		fmt.Println("configuration and script set valid")
		return nil
	}
	if err = gateway.PrepareRuntime(root); err != nil {
		return err
	}
	pid, err := claimPID(root)
	if err != nil {
		return err
	}
	defer pid.Close()
	logs, err := gateway.OpenLog(filepath.Join(root, "runtime/logs/mcp-gateway.log"), cfg.Log.MaxSizeMB, cfg.Log.MaxBackups, cfg.Server.APIKey, cfg.Tunnel.APIKey)
	if err != nil {
		return err
	}
	defer logs.Close()
	logger := slog.New(slog.NewTextHandler(logs, nil))
	store, err := gateway.OpenStore(filepath.Join(root, "runtime/data/mcp-gateway.sqlite"))
	if err != nil {
		return err
	}
	defer store.Close()
	ego := gateway.NewEgo(root, logger, cfg.Ego.SpaceName)
	if err = ego.Restore(store); err != nil {
		return err
	}
	manager, err := gateway.NewManager(store, cfg, logger, ego)
	if err != nil {
		return err
	}
	if cfg.Dots.Enabled {
		logger.Warn("dots enabled in configuration; model remains unavailable until event and execution readiness is proven")
	}
	server := gateway.NewMCP(manager, version)
	events := gateway.NewEvents(manager, cfg.Tunnel.ID)
	if err := events.Register(server); err != nil {
		return err
	}
	listen, err := net.Listen("tcp", cfg.Server.Listen)
	if err != nil {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = manager.Drain(ctx)
		return err
	}
	httpServer := &http.Server{Handler: gateway.NewAPI(manager, cfg.Server.APIKey), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, IdleTimeout: 90 * time.Second, MaxHeaderBytes: 64 * 1024}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	tunnelCtx, stopTunnel := context.WithCancel(context.Background())
	defer stopTunnel()
	monitorCtx, stopMonitor := context.WithCancel(context.Background())
	defer stopMonitor()
	errorsCh := make(chan error, 2)
	tunnelDone := make(chan struct{})
	monitorDone := make(chan struct{})
	eventsDone := make(chan struct{})
	go func() { defer close(tunnelDone); errorsCh <- gateway.RunTunnel(tunnelCtx, cfg, manager, server, logs) }()
	go func() { defer close(monitorDone); ego.Monitor(monitorCtx, manager) }()
	go func() { defer close(eventsDone); events.Run(tunnelCtx) }()
	go func() {
		err := httpServer.Serve(listen)
		if !errors.Is(err, http.ErrServerClosed) {
			errorsCh <- err
		}
	}()
	logger.Info("gateway started", "version", version, "listen", cfg.Server.Listen)
	select {
	case <-ctx.Done():
	case err = <-errorsCh:
		logger.Error("gateway component stopped", "error", err)
	}
	manager.BeginDrain()
	stopMonitor()
	<-monitorDone
	// Keep MCP and HTTP alive while accepted tasks drain. Reject new work.
	drainCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	drainErr := manager.Drain(drainCtx)
	cancel()
	if drainErr != nil {
		logger.Warn("drain deadline reached")
	}
	stopTunnel()
	<-eventsDone
	<-tunnelDone
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(shutdownCtx)
	logger.Info("gateway stopped")
	return err
}
