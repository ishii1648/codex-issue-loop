package observe

import (
	"context"
	"fmt"
	"math/rand"
	"path/filepath"
	"sort"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/ishii1648/codex-issue-loop/internal/state"
)

type Result struct {
	Reason          string           `json:"reason"`
	PendingRequests []*state.Request `json:"pending_requests"`
	Snapshot        state.Snapshot   `json:"snapshot"`
}

type eventSubscription struct {
	wake   <-chan struct{}
	errors <-chan error
	close  func() error
}

type subscribeFunc func(context.Context, string) (eventSubscription, error)

func Wait(ctx context.Context, store state.Store, interval time.Duration, jitter float64, untilIdle bool) (Result, error) {
	return waitWithSubscription(ctx, store, interval, jitter, untilIdle, subscribeFSNotify, nil)
}

func waitWithSubscribeHook(ctx context.Context, store state.Store, interval time.Duration, jitter float64, untilIdle bool, subscribed func()) (Result, error) {
	return waitWithSubscription(ctx, store, interval, jitter, untilIdle, subscribeFSNotify, subscribed)
}

func waitWithSubscription(ctx context.Context, store state.Store, interval time.Duration, jitter float64, untilIdle bool, subscribe subscribeFunc, subscribed func()) (Result, error) {
	if interval <= 0 {
		return Result{}, fmt.Errorf("reconcile interval must be positive")
	}
	if snapshot, result, err := check(store, untilIdle); err != nil || result != "" {
		return newResult(result, snapshot), err
	}

	subscription, err := subscribe(ctx, filepath.Clean(store.Dir))
	if err != nil {
		// Event delivery is only a latency optimization. Descriptor exhaustion,
		// backend errors, or a temporarily unavailable directory must not disable
		// durable reconciliation.
		return wait(ctx, store, interval, jitter, untilIdle, nil, nil)
	}
	if subscription.close != nil {
		defer func() { _ = subscription.close() }()
	}
	if subscribed != nil {
		subscribed()
	}

	// Read again after subscribing so a transition between the first read and
	// event registration cannot be lost.
	if snapshot, result, err := check(store, untilIdle); err != nil || result != "" {
		return newResult(result, snapshot), err
	}

	return wait(ctx, store, interval, jitter, untilIdle, subscription.wake, subscription.errors)
}

func subscribeFSNotify(ctx context.Context, directory string) (eventSubscription, error) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return eventSubscription{}, fmt.Errorf("create event watcher: %w", err)
	}
	if err := w.Add(directory); err != nil {
		_ = w.Close()
		return eventSubscription{}, fmt.Errorf("watch state directory: %w", err)
	}
	wake := make(chan struct{}, 1)
	errors := make(chan error, 1)
	go func() {
		defer close(wake)
		defer close(errors)
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
	return eventSubscription{wake: wake, errors: errors, close: w.Close}, nil
}

func wait(ctx context.Context, store state.Store, interval time.Duration, jitter float64, untilIdle bool, wake <-chan struct{}, eventErrors <-chan error) (Result, error) {
	if interval <= 0 {
		return Result{}, fmt.Errorf("reconcile interval must be positive")
	}
	if snapshot, result, err := check(store, untilIdle); err != nil || result != "" {
		return newResult(result, snapshot), err
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
		case _, ok := <-eventErrors:
			if !ok {
				eventErrors = nil
			}
		case <-timer.C:
		}

		snapshot, result, err := check(store, untilIdle)
		if err != nil {
			return Result{}, err
		}
		if result != "" {
			return newResult(result, snapshot), nil
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

func pendingRequests(snapshot state.Snapshot) []*state.Request {
	requests := make([]*state.Request, 0)
	for _, request := range snapshot.PendingRequests {
		if request != nil && request.Status == "pending" {
			copy := *request
			copy.Options = append([]state.Option(nil), request.Options...)
			requests = append(requests, &copy)
		}
	}
	sort.Slice(requests, func(i, j int) bool { return requests[i].ID < requests[j].ID })
	return requests
}

func newResult(reason string, snapshot state.Snapshot) Result {
	return Result{Reason: reason, PendingRequests: pendingRequests(snapshot), Snapshot: snapshot}
}

func jittered(base time.Duration, ratio float64) time.Duration {
	if ratio <= 0 {
		return base
	}
	delta := (rand.Float64()*2 - 1) * ratio
	return time.Duration(float64(base) * (1 + delta))
}
