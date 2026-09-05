package monitor

import (
	"context"
	"fmt"
	"time"

	"github.com/ishii1648/codex-issue-loop/monitor/internal/config"
	gh "github.com/ishii1648/codex-issue-loop/monitor/internal/github"
	"github.com/ishii1648/codex-issue-loop/monitor/internal/model"
	"github.com/ishii1648/codex-issue-loop/monitor/internal/store"
)

type Runner struct {
	Observer           gh.Observer
	Store              store.Store
	ObservationTimeout time.Duration
	Now                func() time.Time
}

func (r Runner) Poll(ctx context.Context, repo config.Repository) (model.Snapshot, error) {
	now := time.Now().UTC()
	if r.Now != nil {
		now = r.Now().UTC()
	}
	previous, err := r.Store.Load(repo.Name)
	if err != nil {
		return model.Snapshot{}, err
	}
	cursor := int64(0)
	if previous != nil {
		cursor = previous.EventCursor
		if r.ObservationTimeout > 0 {
			gapAt := previous.LastObservationAt.Add(r.ObservationTimeout)
			if now.After(gapAt) {
				for _, item := range previous.Queue {
					if item.Deadline.After(previous.LastObservationAt) && item.Deadline.Before(gapAt) {
						gapAt = item.Deadline
					}
				}
				gap := model.Observation{Repository: repo.Name, ObservedAt: gapAt, Cursor: cursor, Error: "monitor observation history has a gap"}
				unknown, closed, applyErr := model.Apply(previous, gap)
				if applyErr != nil {
					return model.Snapshot{}, applyErr
				}
				if commitErr := r.Store.Commit(unknown, closed); commitErr != nil {
					return model.Snapshot{}, commitErr
				}
				previous = &unknown
			}
		}
	}
	observation, observeErr := r.Observer.Observe(ctx, repo, cursor, now)
	if observeErr != nil {
		observation = model.Observation{Repository: repo.Name, ObservedAt: now, Cursor: cursor, Error: observeErr.Error()}
	}
	steps, replayErr := model.Replay(previous, observation)
	if replayErr != nil {
		observeErr = replayErr
		steps = []model.Observation{{Repository: repo.Name, ObservedAt: now, Cursor: cursor, Error: replayErr.Error()}}
	}
	var next model.Snapshot
	for _, step := range steps {
		var closed *model.Interval
		next, closed, err = model.Apply(previous, step)
		if err != nil {
			return model.Snapshot{}, err
		}
		if err := r.Store.Commit(next, closed); err != nil {
			return model.Snapshot{}, err
		}
		copy := next
		previous = &copy
	}
	if observeErr != nil {
		return next, fmt.Errorf("observe %s: %w", repo.Name, observeErr)
	}
	return next, nil
}
