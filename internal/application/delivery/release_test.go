package delivery

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ishii1648/codex-issue-loop/internal/platform/fsutil"
	"github.com/ishii1648/codex-issue-loop/internal/platform/layout"
	"github.com/ishii1648/codex-issue-loop/internal/platform/registry"
)

const testCommit = "0123456789abcdef0123456789abcdef01234567"
const testTagObject = "abcdef0123456789abcdef0123456789abcdef01"

type releaseRunner struct {
	binaryRuns, updates, doctors, rollbacks int
	releaseViews                            int
	replaceReleaseAtView                    int
	attestationFailure                      bool
	badChecksum                             bool
	failFirstDoctor                         bool
	rollbackFailure                         bool
}

func (r *releaseRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	if filepath.Base(name) == BinaryAsset && len(args) > 0 && args[0] == "version" {
		r.binaryRuns++
		return json.Marshal(BinaryInfo{Version: "v1.2.3", Commit: testCommit, Target: "darwin/arm64", DeliveryProtocol: 1, StateSchemaCurrent: 4, StateSchemaMigrationFrom: 3, SemanticContractCurrent: 1, SemanticContractMinimum: 0})
	}
	if filepath.Base(name) == BinaryAsset && len(args) > 0 && args[0] == "update" {
		r.updates++
		backup := ""
		for index, arg := range args {
			if arg == "--delivery-backup" && index+1 < len(args) {
				backup = args[index+1]
			}
		}
		return json.Marshal(updateResult{Changed: true, Backup: backup})
	}
	if filepath.Base(name) == "agent-loop" && len(args) > 0 && args[0] == "doctor" {
		r.doctors++
		if r.failFirstDoctor && r.doctors == 1 {
			return nil, errors.New("doctor fixture failure")
		}
		return []byte(`{"schema_version":1,"ok":true}`), nil
	}
	if filepath.Base(name) == "agent-loop" && len(args) > 0 && args[0] == "rollback" {
		r.rollbacks++
		if r.rollbackFailure {
			return nil, errors.New("rollback fixture failure")
		}
		return []byte(`{"state_preserved":true}`), nil
	}
	if len(args) >= 2 && args[0] == "release" && args[1] == "view" {
		r.releaseViews++
		if r.replaceReleaseAtView > 0 && r.releaseViews >= r.replaceReleaseAtView {
			return []byte(`{"tagName":"v1.2.4","isDraft":false,"isPrerelease":false}`), nil
		}
		return []byte(`{"tagName":"v1.2.3","isDraft":false,"isPrerelease":false}`), nil
	}
	if len(args) >= 2 && args[0] == "api" && strings.Contains(args[1], "/git/ref/tags/") {
		return []byte(fmt.Sprintf(`{"object":{"type":"tag","sha":"%s"}}`, testTagObject)), nil
	}
	if len(args) >= 2 && args[0] == "api" && strings.Contains(args[1], "/git/tags/") {
		return []byte(fmt.Sprintf(`{"tag":"v1.2.3","object":{"type":"commit","sha":"%s"}}`, testCommit)), nil
	}
	if len(args) >= 2 && args[0] == "release" && args[1] == "download" {
		dir := ""
		for index, arg := range args {
			if arg == "--dir" && index+1 < len(args) {
				dir = args[index+1]
			}
		}
		return nil, r.writeAssets(dir)
	}
	if len(args) >= 2 && args[0] == "attestation" && args[1] == "verify" {
		if r.attestationFailure {
			return nil, errors.New("untrusted attestation")
		}
		return []byte("verified"), nil
	}
	return nil, fmt.Errorf("unexpected command %s %v", name, args)
}

