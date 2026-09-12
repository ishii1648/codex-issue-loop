package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ishii1648/codex-issue-loop/monitor/internal/config"
	"github.com/ishii1648/codex-issue-loop/monitor/internal/model"
)

var errHistoryIncomplete = errors.New("issue history is incomplete")

// Reverse replay is anchored in the current snapshot; a retained label alone
// does not prove that an issue was open or eligible before a reentry.
func issueHistory(repo config.Repository, issue rawIssue, events []rawEvent, boundary time.Time) ([]model.QueueEvent, time.Time, error) {
	if issue.PullRequest != nil {
		return nil, time.Time{}, nil
	}
	labels := map[string]bool{}
	for _, label := range issue.Labels {
		labels[strings.ToLower(label.Name)] = true
	}
	open := issue.State != "closed"
	phase := func() model.Phase {
		if !open {
			return ""
		}
		for _, label := range append(append([]string{}, repo.TerminalLabels...), repo.ExcludeLabels...) {
			if labels[strings.ToLower(label)] {
				return ""
			}
		}
		if labels[strings.ToLower(repo.RunningLabel)] {
			return model.Running
		}
		for _, label := range repo.ReadyLabels {
			if labels[strings.ToLower(label)] {
				return model.Ready
			}
		}
		return ""
	}
	events = append([]rawEvent(nil), events...)
	sort.Slice(events, func(i, j int) bool { return events[i].ID > events[j].ID })
	var result []model.QueueEvent
	var since time.Time
	var pendingClose int64
	completed := map[int64]bool{}
	emptyClose := map[int64]bool{}
	current := phase()
	var later time.Time
	var laterEntry model.Phase
	exclusions := stringSet(append(append([]string{}, repo.TerminalLabels...), repo.ExcludeLabels...))
	monitored := stringSet(repo.ReadyLabels)
	monitored[strings.ToLower(repo.RunningLabel)] = true
	for label := range exclusions {
		monitored[label] = true
	}
	for i, event := range events {
		if i > 0 && event.ID == events[i-1].ID {
			continue
		}
		if event.ID <= 0 || event.CreatedAt.IsZero() || (!boundary.IsZero() && event.CreatedAt.After(boundary)) || (!later.IsZero() && event.CreatedAt.After(later)) {
			return nil, time.Time{}, fmt.Errorf("invalid history for issue %d", issue.Number)
		}
		later = event.CreatedAt
		after := phase()
		label := eventLabel(event)
		if (event.Event == "labeled" || event.Event == "unlabeled") && !monitored[label] {
			continue
		}
		if event.Event == "closed" || event.Event == "reopened" || exclusions[label] {
			laterEntry = ""
		}
		switch event.Event {
		case "labeled":
			if !labels[label] {
				return nil, time.Time{}, fmt.Errorf("%w: label history disagrees with issue %d", errHistoryIncomplete, issue.Number)
			}
			delete(labels, label)
		case "unlabeled":
			if labels[label] {
				return nil, time.Time{}, fmt.Errorf("%w: label history disagrees with issue %d", errHistoryIncomplete, issue.Number)
			}
			labels[label] = true
		case "closed":
			if open {
				return nil, time.Time{}, fmt.Errorf("%w: close history disagrees with issue %d", errHistoryIncomplete, issue.Number)
			}
			open = true
		case "reopened":
			if !open {
				return nil, time.Time{}, fmt.Errorf("%w: reopen history disagrees with issue %d", errHistoryIncomplete, issue.Number)
			}
			open = false
		default:
			continue
		}
		before := phase()
		if event.Event == "closed" {
			pendingClose = event.ID
			emptyClose[event.ID] = before == ""
		}
		if event.Event == "reopened" || (event.Event == "labeled" && stringSet(repo.ReadyLabels)[label]) {
			pendingClose = 0
		}
		if pendingClose != 0 && before == model.Running {
			completed[pendingClose] = true
			pendingClose = 0
		}
		if before == model.Ready {
			pendingClose = 0
		}
		if event.Event == "closed" {
			result = append(result, model.QueueEvent{ID: event.ID, IssueNumber: issue.Number, Kind: model.QueueExited, At: event.CreatedAt.UTC()})
			continue
		}
		if before == after {
			continue
		}
		if since.IsZero() && after == current {
			since = event.CreatedAt.UTC()
		}
		if after == "" && event.Event == "unlabeled" && before == laterEntry && len(result) > 0 {
			entry := result[len(result)-1]
			if since.Equal(entry.At) {
				since = time.Time{}
			}
			result = result[:len(result)-1]
			laterEntry = ""
			continue
		}
		// A label replacement keeps the admission window until the next phase.
		if after == "" && event.Event == "unlabeled" && laterEntry != "" && before != laterEntry {
			laterEntry = ""
			continue
		}
		laterEntry = after
		kind := model.QueueExited
		if after == "" && event.Event == "unlabeled" {
			kind = model.ReadyUnlabeled
			if before == model.Running {
				kind = model.RunningUnlabeled
			}
		}
		if after == model.Ready {
			kind = model.ReadyLabeled
		}
		if after == model.Running {
			kind = model.RunningLabeled
		}
		result = append(result, model.QueueEvent{ID: event.ID, IssueNumber: issue.Number, Kind: kind, At: event.CreatedAt.UTC()})
	}
	filtered := result[:0]
	for i := range result {
		if completed[result[i].ID] {
			result[i].Kind = model.ProcessingClosed
		} else if result[i].Kind == model.ReadyUnlabeled || result[i].Kind == model.RunningUnlabeled {
			result[i].Kind = model.QueueExited
		}
		if !emptyClose[result[i].ID] || completed[result[i].ID] {
			filtered = append(filtered, result[i])
		}
	}
	return filtered, since, nil
}

func (c CLI) resolveEvents(ctx context.Context, repo config.Repository, batch []model.QueueEvent, issues []rawIssue, cursor, head int64, at time.Time) ([]model.QueueEvent, error) {
	touched := map[int]bool{}
	for _, event := range batch {
		touched[event.IssueNumber] = true
	}
	var result []model.QueueEvent
	var historyErr error
	for number := range touched {
		var issue rawIssue
		for _, candidate := range issues {
			if candidate.Number == number {
				issue = candidate
				break
			}
		}
		if issue.Number == 0 {
			data, err := c.getPage(ctx, "repos/"+repo.Name+"/issues/"+strconv.Itoa(number))
			if err != nil {
				return nil, err
			}
			if err := json.Unmarshal(data, &issue); err != nil {
				return nil, err
			}
			if issue.Number != number || (issue.State != "closed" && issue.State != "open") {
				return nil, fmt.Errorf("invalid current issue %d", number)
			}
			if issue.State == "open" && issue.PullRequest == nil {
				return nil, fmt.Errorf("open issue %d missing from snapshot", number)
			}
		}
		if issue.PullRequest != nil {
			continue
		}
		history, err := c.issueEvents(ctx, repo, number)
		if err != nil {
			return nil, err
		}
		for _, event := range history {
			if event.ID > head {
				return nil, fmt.Errorf("issue history changed during observation")
			}
		}
		events, _, err := issueHistory(repo, issue, history, at)
		if err != nil {
			if !errors.Is(err, errHistoryIncomplete) {
				return nil, err
			}
			historyErr = err
			continue
		}
		for _, event := range events {
			if event.ID > cursor {
				result = append(result, event)
			}
		}
	}
	return result, historyErr
}
