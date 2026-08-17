package state

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ishii1648/codex-issue-loop/internal/retention"
)

const legacyManualExclusionError = "startup reconciliation blocked: GitHub exclusion label was applied manually"

// MayHaveLegacyWorkerBlockProvenance identifies records created before
// BlockedCause was introduced, including records whose legacy marker was
// overwritten by the old startup reconciliation behavior. It is only a hint;
// LegacyWorkerBlockProvenance is the authority for normalization.
func MayHaveLegacyWorkerBlockProvenance(issue *Issue) bool {
	if issue == nil || issue.Status != "blocked" || issue.BlockedCause != nil || issue.RunID == "" || issue.FailureKind != "issue" {
		return false
	}
	return strings.HasPrefix(issue.LastError, "issue: worker blocked: ") || issue.LastError == legacyManualExclusionError
}

// LegacyWorkerBlockProvenance reconstructs typed provenance only when durable
// history contains one unambiguous, same-run issue_blocked transaction followed
// immediately by github_state_synced(state=blocked). Any later Issue event must
// be the exact startup reconciliation that older versions used to overwrite the
// marker. Missing, reordered, cross-run, or conflicting history fails closed.
func (s Store) LegacyWorkerBlockProvenance(issue Issue) (*BlockedCause, error) {
	if !MayHaveLegacyWorkerBlockProvenance(&issue) {
		return nil, fmt.Errorf("Issue #%d is not a legacy worker block candidate", issue.Number)
	}
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
	return legacyWorkerBlockFromEvents(finder.events, issue)
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

func legacyWorkerBlockFromEvents(events []Event, issue Issue) (*BlockedCause, error) {
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
	var synchronizedAt uint64
	for index, event := range events {
		if event.Type != "issue_blocked" || event.IssueNumber != issue.Number || event.RunID != issue.RunID {
			continue
		}
		var payload blockedPayload
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return nil, fmt.Errorf("decode issue_blocked payload at sequence %d: %w", event.Sequence, err)
		}
		marker := "issue: worker blocked: "
		if payload.FailureKind != "issue" || !strings.HasPrefix(payload.Error, marker) || event.Timestamp.IsZero() ||
			(payload.BlockedOrigin != "" && payload.BlockedOrigin != "worker") ||
			(payload.BlockedKind != "" && payload.BlockedKind != "environment") {
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
		reason := strings.TrimSpace(strings.TrimPrefix(payload.Error, marker))
		if reason == "" {
			continue
		}
		if cause != nil {
			return nil, fmt.Errorf("Issue #%d has ambiguous legacy worker block event chains for run %s", issue.Number, issue.RunID)
		}
		cause = &BlockedCause{Origin: "worker", Kind: "environment", Resumable: true, Reason: reason, BlockedAt: event.Timestamp}
		synchronizedAt = events[next].Sequence
		if issue.LastError != payload.Error && issue.LastError != legacyManualExclusionError {
			return nil, fmt.Errorf("Issue #%d legacy worker block error does not match durable history", issue.Number)
		}
	}
	if cause == nil {
		return nil, fmt.Errorf("Issue #%d has no unambiguous synchronized legacy worker block event chain for run %s", issue.Number, issue.RunID)
	}
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
		if payload.PreviousStatus != "blocked" || payload.Status != "blocked" || payload.Reason != "GitHub exclusion label was applied manually" {
			return nil, fmt.Errorf("Issue #%d has conflicting reconciliation history at sequence %d", issue.Number, event.Sequence)
		}
	}
	return cause, nil
}
