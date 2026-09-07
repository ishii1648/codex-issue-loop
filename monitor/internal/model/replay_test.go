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

func TestRecoveryAfterObservationFailureBackfillsVerifiedHistory(t *testing.T) {
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
	if recovered.Current.Status != Healthy || !recovered.Current.StartedAt.Equal(initial.LastSuccessAt) {
		t.Fatalf("recovered = %+v", recovered.Current)
	}
	if len(closed) != 0 {
		t.Fatalf("closed = %+v", closed)
	}
}

func TestReplayLabelReplacementBoundaries(t *testing.T) {
	base := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	for _, transition := range []string{"ready to running", "running to ready", "terminal"} {
		terminal := transition == "terminal"
		for _, reversed := range []bool{false, true} {
			initialPhase, nextPhase := Ready, Running
			if terminal || transition == "running to ready" {
				initialPhase = Running
			}
			if transition == "running to ready" {
				nextPhase = Ready
			}
			initial := QueueItem{Number: 1, Phase: initialPhase, PhaseSince: base, Deadline: base.Add(time.Hour)}
			previous, _, _ := Apply(nil, Observation{Repository: "owner/repo", ObservedAt: base, Items: []QueueItem{initial}, Cursor: 1, CursorInitialized: true})
			remove := QueueEvent{ID: 2, At: base.Add(time.Minute), IssueNumber: 1, Kind: ReadyUnlabeled}
			change := QueueEvent{ID: 3, At: base.Add(2 * time.Minute), IssueNumber: 1, Kind: RunningLabeled}
			if initialPhase == Running {
				remove.Kind = RunningUnlabeled
			}
			if nextPhase == Ready {
				change.Kind = ReadyLabeled
			}
			if terminal {
				change.Kind = QueueExited
			}
			if reversed {
				remove.ID, change.ID = change.ID, remove.ID
				remove.At, change.At = change.At, remove.At
			}
			events := []QueueEvent{remove, change}
			if reversed {
				events = []QueueEvent{change, remove}
			}
			items := []QueueItem{{Number: 1, Phase: nextPhase}}
			if terminal {
				items = nil
			}
			obs := Observation{Repository: "owner/repo", ObservedAt: base.Add(3 * time.Minute), Items: items, Events: events, Cursor: 3, CursorInitialized: true, AcceptanceTimeout: time.Hour, ProcessingTimeout: time.Hour}
			next, closed, err := Apply(&previous, obs)
			if err != nil {
				t.Fatal(err)
			}
			for _, interval := range closed {
				if terminal && interval.Status == Healthy && !interval.EndedAt.Equal(change.At) {
					t.Fatalf("terminal boundary = %+v", interval)
				}
				if !terminal {
					t.Fatalf("false interval = %+v", interval)
				}
			}
			previous = next
			if terminal && (previous.Current.Status != Idle || !previous.Current.StartedAt.Equal(change.At)) {
				t.Fatalf("terminal = %+v", previous.Current)
			}
		}
	}
}

func TestReplayUnprovenExitAndReentry(t *testing.T) {
	base := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	for _, kind := range []EventKind{ReadyUnlabeled, QueueUnproven} {
		_, err := replayEvents(Snapshot{}, Observation{Repository: "owner/repo", ObservedAt: base.Add(time.Minute), Events: []QueueEvent{{ID: 1, IssueNumber: 1, At: base, Kind: kind}}, Cursor: 1})
		if err == nil {
			t.Fatalf("%s accepted", kind)
		}
	}
}

func TestVerifiedResyncPreservesUnknownAndDeadline(t *testing.T) {
	base := time.Date(2026, 9, 6, 10, 0, 0, 0, time.UTC)
	previous, _, _ := Apply(nil, Observation{Repository: "owner/repo", ObservedAt: base, Cursor: 1, CursorInitialized: true, Items: []QueueItem{{Number: 1, Phase: Ready, PhaseSince: base, Deadline: base.Add(time.Minute)}, {Number: 2, Phase: Ready, PhaseSince: base.Add(-time.Minute), Deadline: base.Add(11 * time.Minute)}}})
	obs := Observation{Repository: "owner/repo", ObservedAt: base.Add(10 * time.Minute), Cursor: 3, CursorInitialized: true, CurrentVerified: true, Resynchronized: true, Items: []QueueItem{{Number: 2, Phase: Ready, PhaseSince: base.Add(-time.Minute), Deadline: base.Add(11 * time.Minute)}}}
	next, closed, err := Apply(&previous, obs)
	if err != nil {
		t.Fatal(err)
	}
	if next.Current.Status != Down || !next.QueueDeadline.Equal(base.Add(time.Minute)) || len(closed) != 1 || closed[0].Status != Unknown || !closed[0].StartedAt.Equal(base) || !closed[0].EndedAt.Equal(obs.ObservedAt) {
		t.Fatalf("next=%+v closed=%+v", next, closed)
	}
}

