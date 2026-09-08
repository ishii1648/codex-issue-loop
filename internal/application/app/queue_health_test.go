package app

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	gh "github.com/ishii1648/codex-issue-loop/internal/adapter/github"
	"github.com/ishii1648/codex-issue-loop/internal/adapter/state"
	"github.com/ishii1648/codex-issue-loop/internal/adapter/webhook"
	"github.com/ishii1648/codex-issue-loop/internal/platform/layout"
	"github.com/ishii1648/codex-issue-loop/internal/platform/registry"
)

func TestQueueHealthFailsAfterTwoLocalReconciliationIntervalsUntilIssueIsDurable(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	interval := time.Minute
	sweep := webhook.SweepState{Pages: map[int]webhook.SweepPageState{1: {Issues: []gh.Issue{{Number: 206}}}}}
	delivery := webhook.Delivery{DeliveryID: "sweep-206-reconciled", Event: "issues", Action: "reconciled", IssueNumber: 206, AcceptedAt: now.Add(-2*interval - time.Second)}
	health := assessQueueHealth(now, interval, state.Snapshot{Issues: map[string]*state.Issue{}}, sweep, []webhook.Delivery{delivery})
	if health.OK || health.Code != "ready_issue_stalled" || len(health.StalledIssues) != 1 || health.StalledIssues[0] != 206 {
		t.Fatalf("health=%+v", health)
	}
	health = assessQueueHealth(now, interval, state.Snapshot{Issues: map[string]*state.Issue{"206": {Number: 206}}}, sweep, []webhook.Delivery{delivery})
	if !health.OK || len(health.StalledIssues) != 0 {
		t.Fatalf("durable claim remained stalled: %+v", health)
	}
}

func TestQueueHealthRejectsUnboundedDuplicateMailbox(t *testing.T) {
	now := time.Now().UTC()
	deliveries := make([]webhook.Delivery, 17)
	for index := range deliveries {
		deliveries[index] = webhook.Delivery{DeliveryID: intKey(index + 1), Event: "issues", Action: "reconciled", IssueNumber: 1, AcceptedAt: now}
	}
	health := assessQueueHealth(now, time.Minute, state.Snapshot{Issues: map[string]*state.Issue{}}, webhook.SweepState{}, deliveries)
	if health.OK || health.Code != "mailbox_unbounded" || health.DistinctTargets != 1 {
		t.Fatalf("health=%+v", health)
	}
}

func TestQueueHealthDefersUnclaimedIssuesWhileExecutionOwnsSlot(t *testing.T) {
	now := time.Date(2026, 9, 8, 4, 52, 0, 880000000, time.FixedZone("JST", 9*60*60))
	sweep := webhook.SweepState{Pages: map[int]webhook.SweepPageState{1: {Issues: []gh.Issue{{Number: 206}}}}}
	delivery := webhook.Delivery{Event: "issues", Action: "reconciled", IssueNumber: 206, AcceptedAt: now.Add(-time.Hour)}
	snapshot := state.Snapshot{ActiveExecution: &state.ActiveExecution{IssueNumber: 205}}
	health := assessQueueHealth(now, time.Minute, snapshot, sweep, []webhook.Delivery{delivery})
	if !health.OK || len(health.StalledIssues) != 0 || len(health.ReadyIssues) != 1 || health.MailboxDepth != 1 {
		t.Fatalf("occupied slot reported stalled: %+v", health)
	}
	snapshot.ActiveExecution = nil
	snapshot.LastExecutionReleasedAt = now
	health = assessQueueHealth(now, time.Minute, snapshot, sweep, []webhook.Delivery{delivery})
	if !health.OK {
		t.Fatalf("released slot reported stalled admission: %+v", health)
	}
	snapshot.ActiveExecution = &state.ActiveExecution{IssueNumber: 205}
	deliveries := make([]webhook.Delivery, 17)
	for index := range deliveries {
		deliveries[index] = delivery
	}
	health = assessQueueHealth(now, time.Minute, snapshot, sweep, deliveries)
	if health.OK || health.Code != "mailbox_unbounded" {
		t.Fatalf("occupied slot hid unbounded mailbox: %+v", health)
	}
}

