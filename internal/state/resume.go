package state

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
	"github.com/ishii1648/codex-issue-loop/internal/retention"
)

const missingWorkspaceProvenanceReason = "saved workspace provenance is missing"

// The v0.6.14 writer saved ConfirmedAt before Store.Update attached the event
// envelope timestamp. Production evidence shows a 28ms gap; one second is a
// deliberately short upper bound for that single write-ahead transaction.
const maxV0614ResumeRequestEventDelay = time.Second

// InterruptedWorkspaceResumeEvidence is the durable boundary proving that an
// explicit environment resume reached the worker launch validator and stopped
// only because pre-provenance state had no saved Workspace record.
type InterruptedWorkspaceResumeEvidence struct {
	ResumeID       string
	PreviousReason string
	BaseSHA        string
	CurrentBaseSHA string
	WorktreeHead   string
	LeaseOwner     LeaseOwner
	LeaseSlot      int
	// LegacyLeaseRecovered identifies the exact v0.6.14 path that restored a
	// legacy worker-block lease without a resource park before launch failed.
	// That path must be fenced again when the workspace provenance is repaired.
	LegacyLeaseRecovered bool
}

// MayHaveInterruptedWorkspaceResumeEvidence is intentionally an exact, cheap
// filter. InterruptedWorkspaceResumeEvidence remains the authority and also
// verifies the matching event chain.
func MayHaveInterruptedWorkspaceResumeEvidence(issue *Issue) bool {
	sessionMissing := issue != nil && issue.SessionID == "" && issue.Session == nil
	sessionComplete := issue != nil && issue.SessionID != "" && issue.Session != nil && issue.Session.ID == issue.SessionID
	if issue == nil || issue.Status != issuedomain.StatusBlocked || issue.GitHubSync != issuedomain.GitHubSyncNone || issue.Workspace != nil ||
		issue.RunID == "" || issue.Worktree == "" || issue.Branch == "" || (!sessionMissing && !sessionComplete) ||
		issue.ConflictRecovery != nil || issue.PublicationRecovery != nil || issue.PullRequestChecksRecovery != nil || issue.MergedPullRequestAdoption != nil ||
		issue.WorkerPID != 0 || issue.WorkerPGID != 0 || issue.Lease == nil || issue.LeaseGeneration == 0 ||
		issue.Lease.Owner.RunID != issue.RunID || issue.Lease.Owner.Generation != issue.LeaseGeneration ||
		issue.BlockedCause == nil || issue.BlockedCause.Origin != "supervisor" || issue.BlockedCause.Kind != "worker_workspace" ||
		issue.BlockedCause.Resumable || issue.BlockedCause.BlockedAt.IsZero() || issue.FailureKind != "issue" ||
		issue.LastError != issue.BlockedCause.Reason || issue.EnvironmentResume == nil {
		return false
	}
	resume := issue.EnvironmentResume
	if resume.ID == "" || (resume.Status != issuedomain.EnvironmentResumeStatusRequested && resume.Status != issuedomain.EnvironmentResumeStatusGitHubSynced && resume.Status != issuedomain.EnvironmentResumeStatusRunning) || resume.ConfirmedAt.IsZero() ||
		resume.PreviousReason == "" || resume.BaseSHA == "" || resume.CurrentBaseSHA == "" || issue.Lease.BaseSHA != resume.BaseSHA {
		return false
	}
	expectedReason := fmt.Sprintf("worker workspace validation failed for %s: %s", issue.Worktree, missingWorkspaceProvenanceReason)
	if issue.BlockedCause.Reason != expectedReason {
		return false
	}
	if issue.ResourcePark != nil {
		if resume.Status == issuedomain.EnvironmentResumeStatusRunning {
			return false
		}
		park := issue.ResourcePark
		if park.ID == "" || park.Kind != ResourceParkKindEnvironmentBlock ||
			(park.Status != issuedomain.ResourceParkStatusResuming && park.Status != issuedomain.ResourceParkStatusResumed) || park.ResumeOwner == nil ||
			*park.ResumeOwner != issue.Lease.Owner || park.OriginalLease.Owner.RunID != issue.RunID || park.ResumedAt.IsZero() {
			return false
		}
	}
	return true
}

