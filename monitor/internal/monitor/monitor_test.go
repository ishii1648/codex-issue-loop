package monitor

import (
	"context"
	"errors"
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
