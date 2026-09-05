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

// ValidateMergedPullRequest accepts only the single merged Pull
// Request for the saved branch and only a supervisor-owned terminal label (or
// the idempotent done state after a durable adoption). It never accepts an
// open Pull Request or removes manual/security exclusions.
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
	labels := make(map[string]bool, len(remote.Issue.Labels))
	for _, label := range remote.Issue.Labels {
		labels[strings.ToLower(label)] = true
	}
	hasComment := func(marker string) bool {
		for _, comment := range remote.Issue.Comments {
			if strings.Contains(comment, marker) {
				return true
			}
		}
		return false
	}
	done := labels[strings.ToLower(cfg.GitHub.DoneLabel)]
	if done && !expected.AllowDone {
		return PullRequest{}, fmt.Errorf("Issue #%d is already marked done", expected.IssueNumber)
	}
	failed := labels[strings.ToLower(cfg.GitHub.FailedLabel)]
	if (done || expected.PreviousStatus == issuedomain.StatusBlocked) && failed {
		return PullRequest{}, fmt.Errorf("Issue #%d has conflicting supervisor terminal labels", expected.IssueNumber)
	}
	for _, label := range append(append([]string{cfg.GitHub.RunningLabel, cfg.GitHub.NeedsInputLabel}, cfg.GitHub.ReadyLabels...), cfg.GitHub.ExcludeLabels...) {
		if !labels[strings.ToLower(label)] {
			continue
		}
		if !done && strings.EqualFold(label, "blocked") && expected.PreviousStatus == issuedomain.StatusBlocked &&
			hasComment(fmt.Sprintf("<!-- codex-issue-loop:failed:%d -->", expected.IssueNumber)) {
			continue
		}
		return PullRequest{}, fmt.Errorf("GitHub label %q excludes merged Pull Request adoption", label)
	}
	if !done {
		required := cfg.GitHub.FailedLabel
		if expected.PreviousStatus == issuedomain.StatusBlocked {
			required = ""
			for _, label := range cfg.GitHub.ExcludeLabels {
				if strings.EqualFold(label, "blocked") {
					required = label
					break
				}
			}
		}
		if required == "" || !labels[strings.ToLower(required)] {
			return PullRequest{}, fmt.Errorf("Issue #%d does not retain its supervisor-owned %s label", expected.IssueNumber, expected.PreviousStatus)
		}
		if !hasComment(fmt.Sprintf("<!-- codex-issue-loop:failed:%d -->", expected.IssueNumber)) {
			return PullRequest{}, fmt.Errorf("Issue #%d does not retain its supervisor failure marker", expected.IssueNumber)
		}
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