// InterruptedWorkspaceResumeEvidence verifies the exact v0.6.14 write-ahead
// resume and spawn-failure chain. Any missing, duplicate, reordered, or
// conflicting same-Issue event fails closed.
func (s Store) InterruptedWorkspaceResumeEvidence(issue Issue) (*InterruptedWorkspaceResumeEvidence, error) {
	if !MayHaveInterruptedWorkspaceResumeEvidence(&issue) {
		return nil, fmt.Errorf("Issue #%d is not an interrupted missing-workspace resume candidate", issue.Number)
	}
	events, err := s.legacyWorkerBlockEvents()
	if err != nil {
		return nil, err
	}
	resume := issue.EnvironmentResume
	evidence := &InterruptedWorkspaceResumeEvidence{}
	stage := 0
	for _, event := range events {
		if event.IssueNumber != issue.Number {
			continue
		}
		if event.RunID != issue.RunID {
			if stage != 0 {
				return nil, fmt.Errorf("Issue #%d interrupted resume was superseded by run %s at sequence %d", issue.Number, event.RunID, event.Sequence)
			}
			continue
		}
		if stage == 0 {
			if event.Type != "environment_resume_requested" {
				continue
			}
			var payload struct {
				ResumeID          string     `json:"resume_id"`
				PreviousReason    string     `json:"previous_reason"`
				ResourceParkID    string     `json:"resource_park_id"`
				ParkedReacquired  bool       `json:"parked_lease_reacquired"`
				BaseSHA           string     `json:"base_sha"`
				CurrentBaseSHA    string     `json:"current_base_sha"`
				LeaseOwner        LeaseOwner `json:"lease_owner"`
				LeaseSlot         int        `json:"lease_slot"`
				LegacyWorkerBlock bool       `json:"legacy_worker_block"`
				LegacyRecovered   bool       `json:"legacy_lease_recovered"`
				InterruptedResume bool       `json:"interrupted_resume"`
			}
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				return nil, fmt.Errorf("decode interrupted environment resume event at sequence %d: %w", event.Sequence, err)
			}
			if payload.ResumeID != resume.ID {
				continue
			}
			parkID := ""
			if issue.ResourcePark != nil {
				parkID = issue.ResourcePark.ID
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(event.Payload, &fields); err != nil {
				return nil, fmt.Errorf("decode interrupted environment resume fields at sequence %d: %w", event.Sequence, err)
			}
			legacyRecovered := payload.LegacyRecovered && payload.LegacyWorkerBlock && !payload.InterruptedResume &&
				parkID == "" && payload.ResourceParkID == "" && !payload.ParkedReacquired
			if legacyRecovered {
				fullHistoryErr := validateExactV0614Zeitreise442History(events, event.Sequence, issue)
				fullHistory := fullHistoryErr == nil
				if resume.Status != issuedomain.EnvironmentResumeStatusRunning || fields["lease_owner"] != nil || fields["lease_slot"] != nil ||
					issue.Lease.Slot != 0 || len(issue.Lease.ResolvedResources) != 1 || issue.Lease.ResolvedResources[0] != RepositoryResource {
					return nil, fmt.Errorf("Issue #%d v0.6.14 recovered-lease resume does not have exact current lease and legacy event provenance", issue.Number)
				}
				if !fullHistory && v0614SameIssueEventCount(events, issue) == 27 {
					return nil, fmt.Errorf("Issue #%d v0.6.14 full-27 recovery evidence: %w", issue.Number, fullHistoryErr)
				}
				if !fullHistory && !exactV0614RecoveredLeaseChain(events, event.Sequence, issue) {
					return nil, fmt.Errorf("Issue #%d v0.6.14 recovered-lease resume does not have exact current lease and legacy event provenance", issue.Number)
				}
				if fullHistory {
					evidence.WorktreeHead = exactV0614ReconciliationHead(events, issue)
				}
			} else if resume.Status == issuedomain.EnvironmentResumeStatusRunning || payload.LeaseOwner != issue.Lease.Owner || payload.LeaseSlot != issue.Lease.Slot {
				return nil, fmt.Errorf("Issue #%d interrupted environment resume lease provenance is inconsistent", issue.Number)
			}
			if evidence.ResumeID != "" || payload.PreviousReason != resume.PreviousReason || payload.BaseSHA != resume.BaseSHA ||
				payload.CurrentBaseSHA != resume.CurrentBaseSHA || payload.ResourceParkID != parkID || payload.ParkedReacquired != (parkID != "") {
				return nil, fmt.Errorf("Issue #%d interrupted environment resume event does not match current resume, SHA, park, or lease provenance", issue.Number)
			}
			evidence = &InterruptedWorkspaceResumeEvidence{
				ResumeID: payload.ResumeID, PreviousReason: payload.PreviousReason, BaseSHA: payload.BaseSHA,
				CurrentBaseSHA: payload.CurrentBaseSHA, LeaseOwner: issue.Lease.Owner, LeaseSlot: issue.Lease.Slot,
				LegacyLeaseRecovered: legacyRecovered, WorktreeHead: evidence.WorktreeHead,
			}
			stage = 1
			continue
		}

		switch stage {
		case 1:
			if evidence.LegacyLeaseRecovered {
				if event.Type != "github_state_synced" || !eventPayloadHasExactState(event.Payload, "environment_resume", "") {
					return nil, fmt.Errorf("Issue #%d v0.6.14 interrupted resume has no resume-ID-less GitHub synchronization event", issue.Number)
				}
				stage = 6
				continue
			}
			if event.Type != "github_state_synced" || !eventPayloadHasExactState(event.Payload, "environment_resume", resume.ID) {
				return nil, fmt.Errorf("Issue #%d interrupted environment resume has no exact GitHub resume synchronization event", issue.Number)
			}
			stage = 2
		case 6:
			if event.Type != "github_state_synced" || !eventPayloadHasExactState(event.Payload, "environment_resume", resume.ID) {
				return nil, fmt.Errorf("Issue #%d v0.6.14 interrupted resume has no resume-ID-bearing GitHub synchronization event", issue.Number)
			}
			stage = 2
		case 2:
			if event.Type != "worker_started" || !eventPayloadHasMode(event.Payload, "environment_block_resume") {
				return nil, fmt.Errorf("Issue #%d interrupted environment resume has no exact worker start event", issue.Number)
			}
			stage = 3
		case 3:
			if event.Type != "worker_workspace_rejected" {
				return nil, fmt.Errorf("Issue #%d interrupted environment resume did not stop at workspace validation", issue.Number)
			}
			if err := validateMissingWorkspaceRejection(event.Payload, issue); err != nil {
				return nil, err
			}
			stage = 4
		case 4:
			if event.Type != "github_state_synced" || !eventPayloadHasExactState(event.Payload, "blocked", "") {
				return nil, fmt.Errorf("Issue #%d interrupted workspace block has no exact GitHub blocked synchronization event", issue.Number)
			}
			stage = 5
		case 5:
			if event.Type != "startup_reconciled" || !eventPayloadHasReason(event.Payload, "supervisor-owned worker workspace safety block preserved") {
				return nil, fmt.Errorf("Issue #%d has unexpected durable events after the interrupted workspace block", issue.Number)
			}
		}
	}
	if stage != 5 {
		return nil, fmt.Errorf("Issue #%d interrupted missing-workspace resume evidence is incomplete", issue.Number)
	}
	return evidence, nil
}

