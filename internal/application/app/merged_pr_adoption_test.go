package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ishii1648/codex-issue-loop/internal/adapter/github"
	"github.com/ishii1648/codex-issue-loop/internal/adapter/state"
	"github.com/ishii1648/codex-issue-loop/internal/platform/registry"
)

func TestAdoptMergedPullRequestReleasesLeaseOnceAndIsIdempotent(t *testing.T) {
	repo, l := testEnvironment(t)
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	runGit := func(path string, args ...string) string {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", path}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	runGit(repo, "config", "user.name", "Test")
	runGit(repo, "config", "user.email", "test@example.invalid")
	runGit(repo, "config", "commit.gpgsign", "false")
	runGit(repo, "add", ".agent-loop.yaml")
	runGit(repo, "commit", "-m", "base")
	runGit(repo, "branch", "-M", "main")
	baseSHA := runGit(repo, "rev-parse", "HEAD")
	remoteDir := filepath.Join(t.TempDir(), "remote.git")
	if out, err := exec.Command("git", "init", "--bare", "-q", remoteDir).CombinedOutput(); err != nil {
		t.Fatalf("git init --bare: %v: %s", err, out)
	}
	runGit(repo, "remote", "add", "origin", remoteDir)
	runGit(repo, "push", "-u", "origin", "main")

	branch := "codex/issue-129-adopt"
	canonicalRoot, err := filepath.EvalSymlinks(l.Root)
	if err != nil {
		t.Fatal(err)
	}
	managedWorktree := filepath.Join(canonicalRoot, "worktrees", "repo-test", "issue-129")
	if err := os.MkdirAll(filepath.Dir(managedWorktree), 0o700); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", repo, "worktree", "add", "-b", branch, managedWorktree).CombinedOutput(); err != nil {
		t.Fatalf("git worktree add: %v: %s", err, out)
	}
	if err := os.WriteFile(filepath.Join(managedWorktree, "fix.txt"), []byte("fixed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(managedWorktree, "add", "fix.txt")
	runGit(managedWorktree, "commit", "-m", "fix")
	runGit(managedWorktree, "push", "-u", "origin", branch)
	headSHA := runGit(managedWorktree, "rev-parse", "HEAD")
	runGit(repo, "merge", "--no-ff", "--no-edit", branch)
	runGit(repo, "push", "origin", "main")
	mergeSHA := runGit(repo, "rev-parse", "HEAD")

	binDir := filepath.Dir(strings.Split(os.Getenv("PATH"), string(os.PathListSeparator))[0] + "/unused")
	fakeGH := filepath.Join(binDir, "gh")
	ghLog := filepath.Join(t.TempDir(), "gh.log")
	issueJSON := `{"number":129,"title":"bootstrap","body":"","url":"https://example.test/issues/129","state":"CLOSED","labels":[{"name":"blocked"}],"assignees":[],"milestone":null,"comments":[{"body":"<!-- codex-issue-loop:failed:129 -->\nAutomation stopped."}]}`
	prJSON := fmt.Sprintf(`[{"number":132,"url":"https://example.test/pull/132","state":"MERGED","isDraft":false,"mergedAt":"2026-08-17T22:25:30Z","headRefName":%q,"baseRefName":"main","headRefOid":%q,"mergeCommit":{"oid":%q},"headRepository":{"name":"repo"},"headRepositoryOwner":{"login":"owner"},"mergeStateStatus":"UNKNOWN","statusCheckRollup":[]}]`, branch, headSHA, mergeSHA)
	writeFakeGH := func(issue string) {
		t.Helper()
		script := fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$*" >> %q
case "$1 $2" in
  "issue view") printf '%%s\n' '%s' ;;
  "pr list") printf '%%s\n' '%s' ;;
  "issue edit"|"issue comment"|"issue close") exit 0 ;;
  *) exit 2 ;;
