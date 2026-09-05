package model

import (
	"fmt"
	"sort"
	"time"
)

func Evaluate(observation Observation) (Status, time.Time, string) {
	at := observation.ObservedAt.UTC()
	if observation.Error != "" {
		return Unknown, at, "GitHub observation failed"
	}
	phase, since, deadline, ok := aggregate(observation.Items)
	if !ok {
		return Unknown, at, "queue history is insufficient"
	}
	if phase == "" {
		return Idle, at, "no actionable queue items"
	}
	if !deadline.After(at) {
		return Down, deadline, "queue progress deadline exceeded"
	}
	return Healthy, since, "queue progress is within deadline"
}

func Apply(previous *Snapshot, observation Observation) (Snapshot, []Interval, error) {
	if observation.Repository == "" || observation.ObservedAt.IsZero() {
		return Snapshot{}, nil, fmt.Errorf("repository and observed_at are required")
	}
	observation.ObservedAt = observation.ObservedAt.UTC()
	if previous == nil || previous.Current.Status == "" {
		return bootstrap(observation)
	}
	if previous.SchemaVersion != SchemaVersion || previous.Repository != observation.Repository {
		return Snapshot{}, nil, fmt.Errorf("snapshot identity or schema mismatch")
	}
	if observation.ObservedAt.Before(previous.LastObservationAt) {
		return Snapshot{}, nil, fmt.Errorf("observation time moved backwards")
	}

	replay := replayState{snapshot: cloneSnapshot(*previous)}
	replay.snapshot.LastObservationAt = observation.ObservedAt
	if observation.Error != "" {
		replay.snapshot.LastError = observation.Error
		if err := replay.transition(Unknown, observation.ObservedAt, "GitHub observation failed"); err != nil {
			return Snapshot{}, nil, err
		}
		return replay.snapshot, replay.closed, nil
	}
	if !observation.CursorInitialized {
		return Snapshot{}, nil, fmt.Errorf("successful observation has no event cursor")
	}
	if previous.EventCursorInitialized && observation.Cursor < previous.EventCursor {
		return Snapshot{}, nil, fmt.Errorf("event cursor moved backwards")
	}

	replay.snapshot.LastSuccessAt = observation.ObservedAt
	replay.snapshot.LastError = ""
	replay.ensureQueuePhase()
	events := append([]QueueEvent(nil), observation.Events...)
	sort.Slice(events, func(i, j int) bool {
		if events[i].At.Equal(events[j].At) {
			return events[i].ID < events[j].ID
		}
		return events[i].At.Before(events[j].At)
	})
	for _, event := range events {
		if event.ID <= 0 || event.IssueNumber <= 0 || event.At.IsZero() {
			return Snapshot{}, nil, fmt.Errorf("invalid queue event")
		}
		if previous.EventCursorInitialized && event.ID <= previous.EventCursor {
			continue
		}
		event.At = event.At.UTC()
		if event.At.Before(replay.snapshot.Current.StartedAt) {
			if replay.snapshot.Current.Status != Unknown {
				return Snapshot{}, nil, fmt.Errorf("queue event time moved backwards")
			}
			shadow := replayState{snapshot: cloneSnapshot(replay.snapshot)}
			shadow.snapshot.Current = newInterval(observation.Repository, Unknown, event.At, replay.snapshot.Current.Reason)
			if err := shadow.applyEvent(event, observation.AcceptanceTimeout, observation.ProcessingTimeout); err != nil {
				return Snapshot{}, nil, err
			}
			replay.copyQueue(shadow.snapshot)
			continue
		}
		if event.At.After(observation.ObservedAt) {
			return Snapshot{}, nil, fmt.Errorf("queue event is newer than observation")
		}
		if err := replay.advanceDeadline(event.At); err != nil {
			return Snapshot{}, nil, err
		}
		if err := replay.applyEvent(event, observation.AcceptanceTimeout, observation.ProcessingTimeout); err != nil {
			return Snapshot{}, nil, err
		}
	}
	queueMismatch := !sameQueue(replay.snapshot.Queue, observation.Items)
	if queueMismatch {
		replay.snapshot.Queue = append([]QueueItem(nil), observation.Items...)
		replay.setAggregateFromQueue()
		if err := replay.transition(Unknown, observation.ObservedAt, "queue history is insufficient"); err != nil {
			return Snapshot{}, nil, err
		}
	} else if err := replay.advanceDeadline(observation.ObservedAt); err != nil {
		return Snapshot{}, nil, err
	}
	replay.snapshot.EventCursor = observation.Cursor
	replay.snapshot.EventCursorInitialized = true
	sortQueue(replay.snapshot.Queue)
	if replay.snapshot.Current.Status == Unknown && (!queueMismatch || !previous.EventCursorInitialized) {
		replay.recoverAt(observation.ObservedAt)
	}
	return replay.snapshot, replay.closed, nil
}

