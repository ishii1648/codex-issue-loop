package model

import (
	"fmt"
	"strings"
	"time"
)

type CompletionEvent struct {
	ID          int64     `json:"event_id"`
	IssueNumber int       `json:"issue_number"`
	At          time.Time `json:"at"`
}

type CompletionLabelEvent struct {
	CompletionEvent
	Label string
}

type CompletionCoverage struct {
	From       time.Time `json:"from"`
	To         time.Time `json:"to"`
	FromCursor int64     `json:"from_cursor"`
	ToCursor   int64     `json:"to_cursor"`
}

type CompletionEpoch struct {
	ID        int                  `json:"id"`
	DoneLabel string               `json:"done_label"`
	From      time.Time            `json:"from"`
	To        *time.Time           `json:"to"`
	Events    []CompletionEvent    `json:"events"`
	Coverage  []CompletionCoverage `json:"coverage"`
}

type CompletionCheckpoint struct {
	EpochID int       `json:"epoch_id"`
	At      time.Time `json:"at"`
	Cursor  int64     `json:"cursor"`
}

type CompletionAttempt struct {
	At     time.Time `json:"at"`
	Result string    `json:"result"`
}

type CompletionHistory struct {
	SchemaVersion int                   `json:"schema_version"`
	Repository    string                `json:"repository"`
	Epochs        []CompletionEpoch     `json:"epochs"`
	Checkpoint    *CompletionCheckpoint `json:"checkpoint"`
	LastAttempt   CompletionAttempt     `json:"last_attempt"`
}

type CompletionObservation struct {
	At         time.Time
	Cursor     int64
	FromCursor int64
	Verified   bool
	Continuous bool
	Result     string
	Events     []CompletionLabelEvent
}

func (h *CompletionHistory) Apply(label string, observation CompletionObservation) error {
	at := observation.At.UTC()
	if at.IsZero() || at.Before(h.LastAttempt.At) {
		return fmt.Errorf("invalid completion observation time")
	}
	if observation.Result == "" {
		observation.Result = "fetch_failed"
	}
	h.LastAttempt = CompletionAttempt{At: at, Result: observation.Result}
	if !observation.Verified {
		return nil
	}
	if observation.Cursor < 0 || (h.Checkpoint != nil && (at.Before(h.Checkpoint.At) || observation.Cursor < h.Checkpoint.Cursor)) {
		return fmt.Errorf("completion checkpoint moved backwards")
	}
	label = strings.ToLower(label)
	if label == "" {
		label = "codex-loop:done"
	}
	if h.Checkpoint != nil && observation.Continuous {
		if observation.FromCursor != h.Checkpoint.Cursor {
			return fmt.Errorf("completion checkpoint does not match batch")
		}
		epoch := &h.Epochs[len(h.Epochs)-1]
		if at.After(h.Checkpoint.At) {
			coverage := CompletionCoverage{From: h.Checkpoint.At, To: at, FromCursor: h.Checkpoint.Cursor, ToCursor: observation.Cursor}
			n := len(epoch.Coverage)
			if n > 0 && epoch.Coverage[n-1].To.Equal(coverage.From) && epoch.Coverage[n-1].ToCursor == coverage.FromCursor {
				epoch.Coverage[n-1].To, epoch.Coverage[n-1].ToCursor = at, observation.Cursor
			} else {
				epoch.Coverage = append(epoch.Coverage, coverage)
			}
		}
	}
	if len(h.Epochs) == 0 || h.Epochs[len(h.Epochs)-1].DoneLabel != label {
		if len(h.Epochs) > 0 {
			h.Epochs[len(h.Epochs)-1].To = &at
		}
		h.Epochs = append(h.Epochs, CompletionEpoch{ID: len(h.Epochs) + 1, DoneLabel: label, From: at, Events: []CompletionEvent{}, Coverage: []CompletionCoverage{}})
	}
	seen := map[int64]bool{}
	for _, epoch := range h.Epochs {
		for _, event := range epoch.Events {
			seen[event.ID] = true
		}
	}
	for _, event := range observation.Events {
		if seen[event.ID] {
			continue
		}
		for i := range h.Epochs {
			epoch := &h.Epochs[i]
			if strings.EqualFold(event.Label, epoch.DoneLabel) && !event.At.Before(epoch.From) && (epoch.To == nil || event.At.Before(*epoch.To)) {
				epoch.Events = append(epoch.Events, event.CompletionEvent)
				seen[event.ID] = true
				break
			}
		}
	}
	h.Checkpoint = &CompletionCheckpoint{EpochID: h.Epochs[len(h.Epochs)-1].ID, At: at, Cursor: observation.Cursor}
	return h.Validate()
}

