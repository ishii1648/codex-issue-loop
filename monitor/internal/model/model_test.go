package model

import (
	"testing"
	"time"
)

func TestEvaluateFourStatesAndDeadlineBoundary(t *testing.T) {
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name  string
		obs   Observation
		want  Status
		start time.Time
	}{
		{name: "idle", obs: Observation{ObservedAt: now}, want: Idle, start: now},
		{name: "healthy", obs: Observation{ObservedAt: now, Items: []QueueItem{{Number: 1, Phase: Ready, PhaseSince: now.Add(-time.Minute), Deadline: now.Add(time.Minute)}}}, want: Healthy, start: now.Add(-time.Minute)},
		{name: "deadline is down", obs: Observation{ObservedAt: now, Items: []QueueItem{{Number: 1, Phase: Running, PhaseSince: now.Add(-time.Hour), Deadline: now}}}, want: Down, start: now},
		{name: "missing history", obs: Observation{ObservedAt: now, Items: []QueueItem{{Number: 1, Phase: Ready}}}, want: Unknown, start: now},
		{name: "observation failure", obs: Observation{ObservedAt: now, Error: "unavailable"}, want: Unknown, start: now},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, start, _ := Evaluate(test.obs)
			if got != test.want || !start.Equal(test.start) {
				t.Fatalf("Evaluate() = %s at %s, want %s at %s", got, start, test.want, test.start)
			}
		})
	}
}

func TestApplyStartsDownAtDeadlineAndRecoversAtProgressEvent(t *testing.T) {
	base := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	healthy := Observation{Repository: "owner/repo", ObservedAt: base.Add(time.Minute), Items: []QueueItem{{Number: 7, Phase: Ready, PhaseSince: base, Deadline: base.Add(10 * time.Minute)}}, Cursor: 10, CursorInitialized: true}
	current, closed, err := Apply(nil, healthy)
	if err != nil || closed != nil || current.Current.Status != Healthy {
		t.Fatalf("initial apply: state=%+v closed=%+v err=%v", current, closed, err)
	}
	overdue := healthy
	overdue.ObservedAt = base.Add(20 * time.Minute)
	current, closed, err = Apply(&current, overdue)
	if err != nil || current.Current.Status != Down || !current.Current.StartedAt.Equal(base.Add(10*time.Minute)) {
		t.Fatalf("deadline transition: state=%+v err=%v", current, err)
	}
	if len(closed) != 1 || closed[0].Status != Healthy || !closed[0].EndedAt.Equal(base.Add(10*time.Minute)) {
		t.Fatalf("closed healthy interval = %+v", closed)
	}
	recoveredAt := base.Add(21 * time.Minute)
	recovered := Observation{Repository: "owner/repo", ObservedAt: base.Add(22 * time.Minute), Items: []QueueItem{{Number: 7, Phase: Running, PhaseSince: recoveredAt, Deadline: recoveredAt.Add(time.Hour)}}, Events: []QueueEvent{{ID: 11, IssueNumber: 7, Kind: RunningLabeled, At: recoveredAt}}, Cursor: 11, CursorInitialized: true, ProcessingTimeout: time.Hour}
	current, closed, err = Apply(&current, recovered)
	if err != nil || current.Current.Status != Healthy || !current.Current.StartedAt.Equal(recoveredAt) {
		t.Fatalf("recovery: state=%+v err=%v", current, err)
	}
	if len(closed) != 1 || closed[0].Status != Down || !closed[0].EndedAt.Equal(recoveredAt) {
		t.Fatalf("closed down interval = %+v", closed)
	}
	if current.EventCursor != 11 || !current.EventCursorInitialized {
		t.Fatalf("event replay metadata = %+v", current)
	}
}

func TestApplyObservationFailureDoesNotBecomeHealthy(t *testing.T) {
	base := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	initial, _, err := Apply(nil, Observation{Repository: "owner/repo", ObservedAt: base})
	if err != nil {
		t.Fatal(err)
	}
	next, closed, err := Apply(&initial, Observation{Repository: "owner/repo", ObservedAt: base.Add(time.Minute), Error: "rate limited"})
	if err != nil || next.Current.Status != Unknown || len(closed) != 1 || closed[0].Status != Idle {
		t.Fatalf("observation outage: next=%+v closed=%+v err=%v", next, closed, err)
	}
}

func TestBuildReportExcludesIdleAndUnknownFromDemandAvailability(t *testing.T) {
	base := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	intervals := []Interval{
		{Status: Idle, StartedAt: base, EndedAt: base.Add(10 * time.Second)},
		{Status: Healthy, StartedAt: base.Add(10 * time.Second), EndedAt: base.Add(40 * time.Second)},
		{Status: Down, StartedAt: base.Add(40 * time.Second), EndedAt: base.Add(50 * time.Second)},
		{Status: Unknown, StartedAt: base.Add(50 * time.Second), EndedAt: base.Add(time.Minute)},
	}
	report := BuildReport("owner/repo", intervals, base, base.Add(time.Minute))
	if report.DemandAvailability == nil || *report.DemandAvailability != 0.75 {
		t.Fatalf("availability = %v", report.DemandAvailability)
	}
	if report.ObservationCoverage != float64(50)/60 {
		t.Fatalf("coverage = %v", report.ObservationCoverage)
	}
}
