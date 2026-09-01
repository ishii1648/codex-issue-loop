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
	legacyPath string
}

func (r *legacyAssignmentRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	if filepath.Clean(name) == filepath.Clean(r.legacyPath) && len(args) > 0 && args[0] == "version" {
		return json.Marshal(BinaryInfo{Version: "v1.2.2", Commit: strings.Repeat("a", 40), Target: "darwin/arm64", DeliveryProtocol: 1, StateSchemaCurrent: 4, StateSchemaMigrationFrom: 3, SemanticContractCurrent: 1})
	}
	return r.releaseRunner.Run(ctx, name, args...)
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
	report, err := controller.switchTo(context.Background(), entries[0].RepoPath, desired, 1, false)
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
	if _, err := controller.switchTo(context.Background(), entries[0].RepoPath, desired, 1, false); err == nil || !strings.Contains(err.Error(), "rolled back") {
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
	if _, err := store.Update("fixture_active_worker", 1, "run-1", nil, func(snapshot *state.Snapshot) error {
		snapshot.Supervisor.State = state.SupervisorStateMaintenance
		snapshot.Issues["1"] = &state.Issue{Number: 1, Status: issuedomain.StatusRunning, RunID: "run-1", WorkerPID: 7101, WorkerPGID: 7101}
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
	commands, _ := os.ReadFile(commandLog)
	if strings.Contains(string(commands), "bootout") || strings.Contains(string(commands), "bootstrap") {
		t.Fatalf("active worker caused LaunchAgent mutation:\n%s", commands)
	}
	snapshot, err := store.Load()
	if err != nil || snapshot.Issues["1"].WorkerPID != 7101 || snapshot.Issues["1"].WorkerPGID != 7101 {
		t.Fatalf("active worker changed: %+v err=%v", snapshot.Issues["1"], err)
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
  bootout) label=${2##*/}; rm -f "$state_dir/$label" ;;
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
	report, err := controller.Apply(context.Background(), entries[0].RepoPath, "v1.2.3", 1)
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