func (r *releaseRunner) writeAssets(dir string) error {
	binary := []byte("verified candidate bytes")
	digest := sha256.Sum256(binary)
	digestText := hex.EncodeToString(digest[:])
	manifest := ReleaseManifest{ManifestVersion: 1, DeliveryProtocol: 1, Version: "v1.2.3", Commit: testCommit, Target: "darwin/arm64", Artifact: BinaryAsset, ArtifactSHA256: digestText, StateSchemaCurrent: 4, StateSchemaMigrationFrom: 3, SemanticContractCurrent: 1, SemanticContractMinimum: 0}
	manifestData, _ := json.MarshalIndent(manifest, "", "  ")
	manifestData = append(manifestData, '\n')
	manifestDigest := sha256.Sum256(manifestData)
	if err := os.WriteFile(filepath.Join(dir, BinaryAsset), binary, 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, ManifestAsset), manifestData, 0o600); err != nil {
		return err
	}
	checksum := digestText + "  " + BinaryAsset + "\n" + hex.EncodeToString(manifestDigest[:]) + "  " + ManifestAsset + "\n"
	if r.badChecksum {
		checksum = strings.Repeat("0", 64) + "  " + BinaryAsset + "\n" + hex.EncodeToString(manifestDigest[:]) + "  " + ManifestAsset + "\n"
	}
	return os.WriteFile(filepath.Join(dir, ChecksumAsset), []byte(checksum), 0o600)
}

func TestVerifierChecksEveryBoundaryBeforeExecutingCandidate(t *testing.T) {
	for _, test := range []struct {
		name     string
		runner   *releaseRunner
		wantRuns int
		wantErr  bool
	}{{"valid", &releaseRunner{}, 1, false}, {"checksum", &releaseRunner{badChecksum: true}, 0, true}, {"attestation", &releaseRunner{attestationFailure: true}, 0, true}} {
		t.Run(test.name, func(t *testing.T) {
			cfg := DefaultConfig("owner/repo")
			candidate, err := (Verifier{GH: "gh", Runner: test.runner, CacheDir: t.TempDir()}).Check(context.Background(), cfg)
			if (err != nil) != test.wantErr {
				t.Fatalf("candidate=%+v err=%v", candidate, err)
			}
			if test.runner.binaryRuns != test.wantRuns {
				t.Fatalf("binary runs=%d want=%d", test.runner.binaryRuns, test.wantRuns)
			}
		})
	}
}

func TestVerifierPersistsVerificationPhasesBeforeCandidateExecution(t *testing.T) {
	for _, test := range []struct {
		name        string
		stopAt      Phase
		wantPhases  []Phase
		wantRuns    int
		wantFailure bool
	}{
		{name: "complete", wantPhases: []Phase{PhaseDiscovered, PhaseDownloaded, PhaseVerified}, wantRuns: 1},
		{name: "interrupted after download", stopAt: PhaseDownloaded, wantPhases: []Phase{PhaseDiscovered, PhaseDownloaded}, wantFailure: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner := &releaseRunner{}
			var phases []Phase
			verifier := Verifier{
				GH:       "gh",
				Runner:   runner,
				CacheDir: t.TempDir(),
				Progress: func(progress VerificationProgress) error {
					phases = append(phases, progress.Phase)
					if progress.Desired != (VersionRef{Version: "v1.2.3", Commit: testCommit}) {
						t.Fatalf("unexpected desired release: %+v", progress.Desired)
					}
					if progress.Phase == test.stopAt {
						return errors.New("simulated process stop")
					}
					return nil
				},
			}
			_, err := verifier.Check(context.Background(), DefaultConfig("owner/repo"))
			if (err != nil) != test.wantFailure {
				t.Fatalf("err=%v want_failure=%t", err, test.wantFailure)
			}
			if fmt.Sprint(phases) != fmt.Sprint(test.wantPhases) || runner.binaryRuns != test.wantRuns {
				t.Fatalf("phases=%v want=%v binary_runs=%d want=%d", phases, test.wantPhases, runner.binaryRuns, test.wantRuns)
			}
		})
	}
}

