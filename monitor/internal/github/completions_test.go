package github

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ishii1648/codex-issue-loop/monitor/internal/config"
	"github.com/ishii1648/codex-issue-loop/monitor/internal/model"
)

func TestCompletionRawEventsIndependentFromQueue(t *testing.T) {
	base := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)
	for _, queueFailure := range []bool{false, true} {
		t.Run(fmt.Sprint(queueFailure), func(t *testing.T) {
			dir := t.TempDir()
			events := []rawEvent{}
			for i, spec := range []struct {
				event, label string
				pr           bool
			}{{"labeled", "done", false}, {"unlabeled", "done", false}, {"reopened", "", false}, {"labeled", "failed", false}, {"labeled", "blocked", false}, {"labeled", "needs-human", false}, {"unlabeled", "ready", false}, {"unlabeled", "running", false}, {"closed", "", false}, {"labeled", "done", true}} {
				event := rawEvent{ID: int64(11 + i), Event: spec.event, CreatedAt: base.Add(time.Duration(i) * time.Second)}
				event.Issue.Number = i + 1
				event.Label.Name = spec.label
				if spec.pr {
					event.Issue.PullRequest = &struct{}{}
				}
				events = append([]rawEvent{event}, events...)
			}
			events = append(events, rawEvent{ID: 10, Event: "renamed", CreatedAt: base.Add(-time.Second)})
			writeEventPage(t, filepath.Join(dir, "events.json"), events)
			issueResponse := "printf '[[]]'"
			if queueFailure {
				issueResponse = "exit 7"
			}
			script := filepath.Join(dir, "gh")
			body := "#!/bin/sh\ncase \"$*\" in\n *issues/events*) cat '" + filepath.Join(dir, "events.json") + "' ;;\n *issues\\?*) " + issueResponse + " ;;\n *) exit 8 ;;\nesac\n"
			if err := os.WriteFile(script, []byte(body), 0700); err != nil {
				t.Fatal(err)
			}
			checkpoint := &model.CompletionCheckpoint{EpochID: 1, At: base, Cursor: 10}
			obs, err := (CLI{Path: script}).Observe(context.Background(), config.Repository{Name: "owner/repo"}, 10, true, base.Add(time.Minute), checkpoint)
			if queueFailure && err == nil {
				t.Fatal("missing queue failure")
			}
			if !obs.Completions.Verified || !obs.Completions.Continuous {
				t.Fatalf("completion=%+v error=%v", obs.Completions, err)
			}
			history := model.CompletionHistory{SchemaVersion: 1, Repository: "owner/repo"}
			if err := history.Apply("done", model.CompletionObservation{At: base, Cursor: 10, Verified: true, Result: "verified"}); err != nil {
				t.Fatal(err)
			}
			if err := history.Apply("done", obs.Completions); err != nil {
				t.Fatal(err)
			}
			report := model.BuildReport(history.Repository, nil, base, base.Add(time.Minute))
			report.AddCompletions(&history, base.Add(time.Minute), time.Minute)
			if report.CompletedIssueCount == nil || *report.CompletedIssueCount != 1 {
				t.Fatal(report)
			}
		})
	}
}

func TestCompletionBatchValidationAndIndependentCursors(t *testing.T) {
	at := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)
	cp := &model.CompletionCheckpoint{EpochID: 1, At: at, Cursor: 10}
	event := rawEvent{ID: 11, Event: "labeled", CreatedAt: at.Add(time.Second)}
	event.Issue.Number = 1
	event.Label.Name = "done"
	anchor := rawEvent{ID: 10, Event: "renamed", CreatedAt: at.Add(-time.Second)}
	for _, tc := range []struct {
		name                 string
		alter                func(*repositoryEvents)
		verified, continuous bool
	}{
		{"complete", func(*repositoryEvents) {}, true, true},
		{"missing completion cursor", func(b *repositoryEvents) { delete(b.found, 10) }, true, false},
		{"missing queue cursor", func(b *repositoryEvents) { delete(b.found, 5) }, true, true},
		{"same event twice", func(b *repositoryEvents) { b.events = append([]rawEvent{event}, b.events...) }, true, true},
		{"conflicting duplicate", func(b *repositoryEvents) { e := event; e.Label.Name = "failed"; b.events = append(b.events, e) }, false, true},
		{"late event", func(b *repositoryEvents) { b.events[0].CreatedAt = at.Add(-time.Second) }, false, true},
		{"future event", func(b *repositoryEvents) { b.events[0].CreatedAt = at.Add(time.Hour) }, false, true},
		{"missing identity", func(b *repositoryEvents) { b.events[0].Issue.Number = 0 }, false, true},
		{"head rollback", func(b *repositoryEvents) { b.head = 9 }, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			batch := repositoryEvents{events: []rawEvent{event, anchor}, head: 11, found: map[int64]bool{10: true, 5: true}}
			tc.alter(&batch)
			got := completionObservation(batch, cp, at.Add(time.Minute))
			if got.Verified != tc.verified || got.Continuous != tc.continuous {
				t.Fatal(got)
			}
		})
	}
}

