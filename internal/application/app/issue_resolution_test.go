package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ishii1648/codex-issue-loop/internal/adapter/state"
	"github.com/ishii1648/codex-issue-loop/internal/adapter/worktree"
	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
	"github.com/ishii1648/codex-issue-loop/internal/platform/layout"
	"github.com/ishii1648/codex-issue-loop/internal/platform/registry"
)

func TestIssuePlanIgnoresEventsAndCancelIsFencedAndIdempotent(t *testing.T) {
	repo, l := testEnvironment(t)
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	cfg := mustConfig(t, repo)
	entry := registry.Entry{
		RepoID: registry.RepoID(cfg.GitHub.Repo, cfg.RepoPath), RepoPath: cfg.RepoPath, GitHubRepo: cfg.GitHub.Repo,
		Commands: map[string]string{"git": "/usr/bin/git", "gh": "/usr/bin/false"},
	}
	writeJSONFixture(t, l.RegistryPath, registry.Registry{Version: registry.CurrentVersion, Repos: map[string]registry.Entry{entry.RepoID: entry}})
	store := state.Store{Dir: l.RepoDir(entry.RepoID), RepoID: entry.RepoID, RepoPath: repo}
	if err := store.Initialize(); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC)
	_, _, err := store.StartExecution(state.ExecutionStart{IssueNumber: 1, Title: "fixture", RunID: "run_1", StartedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update("worker_started", 1, "run_1", nil, func(snapshot *state.Snapshot) error {
		item := snapshot.Issues["1"]
		item.Status, item.Worktree, item.Branch = issuedomain.StatusRunning, repo, "main"
		item.Workspace = testWorkerWorkspace(snapshot, repo, "main")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update("worker_blocked", 1, "run_1", nil, func(snapshot *state.Snapshot) error {
		item := snapshot.Issues["1"]
		item.LastError = "ambiguous fixture"
		item.Worktree, item.Workspace = "", nil
		identity := state.ExecutionIdentity{RunID: item.RunID, Generation: item.Generation}
		if err := state.CaptureContinuation(snapshot, item.Number, identity, state.NewID("checkpoint"), now); err != nil {
			return err
		}
		decision, decisionErr := issuedomain.Fail(item.Status, item.LastError, "issue", true)
		if decisionErr != nil {
			return decisionErr
		}
		return state.ApplyIssueTransition(item, decision.Transition)
	}); err != nil {
		t.Fatal(err)
	}
	stateBefore, err := os.ReadFile(store.StatePath())
	if err != nil {
		t.Fatal(err)
	}
	eventsBefore, err := os.ReadFile(store.EventsPath())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.EventsPath(), []byte("event history is deliberately unavailable\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var out, stderr bytes.Buffer
	a := App{Out: &out, Err: &stderr}
	if code := a.Run(context.Background(), []string{"issue", "plan", "--repo", repo, "--issue", "1", "--json"}); code != 0 {
		t.Fatalf("plan code=%d stderr=%s", code, stderr.String())
	}
	var report issuePlanReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil || !report.ReadOnly || !actionEligibility(report.Actions, issuedomain.ResolutionCancel) {
		t.Fatalf("plan=%+v err=%v", report, err)
	}
	stateAfterPlan, _ := os.ReadFile(store.StatePath())
	eventsAfterPlan, _ := os.ReadFile(store.EventsPath())
	if !bytes.Equal(stateBefore, stateAfterPlan) || string(eventsAfterPlan) != "event history is deliberately unavailable\n" {
		t.Fatal("issue plan changed durable state or consulted/repaired event history")
	}
	if err := os.WriteFile(store.EventsPath(), eventsBefore, 0o600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	stderr.Reset()
	if code := a.Run(context.Background(), []string{"issue", "resolve", "--repo", repo, "--issue", "1", "--action", "cancel", "--json"}); code != 0 {
		t.Fatalf("resolve code=%d stderr=%s", code, stderr.String())
	}
	resolved, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	item := resolved.Issues["1"]
	if resolved.ActiveExecution != nil || item.Suspension == nil || item.Suspension.Status != issuedomain.SuspensionResolved || item.Suspension.Resolution != issuedomain.ResolutionCancel {
		t.Fatalf("resolved Issue=%+v", item)
	}
	revision := resolved.StateRevision
	if code := a.Run(context.Background(), []string{"issue", "resolve", "--repo", repo, "--issue", "1", "--action", "cancel", "--json"}); code != 0 {
		t.Fatalf("idempotent resolve code=%d stderr=%s", code, stderr.String())
	}
	again, err := store.Load()
	if err != nil || again.StateRevision != revision {
		t.Fatalf("idempotent resolution mutated state: before=%d after=%d err=%v", revision, again.StateRevision, err)
	}
}

func TestIssuePlanAndResolveCancelQuarantineOnlyRecord(t *testing.T) {
	repo, l := testEnvironment(t)
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	cfg := mustConfig(t, repo)
	entry := registry.Entry{
		RepoID: registry.RepoID(cfg.GitHub.Repo, cfg.RepoPath), RepoPath: cfg.RepoPath, GitHubRepo: cfg.GitHub.Repo,
		Commands: map[string]string{"git": "/usr/bin/git", "gh": "/usr/bin/false"},
	}
	writeJSONFixture(t, l.RegistryPath, registry.Registry{Version: registry.CurrentVersion, Repos: map[string]registry.Entry{entry.RepoID: entry}})
	store := state.Store{Dir: l.RepoDir(entry.RepoID), RepoID: entry.RepoID, RepoPath: repo}
	if err := store.Initialize(); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 4, 0, 0, 0, 0, time.UTC)
	if _, err := store.Update("fixture_quarantine", 185, "old_run", nil, func(snapshot *state.Snapshot) error {
		snapshot.QuarantinedIssues["185"] = &state.QuarantineRecord{
			IssueNumber: 185, RunID: "old_run", Generation: 1, RejectedStatus: issuedomain.StatusClaiming,
			ReasonCode: "issue_invariant_violation", Reason: "ambiguous old run", QuarantinedAt: now,
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	stateBefore, err := os.ReadFile(store.StatePath())
	if err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	a := App{Out: &out, Err: &stderr}
	if code := a.Run(context.Background(), []string{"issue", "plan", "--repo", repo, "--issue", "185", "--json"}); code != 0 {
		t.Fatalf("plan code=%d stderr=%s", code, stderr.String())
	}
	var report issuePlanReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil || report.Quarantine == nil || !report.ReadOnly ||
		!actionEligibility(report.Actions, issuedomain.ResolutionCancel) {
		t.Fatalf("report=%+v err=%v", report, err)
	}
	stateAfterPlan, _ := os.ReadFile(store.StatePath())
	if !bytes.Equal(stateBefore, stateAfterPlan) {
		t.Fatal("quarantine plan changed durable state")
	}
	out.Reset()
	stderr.Reset()
	if code := a.Run(context.Background(), []string{"issue", "resolve", "--repo", repo, "--issue", "185", "--action", "cancel", "--json"}); code != 0 {
		t.Fatalf("resolve code=%d stdout=%s stderr=%s", code, out.String(), stderr.String())
	}
	resolved, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if resolved.QuarantinedIssues["185"] != nil || resolved.Issues["185"] != nil || resolved.ActiveExecution != nil {
		t.Fatalf("quarantine was not cleared: %+v", resolved)
	}
}

func TestIssueResolveResumeRetriesGitHubWithoutRefencing(t *testing.T) {
	fixture := newIssueResolutionFixture(t, 449, "OPEN", nil)
	failureMarker := filepath.Join(t.TempDir(), "fail-edit-once")
	if err := os.WriteFile(failureMarker, []byte("fail once"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENT_LOOP_TEST_FAIL_EDIT_ONCE", failureMarker)
	fixture.block(t, issuedomain.StatusRunning, "network unavailable", true, "")

	args := []string{"issue", "resolve", "--repo", fixture.repo, "--issue", "449", "--action", "resume", "--json"}
	var out, stderr bytes.Buffer
	a := App{Out: &out, Err: &stderr}
	if code := a.Run(context.Background(), args); code == 0 {
		t.Fatalf("injected GitHub failure was ignored: stdout=%s stderr=%s", out.String(), stderr.String())
	}
	afterFailure, err := fixture.store.Load()
	if err != nil {
		t.Fatal(err)
	}
	item := afterFailure.Issues["449"]
	if item.Status != issuedomain.StatusResumePending || afterFailure.ActiveExecution != nil || item.Continuation == nil || item.Continuation.Generation != 2 ||
		state.PendingEffect(&afterFailure, 449) == nil || state.PendingEffect(&afterFailure, 449).Kind != issuedomain.EffectApplyResolution || item.Suspension == nil ||
		item.Suspension.Status != issuedomain.SuspensionResolved || item.Suspension.Resolution != issuedomain.ResolutionResume {
		t.Fatalf("durable resolution boundary=%+v", item)
	}

	out.Reset()
	stderr.Reset()
	if code := a.Run(context.Background(), args); code != 0 {
		t.Fatalf("retry code=%d stdout=%s stderr=%s", code, out.String(), stderr.String())
	}
	afterRetry, err := fixture.store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := afterRetry.Issues["449"]; afterRetry.ActiveExecution != nil || got.Continuation == nil || got.Continuation.Generation != 2 || state.PendingEffect(&afterRetry, 449) != nil {
		t.Fatalf("retry refenced or failed to converge: %+v", got)
	}
	revision := afterRetry.StateRevision
	out.Reset()
	stderr.Reset()
	if code := a.Run(context.Background(), args); code != 0 {
		t.Fatalf("idempotent code=%d stderr=%s", code, stderr.String())
	}
	idempotent, err := fixture.store.Load()
	if err != nil || idempotent.StateRevision != revision || idempotent.ActiveExecution != nil || idempotent.Issues["449"].Continuation == nil || idempotent.Issues["449"].Continuation.Generation != 2 {
		t.Fatalf("idempotent resume mutated state: revision=%d/%d err=%v", revision, idempotent.StateRevision, err)
	}
}

func TestIssueResolveResumeRejectsChangedWorktreeAndCheckpointBase(t *testing.T) {
	fixture := newIssueResolutionFixture(t, 450, "OPEN", nil)
	fixture.block(t, issuedomain.StatusRunning, "environment unavailable", true, "")
	if err := os.WriteFile(filepath.Join(fixture.worktree, "after-checkpoint.txt"), []byte("changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	args := []string{"issue", "resolve", "--repo", fixture.repo, "--issue", "450", "--action", "resume", "--json"}
	var out, stderr bytes.Buffer
	a := App{Out: &out, Err: &stderr}
	if code := a.Run(context.Background(), args); code == 0 || !strings.Contains(stderr.String(), "worktree content differs") {
		t.Fatalf("changed worktree was accepted: code=%d stdout=%s stderr=%s", code, out.String(), stderr.String())
	}
	if err := os.Remove(filepath.Join(fixture.worktree, "after-checkpoint.txt")); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.Update("fixture_change_checkpoint_base", 450, fixture.runID, nil, func(snapshot *state.Snapshot) error {
		snapshot.Issues["450"].Continuation.BaseSHA = strings.Repeat("f", 40)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	stderr.Reset()
	if code := a.Run(context.Background(), args); code == 0 || !strings.Contains(stderr.String(), "checkpoint base is not an ancestor") {
		t.Fatalf("invalid checkpoint base was accepted: code=%d stdout=%s stderr=%s", code, out.String(), stderr.String())
	}
	snapshot, err := fixture.store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if item := snapshot.Issues["450"]; snapshot.ActiveExecution != nil || item.Suspension == nil || item.Suspension.Status != issuedomain.SuspensionActive {
		t.Fatalf("rejected resolution changed execution state: %+v", item)
	}
}

func TestIssueResolveRetryStageReturnsToCheckpointStage(t *testing.T) {
	mergedAt := "null"
	fixture := newIssueResolutionFixture(t, 102, "OPEN", []map[string]any{{
		"number": 12, "url": "https://example.test/pull/12", "state": "OPEN", "isDraft": false,
		"mergedAt": mergedAt, "baseRefName": "main", "mergeStateStatus": "CLEAN", "statusCheckRollup": []any{},
	}})
	fixture.block(t, issuedomain.StatusAwaitingChecks, "checks failed", false, "https://example.test/pull/12")
	fixture.rewritePullRequests(t, []map[string]any{{
		"number": 12, "url": "https://example.test/pull/12", "state": "OPEN", "isDraft": false,
		"mergedAt": nil, "headRefName": fixture.branch, "baseRefName": "main", "headRefOid": fixture.base,
		"mergeCommit": nil, "headRepository": map[string]any{"name": "repo"},
		"headRepositoryOwner": map[string]any{"login": "owner"}, "mergeStateStatus": "CLEAN", "statusCheckRollup": []any{},
	}})
	var out, stderr bytes.Buffer
	a := App{Out: &out, Err: &stderr}
	args := []string{"issue", "resolve", "--repo", fixture.repo, "--issue", "102", "--action", "retry-stage", "--json"}
	if code := a.Run(context.Background(), args); code == 0 || !strings.Contains(stderr.String(), "not cleanly reproducible") {
		t.Fatalf("mismatched repaired head was accepted: code=%d stdout=%s stderr=%s", code, out.String(), stderr.String())
	}
	fixture.rewritePullRequests(t, []map[string]any{{
		"number": 12, "url": "https://example.test/pull/12", "state": "OPEN", "isDraft": false,
		"mergedAt": nil, "headRefName": fixture.branch, "baseRefName": "main", "headRefOid": fixture.head,
		"mergeCommit": nil, "headRepository": map[string]any{"name": "repo"},
		"headRepositoryOwner": map[string]any{"login": "owner"}, "mergeStateStatus": "CLEAN", "statusCheckRollup": []any{},
	}})

	out.Reset()
	stderr.Reset()
	if code := a.Run(context.Background(), args); code != 0 {
		t.Fatalf("code=%d stdout=%s stderr=%s", code, out.String(), stderr.String())
	}
	snapshot, err := fixture.store.Load()
	if err != nil {
		t.Fatal(err)
	}
	item := snapshot.Issues["102"]
	if item.Status != issuedomain.StatusAwaitingChecks || snapshot.ActiveExecution != nil ||
		item.Suspension == nil || item.Suspension.Resolution != issuedomain.ResolutionRetryStage || state.PendingEffect(&snapshot, 102) != nil {
		t.Fatalf("retry-stage result=%+v", item)
	}
	if item.HeadSHA != fixture.head || item.PullRequestNumber != 12 {
		t.Fatalf("repaired Pull Request identity was not committed: %+v", item)
	}
}

func TestIssueResolveAdoptPRRequiresExactCleanMergedHead(t *testing.T) {
	mergedAt := "2026-09-02T00:00:00Z"
	fixture := newIssueResolutionFixture(t, 129, "CLOSED", nil)
	fixture.block(t, issuedomain.StatusRunning, "external merge", false, "https://example.test/pull/132")
	fixture.rewritePullRequests(t, []map[string]any{{
		"number": 132, "url": "https://example.test/pull/132", "state": "MERGED", "isDraft": false,
		"mergedAt": mergedAt, "headRefName": fixture.branch, "baseRefName": "main", "headRefOid": fixture.head,
		"mergeCommit": map[string]any{"oid": fixture.head}, "headRepository": map[string]any{"name": "repo"},
		"headRepositoryOwner": map[string]any{"login": "owner"}, "mergeStateStatus": "CLEAN", "statusCheckRollup": []any{},
	}})

	dirty := filepath.Join(fixture.worktree, "dirty.txt")
	if err := os.WriteFile(dirty, []byte("unsafe\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	a := App{Out: &out, Err: &stderr}
	args := []string{"issue", "resolve", "--repo", fixture.repo, "--issue", "129", "--action", "adopt-pr", "--json"}
	if code := a.Run(context.Background(), args); code == 0 || !strings.Contains(stderr.String(), "not clean") {
		t.Fatalf("dirty adoption was accepted: code=%d stdout=%s stderr=%s", code, out.String(), stderr.String())
	}
	if err := os.Remove(dirty); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	stderr.Reset()
	if code := a.Run(context.Background(), args); code != 0 {
		t.Fatalf("code=%d stdout=%s stderr=%s", code, out.String(), stderr.String())
	}
	snapshot, err := fixture.store.Load()
	if err != nil {
		t.Fatal(err)
	}
	item := snapshot.Issues["129"]
	if item.Status != issuedomain.StatusCompleted || snapshot.ActiveExecution != nil || item.Continuation != nil || item.Suspension != nil ||
		!item.PullRequestMerged || item.PullRequestNumber != 132 || item.HeadSHA != fixture.head || state.PendingEffect(&snapshot, 129) != nil {
		t.Fatalf("adopted Issue=%+v", item)
	}
}

func TestIssueResolveCancelClearsPendingAttentionWithoutExecution(t *testing.T) {
	fixture := newIssueResolutionFixture(t, 183, "OPEN", nil)
	fixture.block(t, issuedomain.StatusRunning, "stat worktree: no such file or directory", false, "")
	if _, err := fixture.store.Update("pending_operator_request", 183, fixture.runID, nil, func(snapshot *state.Snapshot) error {
		snapshot.PendingRequests["req_cancel"] = &state.Request{
			ID: "req_cancel", IssueNumber: 183, Question: "recover?", RunID: fixture.runID,
			Status: issuedomain.RequestStatusPending, CreatedAt: time.Now().UTC(),
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	var out, stderr bytes.Buffer
	a := App{Out: &out, Err: &stderr}
	args := []string{"issue", "resolve", "--repo", fixture.repo, "--issue", "183", "--action", "cancel", "--json"}
	if code := a.Run(context.Background(), args); code != 0 {
		t.Fatalf("code=%d stdout=%s stderr=%s", code, out.String(), stderr.String())
	}
	snapshot, err := fixture.store.Load()
	if err != nil {
		t.Fatal(err)
	}
	item := snapshot.Issues["183"]
	if snapshot.ActiveExecution != nil || item.Status != issuedomain.StatusBlocked || item.Suspension == nil ||
		item.Suspension.Resolution != issuedomain.ResolutionCancel || snapshot.PendingRequests["req_cancel"].Status != issuedomain.RequestStatusCanceled {
		t.Fatalf("canceled Issue=%+v request=%+v", item, snapshot.PendingRequests["req_cancel"])
	}
	if _, _, err := fixture.store.StartExecution(state.ExecutionStart{
		IssueNumber: 184, Title: "next", RunID: "run_184", StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("isolated suspension stopped the repository queue: %v", err)
	}
}

func TestIssueResolveCancelPreservesCompetingActiveExecution(t *testing.T) {
	fixture := newIssueResolutionFixture(t, 185, "OPEN", nil)
	fixture.block(t, issuedomain.StatusRunning, "environment unavailable", true, "")
	if _, _, err := fixture.store.StartExecution(state.ExecutionStart{
		IssueNumber: 252, Title: "competing fixture", RunID: "run_252", BaseSHA: fixture.base,
		StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	before, err := fixture.store.Load()
	if err != nil {
		t.Fatal(err)
	}

	var out, stderr bytes.Buffer
	a := App{Out: &out, Err: &stderr}
	planArgs := []string{"issue", "plan", "--repo", fixture.repo, "--issue", "185", "--json"}
	if code := a.Run(context.Background(), planArgs); code != 0 {
		t.Fatalf("plan code=%d stdout=%s stderr=%s", code, out.String(), stderr.String())
	}
	var report issuePlanReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	cancelPlan := findActionPlan(t, report.Actions, issuedomain.ResolutionCancel)
	resumePlan := findActionPlan(t, report.Actions, issuedomain.ResolutionResume)
	if !report.ReadOnly || !cancelPlan.Eligible || resumePlan.Eligible ||
		!containsReason(resumePlan.Reasons, "repository active execution is occupied by Issue #252") {
		t.Fatalf("plan=%+v", report)
	}

	out.Reset()
	stderr.Reset()
	resumeArgs := []string{"issue", "resolve", "--repo", fixture.repo, "--issue", "185", "--action", "resume", "--json"}
	if code := a.Run(context.Background(), resumeArgs); code == 0 ||
		!strings.Contains(stderr.String(), "repository active execution is occupied by Issue #252") {
		t.Fatalf("occupied resume resolved: code=%d stdout=%s stderr=%s", code, out.String(), stderr.String())
	}
	afterResume, err := fixture.store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if afterResume.StateRevision != before.StateRevision || !reflect.DeepEqual(afterResume.ActiveExecution, before.ActiveExecution) ||
		!reflect.DeepEqual(afterResume.Issues["252"], before.Issues["252"]) ||
		!reflect.DeepEqual(afterResume.Issues["185"].Suspension, before.Issues["185"].Suspension) {
		t.Fatalf("rejected resume changed state: before=%+v after=%+v", before, afterResume)
	}

	out.Reset()
	stderr.Reset()
	resolveArgs := []string{"issue", "resolve", "--repo", fixture.repo, "--issue", "185", "--action", "cancel", "--json"}
	if code := a.Run(context.Background(), resolveArgs); code != 0 {
		t.Fatalf("resolve code=%d stdout=%s stderr=%s", code, out.String(), stderr.String())
	}
	after, err := fixture.store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after.ActiveExecution, before.ActiveExecution) ||
		!reflect.DeepEqual(after.Issues["252"], before.Issues["252"]) {
		t.Fatalf("competing execution changed: before=%+v/%+v after=%+v/%+v",
			before.ActiveExecution, before.Issues["252"], after.ActiveExecution, after.Issues["252"])
	}
	item := after.Issues["185"]
	if item.Status != issuedomain.StatusBlocked || item.Suspension == nil ||
		item.Suspension.Status != issuedomain.SuspensionResolved || item.Suspension.Resolution != issuedomain.ResolutionCancel {
		t.Fatalf("canceled Issue=%+v", item)
	}
}

func TestIssuePlanAndResolveRetryStageFailClosedWithCompetingActiveExecution(t *testing.T) {
	fixture := newIssueResolutionFixture(t, 186, "OPEN", nil)
	fixture.block(t, issuedomain.StatusAwaitingChecks, "checks unavailable", false, "https://example.test/pull/186")
	fixture.rewritePullRequests(t, []map[string]any{{
		"number": 186, "url": "https://example.test/pull/186", "state": "OPEN", "isDraft": false,
		"mergedAt": nil, "headRefName": fixture.branch, "baseRefName": "main", "headRefOid": fixture.head,
		"mergeCommit": nil, "headRepository": map[string]any{"name": "repo"},
		"headRepositoryOwner": map[string]any{"login": "owner"}, "mergeStateStatus": "CLEAN", "statusCheckRollup": []any{},
	}})

	var out, stderr bytes.Buffer
	a := App{Out: &out, Err: &stderr}
	planArgs := []string{"issue", "plan", "--repo", fixture.repo, "--issue", "186", "--json"}
	if code := a.Run(context.Background(), planArgs); code != 0 {
		t.Fatalf("initial plan code=%d stdout=%s stderr=%s", code, out.String(), stderr.String())
	}
	var initialReport issuePlanReport
	if err := json.Unmarshal(out.Bytes(), &initialReport); err != nil {
		t.Fatal(err)
	}
	if plan := findActionPlan(t, initialReport.Actions, issuedomain.ResolutionRetryStage); !plan.Eligible {
		t.Fatalf("retry-stage fixture is not initially eligible: %+v", plan)
	}

	if _, _, err := fixture.store.StartExecution(state.ExecutionStart{
		IssueNumber: 253, Title: "competing fixture", RunID: "run_253", BaseSHA: fixture.base,
		StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	before, err := fixture.store.Load()
	if err != nil {
		t.Fatal(err)
	}
	out.Reset()
	stderr.Reset()
	if code := a.Run(context.Background(), planArgs); code != 0 {
		t.Fatalf("occupied plan code=%d stdout=%s stderr=%s", code, out.String(), stderr.String())
	}
	var occupiedReport issuePlanReport
	if err := json.Unmarshal(out.Bytes(), &occupiedReport); err != nil {
		t.Fatal(err)
	}
	plan := findActionPlan(t, occupiedReport.Actions, issuedomain.ResolutionRetryStage)
	if plan.Eligible || !containsReason(plan.Reasons, "repository active execution is occupied by Issue #253") {
		t.Fatalf("occupied retry-stage plan=%+v", plan)
	}

	out.Reset()
	stderr.Reset()
	resolveArgs := []string{"issue", "resolve", "--repo", fixture.repo, "--issue", "186", "--action", "retry-stage", "--json"}
	if code := a.Run(context.Background(), resolveArgs); code == 0 ||
		!strings.Contains(stderr.String(), "repository active execution is occupied by Issue #253") {
		t.Fatalf("occupied retry-stage resolved: code=%d stdout=%s stderr=%s", code, out.String(), stderr.String())
	}
	after, err := fixture.store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if after.StateRevision != before.StateRevision || !reflect.DeepEqual(after.ActiveExecution, before.ActiveExecution) ||
		!reflect.DeepEqual(after.Issues["253"], before.Issues["253"]) ||
		!reflect.DeepEqual(after.Issues["186"].Suspension, before.Issues["186"].Suspension) {
		t.Fatalf("rejected retry-stage changed state: before=%+v after=%+v", before, after)
	}
}

type issueResolutionFixture struct {
	repo, branch, worktree, runID, base, head string
	l                                         layout.Layout
	store                                     state.Store
	issueJSON                                 string
	pullRequests                              []map[string]any
	ghPath                                    string
}

func newIssueResolutionFixture(t *testing.T, number int, issueState string, pullRequests []map[string]any) *issueResolutionFixture {
	t.Helper()
	repo, l := testEnvironment(t)
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	runIssueGit(t, repo, "config", "user.name", "Test")
	runIssueGit(t, repo, "config", "user.email", "test@example.invalid")
	runIssueGit(t, repo, "config", "commit.gpgsign", "false")
	runIssueGit(t, repo, "add", ".agent-loop.yaml")
	runIssueGit(t, repo, "commit", "-m", "base")
	runIssueGit(t, repo, "branch", "-M", "main")
	remote := filepath.Join(t.TempDir(), "remote.git")
	if output, err := exec.Command("git", "init", "--bare", "-q", remote).CombinedOutput(); err != nil {
		t.Fatalf("git init --bare: %v: %s", err, output)
	}
	runIssueGit(t, repo, "remote", "add", "origin", remote)
	runIssueGit(t, repo, "push", "-u", "origin", "main")
	base := runIssueGit(t, repo, "rev-parse", "HEAD")
	branch := fmt.Sprintf("codex/issue-%d-resolution", number)
	canonicalRoot, err := filepath.EvalSymlinks(l.Root)
	if err != nil {
		t.Fatal(err)
	}
	worktree := filepath.Join(canonicalRoot, "worktrees", "resolution", fmt.Sprintf("issue-%d", number))
	if err := os.MkdirAll(filepath.Dir(worktree), 0o700); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("git", "-C", repo, "worktree", "add", "-b", branch, worktree).CombinedOutput(); err != nil {
		t.Fatalf("git worktree add: %v: %s", err, output)
	}
	if err := os.WriteFile(filepath.Join(worktree, "change.txt"), []byte("resolved\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runIssueGit(t, worktree, "add", "change.txt")
	runIssueGit(t, worktree, "commit", "-m", "resolution")
	runIssueGit(t, worktree, "push", "-u", "origin", branch)
	head := runIssueGit(t, worktree, "rev-parse", "HEAD")

	issueJSON := fmt.Sprintf(`{"number":%d,"title":"fixture","body":"","url":"https://example.test/issues/%d","state":%q,"labels":[{"name":"blocked"}],"assignees":[],"milestone":null,"comments":[]}`, number, number, issueState)
	fixture := &issueResolutionFixture{repo: repo, l: l, branch: branch, worktree: worktree, runID: fmt.Sprintf("run_%d", number), base: base, head: head, issueJSON: issueJSON, pullRequests: pullRequests}
	fixture.ghPath = filepath.Join(strings.Split(os.Getenv("PATH"), string(os.PathListSeparator))[0], "gh")
	fixture.writeGH(t)
	cfg := mustConfig(t, repo)
	entry, err := (registry.Store{Path: l.RegistryPath}).Add(cfg)
	if err != nil {
		t.Fatal(err)
	}
	fixture.store = state.Store{Dir: l.RepoDir(entry.RepoID), RepoID: entry.RepoID, RepoPath: repo}
	if err := fixture.store.Initialize(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := fixture.store.StartExecution(state.ExecutionStart{
		IssueNumber: number, Title: "fixture", RunID: fixture.runID,
		BaseSHA: base, StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func (f *issueResolutionFixture) block(t *testing.T, from issuedomain.Status, reason string, resumable bool, pullRequestURL string) {
	t.Helper()
	digest, err := worktree.ContentDigest(context.Background(), "/usr/bin/git", f.worktree)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.Update("fixture_terminal", numberFromRunID(t, f.runID), f.runID, nil, func(snapshot *state.Snapshot) error {
		item := snapshot.Issues[fmt.Sprint(numberFromRunID(t, f.runID))]
		item.Status, item.Branch, item.Worktree = from, f.branch, f.worktree
		item.Workspace = testWorkerWorkspace(snapshot, f.worktree, f.branch)
		item.HeadSHA, item.PullRequestURL = f.head, pullRequestURL
		item.LastError = reason
		identity := state.ExecutionIdentity{RunID: item.RunID, Generation: item.Generation}
		if err := state.CaptureContinuation(snapshot, item.Number, identity, state.NewID("checkpoint"), time.Now().UTC()); err != nil {
			return err
		}
		decision, err := issuedomain.Fail(from, reason, "issue", true)
		if err != nil {
			return err
		}
		if err := state.ApplyIssueTransition(item, decision.Transition); err != nil {
			return err
		}
		item.Continuation.WorktreeSHA256 = digest
		if resumable && item.Continuation != nil {
			item.Suspension = &state.Suspension{ID: state.NewID("suspension"), Origin: "worker", Status: issuedomain.SuspensionActive,
				ReasonCode: "environment", Recoverability: issuedomain.RecoverabilityOperator, Reason: reason,
				AllowedActions: []issuedomain.ResolutionAction{issuedomain.ResolutionCancel, issuedomain.ResolutionResume}, CheckpointID: item.Continuation.ID, SuspendedAt: time.Now().UTC()}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func (f *issueResolutionFixture) rewritePullRequests(t *testing.T, pullRequests []map[string]any) {
	t.Helper()
	f.pullRequests = pullRequests
	f.writeGH(t)
}

func (f *issueResolutionFixture) writeGH(t *testing.T) {
	t.Helper()
	pullJSON, err := json.Marshal(f.pullRequests)
	if err != nil {
		t.Fatal(err)
	}
	script := fmt.Sprintf(`#!/bin/sh
if [ "$1" = "--version" ]; then
  printf 'gh version 2.69.0\n'
  exit 0
fi
case "$1 $2" in
  "issue view")
    case "$*" in *--jq*) exit 0 ;; *) printf '%%s\n' '%s' ;; esac ;;
  "pr list") printf '%%s\n' '%s' ;;
  "issue edit")
    if test -n "$AGENT_LOOP_TEST_FAIL_EDIT_ONCE" && test -f "$AGENT_LOOP_TEST_FAIL_EDIT_ONCE"; then
      rm "$AGENT_LOOP_TEST_FAIL_EDIT_ONCE"
      exit 2
    fi
    exit 0 ;;
  "issue comment"|"issue close") exit 0 ;;
  *) exit 2 ;;
esac
`, f.issueJSON, string(pullJSON))
	if err := os.WriteFile(f.ghPath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
}

func runIssueGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	output, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, output)
	}
	return strings.TrimSpace(string(output))
}

func numberFromRunID(t *testing.T, runID string) int {
	t.Helper()
	var number int
	if _, err := fmt.Sscanf(runID, "run_%d", &number); err != nil || number < 1 {
		t.Fatalf("invalid fixture run ID %q", runID)
	}
	return number
}

func actionEligibility(actions []issueActionPlan, target issuedomain.ResolutionAction) bool {
	for _, action := range actions {
		if action.Action == target {
			return action.Eligible
		}
	}
	return false
}

func findActionPlan(t *testing.T, actions []issueActionPlan, target issuedomain.ResolutionAction) issueActionPlan {
	t.Helper()
	for _, action := range actions {
		if action.Action == target {
			return action
		}
	}
	t.Fatalf("action %s is missing from plan", target)
	return issueActionPlan{}
}

func containsReason(reasons []string, target string) bool {
	for _, reason := range reasons {
		if reason == target {
			return true
		}
	}
	return false
}
