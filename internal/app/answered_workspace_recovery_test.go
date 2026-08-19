package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/ishii1648/codex-issue-loop/internal/config"
	gh "github.com/ishii1648/codex-issue-loop/internal/github"
	"github.com/ishii1648/codex-issue-loop/internal/registry"
	"github.com/ishii1648/codex-issue-loop/internal/state"
	"github.com/ishii1648/codex-issue-loop/internal/worktree"
)

func TestRecoverAnsweredWorkspacePreviewConfirmAndIdempotency(t *testing.T) {
	fixture := newAnsweredWorkspaceAppFixture(t, false)
	before, _ := fixture.store.Load()
	beforeDigest, _ := fixture.manager.ContentDigest(context.Background(), fixture.worktree)

	var out, stderr bytes.Buffer
	a := App{Out: &out, Err: &stderr, ProcessController: &appProcessGroups{alive: map[int]bool{}, signals: map[int][]syscall.Signal{}}}
	preview := []string{"recover-answered-workspace", "--repo", fixture.repo, "--issue", "449", "--dry-run", "--json"}
	if code := a.Run(context.Background(), preview); code != 0 {
		t.Fatalf("preview code=%d stderr=%s", code, stderr.String())
	}
	firstPreview := out.String()
	out.Reset()
	stderr.Reset()
	if code := a.Run(context.Background(), preview); code != 0 || out.String() != firstPreview {
		t.Fatalf("preview was not stable: code=%d first=%s second=%s stderr=%s", code, firstPreview, out.String(), stderr.String())
	}
	afterPreview, _ := fixture.store.Load()
	if afterPreview.StateRevision != before.StateRevision || afterPreview.Issues["449"].Workspace != nil {
		t.Fatalf("preview mutated state: before=%d after=%d", before.StateRevision, afterPreview.StateRevision)
	}

	out.Reset()
	stderr.Reset()
	confirm := []string{"recover-answered-workspace", "--repo", fixture.repo, "--issue", "449", "--confirm-exact-chain", "--json"}
	if code := a.Run(context.Background(), confirm); code != 0 {
		t.Fatalf("confirm code=%d stderr=%s", code, stderr.String())
	}
	after, _ := fixture.store.Load()
	item := after.Issues["449"]
	if item.Status != "resume_pending" || item.GitHubSync != "" || item.Workspace == nil ||
		item.LeaseGeneration != 3 || item.Lease.Owner.Generation != 3 || item.ResourcePark.ResumeOwner.Generation != 2 ||
		item.AnsweredWorkspaceRecovery == nil || item.AnsweredWorkspaceRecovery.Status != "github_synced" ||
		!item.AnsweredWorkspaceRecovery.OperatorConfirmed || !item.AnsweredWorkspaceRecovery.OldProvenanceMissing {
		t.Fatalf("recovered issue=%+v", item)
	}
	if item.SessionID != before.Issues["449"].SessionID || item.Attempts != before.Issues["449"].Attempts ||
		item.Continuations != before.Issues["449"].Continuations || !reflectDeepEqualAnswers(item.Answers, before.Issues["449"].Answers) {
		t.Fatalf("continuation metadata changed: before=%+v after=%+v", before.Issues["449"], item)
	}
	afterDigest, _ := fixture.manager.ContentDigest(context.Background(), fixture.worktree)
	if afterDigest != beforeDigest || item.AnsweredWorkspaceRecovery.WorktreeSHA256 != beforeDigest {
		t.Fatalf("worktree content changed: before=%s after=%s recovery=%s", beforeDigest, afterDigest, item.AnsweredWorkspaceRecovery.WorktreeSHA256)
	}
	revision := after.StateRevision
	out.Reset()
	stderr.Reset()
	if code := a.Run(context.Background(), confirm); code != 0 {
		t.Fatalf("idempotent confirm code=%d stderr=%s", code, stderr.String())
	}
	idempotent, _ := fixture.store.Load()
	if idempotent.StateRevision != revision || idempotent.Issues["449"].LeaseGeneration != 3 {
		t.Fatalf("idempotent invocation duplicated the transaction: revision %d -> %d generation=%d", revision, idempotent.StateRevision, idempotent.Issues["449"].LeaseGeneration)
	}
}