// exactV0614RecoveredLeaseChain binds the owner/slot omitted by the affected
// request payload to the only write patterns that could have produced them.
// The long form is the sanitized, event-for-event zeitreise #442 history. The
// six-event form is retained for snapshots already accepted by v0.6.16, but it
// must not be used as a shortcut when a longer same-Issue history is present.
func exactV0614RecoveredLeaseChain(events []Event, requestSequence uint64, issue Issue) bool {
	if validateExactV0614Zeitreise442History(events, requestSequence, issue) == nil {
		return true
	}
	if issue.SessionID == "" || issue.Session == nil || issue.Session.ID != issue.SessionID {
		return false
	}
	sameIssueEvents := 0
	for _, event := range events {
		if event.IssueNumber == issue.Number && event.Type != "event_log_checkpoint" {
			sameIssueEvents++
		}
	}
	if sameIssueEvents != 12 {
		return false
	}
	prior := make([]Event, 0, 6)
	for _, event := range events {
		if event.Sequence >= requestSequence || event.IssueNumber != issue.Number || event.Type == "event_log_checkpoint" {
			continue
		}
		prior = append(prior, event)
	}
	if len(prior) < 6 {
		return false
	}
	wantTypes := []string{"lease_reserved", "worker_started", "issue_blocked", "github_state_synced", "startup_reconciled", "startup_reconciled"}
	chainStart := len(prior) - 6
	for _, event := range prior[:chainStart] {
		if event.RunID != issue.RunID {
			continue
		}
		for _, eventType := range wantTypes {
			if event.Type == eventType {
				return false
			}
		}
	}
	prior = prior[chainStart:]
	for index, event := range prior {
		if event.Type != wantTypes[index] || event.RunID != issue.RunID {
			return false
		}
	}
	var lease struct {
		Owner             LeaseOwner `json:"owner"`
		Slot              int        `json:"slot"`
		ResolvedResources []string   `json:"resolved_resources"`
		BaseSHA           string     `json:"base_sha"`
		ReservedAt        string     `json:"reserved_at"`
	}
	if json.Unmarshal(prior[0].Payload, &lease) != nil || issue.LeaseGeneration < 2 ||
		lease.Owner != (LeaseOwner{RunID: issue.RunID, Generation: issue.LeaseGeneration - 1}) ||
		lease.Slot != issue.Lease.Slot || len(lease.ResolvedResources) != 1 || lease.ResolvedResources[0] != RepositoryResource ||
		lease.BaseSHA != issue.Lease.BaseSHA || strings.TrimSpace(lease.ReservedAt) == "" {
		return false
	}
	var started struct{ Worktree, Branch string }
	if !payloadHasExactKeys(prior[1].Payload, "worktree", "branch") || json.Unmarshal(prior[1].Payload, &started) != nil ||
		started.Worktree != issue.Worktree || started.Branch != issue.Branch {
		return false
	}
	var blocked struct {
		Error       string `json:"error"`
		FailureKind string `json:"failure_kind"`
	}
	if !payloadHasExactKeys(prior[2].Payload, "error", "failure_kind") || json.Unmarshal(prior[2].Payload, &blocked) != nil ||
		blocked.FailureKind != "issue" || blocked.Error != "worker blocked: "+issue.EnvironmentResume.PreviousReason {
		return false
	}
	if !exactStatePayload(prior[3].Payload, "blocked", "") {
		return false
	}
	return payloadHasExactKeys(prior[4].Payload, "previous_status", "status", "reason") &&
		payloadHasExactKeys(prior[5].Payload, "previous_status", "status", "reason") &&
		eventPayloadHasReconciliation(prior[4].Payload, "GitHub exclusion label was applied manually") &&
		eventPayloadHasReconciliation(prior[5].Payload, legacyNormalizedReason)
}

func v0614SameIssueEventCount(events []Event, issue Issue) int {
	count := 0
	for _, event := range events {
		if event.IssueNumber == issue.Number && event.Type != "event_log_checkpoint" {
			count++
		}
	}
	return count
}

// InterruptedWorkspaceResumePredicateReport evaluates the exact legacy
// full-history predicates without changing state or events. It deliberately
// returns a report even when several predicates fail so an operator can repair
// evidence once instead of discovering one boundary per release.
func (s Store) InterruptedWorkspaceResumePredicateReport(issue Issue) (RecoveryPredicateReport, error) {
	events, err := s.legacyWorkerBlockEvents()
	if err != nil {
		return RecoveryPredicateReport{}, err
	}
	return InterruptedWorkspaceResumePredicateReportFromEvents(issue, events), nil
}

