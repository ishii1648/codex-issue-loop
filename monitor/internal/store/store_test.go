package store

import (
	"testing"
	"time"

	"github.com/ishii1648/codex-issue-loop/monitor/internal/model"
)

func TestReplayAndRestartKeepIntervalsNonOverlappingAndIdempotent(t *testing.T) {
	storage := Store{Root: t.TempDir()}
	base := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	observation := model.Observation{Repository: "owner/repo", ObservedAt: base, Items: []model.QueueItem{{Number: 1, Phase: model.Ready, PhaseSince: base, Deadline: base.Add(time.Minute)}}, Cursor: 101, CursorInitialized: true}
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
	if err != nil || loaded.Current.Status != model.Down {
		t.Fatalf("restarted snapshot = %+v err=%v", loaded, err)
	}
	all, err := storage.AllIntervals("owner/repo")
	if err != nil || len(all) != 2 || !all[0].EndedAt.Equal(all[1].StartedAt) {
		t.Fatalf("all intervals = %+v err=%v", all, err)
	}
}

func TestCommitDeduplicatesAReplayedIntervalBatch(t *testing.T) {
	storage := Store{Root: t.TempDir()}
	base := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	closed := []model.Interval{
		{ID: "healthy", Repository: "owner/repo", Status: model.Healthy, StartedAt: base, EndedAt: base.Add(time.Minute)},
		{ID: "down", Repository: "owner/repo", Status: model.Down, StartedAt: base.Add(time.Minute), EndedAt: base.Add(2 * time.Minute)},
	}
	snapshot := model.Snapshot{
		SchemaVersion: model.SchemaVersion, Repository: "owner/repo",
		Current:           model.Interval{ID: "idle", Repository: "owner/repo", Status: model.Idle, StartedAt: base.Add(2 * time.Minute)},
		LastObservationAt: base.Add(3 * time.Minute),
	}
	if err := storage.Commit(snapshot, closed); err != nil {
		t.Fatal(err)
	}
	if err := storage.Commit(snapshot, closed); err != nil {
		t.Fatal(err)
	}
	history, err := storage.History("owner/repo")
	if err != nil || len(history) != 2 {
		t.Fatalf("history = %+v, err = %v", history, err)
	}
}

