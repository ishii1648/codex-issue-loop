package model

import (
	"fmt"
	"sort"
	"time"
)

const SchemaVersion = 1

type Status string

const (
	Idle    Status = "IDLE"
	Healthy Status = "HEALTHY"
	Down    Status = "DOWN"
	Unknown Status = "UNKNOWN"
)

type Phase string

const (
	Ready   Phase = "ready"
	Running Phase = "running"
)

type QueueItem struct {
	Number     int       `json:"number"`
	Phase      Phase     `json:"phase"`
	PhaseSince time.Time `json:"phase_since"`
	Deadline   time.Time `json:"deadline"`
}

type Observation struct {
	Repository string      `json:"repository"`
	ObservedAt time.Time   `json:"observed_at"`
	Items      []QueueItem `json:"queue"`
	EventIDs   []int64     `json:"event_ids,omitempty"`
	Cursor     int64       `json:"event_cursor,omitempty"`
	ChangedAt  time.Time   `json:"changed_at,omitempty"`
	Error      string      `json:"error,omitempty"`
}

type Interval struct {
	ID         string    `json:"id"`
	Repository string    `json:"repository"`
	Status     Status    `json:"status"`
	StartedAt  time.Time `json:"started_at"`
	EndedAt    time.Time `json:"ended_at,omitempty"`
	Reason     string    `json:"reason,omitempty"`
}

type Snapshot struct {
	SchemaVersion     int         `json:"schema_version"`
	Repository        string      `json:"repository"`
	Current           Interval    `json:"current"`
	Queue             []QueueItem `json:"queue,omitempty"`
	LastObservationAt time.Time   `json:"last_observation_at"`
	LastSuccessAt     time.Time   `json:"last_success_at,omitempty"`
	EventCursor       int64       `json:"event_cursor,omitempty"`
	SeenEventIDs      []int64     `json:"seen_event_ids,omitempty"`
	LastError         string      `json:"last_error,omitempty"`
}

func Evaluate(observation Observation) (Status, time.Time, string) {
	at := observation.ObservedAt.UTC()
	if observation.Error != "" {
		return Unknown, at, "GitHub observation failed"
	}
	if len(observation.Items) == 0 {
		if !observation.ChangedAt.IsZero() {
			return Idle, observation.ChangedAt.UTC(), "no actionable queue items"
		}
		return Idle, at, "no actionable queue items"
	}
	deadline := time.Time{}
	latestProgress := time.Time{}
	for _, item := range observation.Items {
		if item.PhaseSince.IsZero() || item.Deadline.IsZero() || item.Number <= 0 || (item.Phase != Ready && item.Phase != Running) {
			return Unknown, at, "queue history is insufficient"
		}
		if !item.Deadline.After(at) && (deadline.IsZero() || item.Deadline.Before(deadline)) {
			deadline = item.Deadline.UTC()
		}
		if item.PhaseSince.After(latestProgress) {
			latestProgress = item.PhaseSince.UTC()
		}
	}
	if !deadline.IsZero() {
		return Down, deadline, "progress deadline exceeded"
	}
	return Healthy, latestProgress, "queue progress is within deadline"
}

func Apply(previous *Snapshot, observation Observation) (Snapshot, *Interval, error) {
	if observation.Repository == "" || observation.ObservedAt.IsZero() {
		return Snapshot{}, nil, fmt.Errorf("repository and observed_at are required")
	}
	status, start, reason := Evaluate(observation)
	next := Snapshot{SchemaVersion: SchemaVersion, Repository: observation.Repository, Queue: append([]QueueItem(nil), observation.Items...), LastObservationAt: observation.ObservedAt.UTC(), EventCursor: observation.Cursor, SeenEventIDs: uniqueIDs(observation.EventIDs), LastError: observation.Error}
	if observation.Error == "" {
		next.LastSuccessAt = observation.ObservedAt.UTC()
	}
	if previous == nil || previous.Current.Status == "" {
		next.Current = newInterval(observation.Repository, status, start, reason)
		return next, nil, nil
	}
	if previous.SchemaVersion != SchemaVersion || previous.Repository != observation.Repository {
		return Snapshot{}, nil, fmt.Errorf("snapshot identity or schema mismatch")
	}
	if observation.ObservedAt.Before(previous.LastObservationAt) {
		return Snapshot{}, nil, fmt.Errorf("observation time moved backwards")
	}
	next.LastSuccessAt = previous.LastSuccessAt
	if observation.Error == "" {
		next.LastSuccessAt = observation.ObservedAt.UTC()
	}
	next.EventCursor = max(previous.EventCursor, observation.Cursor)
	next.SeenEventIDs = mergeIDs(previous.SeenEventIDs, observation.EventIDs)
	if status == previous.Current.Status {
		next.Current = previous.Current
		next.Current.Reason = reason
		return next, nil, nil
	}
	if previous.Current.Status == Unknown && !start.After(previous.Current.StartedAt) {
		start = observation.ObservedAt.UTC()
	}
	if start.Before(previous.Current.StartedAt) {
		start = previous.Current.StartedAt
	}
	if start.After(observation.ObservedAt) {
		start = observation.ObservedAt.UTC()
	}
	closed := previous.Current
	closed.EndedAt = start
	next.Current = newInterval(observation.Repository, status, start, reason)
	if !closed.EndedAt.After(closed.StartedAt) {
		return next, nil, nil
	}
	return next, &closed, nil
}

func newInterval(repository string, status Status, start time.Time, reason string) Interval {
	start = start.UTC()
	return Interval{ID: fmt.Sprintf("%s:%s:%d", repository, status, start.UnixNano()), Repository: repository, Status: status, StartedAt: start, Reason: reason}
}

func uniqueIDs(ids []int64) []int64 {
	return mergeIDs(nil, ids)
}

func mergeIDs(existing, added []int64) []int64 {
	set := make(map[int64]struct{}, len(existing)+len(added))
	for _, id := range append(append([]int64(nil), existing...), added...) {
		if id > 0 {
			set[id] = struct{}{}
		}
	}
	ids := make([]int64, 0, len(set))
	for id := range set {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

func max(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
