package model

import (
	"fmt"
	"sort"
	"time"
)

// Replay validates the entire batch before exposing any interval transitions.
// Unlabeled events alone cannot distinguish label replacement from queue exit.
func Replay(previous *Snapshot, observation Observation) ([]Observation, error) {
	if observation.Events == nil || observation.Error != "" {
		return []Observation{observation}, nil
	}
	queue := map[int]QueueItem{}
	if previous != nil {
		for _, item := range previous.Queue {
			queue[item.Number] = item
		}
	}
	pending := map[int]bool{}
	exited := map[int]bool{}
	var steps []Observation
	lastAt := time.Time{}
	for _, event := range observation.Events {
		if event.At.IsZero() || event.At.After(observation.ObservedAt) || event.At.Before(lastAt) || event.Number <= 0 {
			return nil, fmt.Errorf("invalid issue event ordering or timestamp")
		}
		lastAt = event.At
		before := observation
		before.Events = nil
		before.Items = queueItems(queue)
		before.ObservedAt = event.At
		before.ChangedAt = time.Time{}
		before.Cursor = 0
		before.EventIDs = nil
		switch event.Kind {
		case "remove":
			if item, ok := queue[event.Number]; (!ok && !exited[event.Number]) || (ok && item.Phase == event.Item.Phase) {
				pending[event.Number] = true
			}
			continue
		case "unproven":
			return nil, fmt.Errorf("queue reentry history is insufficient for issue %d", event.Number)
		case "exit":
			exited[event.Number] = true
			delete(queue, event.Number)
			delete(pending, event.Number)
		case "phase":
			delete(exited, event.Number)
			queue[event.Number] = event.Item
			delete(pending, event.Number)
		default:
			return nil, fmt.Errorf("unknown queue event")
		}
		steps = append(steps, before)
		step := observation
		step.Events = nil
		step.ObservedAt = event.At
		step.ChangedAt = event.At
		step.Items = queueItems(queue)
		step.Cursor = event.ID
		step.EventIDs = []int64{event.ID}
		steps = append(steps, step)
	}
	if len(pending) != 0 {
		return nil, fmt.Errorf("queue exit history is insufficient")
	}
	if len(queue) != len(observation.Items) {
		return nil, fmt.Errorf("issue event replay disagrees with open issue snapshot")
	}
	for _, item := range observation.Items {
		replayed, ok := queue[item.Number]
		if !ok || item.Phase != replayed.Phase || (!item.PhaseSince.IsZero() && !item.PhaseSince.Equal(replayed.PhaseSince)) {
			return nil, fmt.Errorf("issue event replay disagrees with open issue snapshot")
		}
	}
	observation.Items = queueItems(queue)
	observation.ChangedAt = time.Time{}
	// A failed observation leaves an unverified gap. Recovery proves only the
	// current snapshot, not the availability of the intervening time.
	if previous == nil || previous.Current.Status == Unknown {
		return []Observation{observation}, nil
	}
	for _, step := range steps {
		if step.ObservedAt.Before(previous.LastObservationAt) {
			return nil, fmt.Errorf("issue event predates the verified observation boundary")
		}
	}
	return append(steps, observation), nil
}

func queueItems(queue map[int]QueueItem) []QueueItem {
	items := make([]QueueItem, 0, len(queue))
	for _, item := range queue {
		items = append(items, item)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Number < items[j].Number })
	return items
}