func (h CompletionHistory) Validate() error {
	if h.SchemaVersion != SchemaVersion || h.Repository == "" {
		return fmt.Errorf("invalid completion history identity or schema")
	}
	switch h.LastAttempt.Result {
	case "verified", "fetch_failed", "cursor_missing", "invalid_batch":
	default:
		return fmt.Errorf("invalid completion attempt")
	}
	if h.LastAttempt.At.IsZero() {
		return fmt.Errorf("missing completion attempt time")
	}
	if h.Checkpoint == nil {
		if len(h.Epochs) != 0 || h.LastAttempt.Result == "verified" || h.LastAttempt.Result == "cursor_missing" {
			return fmt.Errorf("missing completion checkpoint")
		}
		return nil
	}
	cp := h.Checkpoint
	if (h.LastAttempt.Result == "verified" || h.LastAttempt.Result == "cursor_missing") && !cp.At.Equal(h.LastAttempt.At) {
		return fmt.Errorf("completion verification does not match checkpoint")
	}
	if len(h.Epochs) == 0 || cp.EpochID != len(h.Epochs) || cp.At.IsZero() || cp.Cursor < 0 || cp.At.After(h.LastAttempt.At) {
		return fmt.Errorf("invalid completion checkpoint")
	}
	ids := map[int64]bool{}
	for i, epoch := range h.Epochs {
		end := cp.At
		if epoch.To != nil {
			end = *epoch.To
		}
		if epoch.ID != i+1 || strings.TrimSpace(epoch.DoneLabel) == "" || epoch.From.IsZero() || end.Before(epoch.From) || (i == len(h.Epochs)-1) != (epoch.To == nil) {
			return fmt.Errorf("invalid completion epoch")
		}
		if i > 0 && !h.Epochs[i-1].To.Equal(epoch.From) {
			return fmt.Errorf("noncontiguous completion epochs")
		}
		for j, coverage := range epoch.Coverage {
			if coverage.From.Before(epoch.From) || !coverage.To.After(coverage.From) || coverage.To.After(end) || coverage.FromCursor < 0 || coverage.ToCursor < coverage.FromCursor || coverage.ToCursor > cp.Cursor {
				return fmt.Errorf("invalid completion coverage")
			}
			if j > 0 && coverage.From.Before(epoch.Coverage[j-1].To) {
				return fmt.Errorf("overlapping completion coverage")
			}
		}
		for _, event := range epoch.Events {
			if event.ID <= 0 || event.ID > cp.Cursor || event.IssueNumber <= 0 || event.At.Before(epoch.From) || event.At.After(cp.At) || (epoch.To != nil && !event.At.Before(*epoch.To)) || ids[event.ID] {
				return fmt.Errorf("invalid or duplicate completion event")
			}
			ids[event.ID] = true
		}
	}
	return nil
}

type CompletionUncoveredRange struct {
	From   time.Time `json:"from"`
	To     time.Time `json:"to"`
	Reason string    `json:"reason"`
}

func (r *Report) AddCompletions(h *CompletionHistory, now time.Time, timeout time.Duration) {
	r.CompletedIssueCount = nil
	r.ObservedCompletedIssueCount = 0
	r.CompletionHistoryComplete = false
	r.CompletionLastVerifiedAt = nil
	r.CompletionUncoveredRanges = []CompletionUncoveredRange{}
	add := func(from, to time.Time, reason string) {
		if from.Before(r.From) {
			from = r.From
		}
		if to.After(r.To) {
			to = r.To
		}
		if to.After(from) {
			r.CompletionUncoveredRanges = append(r.CompletionUncoveredRanges, CompletionUncoveredRange{from, to, reason})
		}
	}
	if h == nil || h.Repository != r.Repository || h.Checkpoint == nil {
		add(r.From, r.To, "before_observation")
		return
	}
	cp := h.Checkpoint
	r.CompletionLastVerifiedAt = &cp.At
	seen := map[int]bool{}
	cursor := r.From
	addGap := func(from, to time.Time) {
		first := h.Epochs[0].From
		if from.Before(first) {
			add(from, minTime(to, first), "before_observation")
			from = first
		}
		if from.Before(cp.At) {
			add(from, minTime(to, cp.At), "history_gap")
			from = cp.At
		}
		reason := "pending"
		if timeout <= 0 || !now.Before(h.LastAttempt.At.Add(timeout)) || now.Before(cp.At) {
			reason = "stale"
		} else if h.LastAttempt.Result == "fetch_failed" || h.LastAttempt.Result == "invalid_batch" {
			reason = "fetch_failed"
		}
		add(from, to, reason)
	}
	for _, epoch := range h.Epochs {
		for _, event := range epoch.Events {
			if !event.At.Before(r.From) && event.At.Before(r.To) {
				seen[event.IssueNumber] = true
			}
		}
		for _, coverage := range epoch.Coverage {
			if !coverage.To.After(cursor) || !coverage.From.Before(r.To) {
				continue
			}
			if coverage.From.After(cursor) {
				addGap(cursor, coverage.From)
			}
			cursor = coverage.To
		}
	}
	if cursor.Before(r.To) {
		addGap(cursor, r.To)
	}
	r.ObservedCompletedIssueCount = len(seen)
	r.CompletionHistoryComplete = len(r.CompletionUncoveredRanges) == 0
	if r.CompletionHistoryComplete {
		count := len(seen)
		r.CompletedIssueCount = &count
	}
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