func TestVerifierReusesOnlyMatchingVerifiedCache(t *testing.T) {
	cache := t.TempDir()
	runner := &releaseRunner{}
	verifier := Verifier{GH: "gh", Runner: runner, CacheDir: cache}
	cfg := DefaultConfig("owner/repo")
	first, err := verifier.Check(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	second, err := verifier.Check(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if first.Dir != second.Dir || runner.binaryRuns != 2 {
		t.Fatalf("first=%+v second=%+v binary_runs=%d", first, second, runner.binaryRuns)
	}
	if err := os.WriteFile(filepath.Join(first.Dir, BinaryAsset), []byte("tampered"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := verifier.Check(context.Background(), cfg); err == nil || !strings.Contains(err.Error(), "cache entry") {
		t.Fatalf("tampered cache accepted: %v", err)
	}
}

func TestExecRunnerDoesNotExposeCommandOutputOrSignedURL(t *testing.T) {
	command := filepath.Join(t.TempDir(), "gh")
	if err := os.WriteFile(command, []byte("#!/bin/sh\necho 'https://example.test/asset?token=top-secret' >&2\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := (ExecRunner{}).Run(context.Background(), command, "release", "download")
	if err == nil {
		t.Fatal("failing command succeeded")
	}
	if strings.Contains(err.Error(), "top-secret") || strings.Contains(err.Error(), "example.test") {
		t.Fatalf("command output leaked: %v", err)
	}
}

func TestCompatibilityBlocksMajorSchemaDowngradeAndRetag(t *testing.T) {
	base := Candidate{Manifest: ReleaseManifest{Version: "v1.2.3", Commit: testCommit, StateSchemaCurrent: 4, SemanticContractCurrent: 1, SemanticContractMinimum: 0}}
	if plan := PlanCompatibility(VersionRef{Version: "v1.2.2", Commit: strings.Repeat("a", 40)}, 4, 1, base); !plan.Allowed {
		t.Fatalf("compatible update blocked: %+v", plan)
	}
	major := base
	major.Manifest.Version = "v2.0.0"
	if plan := PlanCompatibility(VersionRef{Version: "v1.9.0", Commit: "old"}, 4, 1, major); plan.Result != "blocked_for_approval" {
		t.Fatalf("major plan=%+v", plan)
	}
	schema := base
	schema.Manifest.StateSchemaCurrent = 5
	if plan := PlanCompatibility(VersionRef{Version: "v1.2.2", Commit: "old"}, 4, 1, schema); plan.Result != "blocked_for_approval" {
		t.Fatalf("schema plan=%+v", plan)
	}
	semantic := base
	semantic.Manifest.SemanticContractMinimum = 2
	semantic.Manifest.SemanticContractCurrent = 2
	if plan := PlanCompatibility(VersionRef{Version: "v1.2.2", Commit: "old"}, 4, 1, semantic); plan.Result != "blocked_for_approval" {
		t.Fatalf("semantic plan=%+v", plan)
	}
	down := base
	down.Manifest.Version = "v1.2.1"
	if plan := PlanCompatibility(VersionRef{Version: "v1.2.2", Commit: "old"}, 4, 1, down); plan.Reason != "implicit downgrade is not allowed" {
		t.Fatalf("downgrade plan=%+v", plan)
	}
	if plan := PlanCompatibility(VersionRef{Version: "v1.2.3", Commit: "different"}, 4, 1, base); plan.Reason != "same version resolves to a different commit" {
		t.Fatalf("retag plan=%+v", plan)
	}
}

func TestFaultControllerApplyAndDoctorFailureRollback(t *testing.T) {
	for _, test := range []struct {
		name            string
		failDoctor      bool
		wantResult      string
		wantRollback    int
		rollbackFailure bool
		keepFence       bool
		wantErr         bool
	}{{"success", false, "succeeded", 0, false, false, false}, {"rollback", true, "rolled_back", 1, false, false, true}, {"rollback failure", true, "rollback_failed", 1, true, true, true}} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			l := layout.Layout{Root: root, RegistryPath: filepath.Join(root, "registry.json"), ReposRoot: filepath.Join(root, "repos"), BinDir: filepath.Join(root, "bin"), SkillsDir: filepath.Join(root, "skills"), LaunchAgents: filepath.Join(root, "launch")}
			if err := os.MkdirAll(l.BinDir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := fsutil.WriteJSON(filepath.Join(root, "install.json"), map[string]any{"version": "v1.2.2", "commit": strings.Repeat("a", 40), "schema_version": 4, "semantic_contract_version": 1}, 0o600); err != nil {
				t.Fatal(err)
			}
			configPath := filepath.Join(root, "delivery.yaml")
			if err := WriteConfig(configPath, DefaultConfig("owner/repo")); err != nil {
				t.Fatal(err)
			}
			runner := &releaseRunner{failFirstDoctor: test.failDoctor, rollbackFailure: test.rollbackFailure}
			fixed := time.Date(2026, 8, 19, 1, 2, 3, 0, time.UTC)
			controller := Controller{Layout: l, ConfigPath: configPath, GH: "gh", Runner: runner, Now: func() time.Time { return fixed }, Soak: -1}
			report, err := controller.Reconcile(context.Background(), true)
			if (err != nil) != test.wantErr {
				t.Fatalf("report=%+v err=%v", report, err)
			}
			if report.Result != test.wantResult || runner.rollbacks != test.wantRollback {
				t.Fatalf("report=%+v rollbacks=%d", report, runner.rollbacks)
			}
			_, fenceErr := os.Stat(RuntimePaths(root).Maintenance)
			if test.keepFence && fenceErr != nil {
				t.Fatalf("maintenance fence was not retained: %v", fenceErr)
			}
			if !test.keepFence && !errors.Is(fenceErr, os.ErrNotExist) {
				t.Fatalf("maintenance fence was not cleared: %v", fenceErr)
			}
			if test.wantResult == "rolled_back" {
				binaryRuns := runner.binaryRuns
				next, nextErr := controller.Reconcile(context.Background(), false)
				if nextErr != nil || next.Result != "rolled_back" || runner.binaryRuns != binaryRuns {
					t.Fatalf("automatic reconcile retried failed candidate: next=%+v err=%v binary_runs=%d/%d", next, nextErr, runner.binaryRuns, binaryRuns)
				}
			}
		})
	}
}

func TestRetryRollbackFinalizesAlreadyRestoredPreviousVersion(t *testing.T) {
	controller, paths, tx, runner := rollbackRetryFixture(t, VersionRef{Version: "v1.2.2", Commit: strings.Repeat("a", 40)})
	report, err := controller.RetryRollback(context.Background(), tx.BackupPath)
	if err != nil || report.Result != "rolled_back" || runner.rollbacks != 0 || runner.doctors != 1 {
		t.Fatalf("report=%+v err=%v runner=%+v", report, err, runner)
	}
	if _, err := os.Stat(paths.Maintenance); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("successful retry retained fence: %v", err)
	}
	stored, err := LoadTransaction(paths.Transaction)
	if err != nil || stored.Current != tx.Previous || stored.Phase != PhaseVerified || stored.LastResult != "rolled_back" {
		t.Fatalf("transaction=%+v err=%v", stored, err)
	}
}

func TestEnsureExpectedStartedRestartsLoadedNotRunningRepository(t *testing.T) {
	root := t.TempDir()
	statePath := filepath.Join(root, "launchd-state")
	if err := os.WriteFile(statePath, []byte("loaded\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	launchctl := filepath.Join(root, "launchctl")
	script := `#!/bin/sh
case "$1" in
  print)
    test -f "$DELIVERY_LAUNCHD_STATE" || exit 1
    if grep -q running "$DELIVERY_LAUNCHD_STATE"; then
      printf 'state = running\npid = 123\n'
    else
      printf 'state = not running\n'
    fi
    ;;
  bootout)
    rm -f "$DELIVERY_LAUNCHD_STATE"
    ;;
  bootstrap)
    printf 'running\n' > "$DELIVERY_LAUNCHD_STATE"
    ;;
  *) exit 2 ;;
esac
`
	if err := os.WriteFile(launchctl, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DELIVERY_LAUNCHD_STATE", statePath)
	l := layout.Layout{Root: root, RegistryPath: filepath.Join(root, "registry.json"), LaunchAgents: filepath.Join(root, "launch")}
	entry := registry.Entry{RepoID: "repo-id", RepoPath: filepath.Join(root, "repo"), Commands: map[string]string{"launchctl": launchctl}}
	if err := fsutil.WriteJSON(l.RegistryPath, registry.Registry{Version: registry.CurrentVersion, Repos: map[string]registry.Entry{entry.RepoID: entry}}, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := (Controller{Layout: l}).ensureExpectedStarted(context.Background(), []string{entry.RepoID}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(statePath)
	if err != nil || strings.TrimSpace(string(data)) != "running" {
		t.Fatalf("loaded-not-running service was not restarted: state=%q err=%v", data, err)
	}
}

func TestRetryRollbackRestoresDesiredVersionAndRetainsFenceOnFailure(t *testing.T) {
	controller, paths, tx, runner := rollbackRetryFixture(t, txDesiredVersion())
	runner.rollbackFailure = true
	report, err := controller.RetryRollback(context.Background(), tx.BackupPath)
	if err == nil || report.Result != "rollback_failed" || runner.rollbacks != 1 {
		t.Fatalf("report=%+v err=%v runner=%+v", report, err, runner)
	}
	if _, err := os.Stat(paths.Maintenance); err != nil {
		t.Fatalf("failed retry removed fence: %v", err)
	}
}

func TestRetryRollbackRejectsMismatchedAuthorityWithoutRunningCommands(t *testing.T) {
	controller, paths, tx, runner := rollbackRetryFixture(t, txDesiredVersion())
	if _, err := controller.RetryRollback(context.Background(), tx.BackupPath+"-other"); err == nil {
		t.Fatal("mismatched backup was accepted")
	}
	if runner.binaryRuns != 0 {
		t.Fatalf("commands ran before backup validation: %+v", runner)
	}
	if err := WriteMaintenance(paths.Maintenance, Maintenance{Generation: "different", Desired: tx.Desired}); err != nil {
		t.Fatal(err)
	}
	if _, err := controller.RetryRollback(context.Background(), tx.BackupPath); err == nil {
		t.Fatal("mismatched maintenance fence was accepted")
	}
	if runner.binaryRuns != 0 {
		t.Fatalf("commands ran before fence validation: %+v", runner)
	}
}

func rollbackRetryFixture(t *testing.T, installed VersionRef) (Controller, Paths, Transaction, *releaseRunner) {
	t.Helper()
	root := t.TempDir()
	l := layout.Layout{Root: root, RegistryPath: filepath.Join(root, "registry.json"), ReposRoot: filepath.Join(root, "repos"), BinDir: filepath.Join(root, "bin"), SkillsDir: filepath.Join(root, "skills"), LaunchAgents: filepath.Join(root, "launch")}
	if err := os.MkdirAll(l.BinDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := fsutil.WriteJSON(filepath.Join(root, "install.json"), map[string]any{"version": installed.Version, "commit": installed.Commit, "schema_version": 4, "semantic_contract_version": 1}, 0o600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "delivery.yaml")
	if err := WriteConfig(configPath, DefaultConfig("owner/repo")); err != nil {
		t.Fatal(err)
	}
	paths := RuntimePaths(root)
	if err := paths.Ensure(); err != nil {
		t.Fatal(err)
	}
	previous := VersionRef{Version: "v1.2.2", Commit: strings.Repeat("a", 40)}
	tx := Transaction{Version: 1, Phase: PhaseValidating, Current: txDesiredVersion(), Previous: previous, Desired: txDesiredVersion(), MaintenanceGeneration: "maintenance_retry", BackupPath: filepath.Join(root, "backups", "delivery-maintenance_retry-v1.2.2"), LastResult: "rollback_failed", Reason: "post-update doctor failed"}
	if err := SaveTransaction(paths.Transaction, tx); err != nil {
		t.Fatal(err)
	}
	if err := WriteMaintenance(paths.Maintenance, Maintenance{Generation: tx.MaintenanceGeneration, Desired: tx.Desired}); err != nil {
		t.Fatal(err)
	}
	runner := &releaseRunner{}
	controller := Controller{Layout: l, ConfigPath: configPath, GH: "gh", Runner: runner, Soak: -1}
	return controller, paths, tx, runner
}

func txDesiredVersion() VersionRef {
	return VersionRef{Version: "v1.2.3", Commit: testCommit}
}

func TestFaultControllerResumesPostApplyValidationWithoutReapplying(t *testing.T) {
	root := t.TempDir()
	l := layout.Layout{Root: root, RegistryPath: filepath.Join(root, "registry.json"), ReposRoot: filepath.Join(root, "repos"), BinDir: filepath.Join(root, "bin"), SkillsDir: filepath.Join(root, "skills"), LaunchAgents: filepath.Join(root, "launch")}
	if err := os.MkdirAll(l.BinDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := fsutil.WriteJSON(filepath.Join(root, "install.json"), map[string]any{"version": "v1.2.3", "commit": testCommit, "schema_version": 4, "semantic_contract_version": 1}, 0o600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "delivery.yaml")
	if err := WriteConfig(configPath, DefaultConfig("owner/repo")); err != nil {
		t.Fatal(err)
	}
	paths := RuntimePaths(root)
	if err := paths.Ensure(); err != nil {
		t.Fatal(err)
	}
	tx := Transaction{Version: 1, Phase: PhaseApplying, Attempt: 1, Current: VersionRef{Version: "v1.2.2", Commit: strings.Repeat("a", 40)}, Previous: VersionRef{Version: "v1.2.2", Commit: strings.Repeat("a", 40)}, Desired: VersionRef{Version: "v1.2.3", Commit: testCommit}, MaintenanceGeneration: "maintenance_resume", BackupPath: filepath.Join(root, "backups", "delivery-maintenance_resume-v1.2.2")}
	if err := SaveTransaction(paths.Transaction, tx); err != nil {
		t.Fatal(err)
	}
	if err := WriteMaintenance(paths.Maintenance, Maintenance{Generation: tx.MaintenanceGeneration, Desired: tx.Desired}); err != nil {
		t.Fatal(err)
	}
	runner := &releaseRunner{}
	report, err := (Controller{Layout: l, ConfigPath: configPath, GH: "gh", Runner: runner, Soak: -1}).Reconcile(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if report.Result != "succeeded" || runner.updates != 0 || runner.doctors != 1 {
		t.Fatalf("report=%+v updates=%d doctors=%d", report, runner.updates, runner.doctors)
	}
	if _, err := os.Stat(paths.Maintenance); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fence remains: %v", err)
	}
}

func TestFaultControllerClearsFenceAfterSucceededTransactionWasPersisted(t *testing.T) {
	root := t.TempDir()
	l := layout.Layout{Root: root, RegistryPath: filepath.Join(root, "registry.json"), ReposRoot: filepath.Join(root, "repos"), BinDir: filepath.Join(root, "bin"), SkillsDir: filepath.Join(root, "skills"), LaunchAgents: filepath.Join(root, "launch")}
	if err := os.MkdirAll(l.BinDir, 0o700); err != nil {
		t.Fatal(err)
	}
	current := VersionRef{Version: "v1.2.3", Commit: testCommit}
	if err := fsutil.WriteJSON(filepath.Join(root, "install.json"), map[string]any{"version": current.Version, "commit": current.Commit, "schema_version": 4, "semantic_contract_version": 1}, 0o600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "delivery.yaml")
	cfg := DefaultConfig("owner/repo")
	cfg.Enabled = false
	if err := WriteConfig(configPath, cfg); err != nil {
		t.Fatal(err)
	}
	paths := RuntimePaths(root)
	if err := paths.Ensure(); err != nil {
		t.Fatal(err)
	}
	tx := Transaction{Version: 1, Phase: PhaseSucceeded, Current: current, Previous: VersionRef{Version: "v1.2.2", Commit: strings.Repeat("a", 40)}, Desired: current, MaintenanceGeneration: "maintenance_succeeded", LastResult: "succeeded"}
	if err := SaveTransaction(paths.Transaction, tx); err != nil {
		t.Fatal(err)
	}
	if err := WriteMaintenance(paths.Maintenance, Maintenance{Generation: tx.MaintenanceGeneration, Desired: tx.Desired}); err != nil {
		t.Fatal(err)
	}
	runner := &releaseRunner{}
	report, err := (Controller{Layout: l, ConfigPath: configPath, GH: "gh", Runner: runner, Soak: -1}).Reconcile(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if report.Result != "paused" || runner.binaryRuns != 0 || runner.updates != 0 {
		t.Fatalf("report=%+v runner=%+v", report, runner)
	}
	if _, err := os.Stat(paths.Maintenance); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("succeeded transaction fence remains: %v", err)
	}
}

func TestFaultControllerCompletesInterruptedRollbackWithoutReapplying(t *testing.T) {
	root := t.TempDir()
	l := layout.Layout{Root: root, RegistryPath: filepath.Join(root, "registry.json"), ReposRoot: filepath.Join(root, "repos"), BinDir: filepath.Join(root, "bin"), SkillsDir: filepath.Join(root, "skills"), LaunchAgents: filepath.Join(root, "launch")}
	if err := os.MkdirAll(l.BinDir, 0o700); err != nil {
		t.Fatal(err)
	}
	previous := VersionRef{Version: "v1.2.2", Commit: strings.Repeat("a", 40)}
	if err := fsutil.WriteJSON(filepath.Join(root, "install.json"), map[string]any{"version": previous.Version, "commit": previous.Commit, "schema_version": 4, "semantic_contract_version": 1}, 0o600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "delivery.yaml")
	if err := WriteConfig(configPath, DefaultConfig("owner/repo")); err != nil {
		t.Fatal(err)
	}
	paths := RuntimePaths(root)
	if err := paths.Ensure(); err != nil {
		t.Fatal(err)
	}
	tx := Transaction{Version: 1, Phase: PhaseValidating, Current: VersionRef{Version: "v1.2.3", Commit: testCommit}, Previous: previous, Desired: VersionRef{Version: "v1.2.3", Commit: testCommit}, MaintenanceGeneration: "maintenance_rollback", BackupPath: filepath.Join(root, "backups", "delivery-maintenance_rollback-v1.2.2"), LastResult: "rolling_back", Reason: "doctor failed"}
	if err := SaveTransaction(paths.Transaction, tx); err != nil {
		t.Fatal(err)
	}
	if err := WriteMaintenance(paths.Maintenance, Maintenance{Generation: tx.MaintenanceGeneration, Desired: tx.Desired}); err != nil {
		t.Fatal(err)
	}
	runner := &releaseRunner{}
	report, err := (Controller{Layout: l, ConfigPath: configPath, GH: "gh", Runner: runner, Soak: -1}).Reconcile(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if report.Result != "rolled_back" || runner.updates != 0 || runner.binaryRuns != 0 || runner.doctors != 1 {
		t.Fatalf("report=%+v runner=%+v", report, runner)
	}
	if _, err := os.Stat(paths.Maintenance); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fence remains: %v", err)
	}
}

func TestControllerExplicitVersionCannotRaceToDifferentLatestRelease(t *testing.T) {
	root := t.TempDir()
	l := layout.Layout{Root: root, RegistryPath: filepath.Join(root, "registry.json"), ReposRoot: filepath.Join(root, "repos"), BinDir: filepath.Join(root, "bin"), SkillsDir: filepath.Join(root, "skills"), LaunchAgents: filepath.Join(root, "launch")}
	if err := os.MkdirAll(l.BinDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := fsutil.WriteJSON(filepath.Join(root, "install.json"), map[string]any{"version": "v1.2.2", "commit": strings.Repeat("a", 40), "schema_version": 4, "semantic_contract_version": 1}, 0o600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "delivery.yaml")
	if err := WriteConfig(configPath, DefaultConfig("owner/repo")); err != nil {
		t.Fatal(err)
	}
	runner := &releaseRunner{}
	report, err := (Controller{Layout: l, ConfigPath: configPath, GH: "gh", Runner: runner, ExpectedVersion: "v1.2.4", Soak: -1}).Reconcile(context.Background(), true)
	if err == nil || report.Result != "blocked" || runner.updates != 0 {
		t.Fatalf("report=%+v err=%v updates=%d", report, err, runner.updates)
	}
	if _, statErr := os.Stat(RuntimePaths(root).Maintenance); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("version mismatch created maintenance fence: %v", statErr)
	}
}

func TestFaultControllerRejectsReleaseReplacementAfterDrain(t *testing.T) {
	root := t.TempDir()
	l := layout.Layout{Root: root, RegistryPath: filepath.Join(root, "registry.json"), ReposRoot: filepath.Join(root, "repos"), BinDir: filepath.Join(root, "bin"), SkillsDir: filepath.Join(root, "skills"), LaunchAgents: filepath.Join(root, "launch")}
	if err := os.MkdirAll(l.BinDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := fsutil.WriteJSON(filepath.Join(root, "install.json"), map[string]any{"version": "v1.2.2", "commit": strings.Repeat("a", 40), "schema_version": 4, "semantic_contract_version": 1}, 0o600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "delivery.yaml")
	if err := WriteConfig(configPath, DefaultConfig("owner/repo")); err != nil {
		t.Fatal(err)
	}
	runner := &releaseRunner{replaceReleaseAtView: 3}
	report, err := (Controller{Layout: l, ConfigPath: configPath, GH: "gh", Runner: runner, Soak: -1}).Reconcile(context.Background(), true)
	if err == nil || report.Result != "blocked" || runner.updates != 0 {
		t.Fatalf("report=%+v err=%v runner=%+v", report, err, runner)
	}
	if _, statErr := os.Stat(RuntimePaths(root).Maintenance); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("release replacement retained maintenance fence before apply: %v", statErr)
	}
}
