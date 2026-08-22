package state

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
	"github.com/ishii1648/codex-issue-loop/internal/publication"
)

const legacyPublisherRepositoryMismatchPrefix = "pull_request_mismatch: Pull Request refs do not match: repository= head="

// LegacyPullRequestChecksFailure reconstructs the immutable checks failure
// boundary lost by the v0.6.20 publisher-retry sequence. This compatibility
// path intentionally recognizes only the complete, ordered legacy chain.
func (s Store) LegacyPullRequestChecksFailure(issue Issue, repository, baseBranch string) (*PullRequestChecksFailure, error) {
	if issue.Status != issuedomain.StatusFailed || issue.GitHubSync != issuedomain.GitHubSyncNone || issue.FailureKind != "issue" || issue.PullRequestMerged ||
		issue.PullRequestChecksFailure != nil || issue.PullRequestChecksRecovery != nil || issue.PublicationFailure != nil ||
		issue.PublicationRecovery != nil || issue.RunID == "" || issue.Worktree == "" ||
		issue.Branch == "" || issue.PullRequestURL == "" || issue.PullRequestNumber <= 0 || issue.HeadSHA == "" ||
		issue.Lease == nil || issue.LeaseGeneration == 0 || issue.Lease.BaseSHA == "" || issue.Lease.Owner.RunID != issue.RunID ||
		issue.Lease.Owner.Generation != issue.LeaseGeneration ||
		strings.TrimSpace(repository) == "" || strings.TrimSpace(baseBranch) == "" {
		return nil, fmt.Errorf("Issue #%d is not a legacy Pull Request checks recovery candidate", issue.Number)
	}
	if !completedLegacyConflictRecovery(issue) {
		return nil, fmt.Errorf("Issue #%d does not retain the completed conflict publication boundary", issue.Number)
	}
	events, err := s.legacyWorkerBlockEvents()
	if err != nil {
		return nil, err
	}
	return legacyPullRequestChecksFailureFromEvents(events, issue, repository, baseBranch)
}

func completedLegacyConflictRecovery(issue Issue) bool {
	recovery := issue.ConflictRecovery
	if recovery == nil || recovery.PullRequestURL != issue.PullRequestURL || recovery.TargetBaseSHA == "" ||
		recovery.LastReason != "published; waiting for CI revalidation" || len(recovery.History) == 0 {
		return false
	}
	last := recovery.History[len(recovery.History)-1]
	return last.Status == issuedomain.ConflictAttemptStatusCompleted && last.BaseSHA == recovery.TargetBaseSHA && !last.FinishedAt.IsZero()
}

func legacyPullRequestChecksFailureFromEvents(events []Event, issue Issue, repository, baseBranch string) (*PullRequestChecksFailure, error) {
	history := make([]Event, 0)
	for _, event := range events {
		if event.IssueNumber == issue.Number && event.Type != "event_log_checkpoint" {
			history = append(history, event)
		}
	}
	var matches []*PullRequestChecksFailure
	for index, event := range history {
		if event.Type != "conflict_recovery_published" {
			continue
		}
		failure, err := validateLegacyChecksChain(history[index:], issue, repository, baseBranch)
		if err == nil {
			matches = append(matches, failure)
		}
	}
	if len(matches) != 1 {
		return nil, fmt.Errorf("Issue #%d has %d complete legacy Pull Request checks recovery chains, want exactly one", issue.Number, len(matches))
	}
	return matches[0], nil
}

