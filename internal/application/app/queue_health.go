package app

import (
	"sort"
	"time"

	"github.com/ishii1648/codex-issue-loop/internal/adapter/state"
	"github.com/ishii1648/codex-issue-loop/internal/adapter/webhook"
)

type queueHealth struct {
	OK               bool      `json:"ok"`
	Code             string    `json:"code"`
	ReadyIssues      []int     `json:"ready_issues"`
	StalledIssues    []int     `json:"stalled_issues"`
	MailboxDepth     int       `json:"mailbox_depth"`
	DistinctTargets  int       `json:"distinct_targets"`
	OldestIntentAt   time.Time `json:"oldest_intent_at,omitempty"`
	StallThresholdMS int64     `json:"stall_threshold_ms"`
}

func assessQueueHealth(now time.Time, interval time.Duration, snapshot state.Snapshot, sweep webhook.SweepState, deliveries []webhook.Delivery) queueHealth {
	if interval <= 0 {
		interval = time.Minute
	}
	threshold := 2 * interval
	readySet := map[int]bool{}
	for _, page := range sweep.Pages {
		for _, issue := range page.Issues {
			if issue.Number > 0 {
				readySet[issue.Number] = true
			}
		}
	}
	oldest := map[int]time.Time{}
	targets := map[string]bool{}
	oldestAny := time.Time{}
	for _, delivery := range deliveries {
		key := delivery.Event + ":" + delivery.Action
		switch {
		case delivery.IssueNumber > 0:
			key += ":issue:" + intKey(delivery.IssueNumber)
		case delivery.PullRequestNumber > 0:
			key += ":pr:" + intKey(delivery.PullRequestNumber)
		default:
			key += ":unknown"
		}
		targets[key] = true
		if !delivery.AcceptedAt.IsZero() && (oldestAny.IsZero() || delivery.AcceptedAt.Before(oldestAny)) {
			oldestAny = delivery.AcceptedAt
		}
		if delivery.IssueNumber > 0 && (oldest[delivery.IssueNumber].IsZero() || delivery.AcceptedAt.Before(oldest[delivery.IssueNumber])) {
			oldest[delivery.IssueNumber] = delivery.AcceptedAt
		}
	}
	result := queueHealth{
		OK: true, Code: "ready", MailboxDepth: len(deliveries), DistinctTargets: len(targets),
		OldestIntentAt: oldestAny, StallThresholdMS: threshold.Milliseconds(),
	}
	for number := range readySet {
		result.ReadyIssues = append(result.ReadyIssues, number)
		local := snapshot.Issues[intKey(number)]
		if local != nil || oldest[number].IsZero() || now.Sub(oldest[number]) <= threshold {
			continue
		}
		result.StalledIssues = append(result.StalledIssues, number)
	}
	sort.Ints(result.ReadyIssues)
	sort.Ints(result.StalledIssues)
	if len(result.StalledIssues) > 0 {
		result.OK, result.Code = false, "ready_issue_stalled"
	}
	boundedLimit := len(targets) * 4
	if boundedLimit < 16 {
		boundedLimit = 16
	}
	if len(deliveries) > boundedLimit {
		result.OK, result.Code = false, "mailbox_unbounded"
	}
	return result
}

func intKey(value int) string {
	if value == 0 {
		return "0"
	}
	var buffer [20]byte
	index := len(buffer)
	for value > 0 {
		index--
		buffer[index] = byte('0' + value%10)
		value /= 10
	}
	return string(buffer[index:])
}
