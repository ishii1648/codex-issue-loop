package monitor

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/ishii1648/codex-issue-loop/monitor/internal/config"
	"github.com/ishii1648/codex-issue-loop/monitor/internal/model"
	"github.com/ishii1648/codex-issue-loop/monitor/internal/store"
)

type fakeObserver struct {
	observations map[string]model.Observation
	errors       map[string]error
}

func (f fakeObserver) Observe(_ context.Context, repo config.Repository, _ int64, _ bool, at time.Time) (model.Observation, error) {
	if err := f.errors[repo.Name]; err != nil {
		return model.Observation{}, err
	}
	observation := f.observations[repo.Name]
	observation.Repository = repo.Name
	observation.ObservedAt = at
	observation.CursorInitialized = true
	observation.AcceptanceTimeout = time.Hour
	observation.ProcessingTimeout = time.Hour
	return observation, nil
}

func TestRepositoryObservationFailuresArePersistedIndependently(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	observer := fakeObserver{observations: map[string]model.Observation{"owner/good": {}}, errors: map[string]error{"owner/bad": errors.New("API unavailable")}}
	runner := Runner{Observer: observer, Store: store.Store{Root: root}, Now: func() time.Time { return now }}
	repos := []config.Repository{{Name: "owner/bad"}, {Name: "owner/good"}}
	if _, err := runner.Poll(context.Background(), repos[0]); err == nil {
		t.Fatal("failed repository poll succeeded")
	}
	if _, err := runner.Poll(context.Background(), repos[1]); err != nil {
		t.Fatal(err)
	}
	bad, err := runner.Store.Load("owner/bad")
	if err != nil || bad.Current.Status != model.Unknown {
		t.Fatalf("bad repository state = %+v err=%v", bad, err)
	}
	good, err := runner.Store.Load("owner/good")
	if err != nil || good.Current.Status != model.Idle {
		t.Fatalf("good repository state = %+v err=%v", good, err)
	}
}

