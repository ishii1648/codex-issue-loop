package state

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ishii1648/codex-issue-loop/internal/retention"
)

const missingWorkspaceProvenanceReason = "saved workspace provenance is missing"

// InterruptedWorkspaceResumeEvidence is the durable boundary proving that an
// explicit environment resume reached the worker launch validator and stopped
// only because pre-provenance state had no saved Workspace record.
type InterruptedWorkspaceResumeEvidence struct {
	ResumeID       string
	PreviousReason string
	BaseSHA        string
	CurrentBaseSHA string
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
	if issue == nil || issue.Status != "blocked" || issue.GitHubSync != "" || issue.Workspace != nil ||
		issue.RunID == "" || issue.Worktree == "" || issue.Branch == "" || issue.SessionID == "" || issue.Session == nil || issue.Session.ID != issue.SessionID ||
		issue.ConflictRecovery != nil || issue.PublicationRecovery != nil || issue.PullRequestChecksRecovery != nil || issue.MergedPullRequestAdoption != nil ||
		issue.WorkerPID != 0 || issue.WorkerPGID != 0 || issue.Lease == nil || issue.LeaseGeneration == 0 ||
		issue.Lease.Owner.RunID != issue.RunID || issue.Lease.Owner.Generation != issue.LeaseGeneration ||
		issue.BlockedCause == nil || issue.BlockedCause.Origin != "supervisor" || issue.BlockedCause.Kind != "worker_workspace" ||
		issue.BlockedCause.Resumable || issue.BlockedCause.BlockedAt.IsZero() || issue.FailureKind != "issue" ||
		issue.LastError != issue.BlockedCause.Reason || issue.EnvironmentResume == nil {
		return false
	}
	resume := issue.EnvironmentResume
	if resume.ID == "" || (resume.Status != "requested" && resume.Status != "github_synced" && resume.Status != "running") || resume.ConfirmedAt.IsZero() ||
		resume.PreviousReason == "" || resume.BaseSHA == "" || resume.CurrentBaseSHA == "" || issue.Lease.BaseSHA != resume.BaseSHA {
		return false
	}
	expectedReason := fmt.Sprintf("worker workspace validation failed for %s: %s", issue.Worktree, missingWorkspaceProvenanceReason)
	if issue.BlockedCause.Reason != expectedReason {
		return false
	}
	if issue.ResourcePark != nil {
		if resume.Status == "running" {
			return false
		}
		park := issue.ResourcePark
		if park.ID == "" || park.Kind != ResourceParkKindEnvironmentBlock ||
			(park.Status != "resuming" && park.Status != "resumed") || park.ResumeOwner == nil ||
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
				if resume.Status != "running" || fields["lease_owner"] != nil || fields["lease_slot"] != nil ||
					issue.Lease.Slot != 0 || len(issue.Lease.ResolvedResources) != 1 || issue.Lease.ResolvedResources[0] != RepositoryResource ||
					!exactV0614RecoveredLeaseChain(events, event.Sequence, issue) {
					return nil, fmt.Errorf("Issue #%d v0.6.14 recovered-lease resume does not have exact current lease and legacy event provenance", issue.Number)
				}
			} else if resume.Status == "running" || payload.LeaseOwner != issue.Lease.Owner || payload.LeaseSlot != issue.Lease.Slot {
				return nil, fmt.Errorf("Issue #%d interrupted environment resume lease provenance is inconsistent", issue.Number)
			}
			if evidence.ResumeID != "" || payload.PreviousReason != resume.PreviousReason || payload.BaseSHA != resume.BaseSHA ||
				payload.CurrentBaseSHA != resume.CurrentBaseSHA || payload.ResourceParkID != parkID || payload.ParkedReacquired != (parkID != "") {
				return nil, fmt.Errorf("Issue #%d interrupted environment resume event does not match current resume, SHA, park, or lease provenance", issue.Number)
			}
			evidence = &InterruptedWorkspaceResumeEvidence{
				ResumeID: payload.ResumeID, PreviousReason: payload.PreviousReason, BaseSHA: payload.BaseSHA,
				CurrentBaseSHA: payload.CurrentBaseSHA, LeaseOwner: issue.Lease.Owner, LeaseSlot: issue.Lease.Slot,
				LegacyLeaseRecovered: legacyRecovered,
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
// request payload to the only write pattern that could have produced them.
// The six immediately preceding same-Issue events are intentionally exact;
// accepting any subset would turn ordinary running resumes into recovery
// authority.
func exactV0614RecoveredLeaseChain(events []Event, requestSequence uint64, issue Issue) bool {
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
	if json.Unmarshal(prior[1].Payload, &started) != nil || started.Worktree != issue.Worktree || started.Branch != issue.Branch {
		return false
	}
	var blocked struct {
		Error       string `json:"error"`
		FailureKind string `json:"failure_kind"`
	}
	if json.Unmarshal(prior[2].Payload, &blocked) != nil || blocked.FailureKind != "issue" || blocked.Error != "worker blocked: "+issue.EnvironmentResume.PreviousReason {
		return false
	}
	if !eventPayloadHasExactState(prior[3].Payload, "blocked", "") {
		return false
	}
	return eventPayloadHasReconciliation(prior[4].Payload, "GitHub exclusion label was applied manually") &&
		eventPayloadHasReconciliation(prior[5].Payload, legacyNormalizedReason)
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
