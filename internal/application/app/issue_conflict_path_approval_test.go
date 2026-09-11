package app

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ishii1648/codex-issue-loop/internal/adapter/state"
	"github.com/ishii1648/codex-issue-loop/internal/adapter/worktree"
	"github.com/ishii1648/codex-issue-loop/internal/application/supervisor"
	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
)

func newConflictPathApprovalFixture(t *testing.T) (*issueResolutionFixture, []string) {
	t.Helper()
	data, err := os.ReadFile("testdata/active-conflict-path-approval.json")
	if err != nil {
		t.Fatal(err)
	}
	var saved struct {
		Status           issuedomain.Status             `json:"status"`
		Stage            issuedomain.ContinuationStage  `json:"stage"`
		SuspensionStatus issuedomain.SuspensionStatus   `json:"suspension_status"`
		Recoverability   issuedomain.Recoverability     `json:"recoverability"`
		AllowedActions   []issuedomain.ResolutionAction `json:"allowed_actions"`
		Reason           string                         `json:"reason"`
		AdditionalPaths  []string                       `json:"additional_paths"`
		Request          state.Request                  `json:"request"`
	}
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	f := newIssueResolutionFixture(t, 287, "OPEN", nil)
	f.prepareQuarantinedConflict(t, 287)
	for _, path := range saved.AdditionalPaths {
		full := filepath.Join(f.worktree, path)
		if err := os.MkdirAll(filepath.Dir(full), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("package state\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	digest, err := worktree.ContentDigest(context.Background(), "/usr/bin/git", f.worktree)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.Update("fixture_active_conflict", 287, f.runID, nil, func(snapshot *state.Snapshot) error {
		item := snapshot.Issues["287"]
		item.Status = saved.Status
		item.Continuation.Stage = saved.Stage
		item.Continuation.WorktreeSHA256 = digest
		item.Continuation.PullRequestNumber = item.PullRequestNumber
		cfg := mustConfig(t, f.repo)
		item.Workspace.MainCheckout = cfg.RepoPath
		item.Workspace.GitCommonDir = filepath.Join(cfg.RepoPath, ".git")
		workspace := *item.Workspace
		item.Continuation.Workspace = &workspace
		item.Suspension.Status = saved.SuspensionStatus
		item.Suspension.Recoverability = saved.Recoverability
		item.Suspension.MissingEvidence = nil
		item.Suspension.AllowedActions = saved.AllowedActions
		item.Suspension.Reason = saved.Reason
		item.LastError = saved.Reason
		snapshot.PendingRequests[saved.Request.ID] = &saved.Request
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	f.rewritePullRequests(t, []map[string]any{{
		"number": 287, "url": "https://example.test/pull/287", "state": "OPEN", "isDraft": false,
		"mergedAt": nil, "headRefName": f.branch, "baseRefName": "main", "headRefOid": f.head,
		"mergeCommit": nil, "headRepository": map[string]any{"name": "repo"},
		"headRepositoryOwner": map[string]any{"login": "owner"}, "mergeStateStatus": "DIRTY", "statusCheckRollup": []any{},
	}})
	return f, saved.AdditionalPaths
}

func approvalArgs(f *issueResolutionFixture, paths []string) []string {
	args := []string{"issue", "resolve", "--repo", f.repo, "--issue", "287", "--action", "approve-conflict-paths", "--json"}
	for _, path := range paths {
		args = append(args, "--allow-path", path)
	}
	return args
}

func TestConflictPathApprovalPreviewApplyRetry(t *testing.T) {
	for _, stage := range []issuedomain.ContinuationStage{issuedomain.ContinuationStageResume, issuedomain.ContinuationStageConflict} {
		t.Run(string(stage), func(t *testing.T) { testConflictPathApprovalPreviewApplyRetry(t, stage) })
	}
}

func testConflictPathApprovalPreviewApplyRetry(t *testing.T, stage issuedomain.ContinuationStage) {
	f, paths := newConflictPathApprovalFixture(t)
	if stage != issuedomain.ContinuationStageResume {
		if _, err := f.store.Update("fixture_stage", 287, f.runID, nil, func(s *state.Snapshot) error { s.Issues["287"].Continuation.Stage = stage; return nil }); err != nil {
			t.Fatal(err)
		}
	}
	before, err := f.store.Load()
	if err != nil {
		t.Fatal(err)
	}
	original, _ := os.ReadFile(f.store.StatePath())
	var out, stderr bytes.Buffer
	a := App{Out: &out, Err: &stderr}
	args := []string{"issue", "plan", "--repo", f.repo, "--issue", "287", "--json"}
	for _, path := range paths {
		args = append(args, "--allow-path", path)
	}
	if code := a.Run(context.Background(), args); code != 0 {
		t.Fatalf("plan: %d %s", code, &stderr)
	}
	var report issuePlanReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if action := findActionPlan(t, report.Actions, issuedomain.ResolutionApproveConflictPaths); !action.Eligible {
		t.Fatalf("plan: %+v observations=%+v", action, report.Observations)
	}
	after, _ := os.ReadFile(f.store.StatePath())
	if !bytes.Equal(original, after) {
		t.Fatal("preview changed state")
	}
	for i := 0; i < 2; i++ {
		out.Reset()
		stderr.Reset()
		if code := a.Run(context.Background(), approvalArgs(f, paths)); code != 0 {
			t.Fatalf("approve: %d %s", code, &stderr)
		}
		if strings.Contains(out.String(), `"idempotent": true`) != (i == 1) {
			t.Fatalf("unexpected result: %s", &out)
		}
	}
	approved, err := f.store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if approved.StateRevision != before.StateRevision+1 {
		t.Fatal("duplicate approval transaction")
	}
	expected := before.Issues["287"]
	expected.ConflictRecovery.AllowedPaths = append(expected.ConflictRecovery.AllowedPaths, paths...)
	if !reflect.DeepEqual(expected, approved.Issues["287"]) || !reflect.DeepEqual(before.PendingRequests, approved.PendingRequests) || !reflect.DeepEqual(before.PendingEffects, approved.PendingEffects) || approved.ActiveExecution != nil {
		t.Fatal("approval changed saved evidence or ownership")
	}
	digest, err := worktree.ContentDigest(context.Background(), "/usr/bin/git", f.worktree)
	if err != nil || digest != expected.Continuation.WorktreeSHA256 {
		t.Fatal("worktree changed")
	}
	events, _ := os.ReadFile(f.store.EventsPath())
	if strings.Count(string(events), `"type":"issue_conflict_paths_approved"`) != 1 || !bytes.Contains(events, []byte(`"allowed_paths_added":["internal/adapter/state/diagnosis.go","internal/adapter/state/diagnosis_test.go"]`)) {
		t.Fatalf("approval audit: %s", events)
	}
	out.Reset()
	stderr.Reset()
	if code := a.Run(context.Background(), []string{"issue", "resolve", "--repo", f.repo, "--issue", "287", "--action", "retry-stage", "--json"}); code != 0 {
		t.Fatalf("retry: %d %s", code, &stderr)
	}
	resumed, err := f.store.Load()
	if err != nil {
		t.Fatal(err)
	}
	wantStatus := issuedomain.StatusResumePending
	if stage == issuedomain.ContinuationStageConflict {
		wantStatus = issuedomain.StatusResolvingConflict
	}
	if resumed.Issues["287"].Status != wantStatus || resumed.Issues["287"].ConflictRecovery.Attempts != 0 {
		t.Fatal("retry did not resume checkpoint")
	}
}

func TestConflictPathApprovalRejectsWithoutMutation(t *testing.T) {
	for _, name := range []string{"no_paths", "partial_paths", "unrelated_path", "digest", "merge_target", "head", "pr_head", "pr_number", "pr_base", "multiple_prs", "closed_issue", "worker", "execution", "workspace", "checkpoint", "generation", "staged_path", "pending", "unknown_worktree"} {
		t.Run(name, func(t *testing.T) {
			f, paths := newConflictPathApprovalFixture(t)
			switch name {
			case "no_paths":
				paths = nil
			case "partial_paths":
				paths = paths[:1]
			case "unrelated_path":
				paths = append(paths, "future.go")
			case "staged_path":
				file := filepath.Join(f.worktree, ".agent-loop.yaml")
				content, err := os.ReadFile(file)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(file, append(content, []byte("\n# staged change\n")...), 0600); err != nil {
					t.Fatal(err)
				}
				runIssueGit(t, f.worktree, "add", ".agent-loop.yaml")
				if err := os.WriteFile(file, content, 0600); err != nil {
					t.Fatal(err)
				}
				digest, err := worktree.ContentDigest(context.Background(), "/usr/bin/git", f.worktree)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := f.store.Update("fixture_staged_change", 287, f.runID, nil, func(snapshot *state.Snapshot) error {
					snapshot.Issues["287"].Continuation.WorktreeSHA256 = digest
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			case "digest":
				if err := os.WriteFile(filepath.Join(f.worktree, paths[0]), []byte("changed\n"), 0600); err != nil {
					t.Fatal(err)
				}
			case "merge_target":
				mergePath := runIssueGit(t, f.worktree, "rev-parse", "--git-path", "MERGE_HEAD")
				if err := os.WriteFile(mergePath, []byte(f.base+"\n"), 0600); err != nil {
					t.Fatal(err)
				}
			case "head":
				runIssueGit(t, f.worktree, "update-ref", "HEAD", f.base)
			case "pr_head":
				f.pullRequests[0]["headRefOid"] = f.base
				f.writeGH(t)
			case "pr_number":
				f.pullRequests[0]["number"] = 288
				f.writeGH(t)
			case "pr_base":
				f.pullRequests[0]["baseRefName"] = "other"
				f.writeGH(t)
			case "multiple_prs":
				f.pullRequests = append(f.pullRequests, f.pullRequests[0])
				f.writeGH(t)
			case "closed_issue":
				f.issueJSON = strings.Replace(f.issueJSON, `"state":"OPEN"`, `"state":"CLOSED"`, 1)
				f.writeGH(t)
			case "execution":
				if _, _, err := f.store.StartExecution(state.ExecutionStart{IssueNumber: 288, RunID: "run_other", BaseSHA: f.base, StartedAt: time.Now().UTC()}); err != nil {
					t.Fatal(err)
				}
			default:
				if _, err := f.store.Update("fixture_mismatch", 287, f.runID, nil, func(s *state.Snapshot) error {
					item := s.Issues["287"]
					switch name {
					case "worker":
						item.WorkerPID = os.Getpid()
					case "pending":
						s.PendingRequests["req_new"] = &state.Request{ID: "req_new", IssueNumber: 287, Status: issuedomain.RequestStatusPending, Question: "another question", CreatedAt: time.Now().UTC()}
					case "unknown_worktree":
						item.Worktree += "-other"
					case "workspace":
						item.Workspace.Path += "-other"
					case "checkpoint":
						item.Continuation.HeadSHA = f.base
					case "generation":
						item.Continuation.Generation++
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			before, _ := os.ReadFile(f.store.StatePath())
			events, _ := os.ReadFile(f.store.EventsPath())
			digest, _ := worktree.ContentDigest(context.Background(), "/usr/bin/git", f.worktree)
			var out, stderr bytes.Buffer
			if code := (App{Out: &out, Err: &stderr}).Run(context.Background(), approvalArgs(f, paths)); code == 0 {
				t.Fatalf("accepted: %s", &out)
			}
			after, _ := os.ReadFile(f.store.StatePath())
			afterEvents, _ := os.ReadFile(f.store.EventsPath())
			afterDigest, _ := worktree.ContentDigest(context.Background(), "/usr/bin/git", f.worktree)
			if !bytes.Equal(before, after) || !bytes.Equal(events, afterEvents) || digest != afterDigest {
				t.Fatal("rejection changed state, history or worktree")
			}
		})
	}
}

type approvalProcessObservation struct {
	supervisor.OSProcessGroupController
	calls   int
	observe func(int) bool
}

func (p *approvalProcessObservation) Alive(int) bool {
	p.calls++
	return p.observe(p.calls)
}

func TestFaultConflictPathApprovalRevalidatesBeforeCommit(t *testing.T) {
	for _, name := range []string{"revision", "remote", "worktree", "merge", "worker"} {
		t.Run(name, func(t *testing.T) {
			f, paths := newConflictPathApprovalFixture(t)
			before, _ := os.ReadFile(f.store.StatePath())
			events, _ := os.ReadFile(f.store.EventsPath())
			var changedDigest string
			process := &approvalProcessObservation{observe: func(call int) bool {
				switch {
				case name == "revision" && call == 2:
					if _, err := f.store.Update("concurrent_operator", 0, "", nil, func(s *state.Snapshot) error { s.Supervisor.Message = "another operation"; return nil }); err != nil {
						t.Fatal(err)
					}
					before, _ = os.ReadFile(f.store.StatePath())
					events, _ = os.ReadFile(f.store.EventsPath())
				case name == "remote" && call == 2:
					f.pullRequests[0]["headRefOid"] = f.base
					f.writeGH(t)
				case name == "worktree" && call == 3:
					if err := os.WriteFile(filepath.Join(f.worktree, paths[0]), []byte("concurrent change\n"), 0600); err != nil {
						t.Fatal(err)
					}
					changedDigest, _ = worktree.ContentDigest(context.Background(), "/usr/bin/git", f.worktree)
				case name == "merge" && call == 3:
					mergePath := runIssueGit(t, f.worktree, "rev-parse", "--git-path", "MERGE_HEAD")
					if err := os.WriteFile(mergePath, []byte(f.base+"\n"), 0600); err != nil {
						t.Fatal(err)
					}
				case name == "worker" && call == 3:
					return true
				}
				return false
			}}
			var out, stderr bytes.Buffer
			if code := (App{Out: &out, Err: &stderr, ProcessController: process}).Run(context.Background(), approvalArgs(f, paths)); code == 0 {
				t.Fatalf("concurrent change accepted: %s", &out)
			}
			after, _ := os.ReadFile(f.store.StatePath())
			afterEvents, _ := os.ReadFile(f.store.EventsPath())
			if !bytes.Equal(before, after) || !bytes.Equal(events, afterEvents) {
				t.Fatal("rejection wrote state or audit")
			}
			if changedDigest != "" {
				afterDigest, _ := worktree.ContentDigest(context.Background(), "/usr/bin/git", f.worktree)
				if afterDigest != changedDigest {
					t.Fatal("concurrent worktree change lost")
				}
			}
		})
	}
}

func TestFaultConcurrentConflictPathApprovalCommitsOnce(t *testing.T) {
	f, paths := newConflictPathApprovalFixture(t)
	before, err := f.store.Load()
	if err != nil {
		t.Fatal(err)
	}
	ready, release := make(chan struct{}, 2), make(chan struct{})
	results := make(chan int, 2)
	for i := 0; i < 2; i++ {
		go func() {
			process := &approvalProcessObservation{observe: func(call int) bool {
				if call == 2 {
					ready <- struct{}{}
					<-release
				}
				return false
			}}
			var out, stderr bytes.Buffer
			results <- (App{Out: &out, Err: &stderr, ProcessController: process}).Run(context.Background(), approvalArgs(f, paths))
		}()
	}
	for i := 0; i < 2; i++ {
		select {
		case <-ready:
		case <-time.After(20 * time.Second):
			t.Fatal("concurrent plans did not reach barrier")
		}
	}
	close(release)
	codes := []int{<-results, <-results}
	if !((codes[0] == 0 && codes[1] == 4) || (codes[0] == 4 && codes[1] == 0)) {
		t.Fatalf("concurrent results=%v", codes)
	}
	after, err := f.store.Load()
	if err != nil || after.StateRevision != before.StateRevision+1 {
		t.Fatalf("concurrent commit revision: %d err=%v", after.StateRevision, err)
	}
	events, _ := os.ReadFile(f.store.EventsPath())
	if strings.Count(string(events), `"type":"issue_conflict_paths_approved"`) != 1 {
		t.Fatal("approval event duplicated")
	}
}
