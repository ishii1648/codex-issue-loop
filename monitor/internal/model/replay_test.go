package model

import (
	"testing"
	"time"
)

func TestEvaluateUsesRunningDeadlineWhileOldReadyWaits(t *testing.T) {
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	observation := Observation{ObservedAt: now, Items: []QueueItem{
		{Number: 1, Phase: Ready, PhaseSince: now.Add(-time.Hour), Deadline: now.Add(-50 * time.Minute)},
		{Number: 2, Phase: Running, PhaseSince: now.Add(-time.Minute), Deadline: now.Add(time.Hour)},
	}}
	status, startedAt, _ := Evaluate(observation)
	if status != Healthy || !startedAt.Equal(now.Add(-time.Minute)) {
		t.Fatalf("Evaluate() = %s at %s", status, startedAt)
	}
}

func TestReplayRestoresDeadlineRecoveryAndTerminalIntervalsInOnePoll(t *testing.T) {
	base := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	initial, _, err := Apply(nil, Observation{
		Repository: "owner/repo", ObservedAt: base.Add(time.Minute),
		Items:  []QueueItem{{Number: 1, Phase: Ready, PhaseSince: base, Deadline: base.Add(10 * time.Minute)}},
		Cursor: 100, CursorInitialized: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	observation := Observation{
		Repository: "owner/repo", ObservedAt: base.Add(40 * time.Minute),
		Events: []QueueEvent{
			{ID: 101, IssueNumber: 1, Kind: RunningLabeled, At: base.Add(20 * time.Minute)},
			{ID: 102, IssueNumber: 1, Kind: QueueExited, At: base.Add(30 * time.Minute)},
		},
		Cursor: 102, CursorInitialized: true,
		AcceptanceTimeout: 10 * time.Minute, ProcessingTimeout: time.Hour,
	}
	next, closed, err := Apply(&initial, observation)
	if err != nil {
		t.Fatal(err)
	}
	if next.Current.Status != Idle || !next.Current.StartedAt.Equal(base.Add(30*time.Minute)) {
		t.Fatalf("current = %+v", next.Current)
	}
	if len(closed) != 3 || closed[0].Status != Healthy || closed[1].Status != Down || closed[2].Status != Healthy {
		t.Fatalf("closed intervals = %+v", closed)
	}
	if !closed[0].EndedAt.Equal(base.Add(10*time.Minute)) || !closed[1].EndedAt.Equal(base.Add(20*time.Minute)) || !closed[2].EndedAt.Equal(base.Add(30*time.Minute)) {
		t.Fatalf("closed boundaries = %+v", closed)
	}
	again, duplicate, err := Apply(&next, observation)
	if err != nil || len(duplicate) != 0 || again.Current.ID != next.Current.ID {
		t.Fatalf("duplicate replay: next=%+v closed=%+v err=%v", again, duplicate, err)
	}
}

func TestRunningTerminalStartsNextAdmissionWindowAtEvent(t *testing.T) {
	base := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	initial, _, err := Apply(nil, Observation{
		Repository: "owner/repo", ObservedAt: base.Add(6 * time.Minute), Cursor: 200, CursorInitialized: true,
		Items: []QueueItem{
			{Number: 1, Phase: Running, PhaseSince: base.Add(5 * time.Minute), Deadline: base.Add(65 * time.Minute)},
			{Number: 2, Phase: Ready, PhaseSince: base, Deadline: base.Add(10 * time.Minute)},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	terminalAt := base.Add(20 * time.Minute)
	next, _, err := Apply(&initial, Observation{
		Repository: "owner/repo", ObservedAt: base.Add(25 * time.Minute), Cursor: 201, CursorInitialized: true,
		Items:             []QueueItem{{Number: 2, Phase: Ready}},
		Events:            []QueueEvent{{ID: 201, IssueNumber: 1, Kind: QueueExited, At: terminalAt}},
		AcceptanceTimeout: 10 * time.Minute, ProcessingTimeout: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	if next.Current.Status != Healthy || next.QueuePhase != Ready || !next.QueuePhaseSince.Equal(terminalAt) || !next.QueueDeadline.Equal(terminalAt.Add(10*time.Minute)) {
		t.Fatalf("next admission window = %+v", next)
	}
}

func TestRecoveryAfterObservationFailureDoesNotMoveIntervalsBackwards(t *testing.T) {
	base := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	initial, _, err := Apply(nil, Observation{
		Repository: "owner/repo", ObservedAt: base.Add(time.Minute), Cursor: 10, CursorInitialized: true,
		Items: []QueueItem{{Number: 1, Phase: Ready, PhaseSince: base, Deadline: base.Add(10 * time.Minute)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	failed, _, err := Apply(&initial, Observation{Repository: "owner/repo", ObservedAt: base.Add(5 * time.Minute), Error: "unavailable"})
	if err != nil {
		t.Fatal(err)
	}
	recovered, closed, err := Apply(&failed, Observation{
		Repository: "owner/repo", ObservedAt: base.Add(8 * time.Minute), Cursor: 11, CursorInitialized: true,
		Items:             []QueueItem{{Number: 1, Phase: Running}},
		Events:            []QueueEvent{{ID: 11, IssueNumber: 1, Kind: RunningLabeled, At: base.Add(4 * time.Minute)}},
		ProcessingTimeout: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Current.Status != Healthy || !recovered.Current.StartedAt.Equal(base.Add(8*time.Minute)) {
		t.Fatalf("recovered = %+v", recovered.Current)
	}
	if len(closed) != 1 || closed[0].Status != Unknown || !closed[0].EndedAt.Equal(base.Add(8*time.Minute)) {
		t.Fatalf("closed = %+v", closed)
	}
}
