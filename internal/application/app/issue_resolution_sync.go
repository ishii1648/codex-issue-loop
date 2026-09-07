package app

import (
	"context"
	"fmt"
	"reflect"
	"strconv"

	gh "github.com/ishii1648/codex-issue-loop/internal/adapter/github"
	"github.com/ishii1648/codex-issue-loop/internal/adapter/state"
	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
)

func (a App) synchronizeIssueResolution(ctx context.Context, planned issuePlanningContext, action issuedomain.ResolutionAction, number int) error {
	before, loadErr := planned.store.Load()
	if loadErr != nil {
		return loadErr
	}
	executable := action == issuedomain.ResolutionResume || action == issuedomain.ResolutionRetryStage
	matchesResolution := func(item *state.Issue, effectAbsent bool) bool {
		if item == nil || item.RunID != planned.issue.RunID || item.Generation != planned.issue.Generation {
			return false
		}
		if item.Suspension == nil && effectAbsent {
			return true
		}
		return item.Suspension != nil && planned.issue.Suspension != nil &&
			item.Suspension.ID == planned.issue.Suspension.ID &&
			item.Suspension.Status == planned.issue.Suspension.Status &&
			item.Suspension.Resolution == planned.issue.Suspension.Resolution &&
			item.Suspension.ResolvedAt.Equal(planned.issue.Suspension.ResolvedAt)
	}
	item := before.Issues[strconv.Itoa(number)]
	if (executable && !matchesResolution(item, state.PendingEffect(&before, number) == nil)) || (!executable && !reflect.DeepEqual(item, planned.issue)) {
		return fmt.Errorf("Issue #%d changed before resolution synchronization", number)
	}
	expectedEffect := state.PendingEffect(&before, number)
	if executable && expectedEffect == nil {
		return nil
	}
	if executable && expectedEffect.Kind != issuedomain.EffectApplyResolution {
		return fmt.Errorf("Issue #%d resolution synchronization changed", number)
	}
	client := gh.CLI{Path: planned.ghPath, Secrets: planned.cfg.RedactionValues()}
	var err error
	if action == issuedomain.ResolutionAdoptPR {
		err = client.MarkDone(ctx, planned.cfg, number, planned.issue.PullRequestURL)
	} else if action != issuedomain.ResolutionCancel {
		err = client.MarkRunning(ctx, planned.cfg, number)
	}
	if err == nil {
		err = client.ReconcileIssue(ctx, planned.cfg, number, planned.issue.Status, before.NeedsHuman(number, planned.cfg.Completion.AutoMerge))
	}
	if err != nil {
		return fmt.Errorf("synchronize Issue #%d resolution %s: %w", number, action, err)
	}
	_, err = planned.store.Update("issue_resolution_github_synced", number, planned.issue.RunID, map[string]any{"action": action}, func(snapshot *state.Snapshot) error {
		item := snapshot.Issues[strconv.Itoa(number)]
		if item == nil {
			return fmt.Errorf("Issue #%d disappeared during GitHub synchronization", number)
		}
		if (executable && !matchesResolution(item, state.PendingEffect(snapshot, number) == nil)) || (!executable &&
			(item.Status != planned.issue.Status || item.RunID != planned.issue.RunID || item.Generation != planned.issue.Generation)) {
			return fmt.Errorf("Issue #%d changed during resolution synchronization", number)
		}
		expected := issuedomain.EffectApplyResolution
		if action == issuedomain.ResolutionAdoptPR {
			expected = issuedomain.EffectMarkDone
		}
		effect := state.PendingEffect(snapshot, number)
		if effect == nil {
			return nil
		}
		if effect.Kind != expected || expectedEffect == nil || effect.ID != expectedEffect.ID {
			return fmt.Errorf("Issue #%d resolution synchronization changed", number)
		}
		if err := state.ClearEffect(snapshot, number, effect.ID); err != nil {
			return err
		}
		if action == issuedomain.ResolutionAdoptPR {
			item.Continuation = nil
			item.Suspension = nil
		}
		return nil
	})
	return err
}
