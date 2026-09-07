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
	if r.ObservationTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.ObservationTimeout)
		defer cancel()
	}
	now := time.Now().UTC()
	if r.Now != nil {
		now = r.Now().UTC()
	}
	previous, err := r.Store.Load(repo.Name)
	if err != nil {
		return model.Snapshot{}, err
	}
	cursor := int64(0)
	initialized := false
	if previous != nil {
		cursor = previous.EventCursor
		initialized = previous.EventCursorInitialized
	}
	observation, observeErr := r.Observer.Observe(ctx, repo, cursor, initialized, now)
	var next model.Snapshot
	var closed []model.Interval
	if observeErr == nil {
		next, closed, observeErr = model.Apply(previous, observation)
	}
	if observeErr != nil {
		errorAt := now
		if previous != nil {
			if r.ObservationTimeout > 0 {
				gapAt := previous.LastObservationAt.Add(r.ObservationTimeout)
				if gapAt.Before(errorAt) {
					errorAt = gapAt
				}
			}
			if deadline := previous.QueueDeadline; deadline.After(previous.LastObservationAt) && deadline.Before(errorAt) {
				errorAt = deadline
			}
		}
		observation = model.Observation{Repository: repo.Name, ObservedAt: errorAt, Cursor: cursor, Error: observeErr.Error()}
		next, closed, err = model.Apply(previous, observation)
		if err != nil {
			return model.Snapshot{}, err
		}
	}
	if err := r.Store.Commit(next, closed); err != nil {
		return model.Snapshot{}, err
	}
	if observeErr != nil {
		return next, fmt.Errorf("observe %s: %w", repo.Name, observeErr)
	}
	return next, nil
}