// InterruptedWorkspaceResumePredicateReportFromEvents evaluates detached
// read-only evidence, including sanitized fixture replays.
func InterruptedWorkspaceResumePredicateReportFromEvents(issue Issue, events []Event) RecoveryPredicateReport {
	requestSequence := uint64(0)
	if issue.EnvironmentResume != nil {
		for _, event := range events {
			if event.IssueNumber != issue.Number || event.RunID != issue.RunID || event.Type != "environment_resume_requested" {
				continue
			}
			var payload struct {
				ResumeID string `json:"resume_id"`
			}
			if json.Unmarshal(event.Payload, &payload) == nil && payload.ResumeID == issue.EnvironmentResume.ID {
				requestSequence = event.Sequence
			}
		}
	}
	return evaluateExactV0614Zeitreise442History(events, requestSequence, issue)
}

func recoveryStatus(ok bool) string {
	if ok {
		return "pass"
	}
	return "fail"
}

func evaluateExactV0614Zeitreise442History(events []Event, requestSequence uint64, issue Issue) RecoveryPredicateReport {
	report := newRecoveryPredicateReport("resume-blocked", issue.Number)
	history := make([]Event, 0, 27)
	for _, event := range events {
		if event.IssueNumber == issue.Number && event.Type != "event_log_checkpoint" {
			history = append(history, event)
		}
	}
	countOK := len(history) == 27
	report.add(RecoveryCodeEventCount, recoveryStatus(countOK), "durable.events", "exactly 27 same-Issue events", fmt.Sprintf("%d same-Issue events", len(history)), "operator", "restore the complete ordered event history from a reviewed backup", fmt.Sprintf("event count boundary: got %d events, want 27", len(history)))

	wantTypes := []string{
		"lease_reserved", "issue_claimed", "worker_started", "worker_process_started", "worker_preflight_completed", "retry_scheduled",
		"worker_continuation_started", "worker_process_started", "worker_preflight_completed", "retry_scheduled",
		"worker_continuation_started", "worker_process_started", "worker_preflight_completed", "issue_blocked", "github_state_synced",
		"startup_reconciled", "startup_reconciled", "startup_reconciled", "startup_reconciled", "startup_reconciled", "startup_reconciled",
		"environment_resume_requested", "github_state_synced", "github_state_synced", "worker_started", "worker_workspace_rejected", "github_state_synced",
	}
	orderOK := len(history) >= len(wantTypes)
	orderDetail := "event order boundary: history is incomplete"
	for index := 0; index < len(history) && index < len(wantTypes); index++ {
		if history[index].Type != wantTypes[index] || history[index].RunID != issue.RunID {
			orderOK = false
			orderDetail = fmt.Sprintf("event order boundary at index %d: got type=%q", index, history[index].Type)
			break
		}
	}
	if orderOK {
		orderDetail = ""
	}
	report.add(RecoveryCodeEventOrder, recoveryStatus(orderOK), "durable.events", "exact production event type/order and one run identity", map[bool]string{true: "order matches", false: "order or run identity differs"}[orderOK], "none", "do not reorder or synthesize durable events; restore reviewed evidence", orderDetail)

	sessionOK := issue.EnvironmentResume != nil && issue.Lease != nil && len(history) > 21 && history[21].Sequence == requestSequence && issue.SessionID == "" && issue.Session == nil
	report.add(RecoveryCodeSessionIdentity, recoveryStatus(sessionOK), "durable.state+events", "request sequence matches and legacy session fields are null", map[bool]string{true: "legacy null session provenance matches", false: "request sequence or session provenance differs"}[sessionOK], "none", "use only the original legacy snapshot and matching request event", "request marker boundary: sequence or null session provenance differs")

	timestampOK := issue.EnvironmentResume != nil && issue.Lease != nil && len(history) > 21 &&
		!issue.EnvironmentResume.ConfirmedAt.IsZero() && !issue.Lease.ReservedAt.IsZero() && !history[21].Timestamp.IsZero() &&
		issue.Lease.ReservedAt == issue.EnvironmentResume.ConfirmedAt
	timestampDetail := "timestamp boundary: confirmed, reservation, and request event timestamps differ"
	if timestampOK {
		delay := history[21].Timestamp.Sub(issue.EnvironmentResume.ConfirmedAt)
		timestampOK = delay >= 0 && delay <= maxV0614ResumeRequestEventDelay
		if !timestampOK {
			timestampDetail = fmt.Sprintf("timestamp boundary: request event delay %s is outside [0s,%s]", delay, maxV0614ResumeRequestEventDelay)
		}
	}
	report.add(RecoveryCodeTimestamps, recoveryStatus(timestampOK), "durable.state+events", "non-zero reservation/confirmation and request delay within one second", map[bool]string{true: "timestamp relation matches", false: "timestamp relation differs"}[timestampOK], "none", "restore the original timestamp-bearing records; do not rewrite timestamps", timestampDetail)

	leaseOK := issue.Lease != nil && len(issue.Lease.DeclaredResources) == 0
	if len(history) > 0 {
		leaseOK = leaseOK && exactOriginalLeasePayload(history[0].Payload, issue)
	} else {
		leaseOK = false
	}
	report.add(RecoveryCodeLeaseIdentity, recoveryStatus(leaseOK), "durable.state+events", "legacy generation transition and exact repository lease", map[bool]string{true: "lease identity matches", false: "lease generation, resources, or base identity differs"}[leaseOK], "none", "restore the matching state and lease reservation event", "current lease boundary: recovered lease identity differs")

	payloadOK := len(history) >= 27 && issue.EnvironmentResume != nil && issue.Lease != nil && issue.BlockedCause != nil
	requestPayloadOK := false
	if payloadOK {
		payloadOK = exactNonEmptyStringPayload(history[1].Payload, "title") && exactInitialWorkerPayload(history[2].Payload, issue)
		for _, index := range []int{3, 7, 11} {
			payloadOK = payloadOK && exactWorkerProcessPayload(history[index].Payload, issue.Worktree)
		}
		for _, index := range []int{4, 8, 12} {
			payloadOK = payloadOK && exactStringPayload(history[index].Payload, "execution_profile", "extended")
		}
		for _, index := range []int{5, 9} {
			payloadOK = payloadOK && exactRetryPayload(history[index].Payload)
		}
		payloadOK = payloadOK && exactIntegerPayload(history[6].Payload, "continuation", 1) && exactIntegerPayload(history[10].Payload, "continuation", 2)
		var blocked struct {
			Error       string `json:"error"`
			FailureKind string `json:"failure_kind"`
		}
		payloadOK = payloadOK && payloadHasExactKeys(history[13].Payload, "error", "failure_kind") && json.Unmarshal(history[13].Payload, &blocked) == nil &&
			blocked.FailureKind == "issue" && blocked.Error == "worker blocked: "+issue.EnvironmentResume.PreviousReason && exactStatePayload(history[14].Payload, "blocked", "") &&
			exactStringPayload(history[24].Payload, "mode", "environment_block_resume") &&
			exactWorkspaceRejectionPayload(history[25].Payload, issue)
		requestPayloadOK = exactLegacyResumeRequestPayload(history[21].Payload, issue)
	}
	payloadOK = payloadOK && requestPayloadOK
	payloadDetail := "payload shape boundary: one or more exact payload predicates differ"
	if !requestPayloadOK && len(history) >= 27 && issue.EnvironmentResume != nil {
		payloadDetail = "request payload boundary"
	}
	report.add(RecoveryCodePayloadShape, recoveryStatus(payloadOK), "durable.events", "exact payload keys, types, and bound values", map[bool]string{true: "payload shapes match", false: "one or more payload shapes or values differ"}[payloadOK], "none", "restore unmodified event payloads from the reviewed recovery evidence", payloadDetail)

	remoteOK := len(history) >= 27
	worktreeHead := ""
	if remoteOK {
		for index := 15; index <= 19; index++ {
			head, err := exactReconciliationPayload(history[index].Payload, "GitHub exclusion label was applied manually", issue, index >= 19)
			if err != nil || (worktreeHead != "" && head != worktreeHead) {
				remoteOK = false
			}
			if head != "" {
				worktreeHead = head
			}
		}
		lastHead, err := exactReconciliationPayload(history[20].Payload, legacyNormalizedReason, issue, true)
		remoteOK = remoteOK && err == nil && worktreeHead != "" && issue.Lease != nil && worktreeHead != issue.Lease.BaseSHA && lastHead == worktreeHead
	}
	report.add(RecoveryCodeRemoteIdentity, recoveryStatus(remoteOK), "durable.events.reconciliation", "stable dirty local-only HEAD and exact remote field evolution", map[bool]string{true: "branch/remote identity matches", false: "branch, HEAD, PR, or remote identity differs"}[remoteOK], "operator", "restore the original local-only branch boundary or abandon this recovery", "reconciliation remote boundary: branch, HEAD, PR, or remote evidence differs")

	markerOK := len(history) >= 27 && issue.EnvironmentResume != nil &&
		exactStatePayload(history[22].Payload, "environment_resume", "") &&
		exactStatePayload(history[23].Payload, "environment_resume", issue.EnvironmentResume.ID) &&
		exactStatePayload(history[26].Payload, "blocked", "")
	report.add(RecoveryCodeGitHubMarkers, recoveryStatus(markerOK), "durable.events.github_state_synced", "exact resume-ID-less, resume-ID-bearing, and blocked markers", map[bool]string{true: "marker provenance matches", false: "marker sequence or identity differs"}[markerOK], "none", "do not recreate comments manually; restore matching automation evidence", "marker boundary: exact synchronization markers differ")
	return report
}

