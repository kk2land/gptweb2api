package gateway

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type runnerFunc func(context.Context, *ScriptSet, string, map[string]any) (EgoResult, error)

func (f runnerFunc) Call(ctx context.Context, s *ScriptSet, op string, input map[string]any) (EgoResult, error) {
	return f(ctx, s, op, input)
}

func TestUncertainEgoSubmissionNeverReplaysTask(t *testing.T) {
	h := newHarness(t, testConfig())
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	ego := NewEgo(root, slog.New(slog.NewTextHandler(io.Discard, nil)), "")
	var mu sync.Mutex
	calls := map[string]int{}
	hash := ""
	ego.Runner = runnerFunc(func(ctx context.Context, s *ScriptSet, op string, input map[string]any) (EgoResult, error) {
		mu.Lock()
		calls[op]++
		if hash == "" {
			hash = s.Hash
		} else if hash != s.Hash {
			t.Error("mixed script versions in one attempt")
		}
		mu.Unlock()
		if op == "submit-task" {
			return EgoResult{}, fault("submission_uncertain", "cannot establish whether submission succeeded", 409)
		}
		return EgoResult{OK: true, Operation: op, Binding: Binding{SpaceID: 1, PageID: "page-1", ProfileID: "profile-1"}, PluginID: "plugin-1"}, nil
	})
	h.m.mu.Lock()
	h.m.executor = ego
	h.m.mu.Unlock()
	task := h.submit(t, request("payload with `quotes`, ${expressions}, and a </script> marker"))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	got, err := h.m.Wait(ctx, task.ID)
	if err != nil || got.Error == nil || got.Error.Code != "submission_uncertain" {
		t.Fatalf("%+v %v", got, err)
	}
	// Completion is persisted before the worker releases its slot. Drain waits
	// for the cleanup, making the call-count assertions deterministic.
	if err = h.m.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls["submit-task"] != 1 || calls["recover-completion"] != 0 || calls["cancel-turn"] != 1 {
		t.Fatal(calls)
	}
	persisted, err := h.s.loadTask(task.ID)
	if err != nil || persisted.ScriptHash != hash {
		t.Fatal("attempt fingerprint not durable", err)
	}
}
func TestScriptSnapshotAndPersistentEgoIdentity(t *testing.T) {
	root := t.TempDir()
	_ = os.Mkdir(filepath.Join(root, "ego_scripts"), 0700)
	for _, name := range scriptNames {
		if err := os.WriteFile(filepath.Join(root, "ego_scripts", name), []byte("// version one "+name), 0600); err != nil {
			t.Fatal(err)
		}
	}
	first, err := LoadScripts(root)
	if err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(root, "ego_scripts/probe.js"), []byte("// version two"), 0600)
	second, err := LoadScripts(root)
	if err != nil {
		t.Fatal(err)
	}
	if first.Hash == second.Hash || first.files["probe.js"] == second.files["probe.js"] {
		t.Fatal("scripts were not frozen per attempt")
	}
	s, err := OpenStore(filepath.Join(root, "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	e := NewEgo(root, slog.New(slog.NewTextHandler(io.Discard, nil)), "")
	if err = e.Restore(s); err != nil {
		t.Fatal(err)
	}
	e.mu.Lock()
	e.profile = "profile-1"
	e.pluginID = "plugin-1"
	e.slots[0] = Binding{SpaceID: 7, PageID: "page-7", ProfileID: "profile-1", ConversationID: "conversation-7"}
	e.slotRequests[0] = "req_prior"
	err = e.saveLocked()
	e.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	restarted := NewEgo(root, e.Log, "")
	if err = restarted.Restore(s); err != nil {
		t.Fatal(err)
	}
	if restarted.profile != "profile-1" || restarted.pluginID != "plugin-1" || restarted.slots[0].SpaceID != 7 || !restarted.quarantined[0] {
		t.Fatal("restart did not preserve and quarantine browser identity")
	}
}

func TestConfiguredSpaceNamesAndSelectionChange(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, test := range []struct {
		configured string
		slot       int
		want       string
	}{{"", 0, "gptweb2api-slot-0"}, {"", 2, "gptweb2api-slot-2"}, {"我的 Space", 0, "我的 Space"}, {"我的 Space", 1, "我的 Space-1"}} {
		e := NewEgo(t.TempDir(), log, test.configured)
		input := e.base(test.slot)
		if input["space_name"] != test.want || input["reuse_space"] != (test.configured != "") {
			t.Fatal(input)
		}
	}
	s, err := OpenStore(filepath.Join(t.TempDir(), "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	previous := NewEgo(t.TempDir(), log, "old Space")
	if err = previous.Restore(s); err != nil {
		t.Fatal(err)
	}
	previous.mu.Lock()
	previous.profile = "profile-1"
	previous.pluginID = "plugin-1"
	previous.slots[0] = Binding{SpaceID: 7, PageID: "old-page", ProfileID: "profile-1"}
	previous.slotRequests[0] = "old-request"
	err = previous.saveLocked()
	previous.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	same := NewEgo(t.TempDir(), log, "old Space")
	if err = same.Restore(s); err != nil {
		t.Fatal(err)
	}
	if same.slots[0].SpaceID != 7 {
		t.Fatal("unchanged selection lost its pinned ID")
	}
	changed := NewEgo(t.TempDir(), log, "new Space")
	if err = changed.Restore(s); err != nil {
		t.Fatal(err)
	}
	if len(changed.slots) != 0 || len(changed.slotRequests) != 0 || changed.profile != "profile-1" || changed.pluginID != "plugin-1" {
		t.Fatal("configuration change retained old routing or lost account identity")
	}
	again := NewEgo(t.TempDir(), log, "new Space")
	if err = again.Restore(s); err != nil {
		t.Fatal(err)
	}
	if len(again.slots) != 0 {
		t.Fatal("old Space routing returned after another restart")
	}
}

func TestEgoSlotsCannotShareOrRetargetSpace(t *testing.T) {
	e := NewEgo(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)), "configured")
	id := 7
	e.Runner = runnerFunc(func(context.Context, *ScriptSet, string, map[string]any) (EgoResult, error) {
		return EgoResult{OK: true, Binding: Binding{SpaceID: id, ProfileID: "profile-1"}}, nil
	})
	if _, err := e.provision(context.Background(), &ScriptSet{}, 0, false); err != nil {
		t.Fatal(err)
	}
	_, err := e.provision(context.Background(), &ScriptSet{}, 1, false)
	code(t, err, "space_binding_conflict")
	id = 8
	_, err = e.provision(context.Background(), &ScriptSet{}, 0, false)
	code(t, err, "space_binding_changed")
	if e.slots[0].SpaceID != 7 || e.slots[1].SpaceID != 0 {
		t.Fatal("rejected selection changed slot routing")
	}
}
