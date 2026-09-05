package github

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ishii1648/codex-issue-loop/monitor/internal/config"
	"github.com/ishii1648/codex-issue-loop/monitor/internal/model"
)

const maxEventPages = 10

func (c CLI) Observe(ctx context.Context, repo config.Repository, cursor int64, initialized bool, observedAt time.Time) (model.Observation, error) {
	result := model.Observation{
		Repository:        repo.Name,
		ObservedAt:        observedAt.UTC(),
		Cursor:            cursor,
		CursorInitialized: true,
		AcceptanceTimeout: repo.AcceptanceTimeout.Duration,
		ProcessingTimeout: repo.ProcessingTimeout.Duration,
	}
	if initialized {
		events, head, err := c.eventsSince(ctx, repo, cursor)
		if err != nil {
			return model.Observation{}, err
		}
		result.Events = events
		result.Cursor = head
	} else {
		head, err := c.eventHead(ctx, repo)
		if err != nil {
			return model.Observation{}, err
		}
		result.Cursor = head
	}
	issues, err := c.openIssues(ctx, repo)
	if err != nil {
		return model.Observation{}, err
	}
	result.Items, err = c.queueItems(ctx, repo, issues, !initialized)
	if err != nil {
		return model.Observation{}, err
	}
	return result, nil
}

func (c CLI) eventsSince(ctx context.Context, repo config.Repository, cursor int64) ([]model.QueueEvent, int64, error) {
	var result []model.QueueEvent
	head := cursor
	for page := 1; page <= maxEventPages; page++ {
		events, err := c.eventPage(ctx, repo, page)
		if err != nil {
			return nil, cursor, err
		}
		if page == 1 {
			for _, event := range events {
				if event.ID > head {
					head = event.ID
				}
			}
		}
		found := false
		for _, event := range events {
			if event.ID == cursor {
				found = true
				break
			}
			if event.ID > cursor {
				if converted, ok := queueEvent(repo, event); ok {
					result = append(result, converted)
				}
			}
		}
		if found {
			return result, head, nil
		}
		if len(events) < 100 {
			if cursor == 0 {
				return result, head, nil
			}
			return nil, cursor, fmt.Errorf("event cursor %d was not found for %s", cursor, repo.Name)
		}
	}
	return nil, cursor, fmt.Errorf("event cursor %d was not found within %d pages for %s", cursor, maxEventPages, repo.Name)
}

func (c CLI) eventHead(ctx context.Context, repo config.Repository) (int64, error) {
	events, err := c.eventPage(ctx, repo, 1)
	if err != nil {
		return 0, err
	}
	var head int64
	for _, event := range events {
		if event.ID > head {
			head = event.ID
		}
	}
	return head, nil
}

func (c CLI) eventPage(ctx context.Context, repo config.Repository, page int) ([]rawEvent, error) {
	endpoint := "repos/" + repo.Name + "/issues/events?per_page=100&page=" + strconv.Itoa(page)
	data, err := c.getPage(ctx, endpoint)
	if err != nil {
		return nil, fmt.Errorf("list issue events for %s: %w", repo.Name, err)
	}
	var events []rawEvent
	if err := json.Unmarshal(data, &events); err != nil {
		return nil, fmt.Errorf("decode issue events for %s: %w", repo.Name, err)
	}
	return events, nil
}

func (c CLI) openIssues(ctx context.Context, repo config.Repository) ([]rawIssue, error) {
	data, err := c.get(ctx, "repos/"+repo.Name+"/issues?state=open&per_page=100")
	if err != nil {
		return nil, fmt.Errorf("list open issues for %s: %w", repo.Name, err)
	}
	var pages [][]rawIssue
	if err := json.Unmarshal(data, &pages); err != nil {
		return nil, fmt.Errorf("decode open issues for %s: %w", repo.Name, err)
	}
	var issues []rawIssue
	for _, page := range pages {
		issues = append(issues, page...)
	}
	return issues, nil
}