// validateExactV0614Zeitreise442History recognizes the complete 27-event production
// history described by the fixture. It deliberately validates the events
// after the request too: adding, deleting, duplicating, reordering, or moving
// any authority/retry/reconciliation/resume event to another run must not make
// the shorter compatibility pattern authoritative. Named boundary errors are
// returned before recovery can mutate durable state.
func validateExactV0614Zeitreise442History(events []Event, requestSequence uint64, issue Issue) error {
	report := evaluateExactV0614Zeitreise442History(events, requestSequence, issue)
	if err := report.FirstFailure(); err != nil {
		return err
	}
	return nil
}

func exactOriginalLeasePayload(raw json.RawMessage, issue Issue) bool {
	var payload struct {
		Owner             LeaseOwner `json:"owner"`
		Slot              int        `json:"slot"`
		DeclaredResources []string   `json:"declared_resources"`
		ResolvedResources []string   `json:"resolved_resources"`
		BaseSHA           string     `json:"base_sha"`
		ReservedAt        string     `json:"reserved_at"`
	}
	return payloadHasExactKeys(raw, "owner", "slot", "declared_resources", "resolved_resources", "base_sha", "reserved_at") &&
		json.Unmarshal(raw, &payload) == nil && issue.LeaseGeneration == 2 &&
		payload.Owner == (LeaseOwner{RunID: issue.RunID, Generation: 1}) && payload.Slot == issue.Lease.Slot &&
		len(payload.DeclaredResources) == 1 && payload.DeclaredResources[0] == RepositoryResource &&
		len(payload.ResolvedResources) == 1 && payload.ResolvedResources[0] == RepositoryResource &&
		payload.BaseSHA == issue.Lease.BaseSHA && strings.TrimSpace(payload.ReservedAt) != ""
}

