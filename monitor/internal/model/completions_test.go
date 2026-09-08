package model

import (
	"reflect"
	"testing"
	"time"
)

func TestCompletionEpochsAndDistinctHalfOpenCounts(t *testing.T) {
	base := time.Date(2026, 9, 7, 15, 0, 0, 0, time.UTC)
	h := CompletionHistory{SchemaVersion: 1, Repository: "owner/repo"}
	event := func(id int64, issue int, minutes int, label string) CompletionLabelEvent {
		return CompletionLabelEvent{CompletionEvent: CompletionEvent{ID: id, IssueNumber: issue, At: base.Add(time.Duration(minutes) * time.Minute)}, Label: label}
	}
	apply := func(minute int, cursor int64, label string, events ...CompletionLabelEvent) {
		t.Helper()
		observation := CompletionObservation{At: base.Add(time.Duration(minute) * time.Minute), Cursor: cursor, Verified: true, Result: "verified", Events: events}
		if h.Checkpoint != nil {
			observation.FromCursor = h.Checkpoint.Cursor
			observation.Continuous = true
		}
		if err := h.Apply(label, observation); err != nil {
			t.Fatal(err)
		}
	}
	apply(0, 1, "a")
	apply(10, 5, "b", event(2, 1, 0, "a"), event(3, 1, 5, "a"), event(4, 2, 9, "a"), event(5, 3, 10, "b"))
	apply(20, 8, "a", event(6, 1, 15, "b"), event(7, 4, 16, "a"), event(8, 5, 20, "a"))
	apply(30, 9, "a", event(9, 6, 25, "a"))
	if len(h.Epochs) != 3 || h.Epochs[0].DoneLabel != "a" || h.Epochs[1].DoneLabel != "b" || h.Epochs[2].DoneLabel != "a" {
		t.Fatalf("epochs=%+v", h.Epochs)
	}
	for _, tc := range []struct{ from, to, count int }{{0, 10, 2}, {10, 20, 2}, {20, 30, 2}, {0, 30, 5}, {1, 5, 0}, {5, 6, 1}} {
		report := BuildReport(h.Repository, nil, base.Add(time.Duration(tc.from)*time.Minute), base.Add(time.Duration(tc.to)*time.Minute))
		report.AddCompletions(&h, base.Add(time.Hour), 3*time.Minute)
		if !report.CompletionHistoryComplete || report.CompletedIssueCount == nil || *report.CompletedIssueCount != tc.count {
			t.Fatalf("range=%+v report=%+v", tc, report)
		}
	}
	before := h.Epochs[2].Coverage[0]
	apply(30, 9, "a", event(9, 6, 25, "a"))
	if len(h.Epochs[2].Events) != 2 || h.Epochs[2].Coverage[0] != before {
		t.Fatalf("repeated batch=%+v", h)
	}
	other := BuildReport("owner/other", nil, base, base.Add(time.Hour))
	other.AddCompletions(nil, base, 3*time.Minute)
	if other.CompletedIssueCount != nil || other.ObservedCompletedIssueCount != 0 {
		t.Fatal(other)
	}
}

