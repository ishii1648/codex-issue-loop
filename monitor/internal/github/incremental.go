package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ishii1648/codex-issue-loop/monitor/internal/config"
	"github.com/ishii1648/codex-issue-loop/monitor/internal/model"
)

const maxEventPages = 10

var errSnapshotChanged = errors.New("GitHub snapshot changed during observation")

func (c CLI) Observe(ctx context.Context, repo config.Repository, cursor int64, initialized bool, observedAt time.Time, checkpoint *model.CompletionCheckpoint) (result model.Observation, observeErr error) {
	result = model.Observation{Repository: repo.Name, ObservedAt: observedAt.UTC(), Cursor: cursor, CursorInitialized: true, AcceptanceTimeout: repo.AcceptanceTimeout.Duration, ProcessingTimeout: repo.ProcessingTimeout.Duration}
	defer func() {
		if observeErr != nil {
			result = model.Observation{Completions: result.Completions}
		}
	}()
	result.Completions = model.CompletionObservation{At: observedAt.UTC().Truncate(time.Second), Result: "fetch_failed"}
	var cursors []int64
	if initialized {
		cursors = append(cursors, cursor)
	}
	if checkpoint != nil {
		cursors = append(cursors, checkpoint.Cursor)
	}
	batch, scanErr := c.eventsSince(ctx, repo, cursors...)
	if scanErr != nil && len(batch.events) == 0 {
		return result, scanErr
	}
	result.Cursor = batch.head
	result.Resynchronized = initialized && !batch.found[cursor]
	if initialized && !result.Resynchronized {
		for _, raw := range batch.events {
			if raw.ID > cursor {
				if event, ok := queueEvent(repo, raw); ok {
					result.Events = append(result.Events, event)
				}
			}
		}
	}
	completion := result.Completions
	if scanErr == nil || checkpoint == nil || batch.found[checkpoint.Cursor] {
		completion = completionObservation(batch, checkpoint, observedAt)
	}
	// Repository feed verification also runs when queue reconstruction fails.
	defer func() {
		head, err := c.eventHead(ctx, repo)
		if err != nil {
			if observeErr == nil {
				observeErr = err
			}
			return
		}
		if head != batch.head {
			result.Completions.Result = "invalid_batch"
			if observeErr == nil {
				observeErr = errSnapshotChanged
			}
			return
		}
		result.Completions = completion
		if observeErr == nil {
			result.CurrentVerified = true
		}
	}()
	if scanErr != nil && initialized && !batch.found[cursor] {
		return result, scanErr
	}
	issues, err := c.openIssues(ctx, repo)
	if err != nil {
		return result, err
	}
	result.Items, err = c.queueItems(ctx, repo, issues, observedAt, result.Cursor)
	if err != nil {
		return result, err
	}
	if initialized && !result.Resynchronized {
		result.Events, err = c.resolveEvents(ctx, repo, result.Events, issues, cursor, result.Cursor, observedAt)
		if err != nil {
			if !errors.Is(err, errHistoryIncomplete) {
				return result, err
			}
			result.Events = nil
			result.Resynchronized = true
		}
	}
	check, err := c.openIssues(ctx, repo)
	if err != nil {
		return result, err
	}
	if !reflect.DeepEqual(issues, check) {
		return result, errSnapshotChanged
	}
	return result, nil
}

type repositoryEvents struct {
	events []rawEvent
	head   int64
	found  map[int64]bool
}

func (c CLI) eventsSince(ctx context.Context, repo config.Repository, cursors ...int64) (repositoryEvents, error) {
	result := repositoryEvents{found: map[int64]bool{}}
	for page := 1; page <= maxEventPages; page++ {
		events, err := c.eventPage(ctx, repo, page)
		if err != nil {
			return result, err
		}
		result.events = append(result.events, events...)
		for _, event := range events {
			if page == 1 && event.ID > result.head {
				result.head = event.ID
			}
			for _, cursor := range cursors {
				if event.ID == cursor {
					result.found[cursor] = true
				}
			}
		}
		if len(events) < 100 {
			result.found[0] = true
		}
		found := true
		for _, cursor := range cursors {
			if !result.found[cursor] {
				found = false
			}
		}
		if found || len(events) < 100 {
			break
		}
	}
	return result, nil
}

