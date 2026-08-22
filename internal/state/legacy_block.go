package state

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"time"

	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
	"github.com/ishii1648/codex-issue-loop/internal/retention"
)

const (
	legacyManualExclusionError = "startup reconciliation blocked: GitHub exclusion label was applied manually"
	workerBlockErrorMarker     = "worker blocked: "
	legacyWorkerBlockMarker    = "issue: worker blocked: "
	legacyNormalizedReason     = "supervisor-owned worker environment block provenance preserved"
)

// LegacyWorkerBlockRecovery is the durable evidence required to restore a
// lease that an old startup reconciliation released. The recovered base is
// tied to the same run and worker_started worktree/branch as Cause.
type LegacyWorkerBlockRecovery struct {
	Cause      BlockedCause
	BaseSHA    string
	ReservedAt time.Time
}

// MayHaveLegacyWorkerBlockProvenance identifies records created before
// BlockedCause was introduced, including records whose legacy marker was
// overwritten by the old startup reconciliation behavior. It is only a hint;
// LegacyWorkerBlockProvenance is the authority for normalization.
func MayHaveLegacyWorkerBlockProvenance(issue *Issue) bool {
	if issue == nil || issue.Status != issuedomain.StatusBlocked || issue.BlockedCause != nil || issue.RunID == "" || issue.FailureKind != "issue" {
		return false
	}
	_, workerBlock := legacyWorkerBlockReason(issue.LastError)
	return workerBlock || issue.LastError == legacyManualExclusionError
}

// MayHaveLegacyWorkerBlockRecoveryProvenance also admits the exact typed form
// produced when startup reconciliation normalized a legacy record. It remains
// only a cheap hint: durable history and exact BlockedCause equality are the
// authority used by LegacyWorkerBlockRecoveryEvidence.
func MayHaveLegacyWorkerBlockRecoveryProvenance(issue *Issue) bool {
	if MayHaveLegacyWorkerBlockProvenance(issue) {
		return true
	}
	return issue != nil && issue.Status == issuedomain.StatusBlocked && issue.RunID != "" && issue.FailureKind == "issue" &&
		issue.BlockedCause != nil && issue.BlockedCause.Origin == "worker" && issue.BlockedCause.Kind == "environment" &&
		issue.BlockedCause.Resumable && issue.BlockedCause.Reason != "" && !issue.BlockedCause.BlockedAt.IsZero()
}

// LegacyWorkerBlockProvenance reconstructs typed provenance only when durable
// history contains one unambiguous, same-run issue_blocked transaction followed
// immediately by github_state_synced(state=blocked). Any later Issue event must
// be the exact old overwrite or typed normalization startup reconciliation.
// Missing, reordered, cross-run, or conflicting history fails closed.
func (s Store) LegacyWorkerBlockProvenance(issue Issue) (*BlockedCause, error) {
	if !MayHaveLegacyWorkerBlockRecoveryProvenance(&issue) {
		return nil, fmt.Errorf("Issue #%d is not a legacy worker block candidate", issue.Number)
	}
	events, err := s.legacyWorkerBlockEvents()
	if err != nil {
		return nil, err
	}
	evidence, err := legacyWorkerBlockEvidenceFromEvents(events, issue)
	if err != nil {
		return nil, err
	}
	return evidence.cause, nil
}

// LegacyWorkerBlockRecoveryEvidence proves both the legacy block and the
// original lease publication base. Missing, ambiguous, or mismatched
// lease/worktree/branch history fails closed.
func (s Store) LegacyWorkerBlockRecoveryEvidence(issue Issue) (*LegacyWorkerBlockRecovery, error) {
	if !MayHaveLegacyWorkerBlockRecoveryProvenance(&issue) {
		return nil, fmt.Errorf("Issue #%d is not a legacy worker block recovery candidate", issue.Number)
	}
	events, err := s.legacyWorkerBlockEvents()
	if err != nil {
		return nil, err
	}
	block, err := legacyWorkerBlockEvidenceFromEvents(events, issue)
	if err != nil {
		return nil, err
	}
	lease, err := legacyWorkerLeaseFromEvents(events, issue, block.blockedSequence)
	if err != nil {
		return nil, err
	}
	return &LegacyWorkerBlockRecovery{Cause: *block.cause, BaseSHA: lease.baseSHA, ReservedAt: lease.reservedAt}, nil
}

func (s Store) legacyWorkerBlockEvents() ([]Event, error) {
	lock, err := s.lock(false)
	if err != nil {
		return nil, err
	}
	defer unlock(lock)

	finder := &legacyWorkerBlockFinder{repoID: s.RepoID}
	if err := retention.WriteHistory(finder, s.EventsPath()); err != nil {
		return nil, fmt.Errorf("read legacy worker block event history: %w", err)
	}
	if err := finder.finish(); err != nil {
		return nil, err
	}
	return finder.events, nil
}

type legacyWorkerBlockFinder struct {
	repoID  string
	pending []byte
	events  []Event
}

