package app

import (
	"bytes"
	"context"
	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
	"reflect"
	"strings"
	"syscall"
	"testing"

	gh "github.com/ishii1648/codex-issue-loop/internal/adapter/github"
	"github.com/ishii1648/codex-issue-loop/internal/adapter/state"
	"github.com/ishii1648/codex-issue-loop/internal/adapter/worktree"
	"github.com/ishii1648/codex-issue-loop/internal/platform/config"
)

func TestRecoverWorkspacePreviewConfirmAndIdempotency(t *testing.T) {
	fixture := newAnsweredWorkspaceAppFixture(t, false)
	disqualifyAnsweredLifecycleFixture(t, fixture)
	before, err := fixture.store.Load()
	if err != nil {
		t.Fatal(err)
	}
	beforeDigest, err := fixture.manager.ContentDigest(context.Background(), fixture.worktree)
	if err != nil {
		t.Fatal(err)
	}

	var out, stderr bytes.Buffer
	a := App{Out: &out, Err: &stderr, ProcessController: &appProcessGroups{alive: map[int]bool{}, signals: map[int][]syscall.Signal{}}}
	preview := []string{"recover-workspace", "--repo", fixture.repo, "--issue", "449", "--dry-run", "--json"}
	if code := a.Run(context.Background(), preview); code != 0 {
		t.Fatalf("preview code=%d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(out.String(), `"eligible": true`) || !strings.Contains(out.String(), `"confirmation_required": true`) {
		t.Fatalf("preview omitted recovery decision: %s", out.String())
	}
	afterPreview, _ := fixture.store.Load()
	if afterPreview.StateRevision != before.StateRevision || afterPreview.Issues["449"].Workspace != nil {
		t.Fatalf("preview mutated state: before=%d after=%d", before.StateRevision, afterPreview.StateRevision)
	}

	out.Reset()
	stderr.Reset()
	confirm := []string{"recover-workspace", "--repo", fixture.repo, "--issue", "449", "--confirm-verified-workspace", "--json"}
	if code := a.Run(context.Background(), confirm); code != 0 {
		t.Fatalf("confirm code=%d stderr=%s", code, stderr.String())
	}
	after, _ := fixture.store.Load()
	item := after.Issues["449"]
	old := before.Issues["449"]
	if item.Workspace == nil || item.WorkspaceRecovery == nil || item.WorkspaceRecovery.Status != issuedomain.WorkspaceProvenanceRecoveryStatusVerified ||
		!item.WorkspaceRecovery.OperatorConfirmed || item.WorkspaceRecovery.HeadSHA == "" ||
		item.WorkspaceRecovery.WorktreeSHA256 != beforeDigest {
		t.Fatalf("recovery audit is incomplete: %+v", item)
	}
	if item.Status != old.Status || item.RunID != old.RunID || item.LeaseGeneration != old.LeaseGeneration ||
		!reflect.DeepEqual(item.Lease, old.Lease) || !reflect.DeepEqual(item.ResourcePark, old.ResourcePark) ||
		item.SessionID != old.SessionID || !reflect.DeepEqual(item.Session, old.Session) ||
		item.GitHubSync != old.GitHubSync || !reflect.DeepEqual(item.BlockedCause, old.BlockedCause) {
		t.Fatalf("validation-only recovery changed lifecycle authority: before=%+v after=%+v", old, item)
	}
	afterDigest, _ := fixture.manager.ContentDigest(context.Background(), fixture.worktree)
	if afterDigest != beforeDigest {
		t.Fatalf("worktree changed: before=%s after=%s", beforeDigest, afterDigest)
	}
	if violations := state.SemanticViolations(after); len(violations) != 0 {
		t.Fatalf("recovered snapshot still violates semantic contract: %+v", violations)
	}

	revision := after.StateRevision
	out.Reset()
	stderr.Reset()
	if code := a.Run(context.Background(), confirm); code != 0 || !strings.Contains(out.String(), `"idempotent": true`) {
		t.Fatalf("idempotent confirm code=%d out=%s stderr=%s", code, out.String(), stderr.String())
	}
	idempotent, _ := fixture.store.Load()
	if idempotent.StateRevision != revision {
		t.Fatalf("idempotent recovery changed revision: %d -> %d", revision, idempotent.StateRevision)
	}
}

func TestRecoverWorkspaceRejectsMissingConfirmationActiveWorkerAndPendingRequest(t *testing.T) {
	fixture := newAnsweredWorkspaceAppFixture(t, false)
	disqualifyAnsweredLifecycleFixture(t, fixture)
	var out, stderr bytes.Buffer
	a := App{Out: &out, Err: &stderr, ProcessController: &appProcessGroups{alive: map[int]bool{44901: true}, signals: map[int][]syscall.Signal{}}}
	if code := a.Run(context.Background(), []string{"recover-workspace", "--repo", fixture.repo, "--issue", "449", "--json"}); code != 2 {
		t.Fatalf("missing confirmation code=%d stderr=%s", code, stderr.String())
	}
	if _, err := fixture.store.Update("fixture_active_worker", 449, "run_0c0123ac8570c0a8", nil, func(snapshot *state.Snapshot) error {
		snapshot.Issues["449"].WorkerPID = 44901
		snapshot.Issues["449"].WorkerPGID = 44901
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	stderr.Reset()
	if code := a.Run(context.Background(), []string{"recover-workspace", "--repo", fixture.repo, "--issue", "449", "--dry-run", "--json"}); code != 4 {
		t.Fatalf("active worker code=%d stderr=%s", code, stderr.String())
	}
	active, _ := fixture.store.Load()
	if active.Issues["449"].Workspace != nil {
		t.Fatal("active worker rejection changed workspace")
	}

	if _, err := fixture.store.Update("fixture_pending_request", 449, "run_0c0123ac8570c0a8", nil, func(snapshot *state.Snapshot) error {
		item := snapshot.Issues["449"]
		item.WorkerPID, item.WorkerPGID = 0, 0
		snapshot.PendingRequests["req_other"] = &state.Request{ID: "req_other", IssueNumber: 449, Status: issuedomain.RequestStatusPending}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	a.ProcessController = &appProcessGroups{alive: map[int]bool{}, signals: map[int][]syscall.Signal{}}
	out.Reset()
	stderr.Reset()
	if code := a.Run(context.Background(), []string{"recover-workspace", "--repo", fixture.repo, "--issue", "449", "--dry-run", "--json"}); code != 4 {
		t.Fatalf("pending request code=%d stderr=%s", code, stderr.String())
	}
	pending, _ := fixture.store.Load()
	if pending.Issues["449"].Workspace != nil {
		t.Fatal("pending request rejection changed workspace")
	}
}

func TestRecoverWorkspaceDirectsAnsweredLifecycleCandidateToDedicatedCommand(t *testing.T) {
	fixture := newAnsweredWorkspaceAppFixture(t, false)
	before, _ := fixture.store.Load()
	var out, stderr bytes.Buffer
	a := App{Out: &out, Err: &stderr, ProcessController: &appProcessGroups{alive: map[int]bool{}, signals: map[int][]syscall.Signal{}}}
	preview := []string{"recover-workspace", "--repo", fixture.repo, "--issue", "449", "--dry-run", "--json"}
	if code := a.Run(context.Background(), preview); code != 0 || !strings.Contains(out.String(), `"eligible": false`) ||
		!strings.Contains(out.String(), `"lifecycle_candidate": "answered_missing_workspace"`) ||
		!strings.Contains(out.String(), "recover-answered-workspace") {
		t.Fatalf("dedicated remediation preview code=%d out=%s stderr=%s", code, out.String(), stderr.String())
	}
	out.Reset()
	stderr.Reset()
	if code := a.Run(context.Background(), []string{"recover-workspace", "--repo", fixture.repo, "--issue", "449", "--confirm-verified-workspace", "--json"}); code != 4 ||
		!strings.Contains(stderr.String(), "recover-answered-workspace") {
		t.Fatalf("generic confirm was not refused: code=%d stderr=%s", code, stderr.String())
	}
	after, _ := fixture.store.Load()
	if after.StateRevision != before.StateRevision || after.Issues["449"].Workspace != nil {
		t.Fatal("generic remediation/refusal mutated exact lifecycle candidate")
	}
}

func TestValidateWorkspaceRecoveryRemoteRequiresExactLifecycleAndPullRequestIdentity(t *testing.T) {
	cfg := config.Config{}
	cfg.GitHub.RunningLabel = "running"
	cfg.GitHub.NeedsInputLabel = "needs-input"
	cfg.GitHub.DoneLabel = "done"
	cfg.GitHub.FailedLabel = "failed"
	cfg.GitHub.ReadyLabels = []string{"ready"}
	cfg.GitHub.ExcludeLabels = []string{"blocked", "do-not-automate"}
	cfg.Git.BaseBranch = "main"
	issue := &state.Issue{Number: 65, Status: issuedomain.StatusBlocked, Branch: "codex/issue-65", PullRequestURL: "https://example.test/pr/85"}
	inspection := worktree.Inspection{Exists: true, Valid: true, Branch: issue.Branch, Head: "head", RemoteHead: "head", RemoteBranchExists: true, RemoteConsistent: true}
	remote := gh.RemoteState{
		Issue:        gh.Issue{Number: 65, State: "OPEN", Labels: []string{"blocked"}},
		PullRequests: []gh.PullRequest{{Number: 85, URL: issue.PullRequestURL, State: "OPEN", HeadRefName: issue.Branch, BaseRefName: "main", HeadSHA: "head", HeadRepository: cfg.GitHub.Repo}},
	}
	if err := validateWorkspaceRecoveryRemote(cfg, issue, inspection, remote); err != nil {
		t.Fatalf("valid remote rejected: %v", err)
	}
	failedIssue := *issue
	failedIssue.Status = issuedomain.StatusFailed
	failedRemote := remote
	failedRemote.Issue.Labels = []string{"failed"}
	if err := validateWorkspaceRecoveryRemote(cfg, &failedIssue, inspection, failedRemote); err != nil {
		t.Fatalf("valid failed remote rejected: %v", err)
	}
	manual := remote
	manual.Issue.Labels = []string{"blocked", "do-not-automate"}
	if err := validateWorkspaceRecoveryRemote(cfg, issue, inspection, manual); err == nil {
		t.Fatal("manual exclusion accepted")
	}
	mismatch := remote
	mismatch.PullRequests[0].HeadSHA = "changed"
	if err := validateWorkspaceRecoveryRemote(cfg, issue, inspection, mismatch); err == nil {
		t.Fatal("changed Pull Request head accepted")
	}
}

func disqualifyAnsweredLifecycleFixture(t *testing.T, fixture answeredWorkspaceAppFixture) {
	t.Helper()
	if _, err := fixture.store.Update("fixture_non_answered_terminal", 449, "run_0c0123ac8570c0a8", nil, func(*state.Snapshot) error { return nil }); err != nil {
		t.Fatal(err)
	}
}