func TestRecoverAnsweredWorkspaceAfterVerifiedGenericProvenanceRecovery(t *testing.T) {
	fixture := newAnsweredWorkspaceAppFixture(t, false)
	persistVerifiedAnsweredWorkspace(t, fixture)
	before, _ := fixture.store.Load()
	originalWorkspace := *before.Issues["449"].Workspace
	originalRecoveryID := before.Issues["449"].WorkspaceRecovery.ID

	var out, stderr bytes.Buffer
	a := App{Out: &out, Err: &stderr, ProcessController: &appProcessGroups{alive: map[int]bool{}, signals: map[int][]syscall.Signal{}}}
	preview := []string{"recover-answered-workspace", "--repo", fixture.repo, "--issue", "449", "--dry-run", "--json"}
	if code := a.Run(context.Background(), preview); code != 0 || !strings.Contains(out.String(), `"verified_provenance_recovery": true`) {
		t.Fatalf("verified preview code=%d out=%s stderr=%s", code, out.String(), stderr.String())
	}
	out.Reset()
	stderr.Reset()
	confirm := []string{"recover-answered-workspace", "--repo", fixture.repo, "--issue", "449", "--confirm-exact-chain", "--json"}
	if code := a.Run(context.Background(), confirm); code != 0 {
		t.Fatalf("verified confirm code=%d stderr=%s", code, stderr.String())
	}
	after, _ := fixture.store.Load()
	item := after.Issues["449"]
	if item.Status != "resume_pending" || item.LeaseGeneration != 3 || item.AnsweredWorkspaceRecovery == nil ||
		item.AnsweredWorkspaceRecovery.Status != "github_synced" || item.WorkspaceRecovery == nil ||
		item.WorkspaceRecovery.ID != originalRecoveryID || *item.Workspace != originalWorkspace {
		t.Fatalf("verified provenance was not retained across lifecycle recovery: %+v", item)
	}
	if item.SessionID != before.Issues["449"].SessionID || item.Attempts != before.Issues["449"].Attempts ||
		item.Continuations != before.Issues["449"].Continuations || !reflectDeepEqualAnswers(item.Answers, before.Issues["449"].Answers) {
		t.Fatalf("verified continuation metadata changed: before=%+v after=%+v", before.Issues["449"], item)
	}
}