func TestQueueHealthReleaseGraceSurvivesReloadAndObservations(t *testing.T) {
	releasedAt := time.Date(2026, 9, 8, 4, 52, 0, 880000000, time.FixedZone("JST", 9*60*60))
	diagnosticLayout := layout.Layout{Root: t.TempDir()}
	acceptedAt := time.Date(2026, 9, 7, 21, 28, 32, 0, releasedAt.Location())
	sweep := webhook.SweepState{Pages: map[int]webhook.SweepPageState{1: {Issues: []gh.Issue{{Number: 397}}}}}
	deliveries := []webhook.Delivery{{Event: "issues", IssueNumber: 397, AcceptedAt: acceptedAt}}
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "dedicated", true: "legacy"}[legacy], func(t *testing.T) {
			snapshot := state.Snapshot{LastExecutionReleasedAt: releasedAt, Issues: map[string]*state.Issue{"396": {Number: 396, RunID: "run396", Generation: 1, Continuation: &state.ContinuationCheckpoint{ID: "checkpoint396", RunID: "run396", Generation: 1, CreatedAt: releasedAt}}}}
			if legacy {
				snapshot.LastExecutionReleasedAt = time.Time{}
			}
			for _, elapsed := range []time.Duration{0, 2120 * time.Millisecond, 24166 * time.Millisecond, 31 * time.Second, 2*time.Minute - time.Nanosecond, 2 * time.Minute, 2*time.Minute + time.Nanosecond, time.Hour} {
				now := releasedAt.Add(elapsed)
				snapshot.Supervisor.StartedAt, snapshot.Supervisor.UpdatedAt = now, now
				snapshot.Issues["396"].UpdatedAt = now
				data, err := json.Marshal(snapshot)
				if err != nil {
					t.Fatal(err)
				}
				var reloaded state.Snapshot
				if err := json.Unmarshal(data, &reloaded); err != nil {
					t.Fatal(err)
				}
				health := assessQueueHealth(now, time.Minute, reloaded, sweep, deliveries)
				if health.OK != (elapsed <= 2*time.Minute) {
					t.Fatalf("elapsed=%s health=%+v", elapsed, health)
				}
				if diagnostic := diagnoseQueueProgress(diagnosticLayout, registry.Entry{RepoID: "repo"}, health, "fixture"); diagnostic.OK != health.OK {
					t.Fatalf("doctor disagrees with status: %+v / %+v", diagnostic, health)
				}
				if !health.OldestIntentAt.Equal(acceptedAt) {
					t.Fatal("intent age changed")
				}
				if again := assessQueueHealth(now, time.Minute, reloaded, sweep, deliveries); !reflect.DeepEqual(health, again) {
					t.Fatal("observation changed health")
				}
			}
		})
	}
}

func TestQueueHealthRequiresReleaseEvidence(t *testing.T) {
	now := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)
	sweep := webhook.SweepState{Pages: map[int]webhook.SweepPageState{1: {Issues: []gh.Issue{{Number: 397}}}}}
	deliveries := []webhook.Delivery{{IssueNumber: 397, AcceptedAt: now.Add(-time.Hour)}}
	for _, checkpoint := range []*state.ContinuationCheckpoint{nil, {CreatedAt: now}, {ID: "old", RunID: "old", Generation: 1, CreatedAt: now}, {ID: "old", RunID: "run", Generation: 1, CreatedAt: now}} {
		snapshot := state.Snapshot{Issues: map[string]*state.Issue{"396": {RunID: "run", Generation: 2, UpdatedAt: now, Continuation: checkpoint}}}
		health := assessQueueHealth(now, time.Minute, snapshot, sweep, deliveries)
		if health.OK {
			t.Fatalf("unsupported grace: %+v", checkpoint)
		}
	}
	snapshot := state.Snapshot{LastExecutionReleasedAt: now.Add(-time.Hour)}
	deliveries[0].AcceptedAt = now
	if health := assessQueueHealth(now, time.Minute, snapshot, sweep, deliveries); !health.OK {
		t.Fatalf("new intent stalled: %+v", health)
	}
	deliveries[0].AcceptedAt = now.Add(-time.Hour)
	if health := assessQueueHealth(now, time.Minute, snapshot, webhook.SweepState{}, deliveries); !health.OK {
		t.Fatalf("empty queue stalled: %+v", health)
	}
	snapshot.ActiveExecution = &state.ActiveExecution{IssueNumber: 398}
	if health := assessQueueHealth(now, time.Minute, snapshot, sweep, deliveries); !health.OK {
		t.Fatalf("reacquired slot stalled: %+v", health)
	}
}
