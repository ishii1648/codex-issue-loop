package app

import (
	"testing"
	"time"

	"github.com/ishii1648/codex-issue-loop/monitor/internal/model"
	"github.com/ishii1648/codex-issue-loop/monitor/internal/store"
)

func TestStoppedMonitorReportsUnknownAtDeadlineBeforeTimeout(t *testing.T) {
	base := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	item := model.QueueItem{Number: 1, Phase: model.Ready, PhaseSince: base, Deadline: base.Add(time.Minute)}
	snapshot, _, err := model.Apply(nil, model.Observation{Repository: "owner/repo", ObservedAt: base, Items: []model.QueueItem{item}})
	if err != nil {
		t.Fatal(err)
	}
	storage := store.Store{Root: t.TempDir()}
	if err := storage.Commit(snapshot, nil); err != nil {
		t.Fatal(err)
	}
	now := base.Add(5 * time.Minute)
	effective := effectiveSnapshot(snapshot, 3*time.Minute, now)
	if effective.Current.Status != model.Unknown || !effective.Current.StartedAt.Equal(item.Deadline) {
		t.Fatalf("status = %+v", effective)
	}
	intervals, err := effectiveIntervals(storage, snapshot.Repository, 3*time.Minute, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(intervals) != 2 || intervals[1].Status != model.Unknown || !intervals[0].EndedAt.Equal(item.Deadline) || !intervals[1].StartedAt.Equal(item.Deadline) {
		t.Fatalf("report intervals = %+v", intervals)
	}
	stored, err := storage.Load(snapshot.Repository)
	if err != nil || stored.Current.Status != model.Healthy {
		t.Fatalf("read changed storage: %+v err=%v", stored, err)
	}
}
