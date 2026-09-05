package store

import (
	"testing"
	"time"

	"github.com/ishii1648/codex-issue-loop/monitor/internal/model"
)

func TestReplayAndRestartKeepIntervalsNonOverlappingAndIdempotent(t *testing.T) {
	storage := Store{Root: t.TempDir()}
	base := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	observation := model.Observation{Repository: "owner/repo", ObservedAt: base, Items: []model.QueueItem{{Number: 1, Phase: model.Ready, PhaseSince: base, Deadline: base.Add(time.Minute)}}, EventIDs: []int64{101}, Cursor: 101}
	current, closed, err := model.Apply(nil, observation)
	if err != nil || closed != nil {
		t.Fatal(err)
	}
	if err := storage.Commit(current, closed); err != nil {
		t.Fatal(err)
	}
	restarted, err := storage.Load("owner/repo")
	if err != nil {
		t.Fatal(err)
	}
	overdue := observation
	overdue.ObservedAt = base.Add(2 * time.Minute)
	next, closed, err := model.Apply(restarted, overdue)
	if err != nil || closed == nil {
		t.Fatal(err)
	}
	if err := storage.Commit(next, closed); err != nil {
		t.Fatal(err)
	}
	if err := storage.Commit(next, closed); err != nil {
		t.Fatal(err)
	}
	history, err := storage.History("owner/repo")
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 1 || !history[0].EndedAt.Equal(base.Add(time.Minute)) {
		t.Fatalf("history after replay = %+v", history)
	}
	loaded, err := storage.Load("owner/repo")
	if err != nil || loaded.Current.Status != model.Down || len(loaded.SeenEventIDs) != 1 {
		t.Fatalf("restarted snapshot = %+v err=%v", loaded, err)
	}
	all, err := storage.AllIntervals("owner/repo")
	if err != nil || len(all) != 2 || !all[0].EndedAt.Equal(all[1].StartedAt) {
		t.Fatalf("all intervals = %+v err=%v", all, err)
	}
}