func validateLegacyChecksChain(history []Event, issue Issue, repository, baseBranch string) (*PullRequestChecksFailure, error) {
	const fixedEvents = 16
	if len(history) < fixedEvents+1 {
		return nil, fmt.Errorf("legacy checks recovery chain is incomplete")
	}
	want := []string{
		"conflict_recovery_published", "pull_request_head_observed", "retry_scheduled",
		"worker_started", "worker_workspace_validated", "worker_process_started", "worker_preflight_completed",
		"publication_audited", "publication_failed", "publication_retry_scheduled",
		"worker_started", "worker_workspace_validated", "worker_process_started", "worker_preflight_completed",
		"issue_failed", "github_state_synced",
	}
	for index, eventType := range want {
		if history[index].Type != eventType {
			return nil, fmt.Errorf("legacy checks recovery event order differs at index %d", index)
		}
	}
	for _, event := range history[fixedEvents:] {
		if event.Type != "terminal_pull_request_reconciled" {
			return nil, fmt.Errorf("legacy checks recovery has a non-terminal event after failure at sequence %d", event.Sequence)
		}
	}
	if issue.BlockedCause != nil &&
		(issue.BlockedCause.Origin != "worker" || issue.BlockedCause.Kind != "environment" || !issue.BlockedCause.Resumable ||
			strings.TrimSpace(issue.BlockedCause.Reason) == "" || issue.BlockedCause.BlockedAt.IsZero() ||
			!issue.BlockedCause.BlockedAt.Before(history[0].Timestamp)) {
		return nil, fmt.Errorf("legacy checks recovery has an incompatible current or non-worker blocked cause")
	}

	conflictRun := history[0].RunID
	repairRun := history[3].RunID
	finalRun := history[10].RunID
	if !ValidID(conflictRun, "conflict_") || !ValidID(repairRun, "run_") || !ValidID(finalRun, "run_") ||
		conflictRun == repairRun || repairRun == finalRun || finalRun != issue.RunID {
		return nil, fmt.Errorf("legacy checks recovery run boundary is inconsistent")
	}
	for _, index := range []int{0, 1, 2} {
		if history[index].RunID != conflictRun {
			return nil, fmt.Errorf("legacy checks failure crossed its conflict run")
		}
	}
	for _, index := range []int{3, 4, 5, 6, 7, 8, 9} {
		if history[index].RunID != repairRun {
			return nil, fmt.Errorf("legacy checks repair crossed its worker run")
		}
	}
	for _, event := range history[10:] {
		if event.RunID != finalRun {
			return nil, fmt.Errorf("legacy checks terminal chain crossed its final run")
		}
	}

	var published struct {
		PullRequestURL string `json:"pull_request_url"`
		Commit         string `json:"commit"`
		TargetBaseSHA  string `json:"target_base_sha"`
	}
	if json.Unmarshal(history[0].Payload, &published) != nil || published.PullRequestURL != issue.PullRequestURL ||
		published.Commit == "" || published.Commit != issue.HeadSHA || published.TargetBaseSHA != issue.ConflictRecovery.TargetBaseSHA {
		return nil, fmt.Errorf("legacy conflict publication does not match saved Pull Request state")
	}
	if !exactStringPayload(history[1].Payload, "head_sha", published.Commit) {
		return nil, fmt.Errorf("legacy checks failure head observation is inconsistent")
	}
	var checksRetry struct {
		FailureKind string `json:"failure_kind"`
		Reason      string `json:"reason"`
		RetryAt     string `json:"retry_at"`
		Delay       string `json:"delay"`
	}
	if json.Unmarshal(history[2].Payload, &checksRetry) != nil || checksRetry.FailureKind != "transient" ||
		checksRetry.Reason != "Pull Request checks failed: "+issue.PullRequestURL || checksRetry.RetryAt == "" || checksRetry.Delay == "" ||
		history[2].Timestamp.IsZero() {
		return nil, fmt.Errorf("legacy checks retry does not identify the saved Pull Request")
	}

	repairOwner, repairAttempt, err := legacyWorkerStart(history[3])
	if err != nil || repairOwner.RunID != repairRun || repairOwner.Generation+1 != issue.Lease.Owner.Generation || repairAttempt+1 != issue.Attempts {
		return nil, fmt.Errorf("legacy checks repair lease generation is inconsistent")
	}
	finalOwner, finalAttempt, err := legacyWorkerStart(history[10])
	if err != nil || finalOwner != issue.Lease.Owner || finalAttempt != issue.Attempts {
		return nil, fmt.Errorf("legacy final retry lease generation is inconsistent")
	}
	for _, index := range []int{4, 11} {
		var payload struct {
			RunID string `json:"run_id"`
		}
		if json.Unmarshal(history[index].Payload, &payload) != nil || payload.RunID != history[index].RunID {
			return nil, fmt.Errorf("legacy worker workspace validation run is inconsistent")
		}
	}
	for _, index := range []int{5, 12} {
		var payload struct{ PID, PGID int }
		if json.Unmarshal(history[index].Payload, &payload) != nil || payload.PID <= 1 || payload.PGID != payload.PID {
			return nil, fmt.Errorf("legacy worker process boundary is inconsistent")
		}
	}
	for _, index := range []int{6, 13} {
		if !exactStringPayload(history[index].Payload, "execution_profile", "extended") {
			return nil, fmt.Errorf("legacy worker completion profile is inconsistent")
		}
	}

	var audit struct {
		BaseSHA string `json:"base_sha"`
		Reason  string `json:"reason"`
	}
	if json.Unmarshal(history[7].Payload, &audit) != nil || audit.BaseSHA != issue.Lease.BaseSHA || audit.Reason != publication.ReasonPullRequestMismatch {
		return nil, fmt.Errorf("legacy publication audit is inconsistent")
	}
	expectedReason := legacyPublisherRepositoryMismatchPrefix + issue.Branch + " base=" + baseBranch
	var failed publication.FailureProvenance
	if json.Unmarshal(history[8].Payload, &failed) != nil || failed.Origin != publication.FailureOriginPublisher ||
		failed.Phase != publication.FailurePhasePublication || failed.Code != publication.ReasonPullRequestMismatch || failed.Recoverable ||
		failed.Reason != expectedReason || failed.FailedAt.IsZero() {
		return nil, fmt.Errorf("legacy publication failure is not the v0.6.20 empty-repository decode bug")
	}
	var publicationRetry struct {
		FailureKind string `json:"failure_kind"`
		Reason      string `json:"reason"`
		RetryAt     string `json:"retry_at"`
		Delay       string `json:"delay"`
	}
	if json.Unmarshal(history[9].Payload, &publicationRetry) != nil || publicationRetry.FailureKind != "transient" ||
		publicationRetry.Reason != "publish completed work: "+expectedReason || publicationRetry.RetryAt == "" || publicationRetry.Delay == "" {
		return nil, fmt.Errorf("legacy publication retry is inconsistent")
	}
	var terminalFailure struct {
		Error       string `json:"error"`
		FailureKind string `json:"failure_kind"`
	}
	if json.Unmarshal(history[14].Payload, &terminalFailure) != nil || terminalFailure.FailureKind != "issue" ||
		!strings.HasPrefix(terminalFailure.Error, "worker retry limit reached: ") || terminalFailure.Error != issue.LastError ||
		!exactStringPayload(history[15].Payload, "state", "failed") {
		return nil, fmt.Errorf("legacy final worker retry exhaustion is inconsistent")
	}

	var terminalPR any
	for _, event := range history[fixedEvents:] {
		pr, err := validateLegacyTerminalPullRequest(event.Payload, issue, repository, baseBranch, published.Commit)
		if err != nil {
			return nil, err
		}
		if terminalPR != nil && !reflect.DeepEqual(terminalPR, pr) {
			return nil, fmt.Errorf("legacy terminal Pull Request reconciliations changed")
		}
		terminalPR = pr
	}
	return &PullRequestChecksFailure{
		Origin: ChecksFailureOriginPullRequest, Phase: ChecksFailurePhaseRequired, Code: ChecksFailureCodeRetryExhausted,
		Recoverable: true, PullRequestURL: issue.PullRequestURL, PullRequestNumber: issue.PullRequestNumber,
		Branch: issue.Branch, HeadSHA: published.Commit, ChecksStatus: "failure", RetryExhausted: true,
		FailedAt: history[2].Timestamp,
	}, nil
}