func exactInitialWorkerPayload(raw json.RawMessage, issue Issue) bool {
	var payload struct {
		Worktree string          `json:"worktree"`
		Branch   string          `json:"branch"`
		Identity json.RawMessage `json:"identity"`
	}
	if !payloadHasExactKeys(raw, "worktree", "branch", "identity") || json.Unmarshal(raw, &payload) != nil ||
		payload.Worktree != issue.Worktree || payload.Branch != issue.Branch ||
		!payloadHasExactKeys(payload.Identity, "backend", "runtime_version") {
		return false
	}
	var identity WorkerIdentity
	return json.Unmarshal(payload.Identity, &identity) == nil && identity == issue.WorkerIdentity && identity.Backend != "" && identity.RuntimeVersion != ""
}

func exactWorkerProcessPayload(raw json.RawMessage, _ string) bool {
	var payload struct {
		PID  int `json:"pid"`
		PGID int `json:"pgid"`
	}
	return payloadHasExactKeys(raw, "pid", "pgid") && json.Unmarshal(raw, &payload) == nil && payload.PID > 1 && payload.PGID == payload.PID
}

func exactRetryPayload(raw json.RawMessage) bool {
	var payload struct {
		FailureKind string `json:"failure_kind"`
		Reason      string `json:"reason"`
		RetryAt     string `json:"retry_at"`
		Delay       string `json:"delay"`
	}
	return payloadHasExactKeys(raw, "failure_kind", "reason", "retry_at", "delay") && json.Unmarshal(raw, &payload) == nil &&
		payload.FailureKind == "transient" && strings.TrimSpace(payload.Reason) != "" && strings.TrimSpace(payload.RetryAt) != "" && strings.TrimSpace(payload.Delay) != ""
}

func exactReconciliationPayload(raw json.RawMessage, reason string, issue Issue, remoteFields bool) (string, error) {
	var payload struct {
		PreviousStatus string          `json:"previous_status"`
		Status         string          `json:"status"`
		Reason         string          `json:"reason"`
		Worktree       json.RawMessage `json:"worktree"`
		PullRequests   json.RawMessage `json:"pull_requests"`
	}
	worktreeKeys := []string{"Exists", "Valid", "Branch", "Head", "Dirty", "UnpushedCommits", "LocalBranchExists", "RemoteBranchExists"}
	if remoteFields {
		worktreeKeys = append(worktreeKeys, "RemoteHead", "RemoteConsistent")
	}
	if !payloadHasExactKeys(raw, "previous_status", "status", "reason", "worktree", "pull_requests") ||
		json.Unmarshal(raw, &payload) != nil || payload.PreviousStatus != "blocked" || payload.Status != "blocked" || payload.Reason != reason ||
		!payloadHasExactKeys(payload.Worktree, worktreeKeys...) {
		return "", fmt.Errorf("payload shape or remote key placement differs")
	}
	var inspection struct {
		Exists             bool
		Valid              bool
		Branch             string
		Head               string
		RemoteHead         string
		Dirty              bool
		UnpushedCommits    bool
		LocalBranchExists  bool
		RemoteBranchExists bool
		RemoteConsistent   bool
	}
	if json.Unmarshal(payload.Worktree, &inspection) != nil || !inspection.Exists || !inspection.Valid || inspection.Branch != issue.Branch ||
		strings.TrimSpace(inspection.Head) == "" || !inspection.Dirty || inspection.UnpushedCommits || !inspection.LocalBranchExists || inspection.RemoteBranchExists {
		return "", fmt.Errorf("local-only dirty worktree values differ")
	}
	if remoteFields && (inspection.RemoteHead != "" || inspection.RemoteConsistent) {
		return "", fmt.Errorf("unpublished remote values differ")
	}
	if !bytes.Equal(bytes.TrimSpace(payload.PullRequests), []byte("null")) {
		return "", fmt.Errorf("pull_requests is not null")
	}
	return inspection.Head, nil
}

func exactV0614ReconciliationHead(events []Event, issue Issue) string {
	for _, event := range events {
		if event.IssueNumber == issue.Number && event.RunID == issue.RunID && event.Type == "startup_reconciled" {
			var payload struct {
				Worktree struct{ Head string } `json:"worktree"`
			}
			if json.Unmarshal(event.Payload, &payload) == nil {
				return payload.Worktree.Head
			}
		}
	}
	return ""
}

// InterruptedWorkspaceResumeReconciliationHead returns only the saved commit
// identity needed by a detached read-only worktree comparison. Callers must
// not print the returned value in diagnostics.
func InterruptedWorkspaceResumeReconciliationHead(events []Event, issue Issue) string {
	return exactV0614ReconciliationHead(events, issue)
}

