package app

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ishii1648/codex-issue-loop/internal/adapter/state"
	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
)

func TestRetryStageRestoresAnsweredChecksQuarantine(t *testing.T) {
	for _, scenario := range []string{"valid", "path-git-fails", "remote-ahead", "remote-diverged", "wrong-generation", "wrong-answer", "pending", "missing-session", "different-head", "fork", "dirty", "worker-alive"} {
		t.Run(scenario, func(t *testing.T) {
			f := newIssueResolutionFixture(t, 459, "OPEN", nil)
			url := "https://example.test/pull/493"
			f.block(t, issuedomain.StatusRunning, "checks failed", true, url)
			publishedHead := f.head
			if scenario == "remote-ahead" || scenario == "remote-diverged" {
				tree := runIssueGit(t, f.worktree, "rev-parse", "HEAD^{tree}")
				publishedHead = runIssueGit(t, f.worktree, "commit-tree", tree, "-p", f.head, "-m", "remote base update")
				runIssueGit(t, f.worktree, "push", "origin", publishedHead+":refs/heads/"+f.branch)
			}
			now := time.Now().UTC()
			_, err := f.store.Update("fixture", 459, f.runID, nil, func(s *state.Snapshot) error {
				i := s.Issues["459"]
				i.Status, i.Generation, i.Suspension = issuedomain.StatusRetryWait, 7, nil
				i.PullRequestNumber = 493
				i.HeadSHA = publishedHead
				i.Session = &state.WorkerSession{Backend: "codex", ID: "saved-session"}
				i.SessionID = i.Session.ID
				c := i.Continuation
				c.Generation, c.Kind, c.RequestID, c.Stage = 7, state.ContinuationKindNeedsInput, "req_original", issuedomain.ContinuationStageResume
				c.Session, c.PullRequestNumber, c.HeadSHA = i.Session, 493, ""
				r := &state.Request{ID: "req_original", IssueNumber: 459, RunID: f.runID, CheckpointID: c.ID, ReleasedExecution: &state.ExecutionIdentity{RunID: f.runID, Generation: 6}, Status: issuedomain.RequestStatusAnswered, Question: "Which source?", Answer: "Use a dated source", CreatedAt: now.Add(-time.Minute), AnsweredAt: &now}
				i.Answers = []state.AnswerRecord{{RequestID: r.ID, Question: r.Question, Answer: r.Answer, AnsweredAt: now}}
				switch scenario {
				case "wrong-generation":
					r.ReleasedExecution.Generation++
				case "wrong-answer":
					i.Answers[0].Answer = "different"
				case "pending":
					r.Status = issuedomain.RequestStatusPending
				case "missing-session":
					i.Session = nil
				case "worker-alive":
					i.WorkerPID = os.Getpid()
				}
				s.QuarantinedIssues["459"] = &state.QuarantineRecord{IssueNumber: 459, RunID: f.runID, Generation: 8, RejectedStatus: issuedomain.StatusLaunching, ReasonCode: "issue_invariant_violation", Reason: "answered continuation mismatch", QuarantinedAt: now, LastValid: i, Requests: []*state.Request{r}}
				delete(s.Issues, "459")
				delete(s.PendingEffects, "459")
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			pr := map[string]any{"number": 493, "url": url, "state": "OPEN", "isDraft": true, "headRefName": f.branch, "baseRefName": "main", "headRefOid": publishedHead, "headRepository": map[string]any{"name": "repo"}, "headRepositoryOwner": map[string]any{"login": "owner"}, "statusCheckRollup": []any{}}
			if scenario == "different-head" {
				pr["headRefOid"] = f.base
			}
			if scenario == "fork" {
				pr["headRepositoryOwner"] = map[string]any{"login": "other"}
			}
			f.rewritePullRequests(t, []map[string]any{pr})
			if scenario == "remote-ahead" || scenario == "remote-diverged" {
				status := "ahead"
				if scenario == "remote-diverged" {
					status = "diverged"
				}
				script, err := os.ReadFile(f.ghPath)
				if err != nil {
					t.Fatal(err)
				}
				reply := fmt.Sprintf(`  "api repos/owner/repo/compare/%s...%s") printf '%s';;`, f.head, publishedHead, fmt.Sprintf(`{"status":%q,"base_commit":{"sha":%q},"merge_base_commit":{"sha":%q}}`, status, f.head, f.head))
				if err := os.WriteFile(f.ghPath, []byte(strings.Replace(string(script), "  api*)", reply+"\n  api*)", 1)), 0700); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "dirty" {
				if err := os.WriteFile(filepath.Join(f.worktree, "dirty.txt"), []byte("retain me"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			before, err := f.store.ReadCanonicalSnapshot()
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "path-git-fails" {
				failIssueResolutionPathGit(t)
			}
			var out, stderr bytes.Buffer
			code := (App{Out: &out, Err: &stderr}).Run(context.Background(), []string{"issue", "resolve", "--repo", f.repo, "--issue", "459", "--action", "retry-stage", "--json"})
			after, err := f.store.ReadCanonicalSnapshot()
			if err != nil {
				t.Fatal(err)
			}
			if scenario != "valid" && scenario != "path-git-fails" && scenario != "remote-ahead" {
				if code == 0 || !reflect.DeepEqual(before, after) {
					t.Fatalf("code=%d changed=%v stderr=%s", code, !reflect.DeepEqual(before, after), stderr.String())
				}
				return
			}
			if code != 0 {
				t.Fatalf("code=%d stderr=%s", code, stderr.String())
			}
			i := after.Issues["459"]
			saved := before.QuarantinedIssues["459"].LastValid
			if i == nil || i.Status != issuedomain.StatusAwaitingChecks || i.Generation != 7 || i.SessionID != "saved-session" || after.ActiveExecution != nil || after.QuarantinedIssues["459"] != nil {
				t.Fatalf("recovered=%+v", i)
			}
			if i.Continuation.Kind != "" || i.Continuation.RequestID != "" || i.Continuation.Stage != issuedomain.ContinuationStageChecks || i.Continuation.HeadSHA != f.head || i.Continuation.ID == saved.Continuation.ID || !reflect.DeepEqual(i.Answers, saved.Answers) || !reflect.DeepEqual(after.PendingRequests["req_original"], before.QuarantinedIssues["459"].Requests[0]) {
				t.Fatalf("evidence not preserved: %+v", i)
			}
		})
	}
}
