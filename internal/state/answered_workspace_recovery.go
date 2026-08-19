package state

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/ishii1648/codex-issue-loop/internal/worktree"
)

var answeredWorkspaceRequiredChecks = []string{
	"run_id", "session_id", "saved_path", "saved_branch_state", "lease_owner_generation",
	"managed_root", "no_symlink_components", "canonical_path", "not_main_checkout",
	"git_top_level", "repository_identity", "saved_branch",
}

// AnsweredWorkspaceRecoveryEvidence is reconstructed only from the complete
// v0.6.22 needs-input continuation chain. No field is inferred from an error
// string alone.
type AnsweredWorkspaceRecoveryEvidence struct {
	RequestID       string
	ResourceParkID  string
	OriginalOwner   LeaseOwner
	ResumeOwner     LeaseOwner
	RejectedLaunch  worktree.LaunchValidation
	VerifiedLaunch  *worktree.LaunchValidation
	RejectionReason string
}

// AnsweredWorkspaceRecoveryEvidence proves the exact legacy durable chain for
// the supplied current Issue and answered request.
func (s Store) AnsweredWorkspaceRecoveryEvidence(issue Issue, request Request) (*AnsweredWorkspaceRecoveryEvidence, error) {
	if err := validateAnsweredWorkspaceRecoveryState(issue, request); err != nil {
		return nil, err
	}
	events, err := s.legacyWorkerBlockEvents()
	if err != nil {
		return nil, err
	}
	return answeredWorkspaceRecoveryFromEvents(events, issue, request)
}

func validateAnsweredWorkspaceRecoveryState(issue Issue, request Request) error {
	if issue.Status != "blocked" || issue.GitHubSync != "" || issue.FailureKind != "issue" ||
		issue.AnsweredWorkspaceRecovery != nil || issue.EnvironmentResume != nil ||
		issue.PublicationRecovery != nil || issue.PullRequestChecksRecovery != nil || issue.ConflictRecovery != nil ||
		issue.PullRequestURL != "" || issue.PullRequestNumber != 0 || issue.PullRequestMerged ||
		issue.RunID == "" || issue.Worktree == "" || issue.Branch == "" || issue.SessionID == "" ||
		issue.Session == nil || issue.Session.ID != issue.SessionID || issue.WorkerPID != 0 || issue.WorkerPGID != 0 ||
		issue.BlockedCause == nil || issue.BlockedCause.Origin != "supervisor" || issue.BlockedCause.Kind != "worker_workspace" ||
		issue.BlockedCause.Resumable || issue.BlockedCause.Reason == "" || issue.BlockedCause.BlockedAt.IsZero() {
		return fmt.Errorf("Issue #%d is not an exact answered missing-workspace block candidate", issue.Number)
	}
	if (issue.Workspace == nil) != (issue.WorkspaceRecovery == nil) {
		return fmt.Errorf("Issue #%d has incomplete verified workspace provenance recovery", issue.Number)
	}
	if issue.WorkspaceRecovery != nil && !validAnsweredWorkspaceProvenanceState(issue) {
		return fmt.Errorf("Issue #%d verified workspace provenance recovery is inconsistent", issue.Number)
	}
	park := issue.ResourcePark
	if park == nil || park.Kind != ResourceParkKindNeedsInput || park.Status != "resumed" ||
		park.RequestID != request.ID || park.ID == "" || park.ResumeOwner == nil || park.ResumedAt.IsZero() ||
		issue.Lease == nil || issue.LeaseGeneration != issue.Lease.Owner.Generation || issue.Lease.Owner.RunID != issue.RunID ||
		park.OriginalLease.Owner.Generation != 1 || park.ResumeOwner.Generation != 2 || issue.LeaseGeneration != 2 ||
		issue.Lease.Owner != *park.ResumeOwner || issue.LeaseGeneration != park.OriginalLease.Owner.Generation+1 ||
		!issue.Lease.ReservedAt.Equal(park.ResumedAt) || !sameLeaseIdentity(park.OriginalLease, *issue.Lease) {
		return fmt.Errorf("Issue #%d does not retain the exact generation-1 to generation-2 needs-input lease chain", issue.Number)
	}
	if request.ID == "" || request.IssueNumber != issue.Number || request.Status != "answered" || strings.TrimSpace(request.Answer) == "" ||
		request.AnsweredAt == nil || request.RunID != issue.RunID || request.ResourceParkID != park.ID || request.ReleasedOwner == nil ||
		*request.ReleasedOwner != park.OriginalLease.Owner {
		return fmt.Errorf("Issue #%d answered request does not match the retained resource park", issue.Number)
	}
	if len(issue.Answers) != 1 || issue.Answers[0].RequestID != request.ID || issue.Answers[0].Question != request.Question ||
		issue.Answers[0].Answer != request.Answer || !issue.Answers[0].AnsweredAt.Equal(*request.AnsweredAt) {
		return fmt.Errorf("Issue #%d does not retain exactly one matching answer", issue.Number)
	}
	return nil
}

