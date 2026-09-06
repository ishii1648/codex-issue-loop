package github

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ishii1648/codex-issue-loop/monitor/internal/config"
	"github.com/ishii1648/codex-issue-loop/monitor/internal/model"
)

// Reverse replay is anchored in the current snapshot; a retained label alone
// does not prove that an issue was open or eligible before a reentry.
func issueHistory(repo config.Repository, issue rawIssue, events []rawEvent, boundary time.Time) ([]model.QueueEvent, time.Time, error) {
	labels := map[string]bool{}
	for _, label := range issue.Labels {
		labels[strings.ToLower(label.Name)] = true
	}
	open := issue.State != "closed"
	phase := func() model.Phase {
		if !open || issue.PullRequest != nil {
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
	current := phase()
	var later time.Time
	laterEntry := false
	exclusions := stringSet(append(append([]string{}, repo.TerminalLabels...), repo.ExcludeLabels...))
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
		if event.Event == "closed" || event.Event == "reopened" || exclusions[label] {
			laterEntry = false
		}
		switch event.Event {
		case "labeled":
			if !labels[label] {
				return nil, time.Time{}, fmt.Errorf("label history disagrees with issue %d", issue.Number)
			}
			delete(labels, label)
		case "unlabeled":
			if labels[label] {
				return nil, time.Time{}, fmt.Errorf("label history disagrees with issue %d", issue.Number)
			}
			labels[label] = true
		case "closed":
			if open {
				return nil, time.Time{}, fmt.Errorf("close history disagrees with issue %d", issue.Number)
			}
			open = true
		case "reopened":
			if !open {
				return nil, time.Time{}, fmt.Errorf("reopen history disagrees with issue %d", issue.Number)
			}
			open = false
		default:
			continue
		}
		before := phase()
		if before == after {
			continue
		}
		if since.IsZero() && after == current {
			since = event.CreatedAt.UTC()
		}
		// A label replacement keeps the admission window until the next phase.
		if after == "" && event.Event == "unlabeled" && laterEntry {
			laterEntry = false
			continue
		}
		laterEntry = after != ""
		kind := model.QueueExited
		if after == model.Ready {
			kind = model.ReadyLabeled
		}
		if after == model.Running {
			kind = model.RunningLabeled
		}
		result = append(result, model.QueueEvent{ID: event.ID, IssueNumber: issue.Number, Kind: kind, At: event.CreatedAt.UTC()})
	}
	return result, since, nil
}

func (c CLI) resolveEvents(ctx context.Context, repo config.Repository, batch []model.QueueEvent, issues []rawIssue, cursor, head int64, at time.Time) ([]model.QueueEvent, error) {
	touched := map[int]bool{}
	for _, event := range batch {
		touched[event.IssueNumber] = true
	}
	var result []model.QueueEvent
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
			return nil, err
		}
		for _, event := range events {
			if event.ID > cursor {
				result = append(result, event)
			}
		}
	}
	return result, nil
}
