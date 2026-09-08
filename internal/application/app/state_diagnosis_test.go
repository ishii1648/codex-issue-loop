package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/ishii1648/codex-issue-loop/internal/adapter/state"
	"github.com/ishii1648/codex-issue-loop/internal/application/delivery"
	"github.com/ishii1648/codex-issue-loop/internal/platform/fsutil"
	"github.com/ishii1648/codex-issue-loop/internal/platform/launchd"
	"github.com/ishii1648/codex-issue-loop/internal/platform/registry"
)

func durableFiles(t *testing.T, dir string) map[string]string {
	t.Helper()
	result := map[string]string{}
	if err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		result[path] = string(data)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestFaultStatusDoctorReadOnly(t *testing.T) {
	for _, scenario := range []struct{ name, code string }{
		{"normal", "STATE_VALID"}, {"invalid", "STATE_CORRUPT"}, {"version", "STATE_VERSION_UNSUPPORTED"},
		{"recovery", "STATE_RECOVERY_REQUIRED"}, {"prepared", "STATE_UNCONFIRMED"}, {"missing", "STATE_MISSING"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			repo, l := testEnvironment(t)
			cfg := mustConfig(t, repo)
			repo = cfg.RepoPath
			entry := registry.Entry{RepoID: registry.RepoID(cfg.GitHub.Repo, repo), RepoPath: repo, Commands: map[string]string{"launchctl": "/usr/bin/false", "gh": "/usr/bin/false"}}
			if err := fsutil.WriteJSON(l.RegistryPath, registry.Registry{Version: registry.CurrentVersion, Repos: map[string]registry.Entry{entry.RepoID: entry}}, 0600); err != nil {
				t.Fatal(err)
			}
			store := state.Store{Dir: l.RepoDir(entry.RepoID), RepoID: entry.RepoID, RepoPath: repo}
			snapshot, err := store.Update("fixture", 0, "", nil, func(*state.Snapshot) error { return nil })
			if err != nil {
				t.Fatal(err)
			}
			switch scenario.name {
			case "invalid":
				snapshot.Issues["1"] = &state.Issue{Number: 2}
			case "version":
				snapshot.Version = 999
			case "recovery":
				snapshot.Recovery = &state.Recovery{Status: state.RecoveryStateBlocked, Reason: "existing marker"}
			case "prepared":
				if err := os.WriteFile(store.TransactionPath(), []byte("{"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := fsutil.WriteJSON(store.StatePath(), snapshot, 0600); err != nil {
				t.Fatal(err)
			}
			if scenario.name == "missing" {
				if err := os.Remove(store.StatePath()); err != nil {
					t.Fatal(err)
				}
			}
			before := durableFiles(t, store.Dir)
			var out bytes.Buffer
			code := (App{Out: &out, Err: io.Discard}).Run(context.Background(), []string{"status", "--repo", repo, "--json"})
			if scenario.name == "normal" {
				if code != 0 {
					t.Fatalf("status code=%d output=%s", code, &out)
				}
			} else {
				var item diagnostic
				if err := json.Unmarshal(out.Bytes(), &item); err != nil {
					t.Fatal(err)
				}
				if code != 1 || item.Code != scenario.code || item.OK {
					t.Fatalf("status code=%d diagnostic=%+v", code, item)
				}
			}
			if !reflect.DeepEqual(before, durableFiles(t, store.Dir)) {
				t.Fatal("status mutated files")
			}
			item := diagnosticByCode(t, diagnoseDurableState(l, entry, cfg), scenario.code)
			if item.OK != (scenario.name == "normal") {
				t.Fatalf("doctor=%+v", item)
			}
			out.Reset()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			code = (App{Out: &out, Err: io.Discard}).Run(ctx, []string{"doctor", "--repo", repo, "--json"})
			var report doctorResult
			if err := json.Unmarshal(out.Bytes(), &report); err != nil {
				t.Fatalf("doctor output=%s: %v", &out, err)
			}
			diagnosticByCode(t, report.Diagnostics, scenario.code)
			if report.OK != (code == 0) || code != 1 {
				t.Fatalf("doctor code=%d ok=%v", code, report.OK)
			}
			if !reflect.DeepEqual(before, durableFiles(t, store.Dir)) {
				t.Fatal("doctor mutated files")
			}
		})
	}
}

func TestDiagnosticRuntimeAssignment(t *testing.T) {
	repo, l := testEnvironment(t)
	entry := registry.Entry{RepoID: "repo-test", RepoPath: repo}
	if err := diagnosticRuntime(l, entry); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	digest, err := fileSHA256(executable)
	if err != nil {
		t.Fatal(err)
	}
	originalVersion, originalCommit := Version, Commit
	Version, Commit = "v0.12.30", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	defer func() { Version, Commit = originalVersion, originalCommit }()
	ref := delivery.SlotRef(l, Version, Commit, digest)
	if err := delivery.StageSlot(l, ref, executable); err != nil {
		t.Fatal(err)
	}
	cfg := delivery.DefaultConfig("owner/repo")
	cfg.Assignments[entry.RepoID] = delivery.RepositoryAssignment{RepositoryID: entry.RepoID, AssignmentRef: ref, Generation: 1, UpdatedAt: time.Now().UTC()}
	path, err := delivery.ResolveConfigPath("")
	if err != nil {
		t.Fatal(err)
	}
	if err := delivery.WriteConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	before := durableFiles(t, filepath.Dir(path))
	if err := diagnosticRuntime(l, entry); err != nil {
		t.Fatal(err)
	}
	Version = "v0.12.21"
	if err := diagnosticRuntime(l, entry); err == nil {
		t.Fatal("different CLI accepted")
	}
	if err := diagnosticRuntime(l, registry.Entry{RepoID: "other"}); err == nil {
		t.Fatal("unassigned repository accepted")
	}
	if !reflect.DeepEqual(before, durableFiles(t, filepath.Dir(path))) {
		t.Fatal("assignment changed")
	}
	Version = "v0.12.31"
	desired := delivery.SlotRef(l, Version, Commit, digest)
	if err := delivery.StageSlot(l, desired, executable); err != nil {
		t.Fatal(err)
	}
	tx := delivery.AssignmentTransaction{RepositoryID: entry.RepoID, Operation: delivery.AssignmentOperationApply, Phase: delivery.AssignmentValidating, ExpectedGeneration: 1, TargetGeneration: 2, Current: ref, Desired: desired, StartedAt: time.Now().UTC()}
	if err := delivery.SaveAssignmentTransaction(l.DeliveryAssignmentTransactionPath(entry.RepoID), tx); err != nil {
		t.Fatal(err)
	}
	fence := delivery.Maintenance{Generation: "assignment-2", Desired: delivery.VersionRef{Version: Version, Commit: Commit}, RequestedAt: time.Now().UTC()}
	if err := delivery.WriteMaintenance(l.DeliveryAssignmentFencePath(entry.RepoID), fence); err != nil {
		t.Fatal(err)
	}
	if err := (launchd.Manager{Layout: l}).WritePlist(entry, desired.Slot); err != nil {
		t.Fatal(err)
	}
	if err := diagnosticRuntime(l, entry); err != nil {
		t.Fatalf("candidate health check rejected: %v", err)
	}
	fence.Generation = "assignment-3"
	if err := delivery.WriteMaintenance(l.DeliveryAssignmentFencePath(entry.RepoID), fence); err != nil {
		t.Fatal(err)
	}
	if err := diagnosticRuntime(l, entry); err == nil {
		t.Fatal("unrelated maintenance generation accepted")
	}
}