func legacyWorkerStart(event Event) (LeaseOwner, int, error) {
	var payload struct {
		Attempt    int        `json:"attempt"`
		LeaseOwner LeaseOwner `json:"lease_owner"`
	}
	if json.Unmarshal(event.Payload, &payload) != nil || payload.Attempt <= 0 || payload.LeaseOwner.Generation == 0 {
		return LeaseOwner{}, 0, fmt.Errorf("invalid legacy worker start")
	}
	return payload.LeaseOwner, payload.Attempt, nil
}

func validateLegacyTerminalPullRequest(raw json.RawMessage, issue Issue, repository, baseBranch, head string) (any, error) {
	var payload struct {
		Status       string            `json:"status"`
		Reason       string            `json:"reason"`
		PullRequests []json.RawMessage `json:"pull_requests"`
	}
	if json.Unmarshal(raw, &payload) != nil || payload.Status != "failed" || payload.Reason != "saved Pull Request is not merged" || len(payload.PullRequests) != 1 {
		return nil, fmt.Errorf("legacy terminal reconciliation cardinality or status is inconsistent")
	}
	var pr struct {
		Number         int
		URL            string
		State          string
		MergedAt       any
		HeadRefName    string
		BaseRefName    string
		ChecksStatus   string
		HeadSHA        string
		HeadRepository string
	}
	if json.Unmarshal(payload.PullRequests[0], &pr) != nil || pr.Number != issue.PullRequestNumber || pr.URL != issue.PullRequestURL ||
		!strings.EqualFold(pr.State, "open") || pr.MergedAt != nil || pr.HeadRefName != issue.Branch || pr.BaseRefName != baseBranch ||
		pr.ChecksStatus != "failure" || pr.HeadSHA != head || (pr.HeadRepository != "" && !strings.EqualFold(pr.HeadRepository, repository)) {
		return nil, fmt.Errorf("legacy terminal reconciliation Pull Request identity changed")
	}
	return pr, nil
}