func TestSharedScanReachesBothCursors(t *testing.T) {
	dir := t.TempDir()
	page := make([]rawEvent, 100)
	for i := range page {
		page[i].ID = int64(300 - i)
	}
	writeEventPage(t, filepath.Join(dir, "one"), page)
	writeEventPage(t, filepath.Join(dir, "two"), []rawEvent{{ID: 200}, {ID: 150}})
	script := filepath.Join(dir, "gh")
	body := "#!/bin/sh\ncase \"$*\" in\n *page=1) cat '" + filepath.Join(dir, "one") + "' ;;\n *page=2) cat '" + filepath.Join(dir, "two") + "' ;;\n *) exit 9 ;;\nesac\n"
	if err := os.WriteFile(script, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	for _, cursors := range [][]int64{{250, 150}, {150, 250}, {1, 250}, {250, 1}} {
		batch, err := (CLI{Path: script}).eventsSince(context.Background(), config.Repository{Name: "owner/repo"}, cursors...)
		if err != nil || batch.head != 300 || !batch.found[250] || batch.found[1] {
			t.Fatalf("batch=%+v error=%v", batch, err)
		}
		if cursors[0] != 1 && cursors[1] != 1 && !batch.found[150] {
			t.Fatal("second cursor not fetched")
		}
	}
}

func TestDoneWithoutCloseOrQueuePhaseChangeAndIncompleteIssueHistory(t *testing.T) {
	base := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)
	for _, incomplete := range []bool{false, true} {
		t.Run(fmt.Sprint(incomplete), func(t *testing.T) {
			dir := t.TempDir()
			events := []rawEvent{}
			for i, eventType := range []string{"labeled", "unlabeled", "labeled"} {
				event := rawEvent{ID: int64(i + 1), Event: eventType, CreatedAt: base.Add(time.Duration(i-1) * time.Second)}
				event.Issue.Number = 7
				event.Label.Name = "done"
				events = append([]rawEvent{event}, events...)
			}
			writeEventPage(t, filepath.Join(dir, "feed"), events)
			history := events
			if incomplete {
				history = []rawEvent{events[0], events[2]}
			}
			data, err := json.Marshal([][]rawEvent{history})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "history"), data, 0600); err != nil {
				t.Fatal(err)
			}
			script := filepath.Join(dir, "gh")
			body := `#!/bin/sh
case "$*" in
 *issues/events*) cat '` + filepath.Join(dir, "feed") + `' ;;
 *issues/7/events*) cat '` + filepath.Join(dir, "history") + `' ;;
 *issues\?*) printf '%s' '[[{"number":7,"state":"open","labels":[{"name":"done"}]}]]' ;;
 *) exit 9 ;;
esac
`
			if err := os.WriteFile(script, []byte(body), 0700); err != nil {
				t.Fatal(err)
			}
			repo := config.Repository{Name: "owner/repo", DoneLabel: "done", TerminalLabels: []string{"done"}}
			checkpoint := &model.CompletionCheckpoint{EpochID: 1, At: base, Cursor: 1}
			observation, err := (CLI{Path: script}).Observe(context.Background(), repo, 1, true, base.Add(time.Minute), checkpoint)
			if err != nil || observation.Resynchronized != incomplete || !observation.CurrentVerified || len(observation.Events) != 0 {
				t.Fatalf("observation=%+v error=%v", observation, err)
			}
			if !observation.Completions.Verified || !observation.Completions.Continuous {
				t.Fatal(observation.Completions)
			}
			completed := 0
			for _, event := range observation.Completions.Events {
				if event.ID > 1 {
					completed++
				}
			}
			if completed != 1 {
				t.Fatalf("completions=%+v", observation.Completions)
			}
		})
	}
}

func TestCompletionBoundariesUseWholeSeconds(t *testing.T) {
	base := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)
	initial := completionObservation(repositoryEvents{head: 0}, nil, base.Add(900*time.Millisecond))
	if !initial.Verified || !initial.At.Equal(base) {
		t.Fatal(initial)
	}
	cp := &model.CompletionCheckpoint{EpochID: 1, At: initial.At, Cursor: 0}
	event := rawEvent{ID: 1, CreatedAt: base, Event: "labeled"}
	event.Label.Name = "done"
	event.Issue.Number = 7
	batch := repositoryEvents{head: 1, events: []rawEvent{event}, found: map[int64]bool{0: true}}
	next := completionObservation(batch, cp, base.Add(5*time.Second))
	if !next.Verified || !next.Continuous || len(next.Events) != 1 {
		t.Fatal(next)
	}
}

func TestOlderCursorPageFailureDoesNotDiscardVerifiedOtherCursor(t *testing.T) {
	base := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	events := make([]rawEvent, 100)
	for i := range events {
		events[i] = rawEvent{ID: int64(200 - i), Event: "renamed", CreatedAt: base}
	}
	writeEventPage(t, filepath.Join(dir, "feed"), events)
	script := filepath.Join(dir, "gh")
	body := `#!/bin/sh
case "$*" in
 *issues/events*page=1) cat '` + filepath.Join(dir, "feed") + `' ;;
 *issues/events*) exit 7 ;;
 *issues\?*) printf '[[]]' ;;
 *) exit 9 ;;
esac
`
	if err := os.WriteFile(script, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	for _, queueBehind := range []bool{false, true} {
		queueCursor, completionCursor := int64(150), int64(1)
		if queueBehind {
			queueCursor, completionCursor = completionCursor, queueCursor
		}
		cp := &model.CompletionCheckpoint{EpochID: 1, At: base, Cursor: completionCursor}
		observation, err := (CLI{Path: script}).Observe(context.Background(), config.Repository{Name: "owner/repo"}, queueCursor, true, base.Add(time.Minute), cp)
		if queueBehind {
			if err == nil || !observation.Completions.Verified || !observation.Completions.Continuous {
				t.Fatalf("completion discarded: %+v error=%v", observation, err)
			}
		} else if err != nil || !observation.CurrentVerified || observation.Completions.Verified || observation.Completions.Result != "fetch_failed" {
			t.Fatalf("queue discarded: %+v error=%v", observation, err)
		}
	}
}