func TestCompletionRecoveryAndUncoveredReasons(t *testing.T) {
	base := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)
	h := CompletionHistory{SchemaVersion: 1, Repository: "owner/repo"}
	apply := func(minute int, cursor int64, verified, continuous bool, result string) {
		t.Helper()
		observation := CompletionObservation{At: base.Add(time.Duration(minute) * time.Minute), Cursor: cursor, Verified: verified, Continuous: continuous, Result: result}
		if h.Checkpoint != nil {
			observation.FromCursor = h.Checkpoint.Cursor
		}
		if err := h.Apply("done", observation); err != nil {
			t.Fatal(err)
		}
	}
	reasons := func(from, to, now time.Time) []string {
		t.Helper()
		report := BuildReport(h.Repository, nil, from, to)
		report.AddCompletions(&h, now, 3*time.Minute)
		var result []string
		for _, gap := range report.CompletionUncoveredRanges {
			result = append(result, gap.Reason)
		}
		if len(result) > 0 && (report.CompletedIssueCount != nil || report.CompletionHistoryComplete) {
			t.Fatal(report)
		}
		return result
	}
	apply(0, 10, true, false, "verified")
	apply(1, 11, true, true, "verified")
	apply(2, 0, false, false, "fetch_failed")
	if h.Checkpoint.Cursor != 11 {
		t.Fatal(h)
	}
	if got := reasons(base, base.Add(3*time.Minute), base.Add(2*time.Minute)); !reflect.DeepEqual(got, []string{"fetch_failed"}) {
		t.Fatal(got)
	}
	apply(4, 12, true, true, "verified")
	if got := reasons(base, base.Add(4*time.Minute), base.Add(4*time.Minute)); len(got) != 0 {
		t.Fatal(got)
	}
	apply(6, 20, true, false, "cursor_missing")
	apply(7, 21, true, true, "verified")
	got := reasons(base.Add(-time.Minute), base.Add(8*time.Minute), base.Add(8*time.Minute))
	if !reflect.DeepEqual(got, []string{"before_observation", "history_gap", "pending"}) {
		t.Fatal(got)
	}
	got = reasons(base.Add(-time.Minute), base.Add(8*time.Minute), base.Add(10*time.Minute))
	if !reflect.DeepEqual(got, []string{"before_observation", "history_gap", "stale"}) {
		t.Fatal(got)
	}
	if len(h.Epochs[0].Coverage) != 2 || !h.Epochs[0].Coverage[1].From.Equal(base.Add(6*time.Minute)) {
		t.Fatal(h)
	}
}

func TestCompletionLabelChangeDoesNotBridgeMissingEpoch(t *testing.T) {
	base := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)
	h := CompletionHistory{SchemaVersion: 1, Repository: "owner/repo"}
	for i, label := range []string{"a", "b", "a", "a"} {
		observation := CompletionObservation{At: base.Add(time.Duration(i) * time.Hour), Cursor: int64(i + 1), FromCursor: int64(i), Verified: true, Continuous: i == 1 || i == 3, Result: "verified"}
		if i == 2 {
			observation.Result = "cursor_missing"
		}
		if err := h.Apply(label, observation); err != nil {
			t.Fatal(err)
		}
	}
	report := BuildReport(h.Repository, nil, base, base.Add(3*time.Hour))
	report.AddCompletions(&h, base.Add(3*time.Hour), time.Minute)
	if report.CompletionHistoryComplete || len(report.CompletionUncoveredRanges) != 1 || !report.CompletionUncoveredRanges[0].From.Equal(base.Add(time.Hour)) || !report.CompletionUncoveredRanges[0].To.Equal(base.Add(2*time.Hour)) {
		t.Fatal(report)
	}
}

func TestCompletionBootstrapFailureAndInvalidCheckpoint(t *testing.T) {
	at := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)
	h := CompletionHistory{SchemaVersion: 1, Repository: "owner/repo"}
	if err := h.Apply("", CompletionObservation{At: at, Result: "fetch_failed"}); err != nil {
		t.Fatal(err)
	}
	report := BuildReport(h.Repository, nil, at.Add(-time.Hour), at)
	report.AddCompletions(&h, at, time.Minute)
	if report.CompletedIssueCount != nil || report.CompletionHistoryComplete || report.CompletionUncoveredRanges[0].Reason != "before_observation" {
		t.Fatal(report)
	}
	if err := h.Apply("", CompletionObservation{At: at, Cursor: 10, Verified: true, Result: "verified"}); err != nil {
		t.Fatal(err)
	}
	if h.Epochs[0].DoneLabel != "codex-loop:done" {
		t.Fatal(h)
	}
	if err := h.Apply("", CompletionObservation{At: at.Add(time.Minute), Cursor: 12, FromCursor: 9, Verified: true, Continuous: true, Result: "verified"}); err == nil {
		t.Fatal("wrong checkpoint accepted")
	}
}