type replayState struct {
	snapshot Snapshot
	closed   []Interval
}

func bootstrap(observation Observation) (Snapshot, []Interval, error) {
	status, start, reason := Evaluate(observation)
	next := Snapshot{
		SchemaVersion:          SchemaVersion,
		Repository:             observation.Repository,
		Queue:                  append([]QueueItem(nil), observation.Items...),
		LastObservationAt:      observation.ObservedAt,
		EventCursor:            observation.Cursor,
		EventCursorInitialized: observation.CursorInitialized,
		LastError:              observation.Error,
	}
	if observation.Error == "" {
		next.LastSuccessAt = observation.ObservedAt
	}
	phase, since, deadline, ok := aggregate(next.Queue)
	if ok {
		next.QueuePhase = phase
		next.QueuePhaseSince = since
		next.QueueDeadline = deadline
	}
	next.Current = newInterval(observation.Repository, status, start, reason)
	sortQueue(next.Queue)
	return next, nil, nil
}

func (r *replayState) ensureQueuePhase() {
	if r.snapshot.QueuePhase == "" && len(r.snapshot.Queue) > 0 {
		r.setAggregateFromQueue()
	}
}

func (r *replayState) setAggregateFromQueue() {
	phase, since, deadline, ok := aggregate(r.snapshot.Queue)
	if !ok {
		r.snapshot.QueuePhase = ""
		r.snapshot.QueuePhaseSince = time.Time{}
		r.snapshot.QueueDeadline = time.Time{}
		return
	}
	r.snapshot.QueuePhase = phase
	r.snapshot.QueuePhaseSince = since
	r.snapshot.QueueDeadline = deadline
}

func (r *replayState) advanceDeadline(at time.Time) error {
	if r.snapshot.Current.Status == Healthy && r.snapshot.QueuePhase != "" && !r.snapshot.QueueDeadline.IsZero() && !r.snapshot.QueueDeadline.After(at) {
		return r.transition(Down, r.snapshot.QueueDeadline, "queue progress deadline exceeded")
	}
	return nil
}
func (r *replayState) copyQueue(snapshot Snapshot) {
	r.snapshot.Queue = snapshot.Queue
	r.snapshot.QueuePhase = snapshot.QueuePhase
	r.snapshot.QueuePhaseSince = snapshot.QueuePhaseSince
	r.snapshot.QueueDeadline = snapshot.QueueDeadline
}

func (r *replayState) recoverAt(at time.Time) {
	if r.snapshot.QueuePhase == "" {
		if len(r.snapshot.Queue) > 0 {
			return
		}
		_ = r.transition(Idle, at, "no actionable queue items")
		return
	}
	if r.snapshot.QueueDeadline.IsZero() {
		return
	}
	if !r.snapshot.QueueDeadline.After(at) {
		_ = r.transition(Down, at, "queue progress deadline exceeded")
		return
	}
	_ = r.transition(Healthy, at, "queue progress is within deadline")
}

