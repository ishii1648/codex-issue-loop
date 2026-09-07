package app

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/ishii1648/codex-issue-loop/internal/adapter/state"
	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
)

func TestAdoptInputVerifiesRealWorkspaceAndPreservesAnswer(t *testing.T) {
	for _, scenario := range []string{"valid", "path-git-fails", "wrong-head", "published", "missing-session", "worker-alive", "wrong-generation"} {
		t.Run(scenario, func(t *testing.T) {
			f := newIssueResolutionFixture(t, 459, "OPEN", nil)
			runIssueGit(t, f.worktree, "push", "origin", "--delete", f.branch)
			f.block(t, issuedomain.StatusRunning, "environment", true, "")
			now := time.Now().UTC()
			_, err := f.store.Update("fixture", 459, f.runID, nil, func(s *state.Snapshot) error {
				i := s.Issues["459"]
				i.Status = issuedomain.StatusRunning
				i.Generation = 6
				i.WorkerPID = 987654
				i.WorkerPGID = 987654
				i.Session = &state.WorkerSession{Backend: "codex", ID: "saved-session"}
				i.SessionID = i.Session.ID
				i.Continuation.Session = i.Session
				i.Suspension.Status = issuedomain.SuspensionResolved
				i.Suspension.Resolution = issuedomain.ResolutionResume
				i.Suspension.ResolvedAt = now
				r := &state.Request{ID: "req_original", IssueNumber: 459, RunID: f.runID, CheckpointID: "checkpoint_new", ReleasedExecution: &state.ExecutionIdentity{RunID: f.runID, Generation: 6}, Status: issuedomain.RequestStatusPending, Question: "Which source?", CreatedAt: now}
				if scenario == "missing-session" {
					i.Session = nil
				}
				if scenario == "worker-alive" {
					i.WorkerPID = os.Getpid()
				}
				if scenario == "wrong-generation" {
					r.ReleasedExecution.Generation++
				}
				s.QuarantinedIssues["459"] = &state.QuarantineRecord{IssueNumber: 459, RunID: f.runID, Generation: 6, RejectedStatus: issuedomain.StatusNeedsInput, ReasonCode: "issue_invariant_violation", Reason: "checkpoint mismatch", QuarantinedAt: now, LastValid: i, Requests: []*state.Request{r}}
				delete(s.Issues, "459")
				delete(s.PendingEffects, "459")
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := f.store.RecordAnswer("req_original", "Find a dated source", now); err != nil {
				t.Fatal(err)
			}
			if scenario == "published" {
				runIssueGit(t, f.worktree, "push", "origin", f.branch)
			}
			before, err := f.store.ReadCanonicalSnapshot()
			if err != nil {
				t.Fatal(err)
			}
			expected := f.head
			if scenario == "wrong-head" {
				expected = f.base
			}
			if scenario == "path-git-fails" {
				failIssueResolutionPathGit(t)
			}
			var out, stderr bytes.Buffer
			a := App{Out: &out, Err: &stderr}
			code := a.Run(context.Background(), []string{"issue", "resolve", "--repo", f.repo, "--issue", "459", "--action", "adopt-input", "--expected-head", expected, "--json"})
			after, err := f.store.ReadCanonicalSnapshot()
			if err != nil {
				t.Fatal(err)
			}
			if scenario != "valid" && scenario != "path-git-fails" {
				if code == 0 || !reflect.DeepEqual(before, after) {
					t.Fatalf("refusal code=%d changed=%v stderr=%s", code, !reflect.DeepEqual(before, after), stderr.String())
				}
				return
			}
			if code != 0 {
				t.Fatalf("code=%d stderr=%s", code, stderr.String())
			}
			i := after.Issues["459"]
			if i == nil || i.Status != issuedomain.StatusNeedsInput || i.Generation != 6 || i.SessionID != "saved-session" || after.ActiveExecution != nil {
				t.Fatalf("unexpected recovery: %+v", i)
			}
			if r := after.PendingRequests["req_original"]; r == nil || r.Answer != "Find a dated source" {
				t.Fatal(fmt.Sprint(r))
			}
		})
	}
}
