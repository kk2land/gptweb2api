package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
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

func TestMain(m *testing.M) {
	// Run a fake ego-browser as a real subprocess so CLI argument boundaries,
	// stdin delivery and the absence of a fallback are checked end to end.
	if path := os.Getenv("GPTWEB_EGO_TEST_RECORD"); path != "" {
		b, _ := json.Marshal(os.Args[1:])
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
		if err != nil {
			os.Exit(90)
		}
		_, _ = f.Write(append(b, '\n'))
		f.Close()
		if os.Getenv("GPTWEB_EGO_TEST_FAIL") == "1" {
			os.Exit(7)
		}
		source, err := io.ReadAll(os.Stdin)
		if err != nil {
			os.Exit(91)
		}
		marker := regexp.MustCompile(`GPTWEB_RESULT_[a-f0-9]{48}:`).Find(source)
		if marker == nil {
			os.Exit(92)
		}
		fmt.Printf("%s%s\n", marker, `{"ok":true,"operation":"probe"}`)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestConfiguredAppReachesCLIWithoutFallback(t *testing.T) {
	for _, test := range []struct {
		name, app string
		fail      bool
	}{{"default", "", false}, {"named", "app-a", false}, {"literal argument", "app a;$(do-not-execute)", false}, {"unavailable named app", "missing-app", true}} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			exe, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			if err = os.Symlink(exe, filepath.Join(dir, "ego-browser")); err != nil {
				t.Fatal(err)
			}
			record := filepath.Join(dir, "calls.jsonl")
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("GPTWEB_EGO_TEST_RECORD", record)
			if test.fail {
				t.Setenv("GPTWEB_EGO_TEST_FAIL", "1")
			} else {
				t.Setenv("GPTWEB_EGO_TEST_FAIL", "")
			}
			e := NewEgo(dir, slog.New(slog.NewTextHandler(io.Discard, nil)), test.app)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, err = e.Runner.Call(ctx, &ScriptSet{files: map[string]string{"common.js": "", "probe.js": "async function operation() { return {ok:true}; }"}}, "probe", map[string]any{"slot": 0})
			if test.fail {
				code(t, err, "ego_unavailable")
			} else if err != nil {
				t.Fatal(err)
			}
			b, err := os.ReadFile(record)
			if err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(strings.TrimSpace(string(b)), "\n")
			if len(lines) != 1 {
				t.Fatalf("CLI retried or fell back: %s", b)
			}
			var args []string
			if err = json.Unmarshal([]byte(lines[0]), &args); err != nil {
				t.Fatal(err)
			}
			want := []string{"nodejs"}
			if test.app != "" {
				want = []string{"--ego-server-name=" + test.app, "nodejs"}
			}
			if !reflect.DeepEqual(args, want) {
				t.Fatalf("args %q, want %q", args, want)
			}
			if e.base(0)["space_name"] != "gptweb2api-slot-0" || e.base(1)["space_name"] != "gptweb2api-slot-1" {
				t.Fatal("app name changed Space names")
			}
		})
	}
}

func TestConfiguredAppChangeInvalidatesBrowserStateAndSessions(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, test := range []struct {
		oldApp, newApp string
	}{{"", "app-b"}, {"app-a", ""}, {"app-a", "app-b"}} {
		t.Run(test.oldApp+" to "+test.newApp, func(t *testing.T) {
			s, err := OpenStore(filepath.Join(t.TempDir(), "state.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			previous := NewEgo(t.TempDir(), log, test.oldApp)
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
			ss := &Session{ID: "old-session", Model: ModelGPT6, Valid: true, History: []Message{{Role: "user", Content: "previous app input"}}}
			if err = s.save(&Task{ID: "old-task", State: "completed", Updated: time.Now()}, ss); err != nil {
				t.Fatal(err)
			}
			same := NewEgo(t.TempDir(), log, test.oldApp)
			if err = same.Restore(s); err != nil {
				t.Fatal(err)
			}
			if same.slots[0].SpaceID != 7 {
				t.Fatal("unchanged selection lost its pinned ID")
			}
			unchanged, _ := s.session(ss.ID)
			if !unchanged.Valid {
				t.Fatal("same app invalidated its session")
			}
			changed := NewEgo(t.TempDir(), log, test.newApp)
			if err = changed.Restore(s); err != nil {
				t.Fatal(err)
			}
			if len(changed.slots) != 0 || len(changed.slotRequests) != 0 || changed.profile != "" || changed.pluginID != "" {
				t.Fatal("app change retained identifiers from the previous browser instance")
			}
			invalidated, err := s.session(ss.ID)
			if err != nil || invalidated.Valid || len(invalidated.History) != 1 {
				t.Fatal("previous app session was not invalidated without losing history", err)
			}
			again := NewEgo(t.TempDir(), log, test.newApp)
			if err = again.Restore(s); err != nil {
				t.Fatal(err)
			}
			if len(again.slots) != 0 {
				t.Fatal("previous app routing returned after another restart")
			}
		})
	}
}

func TestLegacySpaceSelectionMigration(t *testing.T) {
	s, err := OpenStore(filepath.Join(t.TempDir(), "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	state := egoState{Profile: "profile-1", PluginID: "plugin-1", Slots: map[int]Binding{0: {SpaceID: 7, PageID: "old-page", ProfileID: "profile-1"}}, Requests: map[int]string{0: "old-request"}, Quarantined: map[int]bool{}, LegacySpaceName: "unwanted-space"}
	b, _ := json.Marshal(state)
	if _, err = s.db.Exec(`INSERT INTO runtime_state VALUES('ego',?)`, b); err != nil {
		t.Fatal(err)
	}
	e := NewEgo(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)), "")
	if err = e.Restore(s); err != nil {
		t.Fatal(err)
	}
	if len(e.slots) != 0 || e.profile != "profile-1" || e.pluginID != "plugin-1" {
		t.Fatal("legacy Space selection was not removed while keeping same-app account identity")
	}
	var persisted []byte
	if err = s.db.QueryRow(`SELECT payload FROM runtime_state WHERE name='ego'`).Scan(&persisted); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(persisted), "SpaceName") {
		t.Fatal("deprecated selector was persisted again")
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
