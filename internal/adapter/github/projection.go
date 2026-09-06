package github

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
	"github.com/ishii1648/codex-issue-loop/internal/platform/config"
)

type issueProjection struct {
	label      string
	state      string
	reason     string
	managed    []string
	human      bool
	humanLabel string
}

func desiredIssueProjection(cfg config.Config, status issuedomain.Status) (issueProjection, error) {
	p := issueProjection{state: "OPEN", human: status == issuedomain.StatusNeedsInput, humanLabel: cfg.GitHub.NeedsInputLabel, managed: append([]string{}, cfg.GitHub.ReadyLabels...)}
	p.managed = append(p.managed, cfg.GitHub.RunningLabel, cfg.GitHub.NeedsInputLabel, cfg.GitHub.DoneLabel, cfg.GitHub.FailedLabel, "needs-human", "codex-loop:needs-input", "triage", "do-not-automate")
	blocked := cfg.GitHub.FailedLabel
	for _, label := range cfg.GitHub.ExcludeLabels {
		if labelKey(label) == "blocked" {
			blocked = label
			p.managed = append(p.managed, label)
		}
	}
	switch status {
	case issuedomain.StatusUnset:
		p.state = ""
		p.managed = []string{cfg.GitHub.NeedsInputLabel, "codex-loop:needs-input"}
	case issuedomain.StatusClaiming, issuedomain.StatusClaimed, issuedomain.StatusLaunching, issuedomain.StatusRunning,
		issuedomain.StatusRetryWait, issuedomain.StatusResumePending, issuedomain.StatusResolvingConflict,
		issuedomain.StatusAwaitingChecks, issuedomain.StatusAwaitingMerge:
		p.label = cfg.GitHub.RunningLabel
	case issuedomain.StatusNeedsInput:
		p.label = ""
	case issuedomain.StatusBlocked:
		p.label = blocked
	case issuedomain.StatusFailed:
		p.label = cfg.GitHub.FailedLabel
	case issuedomain.StatusCompleted:
		p.label = cfg.GitHub.DoneLabel
		if cfg.Completion.CloseIssue {
			p.state, p.reason = "CLOSED", "COMPLETED"
		}
	case issuedomain.StatusCanceled:
		p.state, p.reason = "CLOSED", "NOT_PLANNED"
	default:
		return p, fmt.Errorf("cannot project unmanaged Issue status %q", status)
	}
	return p, nil
}

func (p issueProjection) labelChanges(issue Issue) (add, remove []string) {
	present := map[string]bool{}
	for _, label := range issue.Labels {
		present[labelKey(label)] = true
	}
	if p.human && !present[labelKey(p.humanLabel)] {
		add = append(add, p.humanLabel)
	}
	if p.label != "" && !present[labelKey(p.label)] {
		add = append(add, p.label)
	}
	seen := map[string]bool{}
	for _, label := range p.managed {
		key := labelKey(label)
		if label != "" && !(p.human && key == labelKey(p.humanLabel)) && key != labelKey(p.label) && present[key] && !seen[key] {
			remove = append(remove, label)
			seen[key] = true
		}
	}
	return add, remove
}

func (p issueProjection) stateMatches(issue Issue) bool {
	return p.state == "" || strings.EqualFold(issue.State, p.state) && (p.reason == "" || strings.EqualFold(issue.StateReason, p.reason))
}

func ValidateIssueProjection(cfg config.Config, issue Issue, status issuedomain.Status, human bool) error {
	p, err := desiredIssueProjection(cfg, status)
	if err != nil {
		return err
	}
	p.human = human
	if status == issuedomain.StatusUnset && human {
		p.managed = append(p.managed, cfg.GitHub.ReadyLabels...)
	}
	if human && p.label == cfg.GitHub.RunningLabel {
		p.label = ""
	}
	add, remove := p.labelChanges(issue)
	if len(add) > 0 || len(remove) > 0 || !p.stateMatches(issue) {
		return fmt.Errorf("Issue #%d GitHub projection did not converge", issue.Number)
	}
	return nil
}

// ReconcileIssue projects only loop-owned labels and open/closed state. Success
// requires readback; comments and successful partial writes are not evidence
// that the complete projection reached GitHub.
func (c CLI) ReconcileIssue(ctx context.Context, cfg config.Config, number int, status issuedomain.Status, human bool) error {
	p, err := desiredIssueProjection(cfg, status)
	if err != nil {
		return err
	}
	p.human = human
	if status == issuedomain.StatusUnset && human {
		p.managed = append(p.managed, cfg.GitHub.ReadyLabels...)
	}
	if human && p.label == cfg.GitHub.RunningLabel {
		p.label = ""
	}
	remote, err := c.Get(ctx, cfg, number)
	if err != nil {
		return err
	}
	add, remove := p.labelChanges(remote)
	changed := len(add) > 0 || len(remove) > 0 || !p.stateMatches(remote)
	if !changed {
		return nil
	}
	if len(add) > 0 || len(remove) > 0 {
		if err := c.editLabels(ctx, cfg.GitHub.Repo, number, add, remove); err != nil {
			return err
		}
	}
	if !p.stateMatches(remote) {
		path := c.Path
		if path == "" {
			path = "gh"
		}
		args := []string{"api", fmt.Sprintf("repos/%s/issues/%d", cfg.GitHub.Repo, number), "--method", "PATCH", "-f", "state=" + strings.ToLower(p.state)}
		if p.state == "CLOSED" {
			args = append(args, "-f", "state_reason="+strings.ToLower(p.reason))
		}
		out, err := exec.CommandContext(ctx, path, args...).CombinedOutput()
		if err != nil {
			return c.commandError(ctx, path, "project Issue state", err, out)
		}
	}
	remote, err = c.Get(ctx, cfg, number)
	if err != nil {
		return err
	}
	add, remove = p.labelChanges(remote)
	if len(add) > 0 || len(remove) > 0 || !p.stateMatches(remote) {
		return fmt.Errorf("Issue #%d GitHub projection did not converge", number)
	}
	return nil
}
