package github

import (
	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
	"testing"
	"time"

	"github.com/ishii1648/codex-issue-loop/internal/platform/config"
)

func TestValidateMergedPullRequestAdoptionFailsClosed(t *testing.T) {
	cfg := config.Defaults()
	cfg.GitHub.Repo = "owner/repo"
	mergedAt := time.Now().UTC()
	expected := MergedPullRequestAdoptionExpectation{
		IssueNumber: 7, PreviousStatus: issuedomain.StatusBlocked, Branch: "codex/issue-7-test",
		BaseBranch: "main", HeadSHA: "head-7",
	}
	baseline := RemoteState{
		Issue: Issue{Number: 7, State: "CLOSED", Labels: []string{"blocked"}, Comments: []string{"<!-- codex-issue-loop:failed:7 -->"}},
		PullRequests: []PullRequest{{
			Number: 11, URL: "https://example.test/pr/11", State: "MERGED", MergedAt: &mergedAt,
			HeadRefName: expected.Branch, BaseRefName: expected.BaseBranch, HeadSHA: expected.HeadSHA, MergeCommitSHA: "merge-11", HeadRepository: cfg.GitHub.Repo,
		}},
	}
	clone := func() RemoteState {
		result := baseline
		result.Issue.Labels = append([]string(nil), baseline.Issue.Labels...)
		result.Issue.Comments = append([]string(nil), baseline.Issue.Comments...)
		result.PullRequests = append([]PullRequest(nil), baseline.PullRequests...)
		return result
	}
	tests := []struct {
		name   string
		mutate func(*RemoteState)
	}{
		{name: "missing failure marker", mutate: func(remote *RemoteState) { remote.Issue.Comments = nil }},
		{name: "manual exclusion", mutate: func(remote *RemoteState) { remote.Issue.Labels = append(remote.Issue.Labels, "do-not-automate") }},
		{name: "running label", mutate: func(remote *RemoteState) { remote.Issue.Labels = append(remote.Issue.Labels, cfg.GitHub.RunningLabel) }},
		{name: "conflicting failed label", mutate: func(remote *RemoteState) { remote.Issue.Labels = append(remote.Issue.Labels, cfg.GitHub.FailedLabel) }},
		{name: "zero Pull Requests", mutate: func(remote *RemoteState) { remote.PullRequests = nil }},
		{name: "multiple Pull Requests", mutate: func(remote *RemoteState) { remote.PullRequests = append(remote.PullRequests, remote.PullRequests[0]) }},
		{name: "open Pull Request", mutate: func(remote *RemoteState) {
			remote.PullRequests[0].State = "OPEN"
			remote.PullRequests[0].MergedAt = nil
		}},
		{name: "closed without merge", mutate: func(remote *RemoteState) {
			remote.PullRequests[0].State = "CLOSED"
			remote.PullRequests[0].MergedAt = nil
		}},
		{name: "closed with forged merged time", mutate: func(remote *RemoteState) {
			remote.PullRequests[0].State = "CLOSED"
		}},
		{name: "different branch", mutate: func(remote *RemoteState) { remote.PullRequests[0].HeadRefName = "other" }},
		{name: "different head", mutate: func(remote *RemoteState) { remote.PullRequests[0].HeadSHA = "other" }},
		{name: "different base", mutate: func(remote *RemoteState) { remote.PullRequests[0].BaseRefName = "release" }},
		{name: "missing merge commit", mutate: func(remote *RemoteState) { remote.PullRequests[0].MergeCommitSHA = "" }},
		{name: "different head repository", mutate: func(remote *RemoteState) { remote.PullRequests[0].HeadRepository = "attacker/repo" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			remote := clone()
			test.mutate(&remote)
			if _, err := ValidateMergedPullRequestAdoption(cfg, remote, expected); err == nil {
				t.Fatal("unsafe merged Pull Request adoption was accepted")
			}
		})
	}
	pr, err := ValidateMergedPullRequestAdoption(cfg, baseline, expected)
	if err != nil || pr.Number != 11 {
		t.Fatalf("valid merged Pull Request was rejected: pr=%+v err=%v", pr, err)
	}
	done := clone()
	done.Issue.Labels = []string{cfg.GitHub.DoneLabel}
	expected.AllowDone = true
	expected.PullRequestURL = pr.URL
	expected.PullRequestNumber = pr.Number
	expected.MergeCommitSHA = pr.MergeCommitSHA
	if _, err := ValidateMergedPullRequestAdoption(cfg, done, expected); err != nil {
		t.Fatalf("idempotent done synchronization was rejected: %v", err)
	}
	done.Issue.Labels = append(done.Issue.Labels, cfg.GitHub.FailedLabel)
	if _, err := ValidateMergedPullRequestAdoption(cfg, done, expected); err == nil {
		t.Fatal("ambiguous done/failed synchronization was accepted")
	}
}