func sameLeaseIdentity(original, resumed ResourceLease) bool {
	return original.Slot == resumed.Slot && reflect.DeepEqual(original.DeclaredResources, resumed.DeclaredResources) &&
		reflect.DeepEqual(original.ResolvedResources, resumed.ResolvedResources) &&
		reflect.DeepEqual(original.ActualResources, resumed.ActualResources) && original.BaseSHA != "" && original.BaseSHA == resumed.BaseSHA
}

func answeredWorkspaceRecoveryFromEvents(events []Event, issue Issue, request Request) (*AnsweredWorkspaceRecoveryEvidence, error) {
	history := make([]Event, 0, 12)
	for _, event := range events {
		if event.IssueNumber == issue.Number && event.RunID == issue.RunID && event.Type != "event_log_checkpoint" {
			history = append(history, event)
		}
	}
	want := []string{
		"lease_reserved", "issue_claimed", "worker_started", "worker_process_started", "worker_preflight_completed",
		"input_requested", "github_state_synced", "answer_recorded", "worker_started",
		"worker_workspace_rejected", "github_state_synced",
	}
	if issue.WorkspaceRecovery != nil {
		want = append(want, "workspace_provenance_recovered")
	}
	if len(history) != len(want) {
		return nil, fmt.Errorf("Issue #%d has %d events in the candidate run, want the exact %d-event chain", issue.Number, len(history), len(want))
	}
	for index, eventType := range want {
		if history[index].Type != eventType || history[index].Timestamp.IsZero() ||
			(index > 0 && !history[index].Timestamp.After(history[index-1].Timestamp)) {
			return nil, fmt.Errorf("Issue #%d answered workspace recovery event order differs at index %d", issue.Number, index)
		}
	}

	var reserved struct {
		Owner             LeaseOwner `json:"owner"`
		Slot              int        `json:"slot"`
		DeclaredResources []string   `json:"declared_resources"`
		ResolvedResources []string   `json:"resolved_resources"`
		BaseSHA           string     `json:"base_sha"`
		ReservedAt        time.Time  `json:"reserved_at"`
	}
	if json.Unmarshal(history[0].Payload, &reserved) != nil || reserved.Owner != issue.ResourcePark.OriginalLease.Owner ||
		reserved.Slot != issue.ResourcePark.OriginalLease.Slot || reserved.BaseSHA != issue.ResourcePark.OriginalLease.BaseSHA ||
		!reserved.ReservedAt.Equal(issue.ResourcePark.OriginalLease.ReservedAt) ||
		!reflect.DeepEqual(reserved.DeclaredResources, issue.ResourcePark.OriginalLease.DeclaredResources) ||
		!reflect.DeepEqual(reserved.ResolvedResources, issue.ResourcePark.OriginalLease.ResolvedResources) {
		return nil, fmt.Errorf("Issue #%d initial lease reservation payload is inconsistent", issue.Number)
	}
	if !exactStringPayload(history[1].Payload, "title", issue.Title) {
		return nil, fmt.Errorf("Issue #%d claim payload is inconsistent", issue.Number)
	}
	var started struct {
		Worktree string `json:"worktree"`
		Branch   string `json:"branch"`
	}
	if json.Unmarshal(history[2].Payload, &started) != nil || started.Worktree != issue.Worktree || started.Branch != issue.Branch {
		return nil, fmt.Errorf("Issue #%d initial worker worktree payload is inconsistent", issue.Number)
	}
	var process struct {
		PID, PGID   int
		ExpectedCWD string `json:"expected_cwd"`
		ActualCWD   string `json:"actual_cwd"`
	}
	if json.Unmarshal(history[3].Payload, &process) != nil || process.PID <= 1 || process.PGID != process.PID ||
		(process.ExpectedCWD != "" && process.ExpectedCWD != issue.Worktree) || (process.ActualCWD != "" && process.ActualCWD != issue.Worktree) {
		return nil, fmt.Errorf("Issue #%d initial worker process boundary is inconsistent", issue.Number)
	}
	var preflight struct {
		ExecutionProfile string `json:"execution_profile"`
	}
	if json.Unmarshal(history[4].Payload, &preflight) != nil || preflight.ExecutionProfile == "" ||
		(issue.ExecutionProfile != "" && preflight.ExecutionProfile != issue.ExecutionProfile) {
		return nil, fmt.Errorf("Issue #%d initial worker preflight payload is inconsistent", issue.Number)
	}
	var input struct {
		Question struct {
			Text              string   `json:"text"`
			Reason            string   `json:"reason"`
			RecommendedOption string   `json:"recommended_option"`
			Options           []Option `json:"options"`
			AllowFreeText     bool     `json:"allow_free_text"`
		} `json:"question"`
		RequestID      string     `json:"request_id"`
		ResourceParkID string     `json:"resource_park_id"`
		ReleasedOwner  LeaseOwner `json:"released_owner"`
		ParkedAt       time.Time  `json:"parked_at"`
	}
	if json.Unmarshal(history[5].Payload, &input) != nil || input.RequestID != request.ID || input.ResourceParkID != issue.ResourcePark.ID ||
		input.ReleasedOwner != issue.ResourcePark.OriginalLease.Owner || !input.ParkedAt.Equal(issue.ResourcePark.ParkedAt) || input.Question.Text != request.Question || input.Question.Reason != request.Reason ||
		input.Question.RecommendedOption != request.Recommended || input.Question.AllowFreeText != request.AllowFreeText ||
		!reflect.DeepEqual(input.Question.Options, request.Options) {
		return nil, fmt.Errorf("Issue #%d needs-input transaction payload is inconsistent", issue.Number)
	}
	if !exactStringPayload(history[6].Payload, "state", "needs_input") {
		return nil, fmt.Errorf("Issue #%d needs-input GitHub synchronization is missing", issue.Number)
	}
	var answered struct {
		RequestID  string     `json:"request_id"`
		LeaseOwner LeaseOwner `json:"lease_owner"`
		LeaseSlot  int        `json:"lease_slot"`
	}
	if json.Unmarshal(history[7].Payload, &answered) != nil || answered.RequestID != request.ID ||
		answered.LeaseOwner != *issue.ResourcePark.ResumeOwner || answered.LeaseSlot != issue.Lease.Slot {
		return nil, fmt.Errorf("Issue #%d answer/reacquire transaction payload is inconsistent", issue.Number)
	}
	if !exactStringPayload(history[8].Payload, "mode", "user_answer_resume") {
		return nil, fmt.Errorf("Issue #%d answered continuation start payload is inconsistent", issue.Number)
	}
	var rejected struct {
		ExpectedCWD string                    `json:"expected_cwd"`
		Error       string                    `json:"error"`
		RunID       string                    `json:"run_id"`
		Validation  worktree.LaunchValidation `json:"validation"`
	}
	if json.Unmarshal(history[9].Payload, &rejected) != nil || rejected.ExpectedCWD != issue.Worktree || rejected.RunID != issue.RunID ||
		rejected.Error != issue.LastError || rejected.Error != issue.BlockedCause.Reason ||
		rejected.Error != fmt.Sprintf("worker workspace validation failed for %s: saved workspace provenance is missing", issue.Worktree) ||
		!legacyAnsweredLaunchMatches(rejected.Validation, issue) {
		return nil, fmt.Errorf("Issue #%d workspace rejection was not caused solely by missing saved provenance", issue.Number)
	}
	if !exactStringPayload(history[10].Payload, "state", "blocked") {
		return nil, fmt.Errorf("Issue #%d terminal blocked GitHub synchronization is missing", issue.Number)
	}
	evidence := &AnsweredWorkspaceRecoveryEvidence{
		RequestID: request.ID, ResourceParkID: issue.ResourcePark.ID,
		OriginalOwner: issue.ResourcePark.OriginalLease.Owner, ResumeOwner: *issue.ResourcePark.ResumeOwner,
		RejectedLaunch: rejected.Validation, RejectionReason: rejected.Error,
	}
	if issue.WorkspaceRecovery != nil {
		if history[11].Sequence != history[10].Sequence+1 {
			return nil, fmt.Errorf("Issue #%d verified workspace provenance event does not immediately follow the blocked chain", issue.Number)
		}
		verified, err := verifiedAnsweredWorkspaceEvent(history[11], issue)
		if err != nil {
			return nil, err
		}
		evidence.VerifiedLaunch = verified
	}
	return evidence, nil
}

