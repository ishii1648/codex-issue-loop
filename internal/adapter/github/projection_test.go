package github

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
	"github.com/ishii1648/codex-issue-loop/internal/platform/config"
)

func TestIssueProjectionCoversEveryLifecycleState(t *testing.T) {
	cfg := config.Defaults()
	tests := []struct {
		status               issuedomain.Status
		label, state, reason string
	}{
		{issuedomain.StatusClaiming, cfg.GitHub.RunningLabel, "OPEN", ""},
		{issuedomain.StatusClaimed, cfg.GitHub.RunningLabel, "OPEN", ""},
		{issuedomain.StatusLaunching, cfg.GitHub.RunningLabel, "OPEN", ""},
		{issuedomain.StatusRunning, cfg.GitHub.RunningLabel, "OPEN", ""},
		{issuedomain.StatusRetryWait, cfg.GitHub.RunningLabel, "OPEN", ""},
		{issuedomain.StatusResumePending, cfg.GitHub.RunningLabel, "OPEN", ""},
		{issuedomain.StatusResolvingConflict, cfg.GitHub.RunningLabel, "OPEN", ""},
		{issuedomain.StatusAwaitingChecks, cfg.GitHub.RunningLabel, "OPEN", ""},
		{issuedomain.StatusAwaitingMerge, cfg.GitHub.RunningLabel, "OPEN", ""},
		{issuedomain.StatusNeedsInput, "", "OPEN", ""},
		{issuedomain.StatusBlocked, "blocked", "OPEN", ""},
		{issuedomain.StatusFailed, cfg.GitHub.FailedLabel, "OPEN", ""},
		{issuedomain.StatusCompleted, cfg.GitHub.DoneLabel, "CLOSED", "COMPLETED"},
		{issuedomain.StatusCanceled, "", "CLOSED", "NOT_PLANNED"},
	}
	for _, test := range tests {
		t.Run(string(test.status), func(t *testing.T) {
			p, err := desiredIssueProjection(cfg, test.status)
			if err != nil || p.label != test.label || p.state != test.state || p.reason != test.reason {
				t.Fatalf("projection=%+v err=%v", p, err)
			}
			remote := Issue{State: test.state, StateReason: test.reason, Labels: []string{"bug"}}
			if test.status == issuedomain.StatusNeedsInput {
				remote.Labels = append(remote.Labels, cfg.GitHub.NeedsInputLabel)
			}
			if test.label != "" {
				remote.Labels = append(remote.Labels, strings.ToUpper(test.label))
			}
			if err := ValidateIssueProjection(cfg, remote, test.status, test.status == issuedomain.StatusNeedsInput); err != nil {
				t.Fatal(err)
			}
			remote.Labels = append(remote.Labels, cfg.GitHub.ReadyLabels...)
			add, remove := p.labelChanges(remote)
			if len(add) != 0 || !reflect.DeepEqual(remove, cfg.GitHub.ReadyLabels) {
				t.Fatalf("add=%v remove=%v", add, remove)
			}
		})
	}
	cfg.Completion.CloseIssue = false
	if err := ValidateIssueProjection(cfg, Issue{State: "OPEN", Labels: []string{cfg.GitHub.DoneLabel}}, issuedomain.StatusCompleted, false); err != nil {
		t.Fatal(err)
	}
	if _, err := desiredIssueProjection(cfg, issuedomain.StatusUnset); err != nil {
		t.Fatal(err)
	}
}