func TestRecoverAnsweredWorkspaceRejectsChangedVerifiedContent(t *testing.T) {
	fixture := newAnsweredWorkspaceAppFixture(t, false)
	persistVerifiedAnsweredWorkspace(t, fixture)
	if err := os.WriteFile(filepath.Join(fixture.worktree, "untracked-after-verification.txt"), []byte("changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	before, _ := fixture.store.Load()
	var out, stderr bytes.Buffer
	a := App{Out: &out, Err: &stderr, ProcessController: &appProcessGroups{alive: map[int]bool{}, signals: map[int][]syscall.Signal{}}}
	if code := a.Run(context.Background(), []string{"recover-answered-workspace", "--repo", fixture.repo, "--issue", "449", "--confirm-exact-chain", "--json"}); code != 4 {
		t.Fatalf("changed content code=%d stderr=%s", code, stderr.String())
	}
	after, _ := fixture.store.Load()
	if after.StateRevision != before.StateRevision || after.Issues["449"].LeaseGeneration != 2 || after.Issues["449"].AnsweredWorkspaceRecovery != nil {
		t.Fatal("changed verified content mutated lifecycle state")
	}
}

func TestRecoverAnsweredWorkspaceRejectsChangedVerifiedHead(t *testing.T) {
	fixture := newAnsweredWorkspaceAppFixture(t, false)
	persistVerifiedAnsweredWorkspace(t, fixture)
	tree := runGitOutputApp(t, fixture.worktree, "rev-parse", "HEAD^{tree}")
	newHead := runGitOutputApp(t, fixture.worktree, "commit-tree", tree, "-m", "changed verified head")
	runGitApp(t, fixture.worktree, "update-ref", "HEAD", newHead)
	before, _ := fixture.store.Load()
	var out, stderr bytes.Buffer
	a := App{Out: &out, Err: &stderr, ProcessController: &appProcessGroups{alive: map[int]bool{}, signals: map[int][]syscall.Signal{}}}
	if code := a.Run(context.Background(), []string{"recover-answered-workspace", "--repo", fixture.repo, "--issue", "449", "--confirm-exact-chain", "--json"}); code != 4 {
		t.Fatalf("changed HEAD code=%d stderr=%s", code, stderr.String())
	}
	after, _ := fixture.store.Load()
	if after.StateRevision != before.StateRevision || after.Issues["449"].LeaseGeneration != 2 {
		t.Fatal("changed verified HEAD mutated lifecycle state")
	}
}

func TestValidateAnsweredWorkspaceRemoteRequiresExactMarkers(t *testing.T) {
	cfg := config.Config{}
	cfg.GitHub.RunningLabel = "running"
	cfg.GitHub.NeedsInputLabel = "needs-input"
	cfg.GitHub.DoneLabel = "done"
	cfg.GitHub.FailedLabel = "failed"
	cfg.GitHub.ReadyLabels = []string{"ready"}
	cfg.GitHub.ExcludeLabels = []string{"blocked", "do-not-automate"}
	reason := "worker workspace validation failed for /worktree: saved workspace provenance is missing"
	digest := sha256.Sum256([]byte(reason))
	issue := &state.Issue{Number: 449, Status: "blocked", LastError: reason}
	request := &state.Request{ID: "req_exact"}
	remote := gh.RemoteState{Issue: gh.Issue{
		Number: 449, State: "OPEN", Labels: []string{"blocked"},
		Comments: []string{
			"<!-- codex-issue-loop:request:req_exact -->",
			fmt.Sprintf("<!-- codex-issue-loop:failed:449 -->\n<!-- codex-issue-loop:failure:%x -->", digest[:8]),
		},
	}}
	if err := validateAnsweredWorkspaceRemote(cfg, issue, request, remote, ""); err != nil {
		t.Fatalf("exact marker boundary rejected: %v", err)
	}
	missing := remote
	missing.Issue.Comments = append([]string(nil), remote.Issue.Comments[:1]...)
	if err := validateAnsweredWorkspaceRemote(cfg, issue, request, missing, ""); err == nil {
		t.Fatal("missing blocked/failure markers accepted")
	}
	duplicate := remote
	duplicate.Issue.Comments = append(append([]string(nil), remote.Issue.Comments...), remote.Issue.Comments[0])
	if err := validateAnsweredWorkspaceRemote(cfg, issue, request, duplicate, ""); err == nil {
		t.Fatal("duplicate request marker accepted")
	}
	manual := remote
	manual.Issue.Labels = []string{"blocked", "do-not-automate"}
	if err := validateAnsweredWorkspaceRemote(cfg, issue, request, manual, ""); err == nil {
		t.Fatal("manual exclusion marker boundary accepted")
	}
}

func TestFaultRecoverAnsweredWorkspaceRetriesGitHubBoundaryWithoutRefencing(t *testing.T) {
	fixture := newAnsweredWorkspaceAppFixture(t, true)
	args := []string{"recover-answered-workspace", "--repo", fixture.repo, "--issue", "449", "--confirm-exact-chain", "--json"}
	var out, stderr bytes.Buffer
	a := App{Out: &out, Err: &stderr, ProcessController: &appProcessGroups{alive: map[int]bool{}, signals: map[int][]syscall.Signal{}}}
	if code := a.Run(context.Background(), args); code == 0 {
		t.Fatal("injected GitHub synchronization failure was ignored")
	}
	pending, _ := fixture.store.Load()
	if pending.Issues["449"].LeaseGeneration != 3 || pending.Issues["449"].GitHubSync != "answered_workspace_recovery" ||
		pending.Issues["449"].AnsweredWorkspaceRecovery.Status != "requested" {
		t.Fatalf("fault boundary was not durable: %+v", pending.Issues["449"])
	}
	out.Reset()
	stderr.Reset()
	if code := a.Run(context.Background(), args); code != 0 {
		t.Fatalf("retry code=%d stderr=%s", code, stderr.String())
	}
	resumed, _ := fixture.store.Load()
	if resumed.Issues["449"].LeaseGeneration != 3 || resumed.Issues["449"].GitHubSync != "" ||
		resumed.Issues["449"].AnsweredWorkspaceRecovery.Status != "github_synced" {
		t.Fatalf("fault retry duplicated or failed recovery: %+v", resumed.Issues["449"])
	}
}

func TestRecoverAnsweredWorkspaceParallelInvocationsFenceOnce(t *testing.T) {
	fixture := newAnsweredWorkspaceAppFixture(t, false)
	args := []string{"recover-answered-workspace", "--repo", fixture.repo, "--issue", "449", "--confirm-exact-chain", "--json"}
	codes := make([]int, 2)
	errorsText := make([]string, 2)
	start := make(chan struct{})
	var wait sync.WaitGroup
	for index := range codes {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			<-start
			var out, stderr bytes.Buffer
			a := App{Out: &out, Err: &stderr, ProcessController: &appProcessGroups{alive: map[int]bool{}, signals: map[int][]syscall.Signal{}}}
			codes[index] = a.Run(context.Background(), args)
			errorsText[index] = stderr.String()
		}(index)
	}
	close(start)
	wait.Wait()
	for index, code := range codes {
		if code != 0 {
			t.Fatalf("parallel invocation %d code=%d stderr=%s", index, code, errorsText[index])
		}
	}
	snapshot, _ := fixture.store.Load()
	item := snapshot.Issues["449"]
	if item.LeaseGeneration != 3 || item.AnsweredWorkspaceRecovery == nil || item.AnsweredWorkspaceRecovery.Status != "github_synced" {
		t.Fatalf("parallel recovery duplicated or lost its fence: %+v", item)
	}
}

func TestRecoverAnsweredWorkspaceRejectsWithoutConfirmationOrOnActiveWorker(t *testing.T) {
	fixture := newAnsweredWorkspaceAppFixture(t, false)
	before, _ := fixture.store.Load()
	var out, stderr bytes.Buffer
	a := App{Out: &out, Err: &stderr}
	if code := a.Run(context.Background(), []string{"recover-answered-workspace", "--repo", fixture.repo, "--issue", "449", "--json"}); code != 2 {
		t.Fatalf("missing confirmation code=%d stderr=%s", code, stderr.String())
	}
	after, _ := fixture.store.Load()
	if after.StateRevision != before.StateRevision {
		t.Fatal("missing confirmation changed state")
	}

	out.Reset()
	stderr.Reset()
	if _, err := fixture.store.Update("fixture_active_worker", 449, "run_0c0123ac8570c0a8", nil, func(snapshot *state.Snapshot) error {
		snapshot.Issues["449"].WorkerPID = 44901
		snapshot.Issues["449"].WorkerPGID = 44901
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	activeBefore, _ := fixture.store.Load()
	if code := a.Run(context.Background(), []string{"recover-answered-workspace", "--repo", fixture.repo, "--issue", "449", "--confirm-exact-chain", "--json"}); code != 4 {
		t.Fatalf("active worker code=%d stderr=%s", code, stderr.String())
	}
	afterActive, _ := fixture.store.Load()
	if afterActive.StateRevision != activeBefore.StateRevision || afterActive.Issues["449"].Workspace != nil {
		t.Fatal("active worker rejection changed state")
	}
}

func TestRecoverAnsweredWorkspaceRejectsAnotherPendingRequestWithoutMutation(t *testing.T) {
	fixture := newAnsweredWorkspaceAppFixture(t, false)
	if _, err := fixture.store.Update("fixture_pending_request", 449, "run_0c0123ac8570c0a8", nil, func(snapshot *state.Snapshot) error {
		snapshot.PendingRequests["req_other"] = &state.Request{
			ID: "req_other", IssueNumber: 449, RunID: "run_0c0123ac8570c0a8", Status: "pending", Question: "Other?",
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before, _ := fixture.store.Load()
	var out, stderr bytes.Buffer
	a := App{Out: &out, Err: &stderr, ProcessController: &appProcessGroups{alive: map[int]bool{}, signals: map[int][]syscall.Signal{}}}
	if code := a.Run(context.Background(), []string{"recover-answered-workspace", "--repo", fixture.repo, "--issue", "449", "--confirm-exact-chain", "--json"}); code != 4 {
		t.Fatalf("pending request code=%d stderr=%s", code, stderr.String())
	}
	after, _ := fixture.store.Load()
	if after.StateRevision != before.StateRevision || after.Issues["449"].Workspace != nil || after.Issues["449"].LeaseGeneration != 2 {
		t.Fatal("pending request rejection changed recovery state")
	}
}

type answeredWorkspaceAppFixture struct {
	repo, worktree string
	store          state.Store
	manager        worktree.Manager
}

func newAnsweredWorkspaceAppFixture(t *testing.T, failGitHubOnce bool) answeredWorkspaceAppFixture {
	t.Helper()
	repo, l := testEnvironment(t)
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	runGitApp(t, repo, "config", "user.name", "Test User")
	runGitApp(t, repo, "config", "user.email", "test@example.com")
	runGitApp(t, repo, "config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitApp(t, repo, "add", "tracked.txt")
	runGitApp(t, repo, "commit", "-m", "initial")
	runGitApp(t, repo, "branch", "-M", "main")
	baseSHA := runGitOutputApp(t, repo, "rev-parse", "HEAD")
	root := filepath.Dir(repo)
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	managedRoot := filepath.Join(root, "managed-worktrees")
	managedWorktree := filepath.Join(managedRoot, "repo", "issue-449")
	if err := os.MkdirAll(filepath.Dir(managedWorktree), 0o700); err != nil {
		t.Fatal(err)
	}
	remotePath := filepath.Join(root, "remote.git")
	runGitApp(t, root, "init", "--bare", remotePath)
	runGitApp(t, repo, "remote", "add", "origin", remotePath)
	runGitApp(t, repo, "push", "-u", "origin", "main")
	branch := "codex/issue-449-extent-121"
	runGitApp(t, repo, "worktree", "add", "-b", branch, managedWorktree, baseSHA)
	runGitApp(t, managedWorktree, "push", "-u", "origin", branch)
	if err := os.WriteFile(filepath.Join(managedWorktree, "tracked.txt"), []byte("dirty\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(managedWorktree, "staged.txt"), []byte("staged\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitApp(t, managedWorktree, "add", "staged.txt")
	if err := os.WriteFile(filepath.Join(managedWorktree, "untracked.txt"), []byte("untracked\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	configBody := fmt.Sprintf(`version: 4
github:
  repo: owner/repo
  exclude_labels: [blocked, do-not-automate]
  running_label: codex-loop:running
  needs_input_label: codex-loop:needs-input
  failed_label: codex-loop:failed
  done_label: codex-loop:done
watch:
  reconcile_interval: 20ms
git:
  worktree_root: %q
  base_branch: main
`, managedRoot)
	if err := os.WriteFile(filepath.Join(repo, config.FileName), []byte(configBody), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := mustConfig(t, repo)
	reason := fmt.Sprintf("worker workspace validation failed for %s: saved workspace provenance is missing", managedWorktree)
	failureDigest := sha256.Sum256([]byte(reason))
	requestMarker := "<!-- codex-issue-loop:request:req_6058cb295f5cb9ff -->"
	failedMarker := "<!-- codex-issue-loop:failed:449 -->"
	failureMarker := fmt.Sprintf("<!-- codex-issue-loop:failure:%x -->", failureDigest[:8])
	fakeGH := filepath.Join(root, "bin", "gh-answered-workspace")
	failPath := filepath.Join(root, "fail-gh-once")
	failClause := ""
	if failGitHubOnce {
		failClause = `if [ ! -e "$AGENT_LOOP_TEST_GH_FAIL_ONCE" ]; then : > "$AGENT_LOOP_TEST_GH_FAIL_ONCE"; exit 1; fi; `
	}
	script := fmt.Sprintf(`#!/bin/sh
case "$1 $2" in
  "issue view") printf '%%s\n' '{"number":449,"title":"Sanitized 449","body":"","url":"https://example.test/issues/449","state":"OPEN","labels":[{"name":"blocked"}],"assignees":[],"milestone":null,"comments":[{"body":%s},{"body":%s}]}' ;;
  "pr list") printf '%%s\n' '[]' ;;
  "issue edit") %sexit 0 ;;
  "issue comment") exit 0 ;;
  *) exit 2 ;;
esac
`, strconv.Quote(requestMarker), strconv.Quote(failedMarker+"\n"+failureMarker), failClause)
	if err := os.WriteFile(fakeGH, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENT_LOOP_TEST_GH_FAIL_ONCE", failPath)
	entry := registry.Entry{
		RepoID: registry.RepoID(cfg.GitHub.Repo, cfg.RepoPath), RepoPath: cfg.RepoPath, GitHubRepo: cfg.GitHub.Repo,
		Commands: map[string]string{"git": "/usr/bin/git", "gh": fakeGH},
	}
	writeJSONFixture(t, l.RegistryPath, registry.Registry{Version: registry.CurrentVersion, Repos: map[string]registry.Entry{entry.RepoID: entry}})
	store := state.Store{Dir: l.RepoDir(entry.RepoID), RepoID: entry.RepoID, RepoPath: cfg.RepoPath}
	if err := store.Initialize(); err != nil {
		t.Fatal(err)
	}
	persistAnsweredWorkspaceChain(t, store, managedWorktree, branch, baseSHA, reason)
	return answeredWorkspaceAppFixture{repo: repo, worktree: managedWorktree, store: store, manager: worktree.Manager{StateRoot: l.Root, GitPath: "/usr/bin/git"}}
}

func persistAnsweredWorkspaceChain(t *testing.T, store state.Store, worktreePath, branch, baseSHA, reason string) {
	t.Helper()
	runID := "run_0c0123ac8570c0a8"
	times := make([]time.Time, 10)
	for index := range times {
		times[index] = time.Date(2026, 8, 19, 1, 0, index, 0, time.UTC)
	}
	_, owner, err := store.ReserveLease(state.LeaseReservation{
		IssueNumber: 449, Title: "Sanitized 449", RunID: runID, Slot: 0,
		DeclaredResources: []string{state.RepositoryResource}, ResolvedResources: []string{state.RepositoryResource}, BaseSHA: baseSHA, ReservedAt: times[0],
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update("issue_claimed", 449, runID, map[string]string{"title": "Sanitized 449"}, func(s *state.Snapshot) error {
		s.Issues["449"].Status = "claimed"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	_, err = store.Update("worker_started", 449, runID, map[string]string{"worktree": worktreePath, "branch": branch}, func(s *state.Snapshot) error {
		item := s.Issues["449"]
		item.Status, item.Worktree, item.Branch = "running", worktreePath, branch
		item.Attempts = 1
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update("worker_process_started", 449, runID, map[string]any{"pid": 44901, "pgid": 44901}, func(*state.Snapshot) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update("worker_preflight_completed", 449, runID, map[string]string{"execution_profile": "extended"}, func(s *state.Snapshot) error {
		item := s.Issues["449"]
		item.ExecutionProfile = "extended"
		item.SessionID = "01a012e1-b854-7e21-88b4-3cf814c7c5db"
		item.Session = &state.WorkerSession{Backend: "codex", ID: item.SessionID}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	requestID, parkID := "req_6058cb295f5cb9ff", "park_f5edeb293b3960b8"
	question := map[string]any{"text": "Which extent should be used?", "reason": "product behavior", "recommended_option": "121", "options": []state.Option{{ID: "121", Label: "Extent 121"}}, "allow_free_text": true}
	if _, err := store.Update("input_requested", 449, runID, map[string]any{
		"question": question, "request_id": requestID, "resource_park_id": parkID, "released_owner": owner, "parked_at": times[4],
	}, func(s *state.Snapshot) error {
		item := s.Issues["449"]
		if err := state.ParkIssueLease(item, owner, parkID, times[4]); err != nil {
			return err
		}
		item.ResourcePark.Kind, item.ResourcePark.RequestID = state.ResourceParkKindNeedsInput, requestID
		item.Status, item.GitHubSync = "needs_input", "needs_input"
		s.PendingRequests[requestID] = &state.Request{
			ID: requestID, IssueNumber: 449, Question: "Which extent should be used?", Reason: "product behavior", Recommended: "121",
			Options: []state.Option{{ID: "121", Label: "Extent 121"}}, AllowFreeText: true, RunID: runID,
			ResourceParkID: parkID, ReleasedOwner: &owner, Status: "pending", CreatedAt: times[4],
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update("github_state_synced", 449, runID, map[string]string{"state": "needs_input"}, func(s *state.Snapshot) error {
		s.Issues["449"].GitHubSync = ""
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	answerTime := times[6]
	payload := map[string]any{"request_id": requestID}
	if _, err := store.Update("answer_recorded", 449, runID, payload, func(s *state.Snapshot) error {
		item, request := s.Issues["449"], s.PendingRequests[requestID]
		request.Status, request.Answer, request.AnsweredAt = "answered", "121", &answerTime
		resumeOwner, err := state.ResumeParkedLease(s, 449, parkID, 0, answerTime)
		if err != nil {
			return err
		}
		payload["lease_owner"], payload["lease_slot"] = resumeOwner, 0
		item.Status = "resume_pending"
		item.Answers = []state.AnswerRecord{{RequestID: requestID, Question: request.Question, Answer: request.Answer, AnsweredAt: answerTime}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update("worker_started", 449, runID, map[string]string{"mode": "user_answer_resume"}, func(s *state.Snapshot) error {
		item := s.Issues["449"]
		item.Status, item.ResourcePark.Status = "running", "resumed"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	commonDir := runGitOutputApp(t, worktreePath, "rev-parse", "--path-format=absolute", "--git-common-dir")
	mainCheckout := runGitOutputApp(t, worktreePath, "worktree", "list", "--porcelain")
	mainCheckout = strings.TrimPrefix(strings.Split(mainCheckout, "\n")[0], "worktree ")
	checks := map[string]bool{
		"run_id": true, "session_id": true, "saved_path": true, "saved_branch_state": true, "lease_owner_generation": true,
		"managed_root": true, "no_symlink_components": true, "canonical_path": true, "not_main_checkout": true,
		"git_top_level": true, "repository_identity": true, "saved_branch": true,
	}
	validation := worktree.LaunchValidation{Valid: true, ExpectedCWD: worktreePath, CanonicalCWD: worktreePath, TopLevel: worktreePath, Branch: branch, CommonDir: commonDir, MainCheckout: mainCheckout, Checks: checks}
	if _, err := store.Update("worker_workspace_rejected", 449, runID, map[string]any{"expected_cwd": worktreePath, "error": reason, "run_id": runID, "validation": validation}, func(s *state.Snapshot) error {
		item := s.Issues["449"]
		item.Status, item.FailureKind, item.LastError, item.GitHubSync = "blocked", "issue", reason, "blocked"
		item.BlockedCause = &state.BlockedCause{Origin: "supervisor", Kind: "worker_workspace", Resumable: false, Reason: reason, BlockedAt: times[8]}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update("github_state_synced", 449, runID, map[string]string{"state": "blocked"}, func(s *state.Snapshot) error {
		s.Issues["449"].GitHubSync = ""
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func persistVerifiedAnsweredWorkspace(t *testing.T, fixture answeredWorkspaceAppFixture) {
	t.Helper()
	ctx := context.Background()
	cfg := mustConfig(t, fixture.repo)
	snapshot, err := fixture.store.Load()
	if err != nil {
		t.Fatal(err)
	}
	item := snapshot.Issues["449"]
	validation, err := fixture.manager.ValidateLaunch(ctx, cfg, item.Worktree, item.Branch)
	if err != nil || !validation.Valid {
		t.Fatalf("validate fixture workspace: validation=%+v err=%v", validation, err)
	}
	inspection, err := fixture.manager.Inspect(ctx, cfg, item.Worktree, item.Branch)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := fixture.manager.ContentDigest(ctx, item.Worktree)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	workspace := state.WorkerWorkspace{
		Path: validation.CanonicalCWD, Branch: validation.Branch, RepoID: fixture.store.RepoID,
		Repository: cfg.GitHub.Repo, RepositoryID: cfg.GitHub.RepositoryID,
		GitCommonDir: validation.CommonDir, MainCheckout: validation.MainCheckout, CapturedAt: now,
	}
	recoveryID := "workspace_recovery_verified449"
	payload := map[string]any{
		"recovery_id": recoveryID, "operator_confirmation": map[string]bool{"confirm_verified_workspace": true},
		"mutation_scope":  []string{"issues[].workspace", "issues[].workspace_provenance_recovery", "events.jsonl"},
		"previous_status": item.Status, "old_provenance_missing": true, "head_sha": inspection.Head,
		"worktree_sha256": digest, "pull_request_url": item.PullRequestURL,
		"expected_workspace": workspace, "actual_workspace": workspace, "validator": validation,
	}
	if _, err := fixture.store.Update("workspace_provenance_recovered", item.Number, item.RunID, payload, func(s *state.Snapshot) error {
		current := s.Issues["449"]
		current.Workspace = &workspace
		current.WorkspaceRecovery = &state.WorkspaceProvenanceRecovery{
			ID: recoveryID, Status: "verified", ConfirmedAt: now, OperatorConfirmed: true, OldProvenanceMissing: true,
			PreviousStatus: current.Status, RunID: current.RunID, HeadSHA: inspection.Head, WorktreeSHA256: digest,
			ExpectedWorkspace: workspace, ActualWorkspace: workspace, ValidatorChecks: cloneBoolMap(validation.Checks),
		}
		current.UpdatedAt = now
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func reflectDeepEqualAnswers(left, right []state.AnswerRecord) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