func TestUnknownDemandRecoversWhenHistoryBecomesAvailable(t *testing.T) {
	base := time.Date(2026, 9, 6, 10, 0, 0, 0, time.UTC)
	previous, _, _ := Apply(nil, Observation{Repository: "owner/repo", ObservedAt: base, Cursor: 1, CursorInitialized: true, Items: []QueueItem{{Number: 1, Phase: Ready}}})
	obs := Observation{Repository: "owner/repo", ObservedAt: base.Add(time.Minute), Cursor: 1, CursorInitialized: true, CurrentVerified: true, Items: []QueueItem{{Number: 1, Phase: Ready, PhaseSince: base.Add(-time.Hour), Deadline: base.Add(-time.Minute)}}}
	next, closed, err := Apply(&previous, obs)
	if err != nil || next.Current.Status != Down || len(closed) != 1 || closed[0].Status != Unknown || !closed[0].EndedAt.Equal(obs.ObservedAt) {
		t.Fatalf("next=%+v closed=%+v err=%v", next, closed, err)
	}
}

func TestUnknownReplayPreservesIntervalWithExpiredDemand(t *testing.T) {
	base := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	for _, phase := range []Phase{Ready, Running} {
		t.Run(string(phase), func(t *testing.T) {
			old := QueueItem{Number: 239, Phase: phase, PhaseSince: base.Add(-24 * time.Hour), Deadline: base.Add(-23 * time.Hour)}
			previous, _, err := Apply(nil, Observation{Repository: "owner/repo", ObservedAt: base, Cursor: 1, CursorInitialized: true, Items: []QueueItem{old}, Error: "unavailable"})
			if err != nil {
				t.Fatal(err)
			}
			at := base.Add(-time.Minute)
			added := QueueItem{Number: 311, Phase: Ready, PhaseSince: at, Deadline: at.Add(time.Hour)}
			next, closed, err := Apply(&previous, Observation{
				Repository: previous.Repository, ObservedAt: base.Add(time.Minute), Cursor: 2, CursorInitialized: true,
				Items: []QueueItem{old, added}, AcceptanceTimeout: time.Hour, ProcessingTimeout: time.Hour,
				Events: []QueueEvent{{ID: 2, IssueNumber: added.Number, At: at, Kind: ReadyLabeled}},
			})
			if err != nil {
				t.Fatal(err)
			}
			if next.Current.Status != Down || !next.Current.StartedAt.Equal(base.Add(time.Minute)) || !next.QueueDeadline.Equal(old.Deadline) {
				t.Fatalf("next=%+v", next)
			}
			if len(closed) != 1 || closed[0].ID != previous.Current.ID || !closed[0].EndedAt.Equal(next.Current.StartedAt) {
				t.Fatalf("closed=%+v", closed)
			}
		})
	}
}

func TestReplaySamePhaseRelabel(t *testing.T) {
	base := time.Date(2026, 9, 6, 10, 0, 0, 0, time.UTC)
	for _, phase := range []Phase{Ready, Running} {
		t.Run(string(phase), func(t *testing.T) {
			remove, entry := ReadyUnlabeled, ReadyLabeled
			if phase == Running {
				remove, entry = RunningUnlabeled, RunningLabeled
			}
			previous, _, err := Apply(nil, Observation{Repository: "owner/repo", ObservedAt: base, Cursor: 1, CursorInitialized: true,
				Items: []QueueItem{{Number: 1, Phase: phase, PhaseSince: base, Deadline: base.Add(10 * time.Minute)}}})
			if err != nil {
				t.Fatal(err)
			}
			next, closed, err := Apply(&previous, Observation{Repository: previous.Repository, ObservedAt: base.Add(2 * time.Hour), Cursor: 5, CursorInitialized: true,
				Items: []QueueItem{{Number: 1, Phase: phase}}, AcceptanceTimeout: 10 * time.Minute, ProcessingTimeout: 10 * time.Minute,
				Events: []QueueEvent{
					{ID: 2, IssueNumber: 1, Kind: remove, At: base.Add(time.Minute)},
					{ID: 3, IssueNumber: 2, Kind: ReadyLabeled, At: base.Add(time.Hour)},
					{ID: 4, IssueNumber: 2, Kind: QueueExited, At: base.Add(time.Hour + time.Minute)},
					{ID: 5, IssueNumber: 1, Kind: entry, At: base.Add(2 * time.Hour)},
				}})
			if err != nil {
				t.Fatal(err)
			}
			if len(closed) != 4 {
				t.Fatalf("closed=%+v", closed)
			}
			for i, status := range []Status{Healthy, Idle, Healthy, Idle} {
				if closed[i].Status != status {
					t.Fatalf("closed=%+v", closed)
				}
			}
			if !closed[0].EndedAt.Equal(base.Add(time.Minute)) || !closed[1].StartedAt.Equal(base.Add(time.Minute)) || !closed[1].EndedAt.Equal(base.Add(time.Hour)) {
				t.Fatalf("closed=%+v", closed)
			}
			if next.Current.Status != Healthy || !next.Current.StartedAt.Equal(base.Add(2*time.Hour)) || !next.QueueDeadline.Equal(base.Add(2*time.Hour+10*time.Minute)) {
				t.Fatalf("next=%+v", next)
			}
		})
	}
}