esac
		`, ghLog, issue, prJSON)
		if err := os.WriteFile(fakeGH, []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeFakeGH(issueJSON)
	cfg := mustConfig(t, repo)
	entry, err := (registry.Store{Path: l.RegistryPath}).Add(cfg)
	if err != nil {
		t.Fatal(err)
	}
	store := state.Store{Dir: l.RepoDir(entry.RepoID), RepoID: entry.RepoID, RepoPath: repo}
	if err := store.Initialize(); err != nil {
		t.Fatal(err)
	}
	runID := "run_adopt_129"
	_, _, err = store.ReserveLease(state.LeaseReservation{
		IssueNumber: 129, Title: "bootstrap", RunID: runID, Slot: 0,
		DeclaredResources: []string{state.RepositoryResource}, ResolvedResources: []string{state.RepositoryResource},
		BaseSHA: baseSHA, ReservedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Update("worker_environment_blocked", 129, runID, nil, func(snapshot *state.Snapshot) error {
		item := snapshot.Issues["129"]
		item.Status = issuedomain.StatusBlocked
		item.Worktree = managedWorktree
		item.Branch = branch
		item.Attempts = 3
		item.Continuations = 2
		item.SessionID = "session-129"
		item.Session = &state.WorkerSession{Backend: "codex", ID: "session-129"}
		item.FailureKind = "issue"
		item.LastError = "browser unavailable"
		item.BlockedCause = &state.BlockedCause{Origin: "worker", Kind: "environment", Resumable: true, Reason: item.LastError, BlockedAt: time.Now().UTC()}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	args := []string{"adopt-merged-pr", "--repo", repo, "--issue", "129", "--confirm-merged-pr-adoption", "--json"}
	var firstOutput map[string]any
	for attempt := 0; attempt < 2; attempt++ {
		var out, stderr bytes.Buffer
		a := App{Out: &out, Err: &stderr, ProcessController: &appProcessGroups{alive: map[int]bool{}, signals: map[int][]syscall.Signal{}}}
		code := a.Run(context.Background(), args)
		if code != 0 {
			t.Fatalf("attempt=%d code=%d stdout=%s stderr=%s", attempt, code, out.String(), stderr.String())
		}
		var output map[string]any
		if err := json.Unmarshal(out.Bytes(), &output); err != nil {
			t.Fatal(err)
		}
		if attempt == 0 {
			firstOutput = output
		} else if output["adoption_id"] != firstOutput["adoption_id"] || output["idempotent"] != true {
			t.Fatalf("second adoption was not idempotent: first=%v second=%v", firstOutput, output)
		}
	}
	_, err = store.Update("old_supervisor_snapshot_write", 0, "", nil, func(snapshot *state.Snapshot) error {
		snapshot.Issues["129"].MergedPullRequestAdoption = nil
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	doneIssueJSON := `{"number":129,"title":"bootstrap","body":"","url":"https://example.test/issues/129","state":"CLOSED","labels":[{"name":"codex-loop:done"}],"assignees":[],"milestone":null,"comments":[{"body":"<!-- codex-issue-loop:done -->\nCompleted by codex-issue-loop."}]}`
	writeFakeGH(doneIssueJSON)
	var recoveredOut, recoveredErr bytes.Buffer
	recoveryApp := App{Out: &recoveredOut, Err: &recoveredErr, ProcessController: &appProcessGroups{alive: map[int]bool{}, signals: map[int][]syscall.Signal{}}}
	if code := recoveryApp.Run(context.Background(), args); code != 0 {
		t.Fatalf("event recovery code=%d stdout=%s stderr=%s", code, recoveredOut.String(), recoveredErr.String())
	}
	var recoveredOutput map[string]any
	if err := json.Unmarshal(recoveredOut.Bytes(), &recoveredOutput); err != nil {
		t.Fatal(err)
	}
	if recoveredOutput["adoption_id"] != firstOutput["adoption_id"] || recoveredOutput["idempotent"] != true {
		t.Fatalf("event recovery created a different adoption: first=%v recovered=%v", firstOutput, recoveredOutput)
	}
	snapshot, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	item := snapshot.Issues["129"]
	if item.Status != issuedomain.StatusCompleted || item.Lease != nil || !item.PullRequestMerged || item.PullRequestURL != "https://example.test/pull/132" ||
		item.PullRequestNumber != 132 || item.HeadSHA != headSHA || item.GitHubSync != issuedomain.GitHubSyncNone || item.MergedPullRequestAdoption == nil ||
		item.MergedPullRequestAdoption.Status != issuedomain.MergedPullRequestAdoptionStatusSynced || item.MergedPullRequestAdoption.MergeSHA != mergeSHA ||
		item.Attempts != 3 || item.Continuations != 2 || item.SessionID != "session-129" || item.Session == nil {
		t.Fatalf("adopted state is inconsistent: %+v", item)
	}
	events, err := os.ReadFile(store.EventsPath())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(events), `"type":"merged_pull_request_adopted"`) != 1 {
		t.Fatalf("lease-releasing adoption was not recorded exactly once:\n%s", events)
	}
	if strings.Count(string(events), `"type":"merged_pull_request_adoption_recovered"`) != 1 {
		t.Fatalf("stripped metadata was not recovered exactly once:\n%s", events)
	}
}

func TestValidateMergedPullRequestAdoptionFailsClosed(t *testing.T) {
	repo, _ := testEnvironment(t)
	cfg := mustConfig(t, repo)
	current := &state.Issue{Number: 129, Status: issuedomain.StatusBlocked, Branch: "codex/issue-129-adopt"}
	mergedAt := time.Now().UTC()
	baseline := github.RemoteState{
		Issue: github.Issue{Number: 129, State: "CLOSED", Labels: []string{"blocked"}, Comments: []string{"<!-- codex-issue-loop:failed:129 -->"}},
		PullRequests: []github.PullRequest{{Number: 132, URL: "https://example.test/pull/132", State: "MERGED", MergedAt: &mergedAt,
			HeadRefName: current.Branch, BaseRefName: cfg.Git.BaseBranch, HeadSHA: "head", MergeCommitSHA: "merge", HeadRepository: cfg.GitHub.Repo}},
	}
	expected := github.MergedPullRequestAdoptionExpectation{
		IssueNumber: current.Number, PreviousStatus: current.Status, Branch: current.Branch,
		BaseBranch: cfg.Git.BaseBranch, HeadSHA: "head",
	}
	if _, err := github.ValidateMergedPullRequestAdoption(cfg, baseline, expected); err != nil {
		t.Fatalf("safe adoption was rejected: %v", err)
	}
	unsafe := baseline
	unsafe.Issue.Labels = []string{"blocked", "do-not-automate"}
	if _, err := github.ValidateMergedPullRequestAdoption(cfg, unsafe, expected); err == nil {
		t.Fatal("manual exclusion was accepted")
	}
	unsafe = baseline
	unsafe.PullRequests = append([]github.PullRequest(nil), baseline.PullRequests...)
	unsafe.PullRequests[0].MergeCommitSHA = ""
	if _, err := github.ValidateMergedPullRequestAdoption(cfg, unsafe, expected); err == nil {
		t.Fatal("missing authoritative merge SHA was accepted")
	}
}
