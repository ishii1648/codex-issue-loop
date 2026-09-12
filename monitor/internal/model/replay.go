package model

import (
	"fmt"
	"sort"
	"time"
)

// Unlabeled events alone cannot distinguish label replacement from queue exit.
// Their interpretation belongs to replayEvents and github.issueHistory;
// applyEvent must receive only resolved label additions and queue exits.
// Validate the entire batch before exposing any interval transitions.
func replayEvents(previous Snapshot, observation Observation) ([]QueueEvent, error) {
	events := append([]QueueEvent(nil), observation.Events...)
	sort.Slice(events, func(i, j int) bool { return events[i].ID < events[j].ID })
	queue := append([]QueueItem(nil), previous.Queue...)
	pending := map[int]QueueEvent{}
	exited := map[int]bool{}
	var result []QueueEvent
	var lastAt time.Time
	var lastID int64
	for _, event := range events {
		if previous.EventCursorInitialized && event.ID <= previous.EventCursor {
			continue
		}
		if event.ID <= 0 || event.ID > observation.Cursor || event.IssueNumber <= 0 || event.At.IsZero() || event.At.After(observation.ObservedAt) || event.At.Before(lastAt) {
			return nil, fmt.Errorf("invalid issue event ordering or timestamp")
		}
		if event.ID == lastID {
			continue
		}
		if previous.Current.Status != Unknown && event.At.Before(previous.LastObservationAt) {
			return nil, fmt.Errorf("issue event predates the verified observation boundary")
		}
		lastAt, lastID = event.At, event.ID
		index := queueIndex(queue, event.IssueNumber)
		switch event.Kind {
		case ReadyUnlabeled, RunningUnlabeled:
			phase := Ready
			if event.Kind == RunningUnlabeled {
				phase = Running
			}
			if (index < 0 && !exited[event.IssueNumber]) || (index >= 0 && queue[index].Phase == phase) {
				pending[event.IssueNumber] = event
			}
			continue
		case QueueUnproven:
			return nil, fmt.Errorf("queue reentry history is insufficient for issue %d", event.IssueNumber)
		case QueueExited, ProcessingClosed:
			exited[event.IssueNumber] = true
			delete(pending, event.IssueNumber)
			if index >= 0 {
				queue = append(queue[:index], queue[index+1:]...)
			}
		case ReadyLabeled, RunningLabeled:
			delete(exited, event.IssueNumber)
			delete(pending, event.IssueNumber)
			item := QueueItem{Number: event.IssueNumber, Phase: Ready, PhaseSince: event.At, Deadline: event.At.Add(observation.AcceptanceTimeout)}
			if event.Kind == RunningLabeled {
				item.Phase, item.Deadline = Running, event.At.Add(observation.ProcessingTimeout)
			}
			queue = upsertQueue(queue, index, item)
		default:
			return nil, fmt.Errorf("unknown queue event kind %q", event.Kind)
		}
		result = append(result, event)
	}
	if len(pending) != 0 {
		return nil, fmt.Errorf("queue exit history is insufficient")
	}
	if !sameQueue(queue, observation.Items) {
		return nil, fmt.Errorf("issue event replay disagrees with open issue snapshot")
	}
	for _, item := range observation.Items {
		replayed := queue[queueIndex(queue, item.Number)]
		if !item.PhaseSince.IsZero() && !item.PhaseSince.Equal(replayed.PhaseSince) {
			return nil, fmt.Errorf("issue event replay disagrees with open issue snapshot")
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result, nil
}
