package model

import (
	"testing"
	"time"
)

func TestReplayLabelReplacementBoundaries(t *testing.T) {
	base := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	for _, terminal := range []bool{false, true} {
		for _, reversed := range []bool{false, true} {
			initialPhase, nextPhase := Ready, Running
			if terminal {
				initialPhase = Running
			}
			initial := QueueItem{Number: 1, Phase: initialPhase, PhaseSince: base, Deadline: base.Add(time.Hour)}
			previous, _, _ := Apply(nil, Observation{Repository: "owner/repo", ObservedAt: base, Items: []QueueItem{initial}, Cursor: 1})
			remove := QueueEvent{ID: 2, At: base.Add(time.Minute), Number: 1, Kind: "remove", Item: QueueItem{Phase: initialPhase}}
			change := QueueEvent{ID: 3, At: base.Add(2 * time.Minute), Number: 1, Kind: "phase", Item: QueueItem{Number: 1, Phase: nextPhase, PhaseSince: base.Add(2 * time.Minute), Deadline: base.Add(time.Hour)}}
			if terminal {
				change.Kind = "exit"
			}
			if reversed {
				remove.ID, change.ID = change.ID, remove.ID
				remove.At, change.At = change.At, remove.At
				change.Item.PhaseSince = change.At
			}
			events := []QueueEvent{remove, change}
			if reversed {
				events = []QueueEvent{change, remove}
			}
			items := []QueueItem{change.Item}
			if terminal {
				items = nil
			}
			obs := Observation{Repository: "owner/repo", ObservedAt: base.Add(3 * time.Minute), Items: items, Events: events, Cursor: 3}
			steps, err := Replay(&previous, obs)
			if err != nil {
				t.Fatal(err)
			}
			for _, step := range steps {
				next, closed, err := Apply(&previous, step)
				if err != nil {
					t.Fatal(err)
				}
				if terminal && closed != nil && closed.Status == Healthy && !closed.EndedAt.Equal(change.At) {
					t.Fatalf("terminal boundary = %+v", closed)
				}
				if !terminal && next.Current.Status != Healthy {
					t.Fatalf("false interval = %+v", next.Current)
				}
				previous = next
			}
			if terminal && (previous.Current.Status != Idle || !previous.Current.StartedAt.Equal(change.At)) {
				t.Fatalf("terminal = %+v", previous.Current)
			}
		}
	}
}

func TestReplayUnprovenExitAndReentry(t *testing.T) {
	base := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	for _, kind := range []string{"remove", "unproven"} {
		_, err := Replay(nil, Observation{Repository: "owner/repo", ObservedAt: base.Add(time.Minute), Events: []QueueEvent{{ID: 1, Number: 1, At: base, Kind: kind}}})
		if err == nil {
			t.Fatalf("%s accepted", kind)
		}
	}
}
