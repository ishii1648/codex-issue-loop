package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLegacyPullRequestChecksFailureFixture(t *testing.T) {
	events := legacyChecksFixture(t)
	issue := legacyChecksIssue()
	failure, err := legacyPullRequestChecksFailureFromEvents(events, issue, "owner/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	if failure.HeadSHA != issue.HeadSHA || failure.PullRequestNumber != issue.PullRequestNumber || !failure.Recoverable || !failure.RetryExhausted {
		t.Fatalf("failure=%+v", failure)
	}

	store := Store{Dir: t.TempDir(), RepoID: "repo_fixture", RepoPath: t.TempDir()}
	data, err := os.ReadFile("testdata/legacy-checks-recovery-events.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store.Dir, "events.jsonl"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LegacyPullRequestChecksFailure(issue, "owner/repo", "main"); err != nil {
		t.Fatalf("store evidence rejected: %v", err)
	}
	issue.ConflictRecovery.History[len(issue.ConflictRecovery.History)-1].Status = "running"
	if _, err := store.LegacyPullRequestChecksFailure(issue, "owner/repo", "main"); err == nil {
		t.Fatal("active conflict recovery was accepted")
	}
}

func TestLegacyPullRequestChecksFailureRejectsBrokenChains(t *testing.T) {
	baseline := legacyChecksFixture(t)
	for index := range baseline {
		t.Run("missing_"+baseline[index].Type, func(t *testing.T) {
			events := append([]Event(nil), baseline[:index]...)
			events = append(events, baseline[index+1:]...)
			if _, err := legacyPullRequestChecksFailureFromEvents(events, legacyChecksIssue(), "owner/repo", "main"); err == nil {
				t.Fatal("missing event was accepted")
			}
		})
	}
	t.Run("duplicate publication failure", func(t *testing.T) {
		events := append([]Event(nil), baseline[:9]...)
		events = append(events, baseline[8])
		events = append(events, baseline[9:]...)
		if _, err := legacyPullRequestChecksFailureFromEvents(events, legacyChecksIssue(), "owner/repo", "main"); err == nil {
			t.Fatal("duplicate publication failure was accepted")
		}
	})
	t.Run("repeated unchanged terminal reconciliation", func(t *testing.T) {
		events := append(append([]Event(nil), baseline...), baseline[len(baseline)-1])
		if _, err := legacyPullRequestChecksFailureFromEvents(events, legacyChecksIssue(), "owner/repo", "main"); err != nil {
			t.Fatalf("unchanged terminal reconciliation was rejected: %v", err)
		}
	})
	tests := []struct {
		name   string
		mutate func([]Event, *Issue)
	}{
		{name: "reordered repair", mutate: func(events []Event, _ *Issue) { events[5], events[6] = events[6], events[5] }},
		{name: "cross run", mutate: func(events []Event, _ *Issue) { events[8].RunID = "run_other" }},
		{name: "cross generation", mutate: func(events []Event, _ *Issue) {
			events[10].Payload = json.RawMessage(`{"attempt":3,"lease_owner":{"run_id":"run_final","generation":4}}`)
		}},
		{name: "publication code", mutate: func(events []Event, _ *Issue) {
			var payload map[string]any
			_ = json.Unmarshal(events[8].Payload, &payload)
			payload["code"] = "unknown"
			events[8].Payload, _ = json.Marshal(payload)
		}},
		{name: "identity mismatch", mutate: func(_ []Event, issue *Issue) { issue.PullRequestNumber++ }},
		{name: "head mismatch", mutate: func(_ []Event, issue *Issue) { issue.HeadSHA = "2222222222222222222222222222222222222222" }},
		{name: "fork", mutate: func(events []Event, _ *Issue) {
			var payload map[string]any
			_ = json.Unmarshal(events[16].Payload, &payload)
			prs := payload["pull_requests"].([]any)
			prs[0].(map[string]any)["HeadRepository"] = "attacker/repo"
			events[16].Payload, _ = json.Marshal(payload)
		}},
		{name: "terminal cardinality", mutate: func(events []Event, _ *Issue) {
			events[16].Payload = json.RawMessage(`{"status":"failed","reason":"saved Pull Request is not merged","pull_requests":[]}`)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			events := append([]Event(nil), baseline...)
			issue := legacyChecksIssue()
			test.mutate(events, &issue)
			if _, err := legacyPullRequestChecksFailureFromEvents(events, issue, "owner/repo", "main"); err == nil {
				t.Fatal("broken legacy chain was accepted")
			}
		})
	}
}

func legacyChecksFixture(t *testing.T) []Event {
	t.Helper()
	data, err := os.ReadFile("testdata/legacy-checks-recovery-events.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	var result []Event
	for _, line := range fixtureEventLines(data) {
		var event Event
		if err := json.Unmarshal(line, &event); err != nil {
			t.Fatal(err)
		}
		result = append(result, event)
	}
	return result
}

func legacyChecksIssue() Issue {
	finished := time.Date(2026, 8, 18, 0, 0, 0, 0, time.UTC)
	return Issue{
		Number: 91, Status: "failed", RunID: "run_final", Worktree: "/tmp/legacy-checks", Branch: "codex/issue-91-legacy",
		Attempts: 3, PullRequestURL: "https://example.test/pr/91", PullRequestNumber: 91,
		HeadSHA: "1111111111111111111111111111111111111111", FailureKind: "issue",
		LastError:       "issue: worker retry limit reached: final verification failed",
		LeaseGeneration: 3,
		Lease:           &ResourceLease{Owner: LeaseOwner{RunID: "run_final", Generation: 3}, DeclaredResources: []string{RepositoryResource}, ResolvedResources: []string{RepositoryResource}},
		ConflictRecovery: &ConflictRecovery{
			PullRequestURL: "https://example.test/pr/91", TargetBaseSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			LastReason: "published; waiting for CI revalidation",
			History:    []ConflictAttempt{{Number: 1, BaseSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Status: "completed", FinishedAt: finished}},
		},
	}
}
