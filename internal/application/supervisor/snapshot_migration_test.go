package supervisor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	gh "github.com/ishii1648/codex-issue-loop/internal/adapter/github"
	"github.com/ishii1648/codex-issue-loop/internal/adapter/state"
	"github.com/ishii1648/codex-issue-loop/internal/adapter/worker"
	"github.com/ishii1648/codex-issue-loop/internal/application/migration"
	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
	"github.com/ishii1648/codex-issue-loop/internal/platform/config"
	"github.com/ishii1648/codex-issue-loop/internal/platform/layout"
)

type cancelMigrationGitHub struct {
	gh.Client
	closes int
	fail   bool
}

func (g *cancelMigrationGitHub) ClosePullRequest(context.Context, config.Config, string) error {
	g.closes++
	if g.fail {
		g.fail = false
		return errors.New("temporary GitHub failure")
	}
	return nil
}

func TestStartupMigratesLegacyCancellationBeforeGitHubAndRetriesProjection(t *testing.T) {
	for _, withPR := range []bool{false, true} {
		t.Run(map[bool]string{false: "no PR", true: "open PR"}[withPR], func(t *testing.T) {
			loop, github := testLoop(t, worker.Result{})
			root := t.TempDir()
			l := layout.Layout{Root: root, ReposRoot: filepath.Join(root, "repos"), RegistryPath: filepath.Join(root, "registry.json")}
			store := state.Store{Dir: l.RepoDir(loop.Store.RepoID), RepoID: loop.Store.RepoID, RepoPath: loop.Store.RepoPath}
			if err := store.Initialize(); err != nil {
				t.Fatal(err)
			}
			snapshot, err := store.Load()
			if err != nil {
				t.Fatal(err)
			}
			canceledAt := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
			snapshot.Issues["17"] = &state.Issue{Number: 17, Status: issuedomain.StatusBlocked, Worktree: "/missing/worktree", Suspension: &state.Suspension{ID: "suspension_legacy", Origin: "operator", Status: issuedomain.SuspensionResolved, ReasonCode: "environment", Recoverability: issuedomain.RecoverabilityOperator, Reason: "operator canceled", AllowedActions: []issuedomain.ResolutionAction{issuedomain.ResolutionCancel}, SuspendedAt: canceledAt.Add(-time.Hour), ResolvedAt: canceledAt, Resolution: issuedomain.ResolutionCancel}}
			if withPR {
				snapshot.Issues["17"].PullRequestURL = "https://github.com/owner/repo/pull/42"
				snapshot.Issues["17"].PullRequestNumber = 42
			}
			snapshot.Version, snapshot.SemanticContractVersion, snapshot.IssueLifecycleAPIVersion = 5, 4, "2.1"
			data, err := json.Marshal(snapshot)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(store.StatePath(), data, 0600); err != nil {
				t.Fatal(err)
			}
			loop.Store, loop.MigrationLayout = store, l
			adapter := &cancelMigrationGitHub{Client: github, fail: withPR}
			loop.GitHub = adapter
			lock, err := store.AcquireSupervisorLock()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := (migration.Migrator{Layout: l}).ApplySnapshots(store.Dir); err != nil {
				t.Fatal(err)
			}
			state.ReleaseSupervisorLock(lock)
			migrated, err := store.Load()
			if err != nil {
				t.Fatal(err)
			}
			before, _ := os.ReadFile(store.StatePath())
			if err := loop.reconcileIssueProjection(context.Background(), 17); (err != nil) != withPR {
				t.Fatalf("err=%v", err)
			}
			after, _ := os.ReadFile(store.StatePath())
			if !bytes.Equal(before, after) {
				t.Fatal("GitHub failure rolled back or changed internal cancellation")
			}
			if err := loop.reconcileIssueProjection(context.Background(), 17); err != nil {
				t.Fatal(err)
			}
			if migrated.Version != 6 || migrated.Issues["17"].Status != issuedomain.StatusCanceled || !migrated.Issues["17"].Cancellation.CanceledAt.Equal(canceledAt) {
				t.Fatalf("migrated=%+v", migrated)
			}
			if withPR && adapter.closes != 2 || !withPR && adapter.closes != 0 {
				t.Fatalf("closes=%d", adapter.closes)
			}
		})
	}
}

func TestRunCommitsMigrationBeforeAnyGitHubProjection(t *testing.T) {
	loop, github := testLoop(t, worker.Result{})
	root := t.TempDir()
	l := layout.Layout{Root: root, ReposRoot: filepath.Join(root, "repos"), RegistryPath: filepath.Join(root, "registry.json")}
	store := state.Store{Dir: l.RepoDir(loop.Store.RepoID), RepoID: loop.Store.RepoID, RepoPath: loop.Store.RepoPath}
	if err := store.Initialize(); err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Issues["17"] = &state.Issue{Number: 17, Status: issuedomain.StatusBlocked}
	snapshot.Version, snapshot.SemanticContractVersion, snapshot.IssueLifecycleAPIVersion = 5, 4, "2.0"
	data, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.StatePath(), data, 0600); err != nil {
		t.Fatal(err)
	}
	loop.Store, loop.MigrationLayout = store, l
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	projected := false
	github.projectionHook = func(int, issuedomain.Status) error {
		migrated, err := store.Load()
		if err != nil {
			return err
		}
		if migrated.Version != 6 {
			return errors.New("GitHub observed legacy snapshot")
		}
		journal, err := os.ReadFile(filepath.Join(root, "migration.json"))
		if err != nil {
			return err
		}
		if !bytes.Contains(journal, []byte(`"status": "completed"`)) {
			return errors.New("GitHub preceded migration commit")
		}
		projected = true
		cancel()
		return nil
	}
	err = loop.Run(ctx)
	if err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if !projected {
		t.Fatal("startup did not reach GitHub after migration")
	}
}
