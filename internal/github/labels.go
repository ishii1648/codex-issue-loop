package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"strings"

	"github.com/ishii1648/codex-issue-loop/internal/config"
)

type LabelSpec struct {
	Name        string `json:"name"`
	Color       string `json:"color"`
	Description string `json:"description"`
}

type LabelAction struct {
	Action              string    `json:"action"`
	Desired             LabelSpec `json:"desired"`
	ExistingColor       string    `json:"existing_color,omitempty"`
	ExistingDescription string    `json:"existing_description,omitempty"`
	MetadataDiffers     bool      `json:"metadata_differs,omitempty"`
}

type LabelFailure struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

type LabelBootstrapResult struct {
	Repository             string         `json:"repository"`
	Applied                bool           `json:"applied"`
	ExistingMetadataPolicy string         `json:"existing_metadata_policy"`
	DeletesLabels          bool           `json:"deletes_labels"`
	Actions                []LabelAction  `json:"actions"`
	Created                []string       `json:"created,omitempty"`
	Failures               []LabelFailure `json:"failures,omitempty"`
}

type LabelBootstrapError struct{ Failures []LabelFailure }

func (e *LabelBootstrapError) Error() string {
	return fmt.Sprintf("failed to create %d GitHub label(s); successful changes were preserved and rerunning is safe", len(e.Failures))
}

func RequiredLabelSpecs(cfg config.Config) []LabelSpec {
	seen := map[string]bool{}
	specs := []LabelSpec{}
	add := func(names []string, color, description string) {
		for _, name := range names {
			key := strings.ToLower(name)
			if name == "" || seen[key] {
				continue
			}
			seen[key] = true
			specs = append(specs, LabelSpec{Name: name, Color: color, Description: description})
		}
	}
	add(cfg.GitHub.ReadyLabels, "0E8A16", "Ready for codex-issue-loop")
	add([]string{cfg.GitHub.RunningLabel}, "1D76DB", "Being processed by codex-issue-loop")
	add([]string{cfg.GitHub.NeedsInputLabel}, "FBCA04", "Waiting for user input in codex-issue-loop")
	add([]string{cfg.GitHub.FailedLabel}, "D73A4A", "codex-issue-loop processing failed")
	add([]string{cfg.GitHub.DoneLabel}, "5319E7", "Completed by codex-issue-loop")
	add(cfg.Queue.PriorityLabels, "BFD4F2", "Priority for codex-issue-loop queue ordering")
	for _, name := range cfg.GitHub.ExcludeLabels {
		if strings.EqualFold(name, "blocked") {
			add([]string{name}, "B60205", "Blocked from automated processing")
		}
	}
	return specs
}

func (c CLI) BootstrapLabels(ctx context.Context, cfg config.Config, apply bool) (LabelBootstrapResult, error) {
	result := LabelBootstrapResult{
		Repository: cfg.GitHub.Repo, Applied: apply, ExistingMetadataPolicy: "preserve", DeletesLabels: false,
	}
	existing, err := c.listLabels(ctx, cfg.GitHub.Repo)
	if err != nil {
		return result, err
	}
	for _, desired := range RequiredLabelSpecs(cfg) {
		current, exists := existing[strings.ToLower(desired.Name)]
		action := LabelAction{Action: "create", Desired: desired}
		if exists {
			action.Action = "preserve"
			action.ExistingColor = current.Color
			action.ExistingDescription = current.Description
			action.MetadataDiffers = !strings.EqualFold(current.Color, desired.Color) || current.Description != desired.Description
		}
		result.Actions = append(result.Actions, action)
	}
	if !apply {
		return result, nil
	}
	path := c.Path
	if path == "" {
		path = "gh"
	}
	for index := range result.Actions {
		action := &result.Actions[index]
		if action.Action != "create" {
			continue
		}
		out, createErr := exec.CommandContext(ctx, path, "label", "create", action.Desired.Name, "--repo", cfg.GitHub.Repo, "--color", action.Desired.Color, "--description", action.Desired.Description).CombinedOutput()
		if createErr == nil {
			result.Created = append(result.Created, action.Desired.Name)
			continue
		}
		// A concurrent bootstrap may have created the same label after the plan.
		// Re-read before reporting a failure; never use --force or overwrite it.
		latest, listErr := c.listLabels(ctx, cfg.GitHub.Repo)
		if listErr == nil {
			if current, exists := latest[strings.ToLower(action.Desired.Name)]; exists {
				action.Action = "preserve"
				action.ExistingColor = current.Color
				action.ExistingDescription = current.Description
				action.MetadataDiffers = !strings.EqualFold(current.Color, action.Desired.Color) || current.Description != action.Desired.Description
				continue
			}
		}
		result.Failures = append(result.Failures, LabelFailure{Name: action.Desired.Name, Reason: c.safe(out)})
	}
	sort.Strings(result.Created)
	if len(result.Failures) > 0 {
		return result, &LabelBootstrapError{Failures: result.Failures}
	}
	return result, nil
}

func (c CLI) listLabels(ctx context.Context, repository string) (map[string]LabelSpec, error) {
	path := c.Path
	if path == "" {
		path = "gh"
	}
	command := exec.CommandContext(ctx, path, "label", "list", "--repo", repository, "--limit", "1000", "--json", "name,color,description")
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	if err != nil {
		return nil, fmt.Errorf("list GitHub labels: %w: %s", err, c.safe(stderr.Bytes()))
	}
	var labels []LabelSpec
	if err := json.Unmarshal(stdout.Bytes(), &labels); err != nil {
		return nil, fmt.Errorf("decode GitHub labels: %w", err)
	}
	result := make(map[string]LabelSpec, len(labels))
	for _, label := range labels {
		result[strings.ToLower(label.Name)] = label
	}
	return result, nil
}