func TestRestartReplaysCompleteHistoryWithoutUnknownGap(t *testing.T) {
	root := t.TempDir()
	base := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	repo := config.Repository{Name: "owner/repo", ReadyLabels: []string{"ready"}, RunningLabel: "running", TerminalLabels: []string{"done"}, AcceptanceTimeout: config.Duration{Duration: time.Hour}, ProcessingTimeout: config.Duration{Duration: time.Hour}}
	observer := fakeObserver{observations: map[string]model.Observation{"owner/repo": {}}, errors: map[string]error{}}
	runner := Runner{Observer: observer, Store: store.Store{Root: root}, ObservationTimeout: 3 * time.Minute, Now: func() time.Time { return base }}
	if _, err := runner.Poll(context.Background(), repo); err != nil {
		t.Fatal(err)
	}
	runner.Now = func() time.Time { return base.Add(10 * time.Minute) }
	current, err := runner.Poll(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	if current.Current.Status != model.Idle || !current.Current.StartedAt.Equal(base) {
		t.Fatalf("recovered current = %+v", current.Current)
	}
	history, err := runner.Store.History(repo.Name)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 0 {
		t.Fatalf("restart history = %+v", history)
	}
}

func TestSnapshotRaceRetriesVerifiedCursorWithoutDuplicateIntervals(t *testing.T) {
	for _, snapshotAhead := range []bool{false, true} {
		t.Run(map[bool]string{false: "mutation after snapshot before events", true: "mutation after events before snapshot"}[snapshotAhead], func(t *testing.T) {
			base := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
			repo := config.Repository{Name: "owner/repo"}
			ready := model.QueueItem{Number: 1, Phase: model.Ready, PhaseSince: base, Deadline: base.Add(time.Hour)}
			running := model.QueueItem{Number: 1, Phase: model.Running, PhaseSince: base.Add(time.Minute), Deadline: base.Add(time.Hour)}
			events := []model.QueueEvent{{ID: 2, IssueNumber: 1, At: base.Add(time.Minute), Kind: model.ReadyUnlabeled}, {ID: 3, IssueNumber: 1, At: base.Add(time.Minute), Kind: model.RunningLabeled}}
			observer := fakeObserver{observations: map[string]model.Observation{repo.Name: {Items: []model.QueueItem{ready}, Events: []model.QueueEvent{{ID: 1, IssueNumber: 1, At: base, Kind: model.ReadyLabeled}}, Cursor: 1}}}
			now := base
			runner := Runner{Observer: observer, Store: store.Store{Root: t.TempDir()}, Now: func() time.Time { return now }}
			if _, err := runner.Poll(context.Background(), repo); err != nil {
				t.Fatal(err)
			}
			mismatch := model.Observation{Items: []model.QueueItem{ready}, Events: events, Cursor: 3}
			if snapshotAhead {
				mismatch.Items = []model.QueueItem{running}
				mismatch.Events = []model.QueueEvent{}
				mismatch.Cursor = 1
			}
			observer.observations[repo.Name] = mismatch
			for i := 2; i <= 3; i++ {
				now = base.Add(time.Duration(i) * time.Minute)
				got, err := runner.Poll(context.Background(), repo)
				if err == nil || got.Current.Status != model.Unknown || got.EventCursor != 1 || len(got.Queue) != 1 || got.Queue[0].Phase != model.Ready {
					t.Fatalf("mismatch state = %+v err=%v", got, err)
				}
			}
			observer.observations[repo.Name] = model.Observation{Items: []model.QueueItem{running}, Events: events, Cursor: 3}
			now = base.Add(4 * time.Minute)
			got, err := runner.Poll(context.Background(), repo)
			if err != nil || got.Current.Status != model.Healthy || got.EventCursor != 3 || !got.Current.StartedAt.Equal(base) {
				t.Fatalf("recovery = %+v err=%v", got, err)
			}
			observer.observations[repo.Name] = model.Observation{Items: []model.QueueItem{{Number: 1, Phase: model.Running}}, Events: []model.QueueEvent{}, Cursor: 3}
			now = base.Add(5 * time.Minute)
			if _, err := runner.Poll(context.Background(), repo); err != nil {
				t.Fatal(err)
			}
			history, err := runner.Store.AllIntervals(repo.Name)
			if err != nil {
				t.Fatal(err)
			}
			if len(history) != 1 || history[0].Status != model.Healthy {
				t.Fatalf("intervals = %+v", history)
			}
			open := 0
			for i, interval := range history {
				if interval.EndedAt.IsZero() {
					open++
				}
				if i > 0 && interval.StartedAt.Before(history[i-1].EndedAt) {
					t.Fatal("overlapping intervals")
				}
			}
			if open != 1 {
				t.Fatalf("open intervals = %d", open)
			}
		})
	}
}

func TestObservationGapTakesPrecedenceOverDeadline(t *testing.T) {
	for _, failure := range []bool{false, true} {
		base := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
		repo := config.Repository{Name: "owner/repo"}
		item := model.QueueItem{Number: 1, Phase: model.Ready, PhaseSince: base, Deadline: base.Add(time.Minute)}
		observer := fakeObserver{observations: map[string]model.Observation{repo.Name: {Items: []model.QueueItem{item}, Cursor: 1}}, errors: map[string]error{}}
		now := base
		runner := Runner{Observer: observer, Store: store.Store{Root: t.TempDir()}, ObservationTimeout: 3 * time.Minute, Now: func() time.Time { return now }}
		if _, err := runner.Poll(context.Background(), repo); err != nil {
			t.Fatal(err)
		}
		if failure {
			observer.errors[repo.Name] = errors.New("timeout")
			now = base.Add(2 * time.Minute)
		} else {
			now = base.Add(5 * time.Minute)
		}
		got, err := runner.Poll(context.Background(), repo)
		if failure && err == nil {
			t.Fatal("missing observation error")
		}
		history, err := runner.Store.AllIntervals(repo.Name)
		if err != nil {
			t.Fatal(err)
		}
		want := model.Down
		if failure {
			want = model.Unknown
		}
		if !history[0].EndedAt.Equal(item.Deadline) || history[1].Status != want || !history[1].StartedAt.Equal(item.Deadline) {
			t.Fatalf("gap = %+v", history)
		}
		if !failure && (got.Current.Status != model.Down || !got.Current.StartedAt.Equal(item.Deadline)) {
			t.Fatalf("recovery = %+v", got)
		}
	}
}

func TestTerminalSnapshotRaceConvergesToIdle(t *testing.T) {
	base := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	repo := config.Repository{Name: "owner/repo"}
	running := model.QueueItem{Number: 1, Phase: model.Running, PhaseSince: base, Deadline: base.Add(time.Hour)}
	observer := fakeObserver{observations: map[string]model.Observation{repo.Name: {Items: []model.QueueItem{running}, Cursor: 1}}}
	now := base
	runner := Runner{Observer: observer, Store: store.Store{Root: t.TempDir()}, Now: func() time.Time { return now }}
	if _, err := runner.Poll(context.Background(), repo); err != nil {
		t.Fatal(err)
	}
	events := []model.QueueEvent{{ID: 2, IssueNumber: 1, At: base.Add(time.Minute), Kind: model.QueueExited}}
	observer.observations[repo.Name] = model.Observation{Items: []model.QueueItem{running}, Events: events, Cursor: 2}
	now = base.Add(2 * time.Minute)
	got, err := runner.Poll(context.Background(), repo)
	if err == nil || got.Current.Status != model.Unknown || got.EventCursor != 1 {
		t.Fatalf("race = %+v err=%v", got, err)
	}
	observer.observations[repo.Name] = model.Observation{Events: events, Cursor: 2}
	now = base.Add(3 * time.Minute)
	got, err = runner.Poll(context.Background(), repo)
	if err != nil || got.Current.Status != model.Idle || got.EventCursor != 2 || !got.Current.StartedAt.Equal(base.Add(time.Minute)) {
		t.Fatalf("convergence = %+v err=%v", got, err)
	}
}

func TestUnknownRecoveryWithOverdueQueueAndNewEvents(t *testing.T) {
	base := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	repo := config.Repository{Name: "owner/repo"}
	old := model.QueueItem{Number: 239, Phase: model.Running, PhaseSince: base.Add(-24 * time.Hour), Deadline: base.Add(-23 * time.Hour)}
	previous, _, err := model.Apply(nil, model.Observation{Repository: repo.Name, ObservedAt: base, Cursor: 1, CursorInitialized: true, Items: []model.QueueItem{old}, Error: "unavailable"})
	if err != nil {
		t.Fatal(err)
	}
	disk := store.Store{Root: t.TempDir()}
	if err := disk.Commit(previous, nil); err != nil {
		t.Fatal(err)
	}
	ready := model.QueueItem{Number: 311, Phase: model.Ready, PhaseSince: base.Add(time.Minute), Deadline: base.Add(61 * time.Minute)}
	observer := fakeObserver{observations: map[string]model.Observation{repo.Name: {
		Items: []model.QueueItem{old, ready}, Cursor: 2, CurrentVerified: true,
		Events: []model.QueueEvent{{ID: 2, IssueNumber: ready.Number, Kind: model.ReadyLabeled, At: ready.PhaseSince}},
	}}}
	for poll := 0; poll < 4; poll++ {
		at := base.Add(time.Duration(poll+2) * time.Minute)
		runner := Runner{Observer: observer, Store: store.Store{Root: disk.Root}, Now: func() time.Time { return at }}
		next, err := runner.Poll(context.Background(), repo)
		if err != nil {
			t.Fatal(err)
		}
		if next.Current.Status != model.Down || !next.Current.StartedAt.Equal(base.Add(2*time.Minute)) || !next.QueueDeadline.Equal(old.Deadline) || next.EventCursor != 2 {
			t.Fatalf("next=%+v", next)
		}
	}
	history, err := disk.AllIntervals(repo.Name)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 2 || history[0].Status != model.Unknown || !history[0].StartedAt.Equal(base) || !history[0].EndedAt.Equal(base.Add(2*time.Minute)) {
		t.Fatalf("history=%+v", history)
	}
}

func TestFailedPollsBackfillAcrossRestart(t *testing.T) {
	for _, mode := range []string{"events", "idle", "missing cursor", "incomplete events"} {
		t.Run(mode, func(t *testing.T) {
			base := time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)
			repo := config.Repository{Name: "owner/repo"}
			observer := fakeObserver{observations: map[string]model.Observation{repo.Name: {Cursor: 1}}, errors: map[string]error{}}
			disk := store.Store{Root: t.TempDir()}
			now := base
			runner := Runner{Observer: observer, Store: disk, Now: func() time.Time { return now }}
			if _, err := runner.Poll(context.Background(), repo); err != nil {
				t.Fatal(err)
			}
			now = base.Add(time.Minute)
			if _, err := runner.Poll(context.Background(), repo); err != nil {
				t.Fatal(err)
			}
			observer.errors[repo.Name] = errors.New("invalid history during observation")
			for _, minute := range []int{3, 80} {
				now = base.Add(time.Duration(minute) * time.Minute)
				got, err := runner.Poll(context.Background(), repo)
				if err == nil || got.Current.Status != model.Unknown || got.EventCursor != 1 || !got.LastSuccessAt.Equal(base.Add(time.Minute)) {
					t.Fatalf("failed poll=%+v error=%v", got, err)
				}
			}
			delete(observer.errors, repo.Name)
			recovery := model.Observation{Cursor: 1, CurrentVerified: true}
			if mode != "idle" {
				recovery.Cursor = 4
				recovery.Events = []model.QueueEvent{
					{ID: 2, IssueNumber: 7, Kind: model.ReadyLabeled, At: base.Add(2 * time.Minute)},
					{ID: 3, IssueNumber: 7, Kind: model.RunningLabeled, At: base.Add(70 * time.Minute)},
					{ID: 4, IssueNumber: 7, Kind: model.QueueExited, At: base.Add(90 * time.Minute)},
				}
			}
			if mode == "missing cursor" {
				recovery.Resynchronized = true
				recovery.Events = nil
			}
			if mode == "incomplete events" {
				recovery.Events = recovery.Events[:2]
			}
			observer.observations[repo.Name] = recovery
			now = base.Add(100 * time.Minute)
			restarted := Runner{Observer: observer, Store: store.Store{Root: disk.Root}, Now: func() time.Time { return now }}
			got, err := restarted.Poll(context.Background(), repo)
			if err != nil || got.Current.Status != model.Idle || got.EventCursor != recovery.Cursor {
				t.Fatalf("recovery=%+v error=%v", got, err)
			}
			history, err := disk.AllIntervals(repo.Name)
			if err != nil {
				t.Fatal(err)
			}
			report := model.BuildReport(repo.Name, history, base, now)
			switch mode {
			case "events":
				if report.DurationsSeconds[model.Healthy] != 80*60 || report.DurationsSeconds[model.Down] != 8*60 || report.DurationsSeconds[model.Idle] != 12*60 || report.DurationsSeconds[model.Unknown] != 0 {
					t.Fatalf("backfilled durations=%+v intervals=%+v", report, history)
				}
			case "idle":
				if report.DurationsSeconds[model.Idle] != 100*60 || report.ObservationCoverage != 1 {
					t.Fatalf("idle recovery=%+v", report)
				}
			default:
				if report.DurationsSeconds[model.Unknown] != 97*60 {
					t.Fatalf("unproven gap changed=%+v", report)
				}
			}
			observer.observations[repo.Name] = model.Observation{Cursor: got.EventCursor, CurrentVerified: true}
			if _, err := restarted.Poll(context.Background(), repo); err != nil {
				t.Fatal(err)
			}
			again, err := disk.AllIntervals(repo.Name)
			if err != nil || !reflect.DeepEqual(history, again) {
				t.Fatalf("repeated poll changed intervals=%+v error=%v", again, err)
			}
		})
	}
}