func (f *legacyWorkerBlockFinder) Write(data []byte) (int, error) {
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

func (f *legacyWorkerBlockFinder) finish() error {
	if len(bytes.TrimSpace(f.pending)) != 0 {
		if err := f.consume(f.pending); err != nil {
			return err
		}
	}
	var last uint64
	for index, event := range f.events {
		if event.Version != CurrentVersion || event.RepoID != f.repoID || event.Sequence == 0 {
			return fmt.Errorf("invalid legacy worker block event metadata at history index %d", index)
		}
		if index > 0 {
			if event.Type == "event_log_checkpoint" && event.Sequence == last {
				continue
			}
			if event.Sequence != last+1 {
				return fmt.Errorf("non-contiguous legacy worker block event history at sequence %d", event.Sequence)
			}
		}
		last = event.Sequence
	}
	return nil
}

func (f *legacyWorkerBlockFinder) consume(line []byte) error {
	if len(bytes.TrimSpace(line)) == 0 {
		return nil
	}
	var event Event
	if err := json.Unmarshal(line, &event); err != nil {
		return fmt.Errorf("decode legacy worker block event history: %w", err)
	}
	f.events = append(f.events, event)
	return nil
}

type legacyWorkerBlockEvidence struct {
	cause           *BlockedCause
	blockedSequence uint64
}

func legacyWorkerBlockFromEvents(events []Event, issue Issue) (*BlockedCause, error) {
	evidence, err := legacyWorkerBlockEvidenceFromEvents(events, issue)
	if err != nil {
		return nil, err
	}
	return evidence.cause, nil
}

func legacyWorkerBlockEvidenceFromEvents(events []Event, issue Issue) (*legacyWorkerBlockEvidence, error) {
	type blockedPayload struct {
		Error         string `json:"error"`
		FailureKind   string `json:"failure_kind"`
		BlockedOrigin string `json:"blocked_origin"`
		BlockedKind   string `json:"blocked_kind"`
	}
	type syncPayload struct {
		State string `json:"state"`
	}

	var cause *BlockedCause
	var blockedSequence uint64
	var synchronizedAt uint64
	legacyPayload := false
	for index, event := range events {
		if event.Type != "issue_blocked" || event.IssueNumber != issue.Number || event.RunID != issue.RunID {
			continue
		}
		var payload blockedPayload
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return nil, fmt.Errorf("decode issue_blocked payload at sequence %d: %w", event.Sequence, err)
		}
		reason, workerBlock := legacyWorkerBlockReason(payload.Error)
		legacyFields := payload.BlockedOrigin == "" && payload.BlockedKind == ""
		typedFields := payload.BlockedOrigin == "worker" && payload.BlockedKind == "environment"
		if payload.FailureKind != "issue" || !workerBlock || event.Timestamp.IsZero() || (!legacyFields && !typedFields) {
			continue
		}
		next := index + 1
		for next < len(events) && events[next].Type == "event_log_checkpoint" && events[next].Sequence == event.Sequence {
			next++
		}
		if next >= len(events) || events[next].Sequence != event.Sequence+1 || events[next].Type != "github_state_synced" ||
			events[next].IssueNumber != issue.Number || events[next].RunID != issue.RunID {
			continue
		}
		var synced syncPayload
		if err := json.Unmarshal(events[next].Payload, &synced); err != nil {
			return nil, fmt.Errorf("decode github_state_synced payload at sequence %d: %w", events[next].Sequence, err)
		}
		if synced.State != "blocked" {
			continue
		}
		if cause != nil {
			return nil, fmt.Errorf("Issue #%d has ambiguous legacy worker block event chains for run %s", issue.Number, issue.RunID)
		}
		cause = &BlockedCause{Origin: "worker", Kind: "environment", Resumable: true, Reason: reason, BlockedAt: event.Timestamp}
		blockedSequence = event.Sequence
		synchronizedAt = events[next].Sequence
		legacyPayload = legacyFields
		normalizedError := workerBlockErrorMarker + reason
		if issue.LastError != payload.Error && issue.LastError != normalizedError && issue.LastError != legacyManualExclusionError {
			return nil, fmt.Errorf("Issue #%d legacy worker block error does not match durable history", issue.Number)
		}
	}
	if cause == nil {
		return nil, fmt.Errorf("Issue #%d has no unambiguous synchronized legacy worker block event chain for run %s", issue.Number, issue.RunID)
	}
	legacyReconciliation := false
	normalizedReconciliation := false
	for _, event := range events {
		if event.Sequence <= synchronizedAt || event.IssueNumber != issue.Number {
			continue
		}
		if event.RunID != issue.RunID || event.Type != "startup_reconciled" {
			return nil, fmt.Errorf("Issue #%d legacy worker block provenance was superseded at sequence %d", issue.Number, event.Sequence)
		}
		var payload struct {
			PreviousStatus string `json:"previous_status"`
			Status         string `json:"status"`
			Reason         string `json:"reason"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return nil, fmt.Errorf("decode startup_reconciled payload at sequence %d: %w", event.Sequence, err)
		}
		if payload.PreviousStatus != "blocked" || payload.Status != "blocked" {
			return nil, fmt.Errorf("Issue #%d has conflicting reconciliation history at sequence %d", issue.Number, event.Sequence)
		}
		switch payload.Reason {
		case "GitHub exclusion label was applied manually":
			if normalizedReconciliation {
				return nil, fmt.Errorf("Issue #%d legacy reconciliation follows typed normalization at sequence %d", issue.Number, event.Sequence)
			}
			legacyReconciliation = true
		case legacyNormalizedReason:
			if issue.BlockedCause == nil || normalizedReconciliation {
				return nil, fmt.Errorf("Issue #%d has conflicting typed normalization history at sequence %d", issue.Number, event.Sequence)
			}
			normalizedReconciliation = true
		default:
			return nil, fmt.Errorf("Issue #%d has conflicting reconciliation history at sequence %d", issue.Number, event.Sequence)
		}
	}
	if !legacyPayload && !legacyReconciliation {
		return nil, fmt.Errorf("Issue #%d durable worker block chain is typed, not legacy", issue.Number)
	}
	if issue.BlockedCause != nil && !reflect.DeepEqual(issue.BlockedCause, cause) {
		return nil, fmt.Errorf("Issue #%d restored blocked cause does not exactly match durable legacy history", issue.Number)
	}
	if issue.BlockedCause != nil && !normalizedReconciliation {
		return nil, fmt.Errorf("Issue #%d typed blocked cause has no durable startup normalization", issue.Number)
	}
	return &legacyWorkerBlockEvidence{cause: cause, blockedSequence: blockedSequence}, nil
}

type legacyWorkerLeaseEvidence struct {
	baseSHA    string
	reservedAt time.Time
}

func legacyWorkerLeaseFromEvents(events []Event, issue Issue, blockedSequence uint64) (*legacyWorkerLeaseEvidence, error) {
	type leasePayload struct {
		Owner             LeaseOwner `json:"owner"`
		ResolvedResources []string   `json:"resolved_resources"`
		BaseSHA           string     `json:"base_sha"`
		ReservedAt        time.Time  `json:"reserved_at"`
	}
	type workerStartedPayload struct {
		Worktree string `json:"worktree"`
		Branch   string `json:"branch"`
	}

	if issue.Worktree == "" || issue.Branch == "" || issue.LeaseGeneration == 0 {
		return nil, fmt.Errorf("Issue #%d legacy recovery lacks a saved worktree, branch, or lease generation", issue.Number)
	}
	var lease *legacyWorkerLeaseEvidence
	var leaseSequence uint64
	workerStarted := false
	for _, event := range events {
		if event.Sequence >= blockedSequence || event.IssueNumber != issue.Number || event.RunID != issue.RunID {
			continue
		}
		switch event.Type {
		case "lease_reserved":
			var payload leasePayload
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				return nil, fmt.Errorf("decode lease_reserved payload at sequence %d: %w", event.Sequence, err)
			}
			baseSHA := strings.TrimSpace(payload.BaseSHA)
			resolved, normalizeErr := normalizeResources(payload.ResolvedResources, false)
			if lease != nil || baseSHA == "" || baseSHA != payload.BaseSHA || payload.Owner.RunID != issue.RunID ||
				payload.Owner.Generation == 0 || payload.Owner.Generation != issue.LeaseGeneration || payload.ReservedAt.IsZero() ||
				normalizeErr != nil || !reflect.DeepEqual(resolved, payload.ResolvedResources) {
				return nil, fmt.Errorf("Issue #%d has invalid or ambiguous legacy lease reservation history", issue.Number)
			}
			lease = &legacyWorkerLeaseEvidence{baseSHA: baseSHA, reservedAt: payload.ReservedAt}
			leaseSequence = event.Sequence
		case "worker_started":
			var payload workerStartedPayload
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				return nil, fmt.Errorf("decode worker_started payload at sequence %d: %w", event.Sequence, err)
			}
			if payload.Worktree == "" && payload.Branch == "" {
				continue
			}
			if workerStarted || payload.Worktree != issue.Worktree || payload.Branch != issue.Branch {
				return nil, fmt.Errorf("Issue #%d worker_started history does not match the saved worktree and branch", issue.Number)
			}
			workerStarted = true
			if lease == nil || event.Sequence <= leaseSequence {
				return nil, fmt.Errorf("Issue #%d worker_started history does not follow its lease reservation", issue.Number)
			}
		}
	}
	if lease == nil || !workerStarted {
		return nil, fmt.Errorf("Issue #%d has no unambiguous same-run lease and worker_started history", issue.Number)
	}
	return lease, nil
}

// legacyWorkerBlockReason accepts the marker emitted by the worker block path
// and the exact v0.6.9 fixture representation. Keeping the allowlist here
// prevents a worker-block substring elsewhere in an error from being treated
// as durable provenance.
func legacyWorkerBlockReason(message string) (string, bool) {
	for _, marker := range []string{workerBlockErrorMarker, legacyWorkerBlockMarker} {
		if !strings.HasPrefix(message, marker) {
			continue
		}
		reason := strings.TrimSpace(strings.TrimPrefix(message, marker))
		return reason, reason != ""
	}
	return "", false
}