func validAnsweredWorkspaceProvenanceState(issue Issue) bool {
	recovery, workspace := issue.WorkspaceRecovery, issue.Workspace
	if recovery == nil || workspace == nil || !ValidID(recovery.ID, "workspace_recovery_") ||
		recovery.Status != "verified" || !recovery.OperatorConfirmed || !recovery.OldProvenanceMissing ||
		recovery.PreviousStatus != "blocked" || recovery.RunID != issue.RunID || recovery.ConfirmedAt.IsZero() ||
		recovery.HeadSHA == "" || recovery.WorktreeSHA256 == "" || workspace.CapturedAt.IsZero() ||
		*workspace != recovery.ActualWorkspace || recovery.ExpectedWorkspace != recovery.ActualWorkspace ||
		workspace.Path != issue.Worktree || workspace.Branch != issue.Branch || workspace.RepoID == "" ||
		workspace.Repository == "" || workspace.GitCommonDir == "" || workspace.MainCheckout == "" ||
		len(recovery.ValidatorChecks) == 0 {
		return false
	}
	for _, passed := range recovery.ValidatorChecks {
		if !passed {
			return false
		}
	}
	return true
}

func verifiedAnsweredWorkspaceEvent(event Event, issue Issue) (*worktree.LaunchValidation, error) {
	var payload struct {
		RecoveryID           string                    `json:"recovery_id"`
		OperatorConfirmation map[string]bool           `json:"operator_confirmation"`
		MutationScope        []string                  `json:"mutation_scope"`
		PreviousStatus       string                    `json:"previous_status"`
		OldProvenanceMissing bool                      `json:"old_provenance_missing"`
		HeadSHA              string                    `json:"head_sha"`
		WorktreeSHA256       string                    `json:"worktree_sha256"`
		PullRequestURL       string                    `json:"pull_request_url"`
		ExpectedWorkspace    WorkerWorkspace           `json:"expected_workspace"`
		ActualWorkspace      WorkerWorkspace           `json:"actual_workspace"`
		Validator            worktree.LaunchValidation `json:"validator"`
	}
	recovery := issue.WorkspaceRecovery
	if json.Unmarshal(event.Payload, &payload) != nil || recovery == nil || issue.Workspace == nil ||
		payload.RecoveryID != recovery.ID || !payload.OperatorConfirmation["confirm_verified_workspace"] || len(payload.OperatorConfirmation) != 1 ||
		!reflect.DeepEqual(payload.MutationScope, []string{"issues[].workspace", "issues[].workspace_provenance_recovery", "events.jsonl"}) ||
		payload.PreviousStatus != issue.Status || !payload.OldProvenanceMissing || payload.HeadSHA != recovery.HeadSHA ||
		payload.WorktreeSHA256 != recovery.WorktreeSHA256 || payload.PullRequestURL != issue.PullRequestURL ||
		payload.ExpectedWorkspace != recovery.ExpectedWorkspace || payload.ActualWorkspace != recovery.ActualWorkspace ||
		payload.ActualWorkspace != *issue.Workspace || event.RepoID != issue.Workspace.RepoID ||
		!payload.Validator.Valid || payload.Validator.ExpectedCWD != issue.Worktree ||
		payload.Validator.CanonicalCWD != issue.Workspace.Path || payload.Validator.TopLevel != issue.Workspace.Path ||
		payload.Validator.Branch != issue.Workspace.Branch || payload.Validator.CommonDir != issue.Workspace.GitCommonDir ||
		payload.Validator.MainCheckout != issue.Workspace.MainCheckout ||
		!reflect.DeepEqual(payload.Validator.Checks, recovery.ValidatorChecks) {
		return nil, fmt.Errorf("Issue #%d verified workspace provenance event is inconsistent", issue.Number)
	}
	for _, passed := range payload.Validator.Checks {
		if !passed {
			return nil, fmt.Errorf("Issue #%d verified workspace provenance validator did not pass", issue.Number)
		}
	}
	return &payload.Validator, nil
}

func legacyAnsweredLaunchMatches(validation worktree.LaunchValidation, issue Issue) bool {
	if !validation.Valid || validation.ExpectedCWD != issue.Worktree || validation.CanonicalCWD != issue.Worktree ||
		validation.TopLevel != issue.Worktree || validation.Branch != issue.Branch || validation.CommonDir == "" || validation.MainCheckout == "" ||
		validation.Checks["saved_provenance"] {
		return false
	}
	for _, check := range answeredWorkspaceRequiredChecks {
		if !validation.Checks[check] {
			return false
		}
	}
	for check, passed := range validation.Checks {
		if !passed || check == "saved_provenance" {
			return false
		}
	}
	return true
}
