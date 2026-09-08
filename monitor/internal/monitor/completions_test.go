package monitor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ishii1648/codex-issue-loop/monitor/internal/config"
	"github.com/ishii1648/codex-issue-loop/monitor/internal/model"
	"github.com/ishii1648/codex-issue-loop/monitor/internal/store"
)

type completionObserver struct {
	beforeReturn func()
	observation  model.CompletionObservation
	queueError   error
	checkpoint   *model.CompletionCheckpoint
	queueCursor  int64
}

func (o *completionObserver) Observe(_ context.Context, repo config.Repository, cursor int64, _ bool, at time.Time, checkpoint *model.CompletionCheckpoint) (model.Observation, error) {
	o.queueCursor = cursor
	if checkpoint != nil {
		copy := *checkpoint
		o.checkpoint = &copy
	}
	if o.beforeReturn != nil {
		o.beforeReturn()
	}
	observation := o.observation
	observation.At = at
	return model.Observation{Repository: repo.Name, ObservedAt: at, Cursor: observation.Cursor, CursorInitialized: true, CurrentVerified: true, Completions: observation}, o.queueError
}

func TestCompletionCommitPrecedesQueueCursorAndSurvivesInterruption(t *testing.T) {
	for _, phase := range []string{"completion write", "current write", "queue failure"} {
		t.Run(phase, func(t *testing.T) {
			disk := store.Store{Root: t.TempDir()}
			repo := config.Repository{Name: "owner/repo", DoneLabel: "done"}
			base := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)
			now := base
			observer := &completionObserver{observation: model.CompletionObservation{Cursor: 1, Verified: true, Result: "verified"}}
			runner := Runner{Store: disk, Observer: observer, Now: func() time.Time { return now }}
			if _, err := runner.Poll(context.Background(), repo); err != nil {
				t.Fatal(err)
			}
			now = base.Add(time.Minute)
			observer.observation = model.CompletionObservation{Cursor: 2, FromCursor: 1, Verified: true, Continuous: true, Result: "verified", Events: []model.CompletionLabelEvent{{CompletionEvent: model.CompletionEvent{ID: 2, IssueNumber: 7, At: base.Add(30 * time.Second)}, Label: "done"}}}
			dir := filepath.Join(disk.Root, "repositories", "owner--repo")
			var blocked string
			if phase == "queue failure" {
				observer.queueError = errors.New("queue history unavailable")
			} else {
				blocked = filepath.Join(dir, "completions.json")
				if phase == "current write" {
					blocked = filepath.Join(dir, "current.json")
				}
				observer.beforeReturn = func() {
					if err := os.Rename(blocked, blocked+".saved"); err != nil {
						t.Fatal(err)
					}
					if err := os.Mkdir(blocked, 0700); err != nil {
						t.Fatal(err)
					}
				}
			}
			if _, err := runner.Poll(context.Background(), repo); err == nil {
				t.Fatal("missing injected failure")
			}
			if blocked != "" {
				if err := os.Remove(blocked); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(blocked+".saved", blocked); err != nil {
					t.Fatal(err)
				}
			}
			completions, err := disk.Completions(repo.Name)
			if err != nil {
				t.Fatal(err)
			}
			current, err := disk.Load(repo.Name)
			if err != nil || current.EventCursor != 1 {
				t.Fatalf("cursor advanced without commit: %+v error=%v", current, err)
			}
			expected := int64(2)
			if phase == "completion write" {
				expected = 1
			}
			if completions.Checkpoint.Cursor != expected {
				t.Fatalf("completion cursor=%d want=%d", completions.Checkpoint.Cursor, expected)
			}
			observer.beforeReturn = nil
			observer.queueError = nil
			observer.observation.FromCursor = expected
			now = base.Add(2 * time.Minute)
			restarted := Runner{Store: store.Store{Root: disk.Root}, Observer: observer, Now: func() time.Time { return now }}
			if _, err := restarted.Poll(context.Background(), repo); err != nil {
				t.Fatal(err)
			}
			if observer.checkpoint.Cursor != expected || observer.queueCursor != 1 {
				t.Fatalf("restart checkpoint=%+v queue=%d", observer.checkpoint, observer.queueCursor)
			}
			completions, err = disk.Completions(repo.Name)
			if err != nil {
				t.Fatal(err)
			}
			report := model.BuildReport(repo.Name, nil, base, now)
			report.AddCompletions(completions, now, 3*time.Minute)
			if report.CompletedIssueCount == nil || *report.CompletedIssueCount != 1 {
				t.Fatal(report)
			}
		})
	}
}