func (r *replayState) applyEvent(event QueueEvent, acceptanceTimeout, processingTimeout time.Duration) error {
	oldPhase := r.snapshot.QueuePhase
	removedPhase := Phase("")
	index := queueIndex(r.snapshot.Queue, event.IssueNumber)
	switch event.Kind {
	case ReadyLabeled:
		r.snapshot.Queue = upsertQueue(r.snapshot.Queue, index, QueueItem{Number: event.IssueNumber, Phase: Ready, PhaseSince: event.At, Deadline: event.At.Add(acceptanceTimeout)})
	case RunningLabeled:
		r.snapshot.Queue = upsertQueue(r.snapshot.Queue, index, QueueItem{Number: event.IssueNumber, Phase: Running, PhaseSince: event.At, Deadline: event.At.Add(processingTimeout)})
	case ReadyUnlabeled:
		if index >= 0 && r.snapshot.Queue[index].Phase == Ready {
			removedPhase = Ready
			r.snapshot.Queue = append(r.snapshot.Queue[:index], r.snapshot.Queue[index+1:]...)
		}
	case RunningUnlabeled:
		if index >= 0 && r.snapshot.Queue[index].Phase == Running {
			removedPhase = Running
			r.snapshot.Queue = append(r.snapshot.Queue[:index], r.snapshot.Queue[index+1:]...)
		}
	case QueueExited:
		if index >= 0 {
			removedPhase = r.snapshot.Queue[index].Phase
			r.snapshot.Queue = append(r.snapshot.Queue[:index], r.snapshot.Queue[index+1:]...)
		}
	default:
		return fmt.Errorf("unknown queue event kind %q", event.Kind)
	}

	phase, since, deadline, ok := aggregate(r.snapshot.Queue)
	if !ok {
		return r.transition(Unknown, event.At, "queue history is insufficient")
	}
	if phase == "" {
		r.snapshot.QueuePhase = ""
		r.snapshot.QueuePhaseSince = time.Time{}
		r.snapshot.QueueDeadline = time.Time{}
		return r.transition(Idle, event.At, "no actionable queue items")
	}
	if oldPhase == phase {
		if phase == Running {
			r.snapshot.QueuePhaseSince, r.snapshot.QueueDeadline = since, deadline
		} else if r.snapshot.QueueDeadline.IsZero() {
			r.snapshot.QueuePhaseSince, r.snapshot.QueueDeadline = since, deadline
		}
	} else {
		r.snapshot.QueuePhase = phase
		if oldPhase == Running && phase == Ready && removedPhase == Running {
			r.snapshot.QueuePhaseSince = event.At
			r.snapshot.QueueDeadline = event.At.Add(acceptanceTimeout)
		} else {
			r.snapshot.QueuePhaseSince, r.snapshot.QueueDeadline = since, deadline
		}
	}
	if r.snapshot.QueueDeadline.IsZero() {
		return r.transition(Unknown, event.At, "queue history is insufficient")
	}
	if !r.snapshot.QueueDeadline.After(event.At) {
		return r.transition(Down, r.snapshot.QueueDeadline, "queue progress deadline exceeded")
	}
	return r.transition(Healthy, event.At, "queue progress is within deadline")
}

func (r *replayState) transition(status Status, at time.Time, reason string) error {
	at = at.UTC()
	if status == r.snapshot.Current.Status {
		r.snapshot.Current.Reason = reason
		return nil
	}
	if at.Before(r.snapshot.Current.StartedAt) {
		return fmt.Errorf("interval transition time moved backwards")
	}
	closed := r.snapshot.Current
	closed.EndedAt = at
	if closed.EndedAt.After(closed.StartedAt) {
		r.closed = append(r.closed, closed)
	}
	r.snapshot.Current = newInterval(r.snapshot.Repository, status, at, reason)
	return nil
}

func aggregate(items []QueueItem) (Phase, time.Time, time.Time, bool) {
	var readyItem, runningItem *QueueItem
	for index := range items {
		item := &items[index]
		if item.Number <= 0 || item.PhaseSince.IsZero() || item.Deadline.IsZero() || (item.Phase != Ready && item.Phase != Running) {
			return "", time.Time{}, time.Time{}, false
		}
		if item.Phase == Running && (runningItem == nil || item.Deadline.Before(runningItem.Deadline)) {
			runningItem = item
		}
		if item.Phase == Ready && (readyItem == nil || item.Deadline.Before(readyItem.Deadline)) {
			readyItem = item
		}
	}
	if runningItem != nil {
		return Running, runningItem.PhaseSince.UTC(), runningItem.Deadline.UTC(), true
	}
	if readyItem != nil {
		return Ready, readyItem.PhaseSince.UTC(), readyItem.Deadline.UTC(), true
	}
	return "", time.Time{}, time.Time{}, true
}

func sameQueue(left, right []QueueItem) bool {
	if len(left) != len(right) {
		return false
	}
	lcopy := append([]QueueItem(nil), left...)
	rcopy := append([]QueueItem(nil), right...)
	sortQueue(lcopy)
	sortQueue(rcopy)
	for index := range lcopy {
		if lcopy[index].Number != rcopy[index].Number || lcopy[index].Phase != rcopy[index].Phase {
			return false
		}
	}
	return true
}

func queueIndex(items []QueueItem, number int) int {
	for index := range items {
		if items[index].Number == number {
			return index
		}
	}
	return -1
}

func upsertQueue(items []QueueItem, index int, item QueueItem) []QueueItem {
	if index >= 0 {
		items[index] = item
		return items
	}
	return append(items, item)
}

func sortQueue(items []QueueItem) {
	sort.Slice(items, func(i, j int) bool { return items[i].Number < items[j].Number })
}

func cloneSnapshot(snapshot Snapshot) Snapshot {
	snapshot.Queue = append([]QueueItem(nil), snapshot.Queue...)
	return snapshot
}

func newInterval(repository string, status Status, start time.Time, reason string) Interval {
	start = start.UTC()
	return Interval{ID: fmt.Sprintf("%s:%s:%d", repository, status, start.UnixNano()), Repository: repository, Status: status, StartedAt: start, Reason: reason}
}