func (c CLI) queueItems(ctx context.Context, repo config.Repository, issues []rawIssue, bootstrap bool) ([]model.QueueItem, error) {
	var result []model.QueueItem
	for _, issue := range issues {
		if issue.PullRequest != nil || hasAny(issue.Labels, repo.TerminalLabels) || hasAny(issue.Labels, repo.ExcludeLabels) {
			continue
		}
		ready := hasAny(issue.Labels, repo.ReadyLabels)
		running := hasLabel(issue.Labels, repo.RunningLabel)
		if !ready && !running {
			continue
		}
		item := model.QueueItem{Number: issue.Number}
		switch {
		case ready && running:
			item.Phase = model.Phase("conflicting-labels")
		case running:
			item.Phase = model.Running
		case ready:
			item.Phase = model.Ready
		}
		if bootstrap && (item.Phase == model.Ready || item.Phase == model.Running) {
			events, err := c.issueEvents(ctx, repo, issue.Number)
			if err != nil {
				return nil, err
			}
			if item.Phase == model.Running {
				item.PhaseSince = latestLabeled(events, []string{repo.RunningLabel})
				item.Deadline = item.PhaseSince.Add(repo.ProcessingTimeout.Duration)
			} else {
				item.PhaseSince = latestLabeled(events, repo.ReadyLabels)
				item.Deadline = item.PhaseSince.Add(repo.AcceptanceTimeout.Duration)
			}
		}
		result = append(result, item)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Number < result[j].Number })
	return result, nil
}

func (c CLI) issueEvents(ctx context.Context, repo config.Repository, number int) ([]rawEvent, error) {
	endpoint := "repos/" + repo.Name + "/issues/" + strconv.Itoa(number) + "/events?per_page=100"
	data, err := c.get(ctx, endpoint)
	if err != nil {
		return nil, fmt.Errorf("list events for %s#%d: %w", repo.Name, number, err)
	}
	var pages [][]rawEvent
	if err := json.Unmarshal(data, &pages); err != nil {
		return nil, fmt.Errorf("decode events for %s#%d: %w", repo.Name, number, err)
	}
	var events []rawEvent
	for _, page := range pages {
		events = append(events, page...)
	}
	return events, nil
}

func queueEvent(repo config.Repository, event rawEvent) (model.QueueEvent, bool) {
	if event.Issue.PullRequest != nil {
		return model.QueueEvent{}, false
	}
	converted := model.QueueEvent{ID: event.ID, IssueNumber: event.Issue.Number, At: event.CreatedAt.UTC()}
	if event.Event == "closed" {
		converted.Kind = model.QueueExited
		return converted, true
	}
	if event.Event != "labeled" && event.Event != "unlabeled" {
		return model.QueueEvent{}, false
	}
	switch {
	case stringSet(repo.ReadyLabels)[eventLabel(event)]:
		if event.Event == "labeled" {
			converted.Kind = model.ReadyLabeled
		} else {
			converted.Kind = model.ReadyUnlabeled
		}
	case eventLabel(event) == strings.ToLower(repo.RunningLabel):
		if event.Event == "labeled" {
			converted.Kind = model.RunningLabeled
		} else {
			converted.Kind = model.RunningUnlabeled
		}
	case stringSet(append(append([]string{}, repo.TerminalLabels...), repo.ExcludeLabels...))[eventLabel(event)]:
		if event.Event != "labeled" {
			return model.QueueEvent{}, false
		}
		converted.Kind = model.QueueExited
	default:
		return model.QueueEvent{}, false
	}
	return converted, true
}

func eventLabel(event rawEvent) string { return strings.ToLower(event.Label.Name) }

func (c CLI) getPage(ctx context.Context, endpoint string) ([]byte, error) {
	path := c.Path
	if path == "" {
		path = "gh"
	}
	output, err := exec.CommandContext(ctx, path, "api", "--method", "GET", endpoint).Output()
	if err != nil {
		return nil, fmt.Errorf("gh api GET failed: %w", err)
	}
	return output, nil
}
