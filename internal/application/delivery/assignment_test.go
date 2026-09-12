package delivery

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ishii1648/codex-issue-loop/internal/adapter/state"
	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
	"github.com/ishii1648/codex-issue-loop/internal/platform/fsutil"
	"github.com/ishii1648/codex-issue-loop/internal/platform/launchd"
	"github.com/ishii1648/codex-issue-loop/internal/platform/layout"
	"github.com/ishii1648/codex-issue-loop/internal/platform/registry"
)

func TestAssignmentConfigMigrationIsExplicitAndDoesNotRewriteRuntime(t *testing.T) {
	l, configPath, entries, binary := assignmentFixture(t)
	before := make(map[string][]byte, len(entries))
	for _, entry := range entries {
		data, err := os.ReadFile(l.PlistPath(entry.RepoID))
		if err != nil {
			t.Fatal(err)
		}
		before[entry.RepoID] = data
	}
	controller := AssignmentController{Layout: l, ConfigPath: configPath}
	preview, err := controller.MigrateConfig(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if preview.Applied || len(preview.Assignments) != 2 {
		t.Fatalf("preview=%+v", preview)
	}
	if _, err := os.Stat(l.DeliverySlotsDir()); !os.IsNotExist(err) {
		t.Fatalf("preview created slot directory: %v", err)
	}
	applied, err := controller.MigrateConfig(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if !applied.Applied {
		t.Fatalf("migration=%+v", applied)
	}
	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Version != ConfigVersion || cfg.AutoApply != "never" || len(cfg.Assignments) != 2 {
		t.Fatalf("config=%+v", cfg)
	}
	digest, _ := fileDigest(binary)
	for _, entry := range entries {
		assignment := cfg.Assignments[entry.RepoID]
		if assignment.Generation != 1 || assignment.ArtifactSHA256 != digest {
			t.Fatalf("assignment=%+v", assignment)
		}
		if err := VerifySlot(assignment.AssignmentRef); err != nil {
			t.Fatal(err)
		}
		after, _ := os.ReadFile(l.PlistPath(entry.RepoID))
		if string(after) != string(before[entry.RepoID]) {
			t.Fatalf("migration rewrote runtime plist for %s", entry.RepoID)
		}
	}
}

func TestPerRepositoryApplyAndRollbackLeaveOtherRepositoryBytesUnchanged(t *testing.T) {
	l, configPath, entries, _ := assignmentFixture(t)
	controller := AssignmentController{Layout: l, ConfigPath: configPath}
	if _, err := controller.MigrateConfig(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	cfg, _ := LoadConfig(configPath)
	manager := launchd.Manager{Layout: l, Launchctl: entries[0].Commands["launchctl"]}
	for _, entry := range entries {
		if err := manager.WritePlist(entry, cfg.Assignments[entry.RepoID].Slot); err != nil {
			t.Fatal(err)
		}
	}
	otherBefore, _ := os.ReadFile(l.PlistPath(entries[1].RepoID))
	runner := &releaseRunner{}
	controller.Runner = runner
	plan, err := controller.Preview(context.Background(), entries[0].RepoPath, "v1.2.3")
	if err != nil || !plan.Allowed || plan.ExpectedGeneration != 1 {
		t.Fatalf("plan=%+v err=%v", plan, err)
	}
	report, err := controller.Apply(context.Background(), entries[0].RepoPath, "v1.2.3", plan.ExpectedGeneration)
	if err != nil {
		t.Fatal(err)
	}
	if report.Assignment.Version != "v1.2.3" || report.Assignment.Generation != 2 || !report.Runtime.Matches {
		t.Fatalf("report=%+v", report)
	}
	otherAfter, _ := os.ReadFile(l.PlistPath(entries[1].RepoID))
	if string(otherAfter) != string(otherBefore) {
		t.Fatal("targeted apply rewrote the other repository plist")
	}
	rolledBack, err := controller.Rollback(context.Background(), entries[0].RepoPath, 2)
	if err != nil {
		t.Fatal(err)
	}
	if rolledBack.Assignment.Version != "v1.2.2" || rolledBack.Assignment.Generation != 3 || !rolledBack.Runtime.Matches {
		t.Fatalf("rollback=%+v", rolledBack)
	}
	otherFinal, _ := os.ReadFile(l.PlistPath(entries[1].RepoID))
	if string(otherFinal) != string(otherBefore) {
		t.Fatal("targeted rollback rewrote the other repository plist")
	}
}

func TestAssignmentRejectsStaleGenerationAndTamperedSlot(t *testing.T) {
	l, configPath, entries, _ := assignmentFixture(t)
	controller := AssignmentController{Layout: l, ConfigPath: configPath, Runner: &releaseRunner{}}
	if _, err := controller.MigrateConfig(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if _, err := controller.Apply(context.Background(), entries[0].RepoPath, "v1.2.3", 2); err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("stale generation accepted: %v", err)
	}
	cfg, _ := LoadConfig(configPath)
	ref := cfg.Assignments[entries[0].RepoID].AssignmentRef
	if err := os.WriteFile(ref.Slot, []byte("tampered"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := VerifySlot(ref); err == nil || !strings.Contains(err.Error(), "digest") {
		t.Fatalf("tampered slot accepted: %v", err)
	}
}

func TestVerifySlotRejectsMutableMetadataAndNonExecutableBinary(t *testing.T) {
	l, _, _, _ := assignmentFixture(t)
	source := filepath.Join(t.TempDir(), "agent-loop")
	if err := os.WriteFile(source, []byte("slot-binary"), 0o700); err != nil {
		t.Fatal(err)
	}
	digest, err := fileDigest(source)
	if err != nil {
		t.Fatal(err)
	}
	ref := SlotRef(l, "v1.2.3", strings.Repeat("a", 40), digest)
	if err := StageSlot(l, ref, source); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(filepath.Dir(ref.Slot), "slot.json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := VerifySlot(ref); err == nil || !strings.Contains(err.Error(), "manifest") {
		t.Fatalf("mutable manifest accepted: %v", err)
	}
	if err := os.Chmod(filepath.Join(filepath.Dir(ref.Slot), "slot.json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(ref.Slot, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := VerifySlot(ref); err == nil || !strings.Contains(err.Error(), "executable") {
		t.Fatalf("non-executable slot accepted: %v", err)
	}
}

func TestAssignmentSetRejectsUnknownMissingAndNonCanonicalRepositories(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(Config, []registry.Entry) Config
		want   string
	}{
		{
			name: "unknown",
			mutate: func(cfg Config, entries []registry.Entry) Config {
				unknown := cfg.Assignments[entries[0].RepoID]
				unknown.RepositoryID = "repo-unknown"
				cfg.Assignments[unknown.RepositoryID] = unknown
				return cfg
			},
			want: "assignment set has",
		},
		{
			name: "missing",
			mutate: func(cfg Config, entries []registry.Entry) Config {
				delete(cfg.Assignments, entries[1].RepoID)
				return cfg
			},
			want: "assignment set has",
		},
		{
			name: "non-canonical slot",
			mutate: func(cfg Config, entries []registry.Entry) Config {
				assignment := cfg.Assignments[entries[0].RepoID]
				assignment.Slot = filepath.Join(filepath.Dir(assignment.Slot), "other-agent-loop")
				cfg.Assignments[entries[0].RepoID] = assignment
				return cfg
			},
			want: "canonical immutable slot",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			l, configPath, entries, _ := assignmentFixture(t)
			controller := AssignmentController{Layout: l, ConfigPath: configPath}
			if _, err := controller.MigrateConfig(context.Background(), true); err != nil {
				t.Fatal(err)
			}
			cfg, _ := LoadConfig(configPath)
			cfg = test.mutate(cfg, entries)
			if err := WriteConfig(configPath, cfg); err != nil {
				t.Fatal(err)
			}
			if _, err := controller.Status(context.Background(), ""); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("status error=%v", err)
			}
		})
	}
}

func TestRepositoryRegistrationAndUnregistrationMaintainAssignmentSet(t *testing.T) {
	l, configPath, _, _ := assignmentFixture(t)
	controller := AssignmentController{Layout: l, ConfigPath: configPath}
	if _, err := controller.MigrateConfig(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	thirdPath := filepath.Join(l.Root, "repo-c")
	if err := os.MkdirAll(thirdPath, 0o700); err != nil {
		t.Fatal(err)
	}
	thirdPath, _ = filepath.EvalSymlinks(thirdPath)
	entry := registry.Entry{RepoID: "repo-c", RepoPath: thirdPath, GitHubRepo: "owner/c", Commands: map[string]string{"launchctl": filepath.Join(l.Root, "launchctl")}}
	registered, _ := (registry.Store{Path: l.RegistryPath}).Load()
	registered.Repos[entry.RepoID] = entry
	if err := fsutil.WriteJSON(l.RegistryPath, registered, 0o600); err != nil {
		t.Fatal(err)
	}
	assignment, managed, err := controller.EnsureRepositoryAssignment(entry)
	if err != nil || !managed || assignment.Generation != 1 || assignment.Version != "v1.2.2" {
		t.Fatalf("assignment=%+v managed=%v err=%v", assignment, managed, err)
	}
	if err := VerifySlot(assignment.AssignmentRef); err != nil {
		t.Fatal(err)
	}
	if err := (launchd.Manager{Layout: l, Launchctl: entry.Commands["launchctl"]}).WritePlist(entry, assignment.Slot); err != nil {
		t.Fatal(err)
	}
	if _, err := controller.Status(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	delete(registered.Repos, entry.RepoID)
	if err := fsutil.WriteJSON(l.RegistryPath, registered, 0o600); err != nil {
		t.Fatal(err)
	}
	removed, err := controller.RemoveRepositoryAssignment(entry.RepoID)
	if err != nil || !removed {
		t.Fatalf("removed=%v err=%v", removed, err)
	}
	if _, err := controller.Status(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
}

func TestRepositoryAssignmentLifecycleRejectsRetainedState(t *testing.T) {
	for _, test := range []struct {
		name    string
		phase   AssignmentPhase
		fence   bool
		corrupt bool
		want    string
	}{
		{name: "fence", fence: true, want: "maintenance fence"},
		{name: "planned", phase: AssignmentPlanned, want: "unfinished assignment transaction"},
		{name: "draining", phase: AssignmentDraining, want: "unfinished assignment transaction"},
		{name: "applying", phase: AssignmentApplying, want: "unfinished assignment transaction"},
		{name: "validating", phase: AssignmentValidating, want: "unfinished assignment transaction"},
		{name: "rolling_back", phase: AssignmentRollingBack, want: "unfinished assignment transaction"},
		{name: "rollback_failed", phase: AssignmentRollbackFailed, want: "unfinished assignment transaction"},
		{name: "rollback_failed_with_fence", phase: AssignmentRollbackFailed, fence: true, want: "maintenance fence"},
		{name: "corrupt", corrupt: true, want: "inspect repository assignment transaction"},
	} {
		t.Run(test.name, func(t *testing.T) {
			l, configPath, entries, _ := assignmentFixture(t)
			controller := AssignmentController{Layout: l, ConfigPath: configPath}
			if _, err := controller.MigrateConfig(context.Background(), true); err != nil {
				t.Fatal(err)
			}
			entry := entries[0]
			cfg, err := LoadConfig(configPath)
			if err != nil {
				t.Fatal(err)
			}
			current := cfg.Assignments[entry.RepoID]
			desired := current.AssignmentRef
			desired.Version = "v1.2.3"
			if test.phase != "" {
				tx := AssignmentTransaction{RepositoryID: entry.RepoID, Operation: AssignmentOperationApply, Phase: test.phase, ExpectedGeneration: current.Generation, TargetGeneration: current.Generation + 1, Current: current.AssignmentRef, Desired: desired, StartedAt: time.Now().UTC()}
				if err := SaveAssignmentTransaction(l.DeliveryAssignmentTransactionPath(entry.RepoID), tx); err != nil {
					t.Fatal(err)
				}
			}
			if test.corrupt {
				if err := os.MkdirAll(l.DeliveryAssignmentDir(entry.RepoID), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(l.DeliveryAssignmentTransactionPath(entry.RepoID), []byte("{"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if test.fence {
				if err := WriteMaintenance(l.DeliveryAssignmentFencePath(entry.RepoID), Maintenance{Generation: "assignment-2", Desired: VersionRef{Version: desired.Version, Commit: desired.Commit}, RequestedAt: time.Now().UTC()}); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.ReadFile(configPath)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := controller.EnsureRepositoryAssignment(entry); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("existing registration error=%v", err)
			}
			if err := (registry.Store{Path: l.RegistryPath}).Remove(entry.RepoID); err != nil {
				t.Fatal(err)
			}
			if removed, err := controller.RemoveRepositoryAssignment(entry.RepoID); removed || err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("removed=%v err=%v", removed, err)
			}
			after, err := os.ReadFile(configPath)
			if err != nil {
				t.Fatal(err)
			}
			if string(after) != string(before) {
				t.Fatal("rejected lifecycle operations changed assignments")
			}
			registered, err := (registry.Store{Path: l.RegistryPath}).Load()
			if err != nil {
				t.Fatal(err)
			}
			registered.Repos[entry.RepoID] = entry
			if err := fsutil.WriteJSON(l.RegistryPath, registered, 0o600); err != nil {
				t.Fatal(err)
			}
			delete(cfg.Assignments, entry.RepoID)
			if err := WriteConfig(configPath, cfg); err != nil {
				t.Fatal(err)
			}
			if _, managed, err := controller.EnsureRepositoryAssignment(entry); managed || err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("new registration managed=%v err=%v", managed, err)
			}
			cfg, err = LoadConfig(configPath)
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := cfg.Assignments[entry.RepoID]; ok {
				t.Fatal("created assignment over retained state")
			}
		})
	}
}

func TestRunningRepositorySwitchPreservesOtherPIDAndSnapshot(t *testing.T) {
	l, configPath, entries, _ := assignmentFixture(t)
	controller := AssignmentController{Layout: l, ConfigPath: configPath, Runner: &releaseRunner{}}
	if _, err := controller.MigrateConfig(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	cfg, _ := LoadConfig(configPath)
	stateDir := filepath.Join(t.TempDir(), "launchctl-state")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_LAUNCHCTL_STATE", stateDir)
	launchctl := filepath.Join(filepath.Dir(stateDir), "launchctl-running")
	script := `#!/bin/sh
state_dir=$FAKE_LAUNCHCTL_STATE
case "$1" in
  print) label=${2##*/}; [ -f "$state_dir/$label" ] || exit 1; pid=$(cat "$state_dir/$label"); printf 'state = running\npid = %s\n' "$pid" ;;
  bootout) label=${2##*/}; rm -f "$state_dir/$label" ;;
  bootstrap) label=$(basename "$3" .plist); printf '%s\n' 9001 >"$state_dir/$label" ;;
  *) exit 2 ;;
esac
`
	if err := os.WriteFile(launchctl, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	manager := launchd.Manager{Layout: l, Launchctl: launchctl}
	for index := range entries {
		entries[index].Commands["launchctl"] = launchctl
		if err := manager.WritePlist(entries[index], cfg.Assignments[entries[index].RepoID].Slot); err != nil {
			t.Fatal(err)
		}
		pid := "8001"
		if index == 1 {
			pid = "8002"
		}
		if err := os.WriteFile(filepath.Join(stateDir, l.Label(entries[index].RepoID)), []byte(pid), 0o600); err != nil {
			t.Fatal(err)
		}
		store := state.Store{Dir: l.RepoDir(entries[index].RepoID), RepoID: entries[index].RepoID, RepoPath: entries[index].RepoPath}
		if err := store.Ensure(); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Update("fixture_maintenance", 0, "", nil, func(snapshot *state.Snapshot) error {
			snapshot.Supervisor.State = state.SupervisorStateMaintenance
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	registered := registry.Registry{Version: registry.CurrentVersion, Repos: map[string]registry.Entry{entries[0].RepoID: entries[0], entries[1].RepoID: entries[1]}}
	if err := fsutil.WriteJSON(l.RegistryPath, registered, 0o600); err != nil {
		t.Fatal(err)
	}
	otherStateBefore, _ := os.ReadFile(filepath.Join(l.RepoDir(entries[1].RepoID), "state.json"))
	otherStatusBefore, _ := manager.Status(context.Background(), entries[1])
	report, err := controller.Apply(context.Background(), entries[0].RepoPath, "v1.2.3", 1)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Runtime.Launchd.Running || report.Runtime.Launchd.PID != 9001 {
		t.Fatalf("target runtime=%+v", report.Runtime)
	}
	otherStateAfter, _ := os.ReadFile(filepath.Join(l.RepoDir(entries[1].RepoID), "state.json"))
	otherStatusAfter, _ := manager.Status(context.Background(), entries[1])
	if string(otherStateAfter) != string(otherStateBefore) || otherStatusAfter.PID != otherStatusBefore.PID {
		t.Fatalf("other repository changed: before=%+v after=%+v", otherStatusBefore, otherStatusAfter)
	}
}

func TestAssignmentHealthFailureRollsBackOnlyTarget(t *testing.T) {
	l, configPath, entries, _ := assignmentFixture(t)
	makeAssignmentEntryRunning(t, l, entries, 0)
	runner := &releaseRunner{failFirstDoctor: true}
	controller := AssignmentController{Layout: l, ConfigPath: configPath, Runner: runner}
	if _, err := controller.MigrateConfig(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	cfg, _ := LoadConfig(configPath)
	manager := launchd.Manager{Layout: l, Launchctl: entries[0].Commands["launchctl"]}
	for _, entry := range entries {
		if err := manager.WritePlist(entry, cfg.Assignments[entry.RepoID].Slot); err != nil {
			t.Fatal(err)
		}
	}
	otherBefore, _ := os.ReadFile(l.PlistPath(entries[1].RepoID))
	if _, err := controller.Apply(context.Background(), entries[0].RepoPath, "v1.2.3", 1); err == nil || !strings.Contains(err.Error(), "rolled back") {
		t.Fatalf("apply error=%v", err)
	}
	cfg, _ = LoadConfig(configPath)
	assignment := cfg.Assignments[entries[0].RepoID]
	if assignment.Version != "v1.2.2" || assignment.Generation != 1 {
		t.Fatalf("assignment changed after rollback: %+v", assignment)
	}
	tx, err := LoadAssignmentTransaction(l.DeliveryAssignmentTransactionPath(entries[0].RepoID))
	if err != nil || tx.Phase != AssignmentRolledBack {
		t.Fatalf("transaction=%+v err=%v", tx, err)
	}
	if _, err := os.Stat(l.DeliveryAssignmentFencePath(entries[0].RepoID)); !os.IsNotExist(err) {
		t.Fatalf("successful rollback retained fence: %v", err)
	}
	otherAfter, _ := os.ReadFile(l.PlistPath(entries[1].RepoID))
	if string(otherAfter) != string(otherBefore) {
		t.Fatal("target health rollback rewrote the other repository plist")
	}
}

type assignmentFaultRunner struct {
	release  releaseRunner
	onDoctor func()
}

type legacyAssignmentRunner struct {
	releaseRunner
	legacyPath        string
	legacyDoctorCalls int
}

func (r *legacyAssignmentRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	if filepath.Clean(name) == filepath.Clean(r.legacyPath) && len(args) > 0 && args[0] == "version" {
		return json.Marshal(BinaryInfo{Version: "v1.2.2", Commit: strings.Repeat("a", 40), Target: "darwin/arm64", DeliveryProtocol: 1, StateSchemaCurrent: 4, StateSchemaMigrationFrom: 3, SemanticContractCurrent: 1})
	}
	if filepath.Clean(name) == filepath.Clean(r.legacyPath) && len(args) > 0 && args[0] == "doctor" {
		r.legacyDoctorCalls++
		for _, arg := range args {
			if arg == "--assignment-health" {
				return nil, errors.New("legacy doctor does not support --assignment-health")
			}
		}
		return []byte(`{"schema_version":1,"ok":true}`), nil
	}
	return r.releaseRunner.Run(ctx, name, args...)
}

func TestStoppedAssignmentRollbackDoesNotExecuteLegacyDoctor(t *testing.T) {
	l, configPath, entries, _ := assignmentFixture(t)
	controller := AssignmentController{Layout: l, ConfigPath: configPath, Runner: &releaseRunner{}}
	if _, err := controller.MigrateConfig(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if _, err := controller.Apply(context.Background(), entries[0].RepoPath, "v1.2.3", 1); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	previous := cfg.Assignments[entries[0].RepoID].Previous
	if previous == nil {
		t.Fatal("applied assignment has no previous version")
	}
	runner := &legacyAssignmentRunner{legacyPath: previous.Slot}
	controller.Runner = runner
	report, err := controller.Rollback(context.Background(), entries[0].RepoPath, 2)
	if err != nil {
		t.Fatal(err)
	}
	if report.Assignment.Version != "v1.2.2" || report.Assignment.Generation != 3 || runner.legacyDoctorCalls != 0 {
		t.Fatalf("report=%+v legacy_doctor_calls=%d", report, runner.legacyDoctorCalls)
	}
}

func (r *assignmentFaultRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	if filepath.Base(name) == "agent-loop" && len(args) > 0 && args[0] == "doctor" && r.onDoctor != nil {
		r.onDoctor()
		r.onDoctor = nil
		return nil, errors.New("injected assignment doctor failure")
	}
	return r.release.Run(ctx, name, args...)
}

func TestAssignmentRollbackFailureRetainsRepositoryFence(t *testing.T) {
	l, configPath, entries, _ := assignmentFixture(t)
	makeAssignmentEntryRunning(t, l, entries, 0)
	controller := AssignmentController{Layout: l, ConfigPath: configPath}
	if _, err := controller.MigrateConfig(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	cfg, _ := LoadConfig(configPath)
	current := cfg.Assignments[entries[0].RepoID]
	manager := launchd.Manager{Layout: l, Launchctl: entries[0].Commands["launchctl"]}
	if err := manager.WritePlist(entries[0], current.Slot); err != nil {
		t.Fatal(err)
	}
	runner := &assignmentFaultRunner{onDoctor: func() {
		if err := os.WriteFile(current.Slot, []byte("corrupted during switch"), 0o700); err != nil {
			t.Error(err)
		}
	}}
	controller.Runner = runner
	if _, err := controller.Apply(context.Background(), entries[0].RepoPath, "v1.2.3", 1); err == nil || !strings.Contains(err.Error(), "rollback failed") {
		t.Fatalf("apply error=%v", err)
	}
	tx, err := LoadAssignmentTransaction(l.DeliveryAssignmentTransactionPath(entries[0].RepoID))
	if err != nil || tx.Phase != AssignmentRollbackFailed {
		t.Fatalf("transaction=%+v err=%v", tx, err)
	}
	if _, err := os.Stat(l.DeliveryAssignmentFencePath(entries[0].RepoID)); err != nil {
		t.Fatalf("rollback failure did not retain fence: %v", err)
	}
	cfg, _ = LoadConfig(configPath)
	if cfg.Assignments[entries[0].RepoID].Generation != 1 {
		t.Fatal("rollback failure committed the desired assignment")
	}
}

func makeAssignmentEntryRunning(t *testing.T, l layout.Layout, entries []registry.Entry, index int) {
	t.Helper()
	stateDir := filepath.Join(t.TempDir(), "launchctl-state")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_ASSIGNMENT_LAUNCHCTL_STATE", stateDir)
	launchctl := filepath.Join(filepath.Dir(stateDir), "launchctl-running")
	script := `#!/bin/sh
state_dir=$FAKE_ASSIGNMENT_LAUNCHCTL_STATE
case "$1" in
  print) label=${2##*/}; [ -f "$state_dir/$label" ] || exit 1; printf 'state = running\npid = 9001\n' ;;
  bootout) label=${2##*/}; rm -f "$state_dir/$label" ;;
  bootstrap) label=$(basename "$3" .plist); : > "$state_dir/$label" ;;
  *) exit 2 ;;
esac
`
	if err := os.WriteFile(launchctl, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	entries[index].Commands["launchctl"] = launchctl
	registered, err := (registry.Store{Path: l.RegistryPath}).Load()
	if err != nil {
		t.Fatal(err)
	}
	registered.Repos[entries[index].RepoID] = entries[index]
	if err := fsutil.WriteJSON(l.RegistryPath, registered, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, l.Label(entries[index].RepoID)), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	store := state.Store{Dir: l.RepoDir(entries[index].RepoID), RepoID: entries[index].RepoID, RepoPath: entries[index].RepoPath}
	if _, err := store.Update("fixture_maintenance", 0, "", nil, func(snapshot *state.Snapshot) error {
		snapshot.Supervisor.State = state.SupervisorStateMaintenance
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestAssignmentRetryRollbackValidatesRetainedTransactionAndClearsFence(t *testing.T) {
	l, configPath, entries, _ := assignmentFixture(t)
	controller := AssignmentController{Layout: l, ConfigPath: configPath, Runner: &releaseRunner{}}
	if _, err := controller.MigrateConfig(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	current := cfg.Assignments[entries[0].RepoID]
	candidate, err := (Verifier{GH: "gh", Runner: controller.Runner, CacheDir: RuntimePaths(l.Root).Cache, ExpectedVersion: "v1.2.3"}).Check(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	desired := SlotRef(l, candidate.Manifest.Version, candidate.Manifest.Commit, candidate.Digest)
	if err := StageSlot(l, desired, filepath.Join(candidate.Dir, BinaryAsset)); err != nil {
		t.Fatal(err)
	}
	manager := launchd.Manager{Layout: l, Launchctl: entries[0].Commands["launchctl"]}
	if err := manager.WritePlist(entries[0], current.Slot); err != nil {
		t.Fatal(err)
	}
	tx := AssignmentTransaction{RepositoryID: entries[0].RepoID, Operation: AssignmentOperationApply, Phase: AssignmentRollbackFailed, ExpectedGeneration: current.Generation, TargetGeneration: current.Generation + 1, Current: current.AssignmentRef, Desired: desired, WasLoaded: false, Result: "rollback_failed", Reason: "fixture", StartedAt: time.Now().UTC()}
	if err := SaveAssignmentTransaction(l.DeliveryAssignmentTransactionPath(entries[0].RepoID), tx); err != nil {
		t.Fatal(err)
	}
	if err := WriteMaintenance(l.DeliveryAssignmentFencePath(entries[0].RepoID), Maintenance{Generation: "assignment-2", Desired: VersionRef{Version: desired.Version, Commit: desired.Commit}, RequestedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	registered, err := (registry.Store{Path: l.RegistryPath}).Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := (registry.Store{Path: l.RegistryPath}).Remove(entries[0].RepoID); err != nil {
		t.Fatal(err)
	}
	if removed, err := controller.RemoveRepositoryAssignment(entries[0].RepoID); removed || err == nil {
		t.Fatalf("rollback_failed unregister: removed=%v err=%v", removed, err)
	}
	if err := fsutil.WriteJSON(l.RegistryPath, registered, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := controller.EnsureRepositoryAssignment(entries[0]); err == nil {
		t.Fatal("registration accepted retained rollback_failed fence")
	}
	report, err := controller.RetryRollback(context.Background(), entries[0].RepoPath)
	if err != nil {
		t.Fatal(err)
	}
	if report.Result != "rolled_back" || report.Assignment.AssignmentRef != current.AssignmentRef {
		t.Fatalf("report=%+v", report)
	}
	tx, err = LoadAssignmentTransaction(l.DeliveryAssignmentTransactionPath(entries[0].RepoID))
	if err != nil || tx.Phase != AssignmentRolledBack || tx.Result != "rolled_back" {
		t.Fatalf("transaction=%+v err=%v", tx, err)
	}
	if _, err := os.Stat(l.DeliveryAssignmentFencePath(entries[0].RepoID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("retained fence was not cleared: %v", err)
	}
	if err := (registry.Store{Path: l.RegistryPath}).Remove(entries[0].RepoID); err != nil {
		t.Fatal(err)
	}
	if removed, err := controller.RemoveRepositoryAssignment(entries[0].RepoID); err != nil || !removed {
		t.Fatalf("recovered unregister: removed=%v err=%v", removed, err)
	}
	if err := fsutil.WriteJSON(l.RegistryPath, registered, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, managed, err := controller.EnsureRepositoryAssignment(entries[0]); err != nil || !managed {
		t.Fatalf("recovered register: managed=%v err=%v", managed, err)
	}
	if _, err := os.Lstat(l.DeliveryAssignmentFencePath(entries[0].RepoID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("re-registration retained supervisor maintenance fence: %v", err)
	}
}

func TestAssignmentRollbackStopsRefuseUnsafeSnapshot(t *testing.T) {
	for _, operation := range []string{"retry_rollback", "rollback_switch", "retry"} {
		for _, snapshotKind := range []string{"worker_pid", "active_execution", "load_error"} {
			t.Run(operation+"/"+snapshotKind, func(t *testing.T) {
				l, configPath, entries, _ := assignmentFixture(t)
				controller := AssignmentController{Layout: l, ConfigPath: configPath, Runner: &releaseRunner{}}
				if _, err := controller.MigrateConfig(context.Background(), true); err != nil {
					t.Fatal(err)
				}
				cfg, err := LoadConfig(configPath)
				if err != nil {
					t.Fatal(err)
				}
				entry := entries[0]
				current := cfg.Assignments[entry.RepoID]
				candidate, err := (Verifier{GH: "gh", Runner: controller.Runner, CacheDir: RuntimePaths(l.Root).Cache, ExpectedVersion: "v1.2.3"}).Check(context.Background(), cfg)
				if err != nil {
					t.Fatal(err)
				}
				desired := SlotRef(l, candidate.Manifest.Version, candidate.Manifest.Commit, candidate.Digest)
				if err := StageSlot(l, desired, filepath.Join(candidate.Dir, BinaryAsset)); err != nil {
					t.Fatal(err)
				}
				commandLog := filepath.Join(t.TempDir(), "launchctl.log")
				t.Setenv("FAKE_LAUNCHCTL_LOG", commandLog)
				script := `#!/bin/sh
printf '%s\n' "$*" >>"$FAKE_LAUNCHCTL_LOG"
case "$1" in
  print) printf 'state = running\npid = 8101\n' ;;
  *) exit 91 ;;
esac
`
				if err := os.WriteFile(entry.Commands["launchctl"], []byte(script), 0o700); err != nil {
					t.Fatal(err)
				}
				store := state.Store{Dir: l.RepoDir(entry.RepoID), RepoID: entry.RepoID, RepoPath: entry.RepoPath}
				if _, _, err := store.StartExecution(state.ExecutionStart{IssueNumber: 1, Title: "fixture", RunID: "run-1", StartedAt: time.Now().UTC()}); err != nil {
					t.Fatal(err)
				}
				if snapshotKind == "worker_pid" {
					if _, err := store.Update("fixture", 1, "run-1", nil, func(snapshot *state.Snapshot) error {
						item := snapshot.Issues["1"]
						item.WorkerPID, item.WorkerPGID = 7101, 7101
						return nil
					}); err != nil {
						t.Fatal(err)
					}
				}
				if snapshotKind == "load_error" {
					lockPath := filepath.Join(store.Dir, "state.lock")
					if err := os.Remove(lockPath); err != nil {
						t.Fatal(err)
					}
					if err := os.Mkdir(lockPath, 0o700); err != nil {
						t.Fatal(err)
					}
				}
				tx := AssignmentTransaction{
					RepositoryID: entry.RepoID, Operation: AssignmentOperationApply, Phase: AssignmentRollbackFailed,
					ExpectedGeneration: current.Generation, TargetGeneration: current.Generation + 1,
					Current: current.AssignmentRef, Desired: desired, WasLoaded: true,
					Result: "rollback_failed", Reason: "injected health failure", StartedAt: time.Now().UTC(),
				}
				if operation == "rollback_switch" {
					tx.Phase, tx.Result = AssignmentValidating, "validating"
				}
				txPath := l.DeliveryAssignmentTransactionPath(entry.RepoID)
				if err := SaveAssignmentTransaction(txPath, tx); err != nil {
					t.Fatal(err)
				}
				fencePath := l.DeliveryAssignmentFencePath(entry.RepoID)
				fence := Maintenance{Version: 1, Generation: "assignment-2", Desired: VersionRef{Version: desired.Version, Commit: desired.Commit}, RequestedAt: time.Now().UTC()}
				if err := WriteMaintenance(fencePath, fence); err != nil {
					t.Fatal(err)
				}
				switch operation {
				case "retry_rollback":
					_, err = controller.RetryRollback(context.Background(), entry.RepoPath)
				case "rollback_switch":
					_, err = controller.rollbackSwitch(context.Background(), cfg, entry, current, &tx, errors.New("injected health failure"))
				case "retry":
					_, err = controller.Retry(context.Background(), entry.RepoPath, current.Generation)
				}
				want := "refuses to stop an active worker"
				if snapshotKind == "load_error" {
					want = "inspect assignment state"
				}
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Fatalf("error=%v, want %q", err, want)
				}
				commands, err := os.ReadFile(commandLog)
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(string(commands), "bootout") || strings.Contains(string(commands), "bootstrap") {
					t.Fatalf("unsafe snapshot caused LaunchAgent mutation:\n%s", commands)
				}
				retained, err := LoadAssignmentTransaction(txPath)
				if err != nil || retained.Phase != AssignmentRollbackFailed || retained.Result != "rollback_failed" {
					t.Fatalf("transaction=%+v err=%v", retained, err)
				}
				retainedFence, err := LoadMaintenance(fencePath)
				if err != nil || retainedFence != fence {
					t.Fatalf("fence=%+v err=%v", retainedFence, err)
				}
			})
		}
	}
}

func TestAssignmentRetryCompletesExactRollbackFailedTarget(t *testing.T) {
	l, configPath, entries, _ := assignmentFixture(t)
	runner := &releaseRunner{}
	controller := AssignmentController{Layout: l, ConfigPath: configPath, Runner: runner}
	if _, err := controller.MigrateConfig(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	current := cfg.Assignments[entries[0].RepoID]
	candidate, err := (Verifier{GH: "gh", Runner: runner, CacheDir: RuntimePaths(l.Root).Cache, ExpectedVersion: "v1.2.3"}).Check(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	desired := SlotRef(l, candidate.Manifest.Version, candidate.Manifest.Commit, candidate.Digest)
	if err := StageSlot(l, desired, filepath.Join(candidate.Dir, BinaryAsset)); err != nil {
		t.Fatal(err)
	}
	manager := launchd.Manager{Layout: l, Launchctl: entries[0].Commands["launchctl"]}
	if err := manager.WritePlist(entries[0], current.Slot); err != nil {
		t.Fatal(err)
	}
	tx := AssignmentTransaction{
		RepositoryID: entries[0].RepoID, Operation: AssignmentOperationApply, Phase: AssignmentRollbackFailed,
		ExpectedGeneration: 1, TargetGeneration: 2, Current: current.AssignmentRef, Desired: desired,
		Result: "rollback_failed", Reason: "injected health failure", StartedAt: time.Now().UTC(),
	}
	if err := SaveAssignmentTransaction(l.DeliveryAssignmentTransactionPath(entries[0].RepoID), tx); err != nil {
		t.Fatal(err)
	}
	if err := WriteMaintenance(l.DeliveryAssignmentFencePath(entries[0].RepoID), Maintenance{Generation: "assignment-2", Desired: VersionRef{Version: desired.Version, Commit: desired.Commit}, RequestedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	report, err := controller.Retry(context.Background(), entries[0].RepoPath, 1)
	if err != nil {
		t.Fatal(err)
	}
	if report.Result != "succeeded" || report.Assignment.AssignmentRef != desired || report.Assignment.Generation != 2 {
		t.Fatalf("report=%+v", report)
	}
	tx, err = LoadAssignmentTransaction(l.DeliveryAssignmentTransactionPath(entries[0].RepoID))
	if err != nil || tx.Phase != AssignmentSucceeded {
		t.Fatalf("transaction=%+v err=%v", tx, err)
	}
	if _, err := os.Stat(l.DeliveryAssignmentFencePath(entries[0].RepoID)); !os.IsNotExist(err) {
		t.Fatalf("successful retry retained fence: %v", err)
	}
}

func TestAssignmentRetryConvergesUnloadedUnstartedLaunchToStoppedState(t *testing.T) {
	l, configPath, entries, _ := assignmentFixture(t)
	runner := &releaseRunner{}
	controller := AssignmentController{Layout: l, ConfigPath: configPath, Runner: runner, Now: func() time.Time {
		return time.Date(2026, 9, 6, 1, 0, 0, 0, time.UTC)
	}}
	if _, err := controller.MigrateConfig(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	current := cfg.Assignments[entries[0].RepoID]
	candidate, err := (Verifier{GH: "gh", Runner: runner, CacheDir: RuntimePaths(l.Root).Cache, ExpectedVersion: "v1.2.3"}).Check(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	desired := SlotRef(l, candidate.Manifest.Version, candidate.Manifest.Commit, candidate.Digest)
	if err := StageSlot(l, desired, filepath.Join(candidate.Dir, BinaryAsset)); err != nil {
		t.Fatal(err)
	}
	manager := launchd.Manager{Layout: l, Launchctl: entries[0].Commands["launchctl"]}
	if err := manager.WritePlist(entries[0], current.Slot); err != nil {
		t.Fatal(err)
	}
	stateStore := state.Store{Dir: l.RepoDir(entries[0].RepoID), RepoID: entries[0].RepoID, RepoPath: entries[0].RepoPath}
	startedAt := time.Date(2026, 9, 5, 15, 0, 0, 0, time.UTC)
	if _, err := stateStore.Update("fixture_launching", 277, "run_277", nil, func(snapshot *state.Snapshot) error {
		snapshot.Supervisor.State = state.SupervisorStatePolling
		snapshot.Supervisor.PID = 82762
		snapshot.Issues["277"] = &state.Issue{Number: 277, Status: issuedomain.StatusLaunching,
			LaunchSource: issuedomain.StatusRetryWait, RunID: "run_277", Generation: 5, UpdatedAt: startedAt}
		snapshot.ActiveExecution = &state.ActiveExecution{IssueNumber: 277, RunID: "run_277", Generation: 5, StartedAt: startedAt}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	tx := AssignmentTransaction{
		RepositoryID: entries[0].RepoID, Operation: AssignmentOperationApply, Phase: AssignmentRollbackFailed,
		ExpectedGeneration: 1, TargetGeneration: 2, Current: current.AssignmentRef, Desired: desired, WasLoaded: false,
		Result: "rollback_failed", Reason: "stopped assignment requires supervisor state stopped, got polling", StartedAt: startedAt,
	}
	if err := SaveAssignmentTransaction(l.DeliveryAssignmentTransactionPath(entries[0].RepoID), tx); err != nil {
		t.Fatal(err)
	}
	if err := WriteMaintenance(l.DeliveryAssignmentFencePath(entries[0].RepoID), Maintenance{
		Generation: "assignment-2", Desired: VersionRef{Version: desired.Version, Commit: desired.Commit}, RequestedAt: startedAt,
	}); err != nil {
		t.Fatal(err)
	}
	report, err := controller.Retry(context.Background(), entries[0].RepoPath, 1)
	if err != nil {
		t.Fatal(err)
	}
	if report.Result != "succeeded" || report.Assignment.AssignmentRef != desired {
		t.Fatalf("report=%+v", report)
	}
	loaded, err := stateStore.Load()
	if err != nil {
		t.Fatal(err)
	}
	issue := loaded.Issues["277"]
	if loaded.Supervisor.State != state.SupervisorStateStopped || loaded.Supervisor.PID != 0 || loaded.ActiveExecution != nil ||
		issue.Status != issuedomain.StatusRetryWait || issue.LaunchSource != issuedomain.StatusUnset || issue.Generation != 5 {
		t.Fatalf("state=%+v issue=%+v", loaded, issue)
	}
	events, err := os.ReadFile(stateStore.EventsPath())
	if err != nil || !strings.Contains(string(events), `"type":"assignment_stopped_state_reconciled"`) {
		t.Fatalf("events=%s err=%v", events, err)
	}
}

func TestAssignmentRetryRejectsMismatchedRetainedFence(t *testing.T) {
	l, configPath, entries, _ := assignmentFixture(t)
	runner := &releaseRunner{}
	controller := AssignmentController{Layout: l, ConfigPath: configPath, Runner: runner}
	if _, err := controller.MigrateConfig(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	cfg, _ := LoadConfig(configPath)
	current := cfg.Assignments[entries[0].RepoID]
	candidate, err := (Verifier{GH: "gh", Runner: runner, CacheDir: RuntimePaths(l.Root).Cache, ExpectedVersion: "v1.2.3"}).Check(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	desired := SlotRef(l, candidate.Manifest.Version, candidate.Manifest.Commit, candidate.Digest)
	if err := StageSlot(l, desired, filepath.Join(candidate.Dir, BinaryAsset)); err != nil {
		t.Fatal(err)
	}
	tx := AssignmentTransaction{
		RepositoryID: entries[0].RepoID, Operation: AssignmentOperationApply, Phase: AssignmentRollbackFailed,
		ExpectedGeneration: 1, TargetGeneration: 2, Current: current.AssignmentRef, Desired: desired,
		Result: "rollback_failed", Reason: "injected health failure", StartedAt: time.Now().UTC(),
	}
	if err := SaveAssignmentTransaction(l.DeliveryAssignmentTransactionPath(entries[0].RepoID), tx); err != nil {
		t.Fatal(err)
	}
	if err := WriteMaintenance(l.DeliveryAssignmentFencePath(entries[0].RepoID), Maintenance{Generation: "assignment-999", Desired: VersionRef{Version: desired.Version, Commit: desired.Commit}, RequestedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if _, err := controller.Retry(context.Background(), entries[0].RepoPath, 1); err == nil || !strings.Contains(err.Error(), "fence does not match") {
		t.Fatalf("retry error=%v", err)
	}
	tx, err = LoadAssignmentTransaction(l.DeliveryAssignmentTransactionPath(entries[0].RepoID))
	if err != nil || tx.Phase != AssignmentRollbackFailed {
		t.Fatalf("transaction changed after rejection: %+v err=%v", tx, err)
	}
}

func TestAssignmentResumesCrashAfterConfigCommit(t *testing.T) {
	l, configPath, entries, _ := assignmentFixture(t)
	runner := &releaseRunner{}
	controller := AssignmentController{Layout: l, ConfigPath: configPath, Runner: runner}
	if _, err := controller.MigrateConfig(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	cfg, _ := LoadConfig(configPath)
	current := cfg.Assignments[entries[0].RepoID]
	candidate, err := (Verifier{GH: "gh", Runner: runner, CacheDir: RuntimePaths(l.Root).Cache, ExpectedVersion: "v1.2.3"}).Check(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	desired := SlotRef(l, candidate.Manifest.Version, candidate.Manifest.Commit, candidate.Digest)
	if err := StageSlot(l, desired, filepath.Join(candidate.Dir, BinaryAsset)); err != nil {
		t.Fatal(err)
	}
	manager := launchd.Manager{Layout: l, Launchctl: entries[0].Commands["launchctl"]}
	if err := manager.WritePlist(entries[0], desired.Slot); err != nil {
		t.Fatal(err)
	}
	previous := current.AssignmentRef
	committed := current
	committed.AssignmentRef = desired
	committed.Previous = &previous
	committed.Generation = 2
	committed.UpdatedAt = time.Now().UTC()
	cfg.Assignments[entries[0].RepoID] = committed
	if err := WriteConfig(configPath, cfg); err != nil {
		t.Fatal(err)
	}
	tx := AssignmentTransaction{RepositoryID: entries[0].RepoID, Operation: AssignmentOperationApply, Phase: AssignmentValidating, ExpectedGeneration: 1, TargetGeneration: 2, Current: current.AssignmentRef, Desired: desired, StartedAt: time.Now().UTC()}
	if err := SaveAssignmentTransaction(l.DeliveryAssignmentTransactionPath(entries[0].RepoID), tx); err != nil {
		t.Fatal(err)
	}
	if err := WriteMaintenance(l.DeliveryAssignmentFencePath(entries[0].RepoID), Maintenance{Generation: "assignment-2", Desired: VersionRef{Version: desired.Version, Commit: desired.Commit}, RequestedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	report, err := controller.switchTo(context.Background(), entries[0].RepoPath, desired, 1, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if report.Result != "succeeded" || report.Assignment.Generation != 2 {
		t.Fatalf("report=%+v", report)
	}
	tx, _ = LoadAssignmentTransaction(l.DeliveryAssignmentTransactionPath(entries[0].RepoID))
	if tx.Phase != AssignmentSucceeded {
		t.Fatalf("transaction=%+v", tx)
	}
	if _, err := os.Stat(l.DeliveryAssignmentFencePath(entries[0].RepoID)); !os.IsNotExist(err) {
		t.Fatalf("resumed transaction retained fence: %v", err)
	}
}

func TestAssignmentRollbackResumesCrashAfterConfigCommit(t *testing.T) {
	l, configPath, entries, _ := assignmentFixture(t)
	controller := AssignmentController{Layout: l, ConfigPath: configPath, Runner: &releaseRunner{}}
	if _, err := controller.MigrateConfig(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if _, err := controller.Apply(context.Background(), entries[0].RepoPath, "v1.2.3", 1); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	current := cfg.Assignments[entries[0].RepoID]
	if current.Previous == nil {
		t.Fatal("applied assignment has no rollback target")
	}
	desired := *current.Previous
	manager := launchd.Manager{Layout: l, Launchctl: entries[0].Commands["launchctl"]}
	if err := manager.WritePlist(entries[0], desired.Slot); err != nil {
		t.Fatal(err)
	}
	committed := current
	previous := current.AssignmentRef
	committed.AssignmentRef = desired
	committed.Previous = &previous
	committed.Generation = 3
	committed.UpdatedAt = time.Now().UTC()
	cfg.Assignments[entries[0].RepoID] = committed
	if err := WriteConfig(configPath, cfg); err != nil {
		t.Fatal(err)
	}
	tx := AssignmentTransaction{RepositoryID: entries[0].RepoID, Operation: AssignmentOperationRollback, Phase: AssignmentValidating, ExpectedGeneration: 2, TargetGeneration: 3, Current: current.AssignmentRef, Desired: desired, WasLoaded: false, StartedAt: time.Now().UTC()}
	if err := SaveAssignmentTransaction(l.DeliveryAssignmentTransactionPath(entries[0].RepoID), tx); err != nil {
		t.Fatal(err)
	}
	if err := WriteMaintenance(l.DeliveryAssignmentFencePath(entries[0].RepoID), Maintenance{Generation: "assignment-3", Desired: VersionRef{Version: desired.Version, Commit: desired.Commit}, RequestedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	report, err := controller.Rollback(context.Background(), entries[0].RepoPath, 2)
	if err != nil {
		t.Fatal(err)
	}
	if report.Assignment.Version != desired.Version || report.Assignment.Generation != 3 || report.Result != "succeeded" {
		t.Fatalf("report=%+v", report)
	}
	tx, err = LoadAssignmentTransaction(l.DeliveryAssignmentTransactionPath(entries[0].RepoID))
	if err != nil || tx.Phase != AssignmentRolledBack {
		t.Fatalf("transaction=%+v err=%v", tx, err)
	}
	if _, err := os.Stat(l.DeliveryAssignmentFencePath(entries[0].RepoID)); !os.IsNotExist(err) {
		t.Fatalf("resumed rollback retained fence: %v", err)
	}
}

func TestAssignmentResumesInterruptedRollback(t *testing.T) {
	l, configPath, entries, _ := assignmentFixture(t)
	runner := &releaseRunner{}
	controller := AssignmentController{Layout: l, ConfigPath: configPath, Runner: runner}
	if _, err := controller.MigrateConfig(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	cfg, _ := LoadConfig(configPath)
	current := cfg.Assignments[entries[0].RepoID]
	candidate, err := (Verifier{GH: "gh", Runner: runner, CacheDir: RuntimePaths(l.Root).Cache, ExpectedVersion: "v1.2.3"}).Check(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	desired := SlotRef(l, candidate.Manifest.Version, candidate.Manifest.Commit, candidate.Digest)
	if err := StageSlot(l, desired, filepath.Join(candidate.Dir, BinaryAsset)); err != nil {
		t.Fatal(err)
	}
	manager := launchd.Manager{Layout: l, Launchctl: entries[0].Commands["launchctl"]}
	if err := manager.WritePlist(entries[0], desired.Slot); err != nil {
		t.Fatal(err)
	}
	tx := AssignmentTransaction{RepositoryID: entries[0].RepoID, Operation: AssignmentOperationApply, Phase: AssignmentRollingBack, ExpectedGeneration: 1, TargetGeneration: 2, Current: current.AssignmentRef, Desired: desired, Reason: "injected failure", StartedAt: time.Now().UTC()}
	if err := SaveAssignmentTransaction(l.DeliveryAssignmentTransactionPath(entries[0].RepoID), tx); err != nil {
		t.Fatal(err)
	}
	if err := WriteMaintenance(l.DeliveryAssignmentFencePath(entries[0].RepoID), Maintenance{Generation: "assignment-2", Desired: VersionRef{Version: desired.Version, Commit: desired.Commit}, RequestedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if _, err := controller.switchTo(context.Background(), entries[0].RepoPath, desired, 1, false, false); err == nil || !strings.Contains(err.Error(), "rolled back") {
		t.Fatalf("resume error=%v", err)
	}
	program, err := manager.Program(entries[0])
	if err != nil || filepath.Clean(program) != filepath.Clean(current.Slot) {
		t.Fatalf("program=%q err=%v", program, err)
	}
	tx, _ = LoadAssignmentTransaction(l.DeliveryAssignmentTransactionPath(entries[0].RepoID))
	if tx.Phase != AssignmentRolledBack {
		t.Fatalf("transaction=%+v", tx)
	}
}

func TestAssignmentDrainTimeoutDoesNotStopActiveWorker(t *testing.T) {
	l, configPath, entries, _ := assignmentFixture(t)
	controller := AssignmentController{Layout: l, ConfigPath: configPath, Runner: &releaseRunner{}}
	if _, err := controller.MigrateConfig(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	cfg, _ := LoadConfig(configPath)
	commandLog := filepath.Join(t.TempDir(), "launchctl.log")
	t.Setenv("FAKE_LAUNCHCTL_LOG", commandLog)
	launchctl := filepath.Join(filepath.Dir(commandLog), "launchctl-active")
	script := `#!/bin/sh
printf '%s\n' "$*" >>"$FAKE_LAUNCHCTL_LOG"
case "$1" in
  print) printf 'state = running\npid = 8101\n' ;;
  *) exit 91 ;;
esac
`
	if err := os.WriteFile(launchctl, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	entries[0].Commands["launchctl"] = launchctl
	registered, _ := (registry.Store{Path: l.RegistryPath}).Load()
	registered.Repos[entries[0].RepoID] = entries[0]
	if err := fsutil.WriteJSON(l.RegistryPath, registered, 0o600); err != nil {
		t.Fatal(err)
	}
	manager := launchd.Manager{Layout: l, Launchctl: launchctl}
	if err := manager.WritePlist(entries[0], cfg.Assignments[entries[0].RepoID].Slot); err != nil {
		t.Fatal(err)
	}
	store := state.Store{Dir: l.RepoDir(entries[0].RepoID), RepoID: entries[0].RepoID, RepoPath: entries[0].RepoPath}
	if err := store.Ensure(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.StartExecution(state.ExecutionStart{IssueNumber: 1, Title: "fixture", RunID: "run-1", StartedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update("fixture_active_worker", 1, "run-1", nil, func(snapshot *state.Snapshot) error {
		snapshot.Supervisor.State = state.SupervisorStateMaintenance
		item := snapshot.Issues["1"]
		item.Status, item.WorkerPID, item.WorkerPGID = issuedomain.StatusRunning, 7101, 7101
		item.Worktree, item.Branch = "/tmp/issue-1", "codex/issue-1"
		item.Workspace = &state.WorkerWorkspace{Path: item.Worktree, Branch: item.Branch, RepoID: snapshot.RepoID,
			Repository: "owner/repo", GitCommonDir: snapshot.RepoPath + "/.git", MainCheckout: snapshot.RepoPath, CapturedAt: time.Now().UTC()}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	controller.Now = func() time.Time { return now }
	controller.Sleep = func(context.Context, time.Duration) error {
		now = now.Add(3 * time.Hour)
		return nil
	}
	if _, err := controller.Apply(context.Background(), entries[0].RepoPath, "v1.2.3", 1); err == nil || !strings.Contains(err.Error(), "drain deadline") {
		t.Fatalf("apply error=%v", err)
	}
	reports, err := controller.Status(context.Background(), entries[0].RepoPath)
	if err != nil || len(reports) != 1 {
		t.Fatalf("status=%+v err=%v", reports, err)
	}
	if reports[0].Result != "pending" || reports[0].Transaction == nil || reports[0].Transaction.Result != "deferred" || reports[0].FenceActive || reports[0].Assignment.Generation != 1 {
		t.Fatalf("deferred status=%+v", reports[0])
	}
	commands, _ := os.ReadFile(commandLog)
	if strings.Contains(string(commands), "bootout") || strings.Contains(string(commands), "bootstrap") {
		t.Fatalf("active worker caused LaunchAgent mutation:\n%s", commands)
	}
	snapshot, err := store.Load()
	if err != nil || snapshot.Issues["1"].WorkerPID != 7101 || snapshot.Issues["1"].WorkerPGID != 7101 {
		t.Fatalf("active worker changed: %+v err=%v", snapshot.Issues["1"], err)
	}
}

func TestAssignmentDeferredCanBeReplaced(t *testing.T) {
	for _, scenario := range []string{"apply", "rollback", "same target", "fence", "generation changed", "assignment changed", "stale request", "active"} {
		t.Run(scenario, func(t *testing.T) {
			l, configPath, entries, binary := assignmentFixture(t)
			controller := AssignmentController{Layout: l, ConfigPath: configPath, Runner: &releaseRunner{}}
			if _, err := controller.MigrateConfig(context.Background(), true); err != nil {
				t.Fatal(err)
			}
			cfg, err := LoadConfig(configPath)
			if err != nil {
				t.Fatal(err)
			}
			entry := entries[0]
			current := cfg.Assignments[entry.RepoID]
			previous := SlotRef(l, "v1.2.1", current.Commit, current.ArtifactSHA256)
			if err := StageSlot(l, previous, binary); err != nil {
				t.Fatal(err)
			}
			if scenario == "rollback" {
				if _, err := controller.Apply(context.Background(), entry.RepoPath, "v1.2.3", 1); err != nil {
					t.Fatal(err)
				}
				cfg, err = LoadConfig(configPath)
				if err != nil {
					t.Fatal(err)
				}
				current = cfg.Assignments[entry.RepoID]
				previous = *current.Previous
			}
			current.Previous = &previous
			cfg.Assignments[entry.RepoID] = current
			if err := WriteConfig(configPath, cfg); err != nil {
				t.Fatal(err)
			}
			manager := launchd.Manager{Layout: l, Launchctl: entry.Commands["launchctl"]}
			if err := manager.WritePlist(entry, current.Slot); err != nil {
				t.Fatal(err)
			}
			desired := SlotRef(l, "v1.2.4", current.Commit, current.ArtifactSHA256)
			if err := StageSlot(l, desired, current.Slot); err != nil {
				t.Fatal(err)
			}
			tx := AssignmentTransaction{RepositoryID: entry.RepoID, Operation: AssignmentOperationApply, Phase: AssignmentDraining, Result: "deferred", Reason: "drain deadline exceeded", ExpectedGeneration: current.Generation, TargetGeneration: current.Generation + 1, Current: current.AssignmentRef, Desired: desired, StartedAt: time.Now().UTC()}
			expected := current.Generation
			wantError := ""
			switch scenario {
			case "same target":
				plan, err := controller.Preview(context.Background(), entry.RepoPath, "v1.2.3")
				if err != nil {
					t.Fatal(err)
				}
				tx.Desired = plan.Desired
			case "fence":
				if err := WriteMaintenance(l.DeliveryAssignmentFencePath(entry.RepoID), Maintenance{Generation: "assignment-2", Desired: VersionRef{Version: desired.Version, Commit: desired.Commit}, RequestedAt: time.Now().UTC()}); err != nil {
					t.Fatal(err)
				}
				wantError = "still has a repository fence"
			case "generation changed":
				tx.ExpectedGeneration, tx.TargetGeneration = 2, 3
				wantError = "no longer matches"
			case "assignment changed":
				tx.Current = previous
				wantError = "no longer matches"
			case "stale request":
				expected = 2
				wantError = "stale assignment preview"
			case "active":
				tx.Result = "draining"
				wantError = "another assignment target"
			}
			txPath := l.DeliveryAssignmentTransactionPath(entry.RepoID)
			if err := SaveAssignmentTransaction(txPath, tx); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(txPath)
			if err != nil {
				t.Fatal(err)
			}
			var report AssignmentReport
			if scenario == "rollback" {
				report, err = controller.Rollback(context.Background(), entry.RepoPath, expected)
			} else {
				report, err = controller.Apply(context.Background(), entry.RepoPath, "v1.2.3", expected)
			}
			if wantError != "" {
				if err == nil || !strings.Contains(err.Error(), wantError) {
					t.Fatalf("error=%v, want %q", err, wantError)
				}
				after, readErr := os.ReadFile(txPath)
				if readErr != nil || string(after) != string(before) {
					t.Fatalf("rejected operation changed transaction: %v", readErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			wantVersion := "v1.2.3"
			if scenario == "rollback" {
				wantVersion = "v1.2.2"
			}
			if report.Assignment.Generation != current.Generation+1 || report.Assignment.Version != wantVersion || report.FenceActive || report.Result != "succeeded" {
				t.Fatalf("report=%+v", report)
			}
		})
	}
}

func TestAssignmentDrainRecoversProvenUnstartedConflictLaunch(t *testing.T) {
	l, _, entries, _ := assignmentFixture(t)
	entry := entries[0]
	store := state.Store{Dir: l.RepoDir(entry.RepoID), RepoID: entry.RepoID, RepoPath: entry.RepoPath}
	if err := store.Ensure(); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 5, 17, 20, 20, 136638000, time.UTC)
	if _, _, err := store.StartExecution(state.ExecutionStart{
		IssueNumber: 277, Title: "queue-level monitor", RunID: "conflict_174e861a0a076558", BaseSHA: "base", StartedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	workspace := &state.WorkerWorkspace{
		Path: "/state/worktrees/issue-277", Branch: "codex/issue-277", RepoID: entry.RepoID,
		Repository: "owner/a", GitCommonDir: entry.RepoPath + "/.git", MainCheckout: entry.RepoPath, CapturedAt: now,
	}
	if _, err := store.Update("fixture_unstarted_conflict_launch", 277, "conflict_174e861a0a076558", nil, func(snapshot *state.Snapshot) error {
		item := snapshot.Issues["277"]
		item.Status, item.LaunchSource, item.Generation = issuedomain.StatusLaunching, issuedomain.StatusResolvingConflict, 15
		item.Branch, item.Worktree, item.Workspace = workspace.Branch, workspace.Path, workspace
		item.PullRequestURL, item.PullRequestNumber, item.HeadSHA = "https://example.test/pull/281", 281, "head"
		item.Continuation = &state.ContinuationCheckpoint{
			ID: "checkpoint_b5676a437bbfb3dc", CreatedAt: now, RunID: item.RunID, Generation: 14,
			BaseSHA: "base", Workspace: workspace, HeadSHA: item.HeadSHA, PullRequestURL: item.PullRequestURL,
			PullRequestNumber: item.PullRequestNumber, Stage: issuedomain.ContinuationStageChecks,
		}
		item.ConflictRecovery = &state.ConflictRecovery{
			PullRequestURL: item.PullRequestURL, PreviousBaseSHA: "base", TargetBaseSHA: "target",
			OriginalHeadSHA: item.HeadSHA, ConflictFiles: []string{"monitor/internal/model/replay.go"},
		}
		snapshot.ActiveExecution.Generation = item.Generation
		snapshot.Supervisor.State = state.SupervisorStatePolling
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	sleepCalls := 0
	controller := AssignmentController{
		Layout: l, Now: func() time.Time { return now.Add(time.Minute) },
		Sleep: func(context.Context, time.Duration) error {
			sleepCalls++
			before, err := store.Load()
			if err != nil {
				return err
			}
			_, err = store.Update("fixture_drain_progress", 0, "", nil, func(snapshot *state.Snapshot) error {
				if sleepCalls == 1 {
					if before.ActiveExecution == nil || before.Issues["277"].Status != issuedomain.StatusLaunching {
						t.Fatal("assignment drain recovered before the supervisor entered drain")
					}
					snapshot.Supervisor.State = state.SupervisorStateDraining
					snapshot.Supervisor.StartedAt = now.Add(10 * time.Second)
				} else {
					if before.ActiveExecution != nil || before.Issues["277"].Status != issuedomain.StatusResolvingConflict {
						t.Fatal("assignment drain did not recover the prior-supervisor launch")
					}
					snapshot.Supervisor.State = state.SupervisorStateMaintenance
				}
				return nil
			})
			return err
		},
	}
	if err := controller.waitForDrain(context.Background(), DefaultConfig("owner/release"), entry); err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if sleepCalls != 2 || snapshot.ActiveExecution != nil || snapshot.Issues["277"].Status != issuedomain.StatusResolvingConflict {
		t.Fatalf("active=%+v issue=%+v", snapshot.ActiveExecution, snapshot.Issues["277"])
	}
}

func TestLegacyRuntimeSwitchUsesTargetStateLockWithoutGlobalFence(t *testing.T) {
	l, configPath, entries, legacyBinary := assignmentFixture(t)
	runner := &legacyAssignmentRunner{legacyPath: legacyBinary}
	controller := AssignmentController{Layout: l, ConfigPath: configPath, Runner: runner}
	if _, err := controller.MigrateConfig(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(t.TempDir(), "launchctl-state")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_LAUNCHCTL_STATE", stateDir)
	launchctl := filepath.Join(filepath.Dir(stateDir), "launchctl-legacy")
	script := `#!/bin/sh
state_dir=$FAKE_LAUNCHCTL_STATE
case "$1" in
  print) label=${2##*/}; [ -f "$state_dir/$label" ] || exit 1; pid=$(cat "$state_dir/$label"); printf 'state = running\npid = %s\n' "$pid" ;;
  bootout) label=${2##*/}; touch "$state_dir/$label.stopping" ;;
  bootstrap) label=$(basename "$3" .plist); printf '%s\n' 9201 >"$state_dir/$label" ;;
  *) exit 2 ;;
esac
`
	if err := os.WriteFile(launchctl, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	registered, _ := (registry.Store{Path: l.RegistryPath}).Load()
	for index := range entries {
		entries[index].Commands["launchctl"] = launchctl
		registered.Repos[entries[index].RepoID] = entries[index]
		pid := "8201"
		if index == 1 {
			pid = "8202"
		}
		if err := os.WriteFile(filepath.Join(stateDir, l.Label(entries[index].RepoID)), []byte(pid), 0o600); err != nil {
			t.Fatal(err)
		}
		store := state.Store{Dir: l.RepoDir(entries[index].RepoID), RepoID: entries[index].RepoID, RepoPath: entries[index].RepoPath}
		if err := store.Ensure(); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Update("fixture_polling", 0, "", nil, func(snapshot *state.Snapshot) error {
			snapshot.Supervisor.State = state.SupervisorStatePolling
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := fsutil.WriteJSON(l.RegistryPath, registered, 0o600); err != nil {
		t.Fatal(err)
	}
	otherBefore, _ := os.ReadFile(filepath.Join(stateDir, l.Label(entries[1].RepoID)))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	store := state.Store{Dir: l.RepoDir(entries[0].RepoID), RepoID: entries[0].RepoID, RepoPath: entries[0].RepoPath}
	servicePath := filepath.Join(stateDir, l.Label(entries[0].RepoID))
	stopped := make(chan error, 1)
	go func() {
		for {
			if _, err := os.Stat(servicePath + ".stopping"); err == nil {
				break
			}
			select {
			case <-ctx.Done():
				stopped <- ctx.Err()
				return
			case <-time.After(10 * time.Millisecond):
			}
		}
		_, err := store.Update("supervisor_stopped", 0, "", nil, func(snapshot *state.Snapshot) error {
			snapshot.Supervisor.State = state.SupervisorStateStopped
			snapshot.Supervisor.PID = 0
			return nil
		})
		if err == nil {
			err = os.Remove(servicePath)
		}
		stopped <- err
	}()
	report, err := controller.Apply(ctx, entries[0].RepoPath, "v1.2.3", 1)
	if err := <-stopped; err != nil {
		t.Fatal(err)
	}
	if ctx.Err() != nil {
		t.Fatalf("legacy shutdown exceeded deadline: %v", ctx.Err())
	}
	snapshot, loadErr := store.Load()
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if snapshot.Supervisor.State != state.SupervisorStateStopped {
		t.Fatalf("supervisor state=%s", snapshot.Supervisor.State)
	}
	events, readErr := os.ReadFile(store.EventsPath())
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !strings.Contains(string(events), `"type":"supervisor_stopped"`) {
		t.Fatalf("missing supervisor_stopped event: %s", events)
	}
	if err != nil {
		t.Fatal(err)
	}
	if !report.Runtime.Matches || report.Runtime.Launchd.PID != 9201 {
		t.Fatalf("report=%+v", report)
	}
	otherAfter, _ := os.ReadFile(filepath.Join(stateDir, l.Label(entries[1].RepoID)))
	if string(otherAfter) != string(otherBefore) {
		t.Fatalf("legacy bridge changed other repository PID: before=%q after=%q", otherBefore, otherAfter)
	}
	if _, err := os.Stat(RuntimePaths(l.Root).Maintenance); !os.IsNotExist(err) {
		t.Fatalf("legacy bridge created global maintenance fence: %v", err)
	}
}

func assignmentFixture(t *testing.T) (layout.Layout, string, []registry.Entry, string) {
	t.Helper()
	root := t.TempDir()
	l := layout.Layout{Root: root, RegistryPath: filepath.Join(root, "registry.json"), ReposRoot: filepath.Join(root, "repos"), BinDir: filepath.Join(root, "bin"), SkillsDir: filepath.Join(root, "skills"), LaunchAgents: filepath.Join(root, "launch")}
	for _, dir := range []string{l.BinDir, l.ReposRoot, l.LaunchAgents} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	binary := filepath.Join(l.BinDir, "agent-loop")
	data := []byte("v1.2.2 installed binary")
	if err := os.WriteFile(binary, data, 0o700); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	digest := hex.EncodeToString(sum[:])
	if err := fsutil.WriteJSON(filepath.Join(root, "install.json"), map[string]any{"version": "v1.2.2", "commit": strings.Repeat("a", 40), "schema_version": 4, "semantic_contract_version": 1, "binary_sha256": digest}, 0o600); err != nil {
		t.Fatal(err)
	}
	launchctl := filepath.Join(root, "launchctl")
	if err := os.WriteFile(launchctl, []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	entries := []registry.Entry{
		{RepoID: "repo-a", RepoPath: filepath.Join(root, "repo-a"), GitHubRepo: "owner/a", Commands: map[string]string{"launchctl": launchctl}},
		{RepoID: "repo-b", RepoPath: filepath.Join(root, "repo-b"), GitHubRepo: "owner/b", Commands: map[string]string{"launchctl": launchctl}},
	}
	registered := registry.Registry{Version: registry.CurrentVersion, Repos: map[string]registry.Entry{}}
	manager := launchd.Manager{Layout: l, Launchctl: launchctl}
	for index, entry := range entries {
		if err := os.MkdirAll(entry.RepoPath, 0o700); err != nil {
			t.Fatal(err)
		}
		canonical, err := filepath.EvalSymlinks(entry.RepoPath)
		if err != nil {
			t.Fatal(err)
		}
		entry.RepoPath = canonical
		entries[index] = entry
		registered.Repos[entry.RepoID] = entry
		if err := manager.WritePlist(entry, binary); err != nil {
			t.Fatal(err)
		}
	}
	if err := fsutil.WriteJSON(l.RegistryPath, registered, 0o600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "delivery.yaml")
	legacy := "version: 1\nenabled: true\nrelease_repository: owner/release\nchannel: stable\npoll_interval: 15m\ndrain_timeout: 2h30m\nauto_apply: schema_compatible\n"
	if err := os.WriteFile(configPath, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	return l, configPath, entries, binary
}

func TestLegacyDrainBeforeWorkerPIDIsSaved(t *testing.T) {
	for _, tc := range []struct {
		name              string
		originalSchema    bool
		persistNormalized bool
		idle              bool
	}{
		{name: "original legacy schema rejects inspection", originalSchema: true},
		{name: "running without execution authority"},
		{name: "persisted launch quarantine", persistNormalized: true},
		{name: "idle", idle: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l, configPath, entries, binary := assignmentFixture(t)
			entry := entries[0]
			controller := AssignmentController{Layout: l, ConfigPath: configPath, Runner: &legacyAssignmentRunner{legacyPath: binary}}
			if _, err := controller.MigrateConfig(context.Background(), true); err != nil {
				t.Fatal(err)
			}
			store := state.Store{Dir: l.RepoDir(entry.RepoID), RepoID: entry.RepoID, RepoPath: entry.RepoPath}
			if err := os.MkdirAll(store.Dir, 0o700); err != nil {
				t.Fatal(err)
			}
			// Protocol-0 snapshots used schema 4 and had no active_execution.
			raw := []byte(`{"version":4,"repo_id":"repo-a","supervisor":{"state":"polling","updated_at":"2026-09-07T00:00:00Z"},"issues":{"1":{"number":1,"status":"running","run_id":"run-legacy","generation":1,"updated_at":"2026-09-07T00:00:00Z"}},"pending_requests":{},"state_revision":0}`)
			var fixture map[string]any
			if err := json.Unmarshal(raw, &fixture); err != nil {
				t.Fatal(err)
			}
			fixture["repo_path"] = entry.RepoPath
			if !tc.originalSchema {
				// Use the current envelope to exercise normalization beyond schema rejection.
				empty, err := store.Load()
				if err != nil {
					t.Fatal(err)
				}
				fixture["version"] = empty.Version
			}
			if tc.idle {
				fixture["issues"].(map[string]any)["1"].(map[string]any)["status"] = "completed"
			}
			if err := fsutil.WriteJSON(store.StatePath(), fixture, 0o600); err != nil {
				t.Fatal(err)
			}
			if !tc.originalSchema && !tc.idle {
				if err := store.InspectExclusive(func(snapshot state.Snapshot) error {
					item := snapshot.Issues["1"]
					if snapshot.ActiveExecution != nil || item.Status != issuedomain.StatusBlocked ||
						item.Suspension == nil || item.Suspension.ReasonCode != "legacy_launch_authority" {
						t.Fatalf("unexpected normalization: %+v", snapshot)
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				if tc.persistNormalized {
					snapshot, err := store.Load()
					if err != nil {
						t.Fatal(err)
					}
					if err := fsutil.WriteJSON(store.StatePath(), snapshot, 0o600); err != nil {
						t.Fatal(err)
					}
				}
			}
			logPath := filepath.Join(t.TempDir(), "launchctl.log")
			t.Setenv("FAKE_LAUNCHCTL_LOG", logPath)
			script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >>\"$FAKE_LAUNCHCTL_LOG\"\ncase \"$1\" in\nprint) printf 'state = running\\npid = 8101\\n' ;;\n*) exit 91 ;;\nesac\n"
			if tc.idle {
				script = `#!/bin/sh
printf '%s\n' "$*" >>"$FAKE_LAUNCHCTL_LOG"
case "$1" in
  print) [ ! -f "$FAKE_LAUNCHCTL_LOG.stopped" ] || exit 1; printf 'state = running\npid = 8101\n' ;;
  bootout) touch "$FAKE_LAUNCHCTL_LOG.stopped" ;;
  *) exit 91 ;;
esac
`
			}
			if err := os.WriteFile(entry.Commands["launchctl"], []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			sleeps := 0
			controller.Now = func() time.Time { return now }
			controller.Sleep = func(context.Context, time.Duration) error {
				sleeps++
				now = now.Add(3 * time.Hour)
				return nil
			}
			if tc.idle {
				cfg, err := LoadConfig(configPath)
				if err != nil {
					t.Fatal(err)
				}
				err = controller.waitForLegacyIdleAndStop(context.Background(), cfg, entry, launchd.Manager{Layout: l, Launchctl: entry.Commands["launchctl"]})
				if err != nil || sleeps != 0 {
					t.Fatalf("idle drain: sleeps=%d err=%v", sleeps, err)
				}
			} else {
				report, err := controller.Apply(context.Background(), entry.RepoPath, "v1.2.3", 1)
				if err == nil || report.Transaction == nil || report.Transaction.Result != "deferred" {
					t.Fatalf("report=%+v err=%v", report, err)
				}
				if tc.originalSchema && (!strings.Contains(err.Error(), "schema migration required") || sleeps != 0) {
					t.Fatalf("sleeps=%d err=%v", sleeps, err)
				}
				if !tc.originalSchema && (!strings.Contains(err.Error(), "drain deadline") || sleeps != 1) {
					t.Fatalf("sleeps=%d err=%v", sleeps, err)
				}
			}
			commands, _ := os.ReadFile(logPath)
			if strings.Contains(string(commands), "bootout") != tc.idle || strings.Contains(string(commands), "bootstrap") {
				t.Fatalf("unexpected runtime mutation:\n%s", commands)
			}
		})
	}
}