func exactLegacyResumeRequestPayload(raw json.RawMessage, issue Issue) bool {
	if !payloadHasExactKeys(raw, "resume_id", "previous_reason", "resource_park_id", "parked_lease_reacquired", "legacy_worker_block", "legacy_lease_recovered", "interrupted_resume", "base_sha", "current_base_sha") {
		return false
	}
	var payload struct {
		ResumeID              string `json:"resume_id"`
		PreviousReason        string `json:"previous_reason"`
		ResourceParkID        string `json:"resource_park_id"`
		BaseSHA               string `json:"base_sha"`
		CurrentBaseSHA        string `json:"current_base_sha"`
		ParkedLeaseReacquired bool   `json:"parked_lease_reacquired"`
		LegacyWorkerBlock     bool   `json:"legacy_worker_block"`
		LegacyLeaseRecovered  bool   `json:"legacy_lease_recovered"`
		InterruptedResume     bool   `json:"interrupted_resume"`
	}
	return json.Unmarshal(raw, &payload) == nil && payload.ResumeID == issue.EnvironmentResume.ID && payload.PreviousReason == issue.EnvironmentResume.PreviousReason &&
		payload.ResourceParkID == "" && !payload.ParkedLeaseReacquired && payload.LegacyWorkerBlock && payload.LegacyLeaseRecovered && !payload.InterruptedResume &&
		payload.BaseSHA == issue.EnvironmentResume.BaseSHA && payload.CurrentBaseSHA == issue.EnvironmentResume.CurrentBaseSHA
}

func exactWorkspaceRejectionPayload(raw json.RawMessage, issue Issue) bool {
	var payload struct {
		ExpectedCWD string          `json:"expected_cwd"`
		Error       string          `json:"error"`
		RunID       string          `json:"run_id"`
		Validation  json.RawMessage `json:"validation"`
	}
	return payloadHasExactKeys(raw, "expected_cwd", "error", "run_id", "validation") && json.Unmarshal(raw, &payload) == nil &&
		payload.ExpectedCWD == issue.Worktree && payload.Error == issue.BlockedCause.Reason && payload.RunID == issue.RunID &&
		exactLaunchValidationPayload(payload.Validation, issue.Worktree, issue.Branch, true)
}

func exactLaunchValidationPayload(raw json.RawMessage, worktree, branch string, rejection bool) bool {
	if !payloadHasExactKeys(raw, "valid", "expected_cwd", "canonical_cwd", "top_level", "branch", "git_common_dir", "main_checkout", "checks") {
		return false
	}
	var payload struct {
		Valid        bool            `json:"valid"`
		ExpectedCWD  string          `json:"expected_cwd"`
		CanonicalCWD string          `json:"canonical_cwd"`
		TopLevel     string          `json:"top_level"`
		Branch       string          `json:"branch"`
		GitCommonDir string          `json:"git_common_dir"`
		MainCheckout string          `json:"main_checkout"`
		Checks       map[string]bool `json:"checks"`
	}
	if json.Unmarshal(raw, &payload) != nil || !payload.Valid || payload.ExpectedCWD != worktree || payload.CanonicalCWD != worktree ||
		payload.TopLevel != worktree || payload.Branch != branch || payload.GitCommonDir == "" || payload.MainCheckout == "" {
		return false
	}
	wantChecks := []string{"managed_root", "no_symlink_components", "canonical_path", "not_main_checkout", "git_top_level", "repository_identity", "saved_branch"}
	if rejection {
		wantChecks = append([]string{"run_id", "session_id", "saved_path", "saved_branch_state", "lease_owner_generation"}, wantChecks...)
	}
	if len(payload.Checks) != len(wantChecks) {
		return false
	}
	for _, check := range wantChecks {
		if !payload.Checks[check] {
			return false
		}
	}
	return true
}

func exactStatePayload(raw json.RawMessage, state, resumeID string) bool {
	keys := []string{"state"}
	if resumeID != "" {
		keys = append(keys, "resume_id")
	}
	return payloadHasExactKeys(raw, keys...) && eventPayloadHasExactState(raw, state, resumeID)
}

func exactNonEmptyStringPayload(raw json.RawMessage, key string) bool {
	var payload map[string]string
	return payloadHasExactKeys(raw, key) && json.Unmarshal(raw, &payload) == nil && strings.TrimSpace(payload[key]) != ""
}

func exactStringPayload(raw json.RawMessage, key, value string) bool {
	var payload map[string]string
	return payloadHasExactKeys(raw, key) && json.Unmarshal(raw, &payload) == nil && payload[key] == value
}

func exactIntegerPayload(raw json.RawMessage, key string, value int) bool {
	var payload map[string]int
	return payloadHasExactKeys(raw, key) && json.Unmarshal(raw, &payload) == nil && payload[key] == value
}

func payloadHasExactKeys(raw json.RawMessage, keys ...string) bool {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || len(fields) != len(keys) {
		return false
	}
	for _, key := range keys {
		if _, ok := fields[key]; !ok {
			return false
		}
	}
	return true
}

func eventPayloadHasExactState(raw json.RawMessage, state, resumeID string) bool {
	var payload struct {
		State    string `json:"state"`
		ResumeID string `json:"resume_id"`
	}
	return json.Unmarshal(raw, &payload) == nil && payload.State == state && payload.ResumeID == resumeID
}

