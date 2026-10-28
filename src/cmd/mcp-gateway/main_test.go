package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestPIDLockAndStaleIdentity(t *testing.T) {
	// Process lookup is injected: this host's /proc is outside the exec PID
	// namespace. The real flock and durable PID record are exercised below.
	oldStart, oldCommand := processStart, processCommand
	t.Cleanup(func() { processStart, processCommand = oldStart, oldCommand })
	executable, _ := os.Executable()
	executable, _ = filepath.EvalSymlinks(executable)
	processStart = func(int) (string, error) { return "same process generation", nil }
	processCommand = func(int) (string, error) { return executable, nil }
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "runtime"), 0700); err != nil {
		t.Fatal(err)
	}
	f, err := claimPID(root)
	if err != nil {
		t.Fatal(err)
	}
	if second, err := claimPID(root); err == nil {
		second.Close()
		t.Fatal("duplicate process claim succeeded")
	}
	if err = control(root, "status"); err != nil {
		t.Fatal(err)
	}
	// A locked file with an incorrect generation must never signal this PID.
	exe, _ := os.Executable()
	exe, _ = filepath.EvalSymlinks(exe)
	if err = f.Truncate(0); err != nil {
		t.Fatal(err)
	}
	_, _ = f.Seek(0, 0)
	_ = json.NewEncoder(f).Encode(processIdentity{PID: os.Getpid(), Executable: exe, Started: "wrong generation"})
	if err = control(root, "stop"); err == nil {
		t.Fatal("mismatched generation accepted")
	}
	f.Close()
	// Even a stale record naming a live process is ignored without its lock.
	if err = control(root, "stop"); err != nil {
		t.Fatal(err)
	}
	if err = control(root, "status"); err == nil {
		t.Fatal("stale PID reported running")
	}
}
