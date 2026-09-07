package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ishii1648/codex-issue-loop/internal/adapter/state"
	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
	"github.com/ishii1648/codex-issue-loop/internal/domain/publication"
)

func TestRetryPublicationRepairsCheckpointWithRemoteHead(t *testing.T) {
	for _, scenario := range []string{"valid", "diverged", "dirty", "wrong-result", "fork", "wrong-head"} {
		t.Run(scenario, func(t *testing.T) {
			f := newIssueResolutionFixture(t, 459, "OPEN", nil)
			url := "https://example.test/pull/493"
			f.block(t, issuedomain.StatusRunning, "publication failed", false, url)
			tree := runIssueGit(t, f.worktree, "rev-parse", "HEAD^{tree}")
			published := runIssueGit(t, f.worktree, "commit-tree", tree, "-p", f.head, "-m", "remote update")
			runIssueGit(t, f.worktree, "push", "origin", published+":refs/heads/"+f.branch)
			result := []byte(`{"version":1,"status":"completed","execution_profile":"extended","summary":"verified repair","question":null,"tests":[{"command":"test","result":"passed"}],"git":null,"retry":null}`)
			dir := filepath.Join(f.store.Dir, "runs", f.runID)
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "result-1.json"), result, 0600); err != nil {
				t.Fatal(err)
			}
			_, err := f.store.Update("fixture", 459, f.runID, nil, func(s *state.Snapshot) error {
				i := s.Issues["459"]
				i.Status = issuedomain.StatusFailed
				i.HeadSHA = published
				i.PullRequestNumber = 493
				i.PublicationAudit = &publication.Audit{Reason: publication.ReasonPullRequestMismatch}
				c := i.Continuation
				c.Stage = issuedomain.ContinuationStagePublish
				c.HeadSHA = published
				c.PullRequestNumber = 493
				c.ResultSHA256 = fmt.Sprintf("%x", sha256.Sum256(result))
				c.Summary = "verified repair"
				if scenario == "wrong-result" {
					c.ResultSHA256 = strings.Repeat("a", 64)
				}
				i.Suspension.Origin = "runtime"
				i.Suspension.AllowedActions = []issuedomain.ResolutionAction{issuedomain.ResolutionCancel, issuedomain.ResolutionRetryStage}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			pr := map[string]any{"number": 493, "url": url, "state": "OPEN", "headRefName": f.branch, "baseRefName": "main", "headRefOid": published, "headRepository": map[string]any{"name": "repo"}, "headRepositoryOwner": map[string]any{"login": "owner"}, "statusCheckRollup": []any{}}
			if scenario == "fork" {
				pr["headRepositoryOwner"] = map[string]any{"login": "other"}
			}
			if scenario == "wrong-head" {
				pr["headRefOid"] = f.base
			}
			f.rewritePullRequests(t, []map[string]any{pr})
			script, err := os.ReadFile(f.ghPath)
			if err != nil {
				t.Fatal(err)
			}
			status := "ahead"
			if scenario == "diverged" {
				status = "diverged"
			}
			reply := fmt.Sprintf(`  "api repos/owner/repo/compare/%s...%s") printf '%s';;`, f.head, published, fmt.Sprintf(`{"status":%q,"base_commit":{"sha":%q},"merge_base_commit":{"sha":%q}}`, status, f.head, f.head))
			if err := os.WriteFile(f.ghPath, []byte(strings.Replace(string(script), "  api*)", reply+"\n  api*)", 1)), 0700); err != nil {
				t.Fatal(err)
			}
			if scenario == "dirty" {
				if err := os.WriteFile(filepath.Join(f.worktree, "dirty.txt"), []byte("preserve"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			before, err := f.store.ReadCanonicalSnapshot()
			if err != nil {
				t.Fatal(err)
			}
			assertRecoveryGitHubOutsideStateLock(t, f)
			var out, stderr bytes.Buffer
			code := (App{Out: &out, Err: &stderr}).Run(context.Background(), []string{"issue", "resolve", "--repo", f.repo, "--issue", "459", "--action", "retry-stage", "--json"})
			after, err := f.store.ReadCanonicalSnapshot()
			if err != nil {
				t.Fatal(err)
			}
			if scenario != "valid" {
				if code == 0 || !reflect.DeepEqual(before, after) {
					t.Fatalf("code=%d changed=%v stderr=%s", code, !reflect.DeepEqual(before, after), stderr.String())
				}
				return
			}
			if code != 0 {
				t.Fatalf("code=%d stderr=%s", code, stderr.String())
			}
			i := after.Issues["459"]
			if i.Status != issuedomain.StatusResumePending || i.Continuation.HeadSHA != f.head || i.HeadSHA != published || i.Continuation.ResultSHA256 != before.Issues["459"].Continuation.ResultSHA256 || i.Worktree != f.worktree || i.RunID != f.runID {
				t.Fatalf("repair lost evidence: %+v", i)
			}
			if runIssueGit(t, f.worktree, "rev-parse", "HEAD") != f.head {
				t.Fatal("recovery mutated worktree HEAD")
			}
		})
	}
}
