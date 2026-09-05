package github

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/ishii1648/codex-issue-loop/monitor/internal/config"
	"github.com/ishii1648/codex-issue-loop/monitor/internal/model"
)

type Observer interface {
	Observe(context.Context, config.Repository, int64, time.Time) (model.Observation, error)
}

type CLI struct{ Path string }

type rawIssue struct {
	Number      int       `json:"number"`
	CreatedAt   time.Time `json:"created_at"`
	PullRequest *struct{} `json:"pull_request"`
	Labels      []struct {
		Name string `json:"name"`
	} `json:"labels"`
}

type rawEvent struct {
	ID        int64     `json:"id"`
	Event     string    `json:"event"`
	CreatedAt time.Time `json:"created_at"`
	Label     struct {
		Name string `json:"name"`
	} `json:"label"`
	Issue struct {
		Number int `json:"number"`
	} `json:"issue"`
}

func (c CLI) Observe(ctx context.Context, repo config.Repository, cursor int64, observedAt time.Time) (model.Observation, error) {
	issuesData, err := c.get(ctx, "repos/"+repo.Name+"/issues?state=open&per_page=100")
	if err != nil {
		return model.Observation{}, fmt.Errorf("list open issues for %s: %w", repo.Name, err)
	}
	eventsData, err := c.get(ctx, "repos/"+repo.Name+"/issues/events?per_page=100")
	if err != nil {
		return model.Observation{}, fmt.Errorf("list issue events for %s: %w", repo.Name, err)
	}
	var issuePages [][]rawIssue
	if err := json.Unmarshal(issuesData, &issuePages); err != nil {
		return model.Observation{}, fmt.Errorf("decode open issues for %s: %w", repo.Name, err)
	}
	var eventPages [][]rawEvent
	if err := json.Unmarshal(eventsData, &eventPages); err != nil {
		return model.Observation{}, fmt.Errorf("decode issue events for %s: %w", repo.Name, err)
	}
	eventsByIssue := map[int][]rawEvent{}
	result := model.Observation{Repository: repo.Name, ObservedAt: observedAt.UTC(), Cursor: cursor}
	for _, page := range eventPages {
		for _, event := range page {
			if event.ID > result.Cursor {
				result.Cursor = event.ID
			}
			if relevantLabel(repo, event.Label.Name) && (event.Event == "labeled" || event.Event == "unlabeled") {
				eventsByIssue[event.Issue.Number] = append(eventsByIssue[event.Issue.Number], event)
				if event.ID > cursor {
					result.EventIDs = append(result.EventIDs, event.ID)
					if event.CreatedAt.After(result.ChangedAt) {
						result.ChangedAt = event.CreatedAt.UTC()
					}
				}
			}
		}
	}
	for _, page := range issuePages {
		for _, issue := range page {
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
				item.PhaseSince = latestLabeled(eventsByIssue[issue.Number], []string{repo.RunningLabel})
				item.Deadline = item.PhaseSince.Add(repo.ProcessingTimeout.Duration)
			case ready:
				item.Phase = model.Ready
				item.PhaseSince = latestLabeled(eventsByIssue[issue.Number], repo.ReadyLabels)
				item.Deadline = item.PhaseSince.Add(repo.AcceptanceTimeout.Duration)
			}
			result.Items = append(result.Items, item)
		}
	}
	sort.Slice(result.Items, func(i, j int) bool { return result.Items[i].Number < result.Items[j].Number })
	sort.Slice(result.EventIDs, func(i, j int) bool { return result.EventIDs[i] < result.EventIDs[j] })
	return result, nil
}

func (c CLI) get(ctx context.Context, endpoint string) ([]byte, error) {
	path := c.Path
	if path == "" {
		path = "gh"
	}
	output, err := exec.CommandContext(ctx, path, "api", "--method", "GET", "--paginate", "--slurp", endpoint).Output()
	if err != nil {
		return nil, fmt.Errorf("gh api GET failed: %w", err)
	}
	return output, nil
}

func latestLabeled(events []rawEvent, labels []string) time.Time {
	wanted := stringSet(labels)
	var latest time.Time
	for _, event := range events {
		if event.Event == "labeled" && wanted[strings.ToLower(event.Label.Name)] && event.CreatedAt.After(latest) {
			latest = event.CreatedAt.UTC()
		}
	}
	return latest
}

func relevantLabel(repo config.Repository, label string) bool {
	labels := append(append(append([]string{}, repo.ReadyLabels...), repo.TerminalLabels...), repo.RunningLabel)
	return stringSet(labels)[strings.ToLower(label)]
}

func hasAny(labels []struct {
	Name string `json:"name"`
}, wanted []string) bool {
	set := stringSet(wanted)
	for _, label := range labels {
		if set[strings.ToLower(label.Name)] {
			return true
		}
	}
	return false
}

func hasLabel(labels []struct {
	Name string `json:"name"`
}, wanted string) bool {
	return hasAny(labels, []string{wanted})
}

func stringSet(values []string) map[string]bool {
	set := make(map[string]bool, len(values))
	for _, value := range values {
		set[strings.ToLower(value)] = true
	}
	return set
}
