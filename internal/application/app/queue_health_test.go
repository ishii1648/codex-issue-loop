package app

import (
	"testing"
	"time"

	gh "github.com/ishii1648/codex-issue-loop/internal/adapter/github"
	"github.com/ishii1648/codex-issue-loop/internal/adapter/state"
	"github.com/ishii1648/codex-issue-loop/internal/adapter/webhook"
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