func completionObservation(batch repositoryEvents, checkpoint *model.CompletionCheckpoint, at time.Time) model.CompletionObservation {
	at = at.UTC().Truncate(time.Second)
	result := model.CompletionObservation{At: at.UTC(), Cursor: batch.head, Result: "invalid_batch"}
	if checkpoint != nil {
		result.FromCursor = checkpoint.Cursor
		result.Continuous = batch.found[checkpoint.Cursor]
		if batch.head < checkpoint.Cursor || at.Before(checkpoint.At) {
			return result
		}
	}
	seen := map[int64]rawEvent{}
	var previous rawEvent
	for _, event := range batch.events {
		if checkpoint != nil && result.Continuous && event.ID < checkpoint.Cursor {
			continue
		}
		if prior, ok := seen[event.ID]; ok {
			if !reflect.DeepEqual(prior, event) {
				return result
			}
			continue
		}
		if event.ID <= 0 || event.ID > batch.head || event.CreatedAt.IsZero() || event.CreatedAt.After(at) || (previous.ID != 0 && (event.ID > previous.ID || event.CreatedAt.After(previous.CreatedAt))) {
			return result
		}
		if checkpoint != nil && event.ID > checkpoint.Cursor && event.CreatedAt.Before(checkpoint.At) {
			return result
		}
		if event.Event == "labeled" && (event.Issue.Number <= 0 || strings.TrimSpace(event.Label.Name) == "") {
			return result
		}
		seen[event.ID] = event
		previous = event
		if event.Event == "labeled" && event.Issue.PullRequest == nil {
			result.Events = append(result.Events, model.CompletionLabelEvent{CompletionEvent: model.CompletionEvent{ID: event.ID, IssueNumber: event.Issue.Number, At: event.CreatedAt.UTC()}, Label: event.Label.Name})
		}
	}
	result.Verified = true
	result.Result = "verified"
	if checkpoint != nil && !result.Continuous {
		result.Result = "cursor_missing"
	}
	return result
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
	if events == nil {
		return nil, fmt.Errorf("invalid issue events for %s", repo.Name)
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
	if len(pages) == 0 {
		return nil, fmt.Errorf("missing open issue snapshot for %s", repo.Name)
	}
	var issues []rawIssue
	for _, page := range pages {
		if page == nil {
			return nil, fmt.Errorf("invalid open issue page for %s", repo.Name)
		}
		issues = append(issues, page...)
	}
	return issues, nil
}

func (c CLI) queueItems(ctx context.Context, repo config.Repository, issues []rawIssue, observedAt time.Time, head int64) ([]model.QueueItem, error) {
	var result []model.QueueItem
	seen := map[int]bool{}
	for _, issue := range issues {
		if issue.Number <= 0 {
			return nil, fmt.Errorf("invalid open issue snapshot")
		}
		if seen[issue.Number] || issue.State == "closed" {
			return nil, fmt.Errorf("%w: invalid open issue snapshot", errSnapshotChanged)
		}
		seen[issue.Number] = true
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
			return nil, fmt.Errorf("conflicting labels for issue %d", issue.Number)
		case running:
			item.Phase = model.Running
		case ready:
			item.Phase = model.Ready
		}
		if item.Phase == model.Ready || item.Phase == model.Running {
			events, err := c.issueEvents(ctx, repo, issue.Number)
			if err != nil {
				return nil, err
			}
			for _, event := range events {
				if event.ID > head {
					return nil, fmt.Errorf("issue history changed during observation")
				}
			}
			_, since, err := issueHistory(repo, issue, events, observedAt)
			if err != nil {
				return nil, err
			}
			item.PhaseSince = since
			if !since.IsZero() {
				timeout := repo.AcceptanceTimeout.Duration
				if item.Phase == model.Running {
					timeout = repo.ProcessingTimeout.Duration
				}
				item.Deadline = since.Add(timeout)
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
	if len(pages) == 0 {
		return nil, fmt.Errorf("missing issue history for %s#%d", repo.Name, number)
	}
	var events []rawEvent
	for _, page := range pages {
		if page == nil {
			return nil, fmt.Errorf("invalid issue history page for %s#%d", repo.Name, number)
		}
		events = append(events, page...)
	}
	return events, nil
}

func queueEvent(repo config.Repository, event rawEvent) (model.QueueEvent, bool) {
	if event.Issue.PullRequest != nil {
		return model.QueueEvent{}, false
	}
	converted := model.QueueEvent{ID: event.ID, IssueNumber: event.Issue.Number, At: event.CreatedAt.UTC()}
	if event.Event == "reopened" {
		converted.Kind = model.QueueUnproven
		return converted, true
	}
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
			converted.Kind = model.QueueUnproven
			return converted, true
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
