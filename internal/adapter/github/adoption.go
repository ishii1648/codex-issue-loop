package github

import (
	"fmt"
	"strings"

	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
	"github.com/ishii1648/codex-issue-loop/internal/platform/config"
)

// MergedPullRequestExpectation is the immutable boundary used while
// associating a terminal Issue with publication that happened outside the
// supervisor. Empty Pull Request identity fields are populated by the initial
// validation; non-empty fields fence every later synchronization retry.
type MergedPullRequestExpectation struct {
	IssueNumber       int
	PreviousStatus    issuedomain.Status
	Branch            string
	BaseBranch        string
	HeadSHA           string
	PullRequestURL    string
	PullRequestNumber int
	MergeCommitSHA    string
	AllowDone         bool
}

// ValidateMergedPullRequest verifies publication identity independently of the
// Issue's projected labels. It never accepts an open or ambiguous Pull Request.
func ValidateMergedPullRequest(cfg config.Config, remote RemoteState, expected MergedPullRequestExpectation) (PullRequest, error) {
	if expected.IssueNumber <= 0 || expected.Branch == "" || expected.BaseBranch == "" || expected.HeadSHA == "" {
		return PullRequest{}, fmt.Errorf("merged Pull Request adoption expectation is incomplete")
	}
	if expected.PreviousStatus != issuedomain.StatusBlocked && expected.PreviousStatus != issuedomain.StatusFailed {
		return PullRequest{}, fmt.Errorf("Issue #%d previous status %q is not an adoptable terminal state", expected.IssueNumber, expected.PreviousStatus)
	}
	if !strings.EqualFold(remote.Issue.State, "open") && !strings.EqualFold(remote.Issue.State, "closed") {
		return PullRequest{}, fmt.Errorf("Issue #%d returned unknown GitHub state %q", expected.IssueNumber, remote.Issue.State)
	}
	if len(remote.PullRequests) != 1 {
		return PullRequest{}, fmt.Errorf("Issue #%d saved branch must have exactly one Pull Request", expected.IssueNumber)
	}
	pr := remote.PullRequests[0]
	if pr.Number <= 0 || pr.URL == "" || pr.MergedAt == nil || !strings.EqualFold(pr.State, "merged") ||
		pr.HeadRefName != expected.Branch || pr.BaseRefName != expected.BaseBranch || pr.HeadSHA != expected.HeadSHA || pr.MergeCommitSHA == "" ||
		!strings.EqualFold(pr.HeadRepository, cfg.GitHub.Repo) {
		return PullRequest{}, fmt.Errorf("Issue #%d Pull Request is not the authoritative merged publication for the saved branch and head", expected.IssueNumber)
	}
	if expected.PullRequestURL != "" && pr.URL != expected.PullRequestURL {
		return PullRequest{}, fmt.Errorf("Issue #%d merged Pull Request URL changed", expected.IssueNumber)
	}
	if expected.PullRequestNumber > 0 && pr.Number != expected.PullRequestNumber {
		return PullRequest{}, fmt.Errorf("Issue #%d merged Pull Request number changed", expected.IssueNumber)
	}
	if expected.MergeCommitSHA != "" && pr.MergeCommitSHA != expected.MergeCommitSHA {
		return PullRequest{}, fmt.Errorf("Issue #%d merged Pull Request commit changed", expected.IssueNumber)
	}
	return pr, nil
}
