package observe

import (
	"context"
	"fmt"
	"math/rand"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/ishii1648/codex-issue-loop/internal/state"
)

type Result struct {
	Reason   string         `json:"reason"`
	Snapshot state.Snapshot `json:"snapshot"`
}

func Wait(ctx context.Context, store state.Store, interval time.Duration, jitter float64, untilIdle bool) (Result, error) {
	return waitWithSubscribeHook(ctx, store, interval, jitter, untilIdle, nil)
}

func waitWithSubscribeHook(ctx context.Context, store state.Store, interval time.Duration, jitter float64, untilIdle bool, subscribed func()) (Result, error) {
	if interval <= 0 {
		return Result{}, fmt.Errorf("reconcile interval must be positive")
	}
	if snapshot, result, err := check(store, untilIdle); err != nil || result != "" {
		return Result{Reason: result, Snapshot: snapshot}, err
	}

	w, err := fsnotify.NewWatcher()
	if err != nil {
		return Result{}, fmt.Errorf("create event watcher: %w", err)
	}
	defer w.Close()
	if err := w.Add(filepath.Clean(store.Dir)); err != nil {
		return Result{}, fmt.Errorf("watch state directory: %w", err)
	}
	if subscribed != nil {
		subscribed()
	}

	// Read again after subscribing so a transition between the first read and
	// event registration cannot be lost.
	if snapshot, result, err := check(store, untilIdle); err != nil || result != "" {
		return Result{Reason: result, Snapshot: snapshot}, err
	}

	wake := make(chan struct{}, 1)
	errors := make(chan error, 1)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case event, ok := <-w.Events:
				if !ok {
					return
				}
				base := filepath.Base(event.Name)
				if base != "state.json" && base != "events.jsonl" {
					continue
				}
				select {
				case wake <- struct{}{}:
				default:
				}
			case err, ok := <-w.Errors:
				if ok && err != nil {
					select {
					case errors <- err:
					default:
					}
				}
			}
		}
	}()
	return wait(ctx, store, interval, jitter, untilIdle, wake, errors)
}

func wait(ctx context.Context, store state.Store, interval time.Duration, jitter float64, untilIdle bool, wake <-chan struct{}, eventErrors <-chan error) (Result, error) {
	if interval <= 0 {
		return Result{}, fmt.Errorf("reconcile interval must be positive")
	}
	if snapshot, result, err := check(store, untilIdle); err != nil || result != "" {
		return Result{Reason: result, Snapshot: snapshot}, err
	}
	timer := time.NewTimer(jittered(interval, jitter))
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return Result{}, ctx.Err()
		case _, ok := <-wake:
			if !ok {
				wake = nil
			}
		case err, ok := <-eventErrors:
			if !ok {
				eventErrors = nil
			} else if err != nil {
				// Event delivery is only an optimization. Continue to the durable
				// reconciliation path instead of failing the watch.
			}
		case <-timer.C:
		}

		snapshot, result, err := check(store, untilIdle)
		if err != nil {
			return Result{}, err
		}
		if result != "" {
			return Result{Reason: result, Snapshot: snapshot}, nil
		}
		timer.Reset(jittered(interval, jitter))
	}
}

func check(store state.Store, untilIdle bool) (state.Snapshot, string, error) {
	snapshot, err := store.Load()
	if err != nil {
		return state.Snapshot{}, "", err
	}
	reason, ok := snapshot.Attention(untilIdle)
	if !ok {
		return snapshot, "", nil
	}
	return snapshot, reason, nil
}

func jittered(base time.Duration, ratio float64) time.Duration {
	if ratio <= 0 {
		return base
	}
	delta := (rand.Float64()*2 - 1) * ratio
	return time.Duration(float64(base) * (1 + delta))
}