func TestIssueProjectionRetriesPartialCloseAndVerifiesReadback(t *testing.T) {
	for _, mode := range []string{"partial_failure", "unconfirmed_write"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			cfg := config.Defaults()
			cfg.GitHub.Repo = "owner/repo"
			initial := map[string]any{"number": 7, "state": "OPEN", "labels": []map[string]string{{"name": cfg.GitHub.RunningLabel}, {"name": cfg.GitHub.DoneLabel}, {"name": "bug"}}, "comments": []map[string]string{{"body": "<!-- codex-issue-loop:done -->"}}}
			partial := map[string]any{"number": 7, "state": "OPEN", "labels": []map[string]string{{"name": cfg.GitHub.DoneLabel}, {"name": "bug"}}}
			final := map[string]any{"number": 7, "state": "CLOSED", "stateReason": "COMPLETED", "labels": []map[string]string{{"name": cfg.GitHub.DoneLabel}, {"name": "bug"}}}
			for name, value := range map[string]any{"initial.json": initial, "partial.json": partial, "final.json": final} {
				data, err := json.Marshal(value)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("PROJECTION_FIXTURE", dir)
			t.Setenv("PROJECTION_MODE", mode)
			path := filepath.Join(dir, "gh")
			script := `#!/bin/sh
printf '%s\n' "$*" >> "$PROJECTION_FIXTURE/calls"
case "$1 $2" in
 "issue view")
  if [ -f "$PROJECTION_FIXTURE/closed" ]; then cat "$PROJECTION_FIXTURE/final.json"
  elif [ -f "$PROJECTION_FIXTURE/edited" ]; then cat "$PROJECTION_FIXTURE/partial.json"
  else cat "$PROJECTION_FIXTURE/initial.json"; fi ;;
 "api --method") echo '[{"body":"<!-- codex-issue-loop:done -->"}]' ;;
 "issue edit") touch "$PROJECTION_FIXTURE/edited" ;;
 "api repos/owner/repo/issues/7")
  if [ ! -f "$PROJECTION_FIXTURE/tried" ]; then
   touch "$PROJECTION_FIXTURE/tried"
   if [ "$PROJECTION_MODE" = partial_failure ]; then exit 1; fi
  else touch "$PROJECTION_FIXTURE/closed"; fi ;;
 *) exit 2 ;;
esac
`
			if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			client := CLI{Path: path}
			if err := client.ReconcileIssue(context.Background(), cfg, 7, issuedomain.StatusCompleted, false); err == nil {
				t.Fatal("partial or unconfirmed close was accepted")
			}
			if err := client.ReconcileIssue(context.Background(), cfg, 7, issuedomain.StatusCompleted, false); err != nil {
				t.Fatal(err)
			}
			if err := client.ReconcileIssue(context.Background(), cfg, 7, issuedomain.StatusCompleted, false); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(filepath.Join(dir, "calls"))
			if err != nil {
				t.Fatal(err)
			}
			calls := string(data)
			if strings.Count(calls, "issue edit") != 1 || strings.Count(calls, "api repos/") != 2 || strings.Contains(calls, "--remove-label bug") || !strings.Contains(calls, "state_reason=completed") {
				t.Fatal(calls)
			}
		})
	}
}

func TestHumanProjectionRemovesResolvedAttentionAndRetiredLabels(t *testing.T) {
	cfg := config.Defaults()
	p, err := desiredIssueProjection(cfg, issuedomain.StatusRunning)
	if err != nil {
		t.Fatal(err)
	}
	add, remove := p.labelChanges(Issue{Labels: []string{"needs-human", "codex-loop:needs-input", "triage", "do-not-automate", "bug"}})
	if !reflect.DeepEqual(add, []string{cfg.GitHub.RunningLabel}) {
		t.Fatal(add)
	}
	for _, old := range []string{"needs-human", "codex-loop:needs-input", "triage", "do-not-automate"} {
		found := false
		for _, label := range remove {
			if label == old {
				found = true
			}
		}
		if !found {
			t.Fatalf("retired attention label %s retained: %v", old, remove)
		}
	}
	for _, label := range remove {
		if label == "bug" {
			t.Fatal("classification label removed")
		}
	}
	p.human = true
	p.label = ""
	add, remove = p.labelChanges(Issue{Labels: []string{cfg.GitHub.RunningLabel}})
	if !reflect.DeepEqual(add, []string{"needs-human"}) || !reflect.DeepEqual(remove, []string{cfg.GitHub.RunningLabel}) {
		t.Fatalf("human handoff: add=%v remove=%v", add, remove)
	}
}
