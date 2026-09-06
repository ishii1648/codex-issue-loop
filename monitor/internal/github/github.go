package github

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/ishii1648/codex-issue-loop/monitor/internal/config"
	"github.com/ishii1648/codex-issue-loop/monitor/internal/model"
)

type Observer interface {
	Observe(context.Context, config.Repository, int64, bool, time.Time) (model.Observation, error)
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
		Number      int       `json:"number"`
		PullRequest *struct{} `json:"pull_request"`
	} `json:"issue"`
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
