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

const eventPageLimit = 10

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
		Number      int       `json:"number"`
		PullRequest *struct{} `json:"pull_request"`
	} `json:"issue"`
}

func (c CLI) Observe(ctx context.Context, repo config.Repository, cursor int64, observedAt time.Time) (model.Observation, error) {
	issuesData, err := c.get(ctx, "repos/"+repo.Name+"/issues?state=open&per_page=100")
	if err != nil {
		return model.Observation{}, fmt.Errorf("list open issues for %s: %w", repo.Name, err)
	}
	var issuePages [][]rawIssue
	if err := json.Unmarshal(issuesData, &issuePages); err != nil {
		return model.Observation{}, fmt.Errorf("decode open issues for %s: %w", repo.Name, err)
	}
	var eventPages [][]rawEvent
	found := cursor == 0
	for page := 1; page <= eventPageLimit; page++ {
		data, err := c.getPage(ctx, fmt.Sprintf("repos/%s/issues/events?per_page=100&page=%d", repo.Name, page))
		if err != nil {
			return model.Observation{}, err
		}
		var events []rawEvent
		if err := json.Unmarshal(data, &events); err != nil {
			return model.Observation{}, fmt.Errorf("decode issue events: %w", err)
		}
		eventPages = append(eventPages, events)
		for _, event := range events {
			if event.ID == cursor {
				found = true
			}
		}
		if (cursor != 0 && found) || len(events) < 100 {
			break
		}
	}
	if !found {
		return model.Observation{}, fmt.Errorf("issue event cursor not found within %d pages", eventPageLimit)
	}
	eventsByIssue := map[int][]rawEvent{}
	result := model.Observation{Repository: repo.Name, ObservedAt: observedAt.UTC(), Cursor: cursor, Events: []model.QueueEvent{}}
	for _, page := range eventPages {
		for _, event := range page {
			if event.ID > result.Cursor {
				result.Cursor = event.ID
			}
			if event.Issue.PullRequest != nil {
				continue
			}
			if (relevantLabel(repo, event.Label.Name) && (event.Event == "labeled" || event.Event == "unlabeled")) || event.Event == "closed" || event.Event == "reopened" {
				eventsByIssue[event.Issue.Number] = append(eventsByIssue[event.Issue.Number], event)
				if event.ID > cursor {
					result.EventIDs = append(result.EventIDs, event.ID)
					change := model.QueueEvent{ID: event.ID, At: event.CreatedAt.UTC(), Number: event.Issue.Number}
					label := strings.ToLower(event.Label.Name)
					switch {
					case event.Event == "closed", event.Event == "labeled" && (stringSet(repo.TerminalLabels)[label] || stringSet(repo.ExcludeLabels)[label]):
						change.Kind = "exit"
					case event.Event == "labeled":
						change.Kind = "phase"
						change.Item = model.QueueItem{Number: change.Number, Phase: model.Ready, PhaseSince: change.At, Deadline: change.At.Add(repo.AcceptanceTimeout.Duration)}
						if label == strings.ToLower(repo.RunningLabel) {
							change.Item.Phase = model.Running
							change.Item.Deadline = change.At.Add(repo.ProcessingTimeout.Duration)
						}
					case event.Event == "unlabeled" && (stringSet(repo.ReadyLabels)[label] || label == strings.ToLower(repo.RunningLabel)):
						change.Kind = "remove"
						change.Item.Phase = model.Ready
						if label == strings.ToLower(repo.RunningLabel) {
							change.Item.Phase = model.Running
						}
					default:
						change.Kind = "unproven"
					}
					result.Events = append(result.Events, change)
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
	sort.Slice(result.Events, func(i, j int) bool { return result.Events[i].ID < result.Events[j].ID })
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
	labels = append(labels, repo.ExcludeLabels...)
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