func eventPayloadHasReconciliation(raw json.RawMessage, reason string) bool {
	var payload struct {
		PreviousStatus string `json:"previous_status"`
		Status         string `json:"status"`
		Reason         string `json:"reason"`
	}
	return json.Unmarshal(raw, &payload) == nil && payload.PreviousStatus == "blocked" && payload.Status == "blocked" && payload.Reason == reason
}

func eventPayloadHasMode(raw json.RawMessage, mode string) bool {
	var payload struct {
		Mode string `json:"mode"`
	}
	return json.Unmarshal(raw, &payload) == nil && payload.Mode == mode
}

func eventPayloadHasReason(raw json.RawMessage, reason string) bool {
	var payload struct {
		Reason string `json:"reason"`
	}
	return json.Unmarshal(raw, &payload) == nil && payload.Reason == reason
}

func validateMissingWorkspaceRejection(raw json.RawMessage, issue Issue) error {
	var payload struct {
		ExpectedCWD string `json:"expected_cwd"`
		Error       string `json:"error"`
		RunID       string `json:"run_id"`
		Validation  struct {
			Valid        bool            `json:"valid"`
			ExpectedCWD  string          `json:"expected_cwd"`
			CanonicalCWD string          `json:"canonical_cwd"`
			TopLevel     string          `json:"top_level"`
			Branch       string          `json:"branch"`
			Checks       map[string]bool `json:"checks"`
		} `json:"validation"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return fmt.Errorf("decode interrupted workspace rejection: %w", err)
	}
	if payload.ExpectedCWD != issue.Worktree || payload.Error != issue.BlockedCause.Reason || payload.RunID != issue.RunID ||
		!payload.Validation.Valid || payload.Validation.ExpectedCWD != issue.Worktree || payload.Validation.CanonicalCWD != issue.Worktree ||
		payload.Validation.TopLevel != issue.Worktree || payload.Validation.Branch != issue.Branch {
		return fmt.Errorf("Issue #%d workspace rejection does not match the saved run, worktree, branch, and missing-provenance cause", issue.Number)
	}
	for _, check := range []string{"run_id", "session_id", "saved_path", "saved_branch_state", "lease_owner_generation", "managed_root", "no_symlink_components", "canonical_path", "not_main_checkout", "git_top_level", "repository_identity", "saved_branch"} {
		if !payload.Validation.Checks[check] {
			return fmt.Errorf("Issue #%d workspace rejection lacks successful %s validation", issue.Number, check)
		}
	}
	if strings.TrimSpace(payload.Error) == "" {
		return fmt.Errorf("Issue #%d workspace rejection has no durable error", issue.Number)
	}
	return nil
}

// EnvironmentResumeBaseSHA returns the publication base recorded by the
// matching write-ahead resume event. It is the recovery source for snapshots
// written by versions that stored the base only in the lease and event payload.
func (s Store) EnvironmentResumeBaseSHA(issueNumber int, runID, resumeID string) (string, error) {
	lock, err := s.lock(false)
	if err != nil {
		return "", err
	}
	defer unlock(lock)

	finder := &environmentResumeEventFinder{repoID: s.RepoID, issueNumber: issueNumber, runID: runID, resumeID: resumeID}
	if err := retention.WriteHistory(finder, s.EventsPath()); err != nil {
		return "", fmt.Errorf("read environment resume event history: %w", err)
	}
	if err := finder.finish(); err != nil {
		return "", err
	}
	if finder.baseSHA == "" {
		return "", fmt.Errorf("environment resume %s has no durable publication base SHA", resumeID)
	}
	return finder.baseSHA, nil
}

type environmentResumeEventFinder struct {
	repoID      string
	issueNumber int
	runID       string
	resumeID    string
	pending     []byte
	baseSHA     string
}

func (f *environmentResumeEventFinder) Write(data []byte) (int, error) {
	original := len(data)
	f.pending = append(f.pending, data...)
	for {
		index := bytes.IndexByte(f.pending, '\n')
		if index < 0 {
			break
		}
		line := append([]byte(nil), f.pending[:index]...)
		f.pending = f.pending[index+1:]
		if err := f.consume(line); err != nil {
			return 0, err
		}
	}
	return original, nil
}

func (f *environmentResumeEventFinder) finish() error {
	if len(bytes.TrimSpace(f.pending)) == 0 {
		return nil
	}
	return f.consume(f.pending)
}

func (f *environmentResumeEventFinder) consume(line []byte) error {
	if len(bytes.TrimSpace(line)) == 0 {
		return nil
	}
	var event Event
	if err := json.Unmarshal(line, &event); err != nil {
		return fmt.Errorf("decode environment resume event history: %w", err)
	}
	if event.Type != "environment_resume_requested" || event.RepoID != f.repoID || event.IssueNumber != f.issueNumber || event.RunID != f.runID {
		return nil
	}
	var payload struct {
		ResumeID string `json:"resume_id"`
		BaseSHA  string `json:"base_sha"`
	}
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		return fmt.Errorf("decode environment resume event payload at sequence %d: %w", event.Sequence, err)
	}
	if payload.ResumeID != f.resumeID {
		return nil
	}
	if payload.BaseSHA == "" {
		return fmt.Errorf("environment resume event at sequence %d has an empty publication base SHA", event.Sequence)
	}
	if f.baseSHA != "" && f.baseSHA != payload.BaseSHA {
		return fmt.Errorf("environment resume %s has conflicting publication base SHAs", f.resumeID)
	}
	f.baseSHA = payload.BaseSHA
	return nil
}
