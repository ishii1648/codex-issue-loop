package app

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ishii1648/codex-issue-loop/internal/adapter/state"
	"github.com/ishii1648/codex-issue-loop/internal/application/delivery"
	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
	"github.com/ishii1648/codex-issue-loop/internal/platform/launchd"
	"github.com/ishii1648/codex-issue-loop/internal/platform/registry"
)

func TestUnregisterDeletedRepository(t *testing.T) {
	for _, tc := range []struct {
		name      string
		implicit  bool
		pid, pgid int
		corrupt   bool
		wantError string
	}{
		{name: "explicit path"},
		{name: "single registration", implicit: true},
		{name: "worker identity", pid: 123, pgid: 123, wantError: "worker identity remains in state"},
		{name: "unreadable state", corrupt: true, wantError: "cannot verify absence of workers"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, l, entry, store, launchctl := operatorControlFixture(t)
			manager := launchd.Manager{Layout: l, Launchctl: launchctl}
			if err := manager.WritePlist(entry, launchctl); err != nil {
				t.Fatal(err)
			}
			if err := manager.WriteBrokerPlist(launchctl, entry.EnvironmentPath); err != nil {
				t.Fatal(err)
			}
			assignmentPath, err := delivery.DefaultConfigPath()
			if err != nil {
				t.Fatal(err)
			}
			cfg := delivery.DefaultConfig("owner/release")
			cfg.Assignments[entry.RepoID] = delivery.RepositoryAssignment{
				RepositoryID:  entry.RepoID,
				AssignmentRef: delivery.SlotRef(l, "v1.2.3", strings.Repeat("a", 40), strings.Repeat("b", 64)),
				Generation:    1, UpdatedAt: time.Now().UTC(),
			}
			if err := delivery.WriteConfig(assignmentPath, cfg); err != nil {
				t.Fatal(err)
			}
			if tc.pid != 0 || tc.pgid != 0 {
				_, err := store.Update("fixture", 0, "", nil, func(snapshot *state.Snapshot) error {
					snapshot.Issues["1"] = &state.Issue{
						Number: 1, RunID: "run_1", Status: issuedomain.StatusRunning, Generation: 1, Attempts: 1,
						Worktree: "/tmp/issue-1", Branch: "codex/issue-1",
						Workspace: testWorkerWorkspace(snapshot, "/tmp/issue-1", "codex/issue-1"),
						WorkerPID: tc.pid, WorkerPGID: tc.pgid,
					}
					snapshot.ActiveExecution = &state.ActiveExecution{IssueNumber: 1, RunID: "run_1", Generation: 1, StartedAt: time.Now().UTC()}
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			if tc.corrupt {
				if err := os.WriteFile(store.StatePath(), []byte("invalid JSON"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.RemoveAll(repo); err != nil {
				t.Fatal(err)
			}
			args := []string{"--json"}
			if !tc.implicit {
				args = append(args, "--repo", entry.RepoPath)
			}
			groups := &appProcessGroups{alive: map[int]bool{123: true}, signals: map[int][]syscall.Signal{}}
			var output bytes.Buffer
			err = (App{Out: &output, Err: &output, ProcessController: groups}).unregister(context.Background(), l, args)
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) {
					t.Fatalf("error=%v, want %q", err, tc.wantError)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if len(groups.signals) != 0 {
				t.Fatalf("unexpected worker signals: %v", groups.signals)
			}
			registered, err := (registry.Store{Path: l.RegistryPath}).Load()
			if err != nil {
				t.Fatal(err)
			}
			_, retained := registered.Repos[entry.RepoID]
			wantRetained := tc.wantError != ""
			if retained != wantRetained {
				t.Fatalf("registry retained=%v, want %v", retained, wantRetained)
			}
			remaining, err := delivery.LoadConfig(assignmentPath)
			if err != nil {
				t.Fatal(err)
			}
			_, retained = remaining.Assignments[entry.RepoID]
			if retained != wantRetained {
				t.Fatalf("assignment retained=%v, want %v", retained, wantRetained)
			}
			for _, path := range []string{l.PlistPath(entry.RepoID), l.BrokerPlistPath()} {
				_, err := os.Stat(path)
				if wantRetained && err != nil || !wantRetained && !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("plist %s: %v", path, err)
				}
			}
			if _, err := os.Stat(store.StatePath()); err != nil {
				t.Fatalf("state not preserved: %v", err)
			}
		})
	}
}