func TestBackfillReplacesOverlappingHistoryIdempotently(t *testing.T) {
	disk := Store{Root: t.TempDir()}
	base := time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)
	previous, closed, err := model.Apply(nil, model.Observation{Repository: "owner/repo", ObservedAt: base, Cursor: 1, CursorInitialized: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := disk.Commit(previous, closed); err != nil {
		t.Fatal(err)
	}
	previous, closed, err = model.Apply(&previous, model.Observation{Repository: "owner/repo", ObservedAt: base.Add(3 * time.Minute), Error: "unavailable"})
	if err != nil {
		t.Fatal(err)
	}
	if err := disk.Commit(previous, closed); err != nil {
		t.Fatal(err)
	}
	next, closed, err := model.Apply(&previous, model.Observation{
		Repository: "owner/repo", ObservedAt: base.Add(5 * time.Minute), Cursor: 2, CursorInitialized: true,
		Items: []model.QueueItem{{Number: 1, Phase: model.Ready}}, AcceptanceTimeout: time.Hour,
		Events: []model.QueueEvent{{ID: 2, IssueNumber: 1, Kind: model.ReadyLabeled, At: base.Add(time.Minute)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := disk.Commit(next, closed); err != nil {
			t.Fatal(err)
		}
		history, err := disk.AllIntervals("owner/repo")
		if err != nil || len(history) != 2 || history[0].Status != model.Idle || !history[0].EndedAt.Equal(base.Add(time.Minute)) || history[1].Status != model.Healthy || !history[1].StartedAt.Equal(history[0].EndedAt) {
			t.Fatalf("history=%+v error=%v", history, err)
		}
	}
}

func TestDecisionMigrationPreservesLegacyHistoryAndRestartBoundary(t *testing.T) {
	for _, phase := range []model.Phase{model.Ready, model.Running, ""} {
		t.Run(string(phase), func(t *testing.T) {
			base := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)
			disk := Store{Root: t.TempDir()}
			oldClosed := model.Interval{ID: "old-healthy", Repository: "owner/repo", Status: model.Healthy, StartedAt: base, EndedAt: base.Add(time.Hour)}
			legacy := model.Snapshot{SchemaVersion: 1, Repository: "owner/repo", LastObservationAt: base.Add(2 * time.Hour), EventCursor: 1, EventCursorInitialized: true,
				Current: model.Interval{ID: "old-down", Repository: "owner/repo", Status: model.Down, StartedAt: base.Add(time.Hour)}}
			if err := disk.Commit(legacy, []model.Interval{oldClosed}); err != nil {
				t.Fatal(err)
			}
			obs := model.Observation{Repository: legacy.Repository, ObservedAt: base.Add(3 * time.Hour), Cursor: 2, CursorInitialized: true, CurrentVerified: true}
			want := model.Idle
			if phase != "" {
				obs.Items = []model.QueueItem{{Number: 1, Phase: phase, PhaseSince: base, Deadline: base.Add(time.Hour)}}
				want = model.Healthy
				if phase == model.Running {
					want = model.Unknown
				}
			}
			loaded, err := disk.Load(legacy.Repository)
			if err != nil {
				t.Fatal(err)
			}
			next, closed, err := model.Apply(loaded, obs)
			if err != nil || next.DecisionVersion != model.DecisionVersion || !next.DecisionSince.Equal(obs.ObservedAt) || next.Current.Status != want || !next.Current.StartedAt.Equal(obs.ObservedAt) {
				t.Fatalf("next=%+v err=%v", next, err)
			}
			for i := 0; i < 2; i++ {
				if err := disk.Commit(next, closed); err != nil {
					t.Fatal(err)
				}
			}
			history, err := disk.AllIntervals(legacy.Repository)
			if err != nil || len(history) != 4 || history[0] != oldClosed || history[1].Status != model.Down || history[1].DecisionVersion != 0 || history[2].Status != model.Unknown || !history[2].EndedAt.Equal(next.DecisionSince) {
				t.Fatalf("history=%+v err=%v", history, err)
			}
			report := model.BuildReport(legacy.Repository, history, base, base.Add(4*time.Hour))
			if report.LegacySeconds != 10800 {
				t.Fatalf("report=%+v", report)
			}
			if phase == model.Ready && (report.DemandAvailability == nil || *report.DemandAvailability != 1 || report.DurationsSeconds[model.Healthy] != 3600) {
				t.Fatalf("report=%+v", report)
			}
			loaded, err = disk.Load(legacy.Repository)
			if err != nil {
				t.Fatal(err)
			}
			again, duplicate, err := model.Apply(loaded, obs)
			if err != nil || len(duplicate) != 0 || again.Current.ID != next.Current.ID || !again.DecisionSince.Equal(next.DecisionSince) {
				t.Fatalf("again=%+v duplicate=%+v err=%v", again, duplicate, err)
			}
		})
	}
}

func TestRepositoryCaseChangeAcrossRestart(t *testing.T) {
	for _, names := range [][2]string{{"Owner/Repo", "owner/repo"}, {"owner/repo", "Owner/Repo"}} {
		t.Run(names[0], func(t *testing.T) {
			storage := Store{Root: t.TempDir()}
			base := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
			observation := model.Observation{Repository: names[0], ObservedAt: base, Cursor: 1, CursorInitialized: true}
			current, closed, err := model.Apply(nil, observation)
			if err != nil {
				t.Fatal(err)
			}
			if err := storage.Commit(current, closed); err != nil {
				t.Fatal(err)
			}
			previous, err := storage.Load(names[1])
			if err != nil || previous == nil {
				t.Fatalf("Load after case change = %+v, err = %v", previous, err)
			}
			observation.Repository = names[1]
			observation.ObservedAt = base.Add(time.Minute)
			observation.Error = "unavailable"
			next, closed, err := model.Apply(previous, observation)
			if err != nil {
				t.Fatal(err)
			}
			if err := storage.Commit(next, closed); err != nil {
				t.Fatal(err)
			}
			for _, name := range names {
				loaded, err := storage.Load(name)
				if err != nil || loaded == nil || loaded.Current.Status != model.Unknown || !loaded.LastObservationAt.Equal(observation.ObservedAt) {
					t.Fatalf("Load(%q) = %+v, err = %v", name, loaded, err)
				}
				intervals, err := storage.AllIntervals(name)
				if err != nil || len(intervals) != 2 || intervals[0].Status != model.Idle || !intervals[0].EndedAt.Equal(intervals[1].StartedAt) {
					t.Fatalf("AllIntervals(%q) = %+v, err = %v", name, intervals, err)
				}
			}
		})
	}
}

func TestLoadAndApplyRejectIdentityOrSchemaMismatch(t *testing.T) {
	for _, test := range []struct {
		name          string
		repository    string
		schemaVersion int
	}{
		{"repository", "other/repo", model.SchemaVersion},
		{"schema", "Owner/Repo", model.SchemaVersion + 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			storage := Store{Root: t.TempDir()}
			base := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
			snapshot, closed, err := model.Apply(nil, model.Observation{Repository: "owner/repo", ObservedAt: base, CursorInitialized: true})
			if err != nil {
				t.Fatal(err)
			}
			if err := storage.Commit(snapshot, closed); err != nil {
				t.Fatal(err)
			}
			snapshot.Repository = test.repository
			snapshot.SchemaVersion = test.schemaVersion
			if err := writeJSON(storage.currentPath("owner/repo"), snapshot); err != nil {
				t.Fatal(err)
			}
			if _, err := storage.Load("owner/repo"); err == nil {
				t.Fatal("Load accepted mismatched snapshot")
			}
			if _, _, err := model.Apply(&snapshot, model.Observation{Repository: "owner/repo", ObservedAt: base.Add(time.Minute), CursorInitialized: true}); err == nil {
				t.Fatal("Apply accepted mismatched snapshot")
			}
		})
	}
}
