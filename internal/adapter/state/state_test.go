package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
	"github.com/ishii1648/codex-issue-loop/internal/platform/fsutil"
	"github.com/ishii1648/codex-issue-loop/internal/platform/retention"
)

func TestLoadDoesNotEmitPermissionWatchEventsWhenModesAreSecure(t *testing.T) {
	store := newStore(t)
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := watcher.Close(); err != nil {
			t.Error(err)
		}
	}()
	if err := watcher.Add(store.Dir); err != nil {
		t.Fatal(err)
	}

	for range 5 {
		if _, err := store.Load(); err != nil {
			t.Fatal(err)
		}
	}

	select {
	case event := <-watcher.Events:
		t.Fatalf("secure state load emitted fsnotify event: %s", event)
	case err := <-watcher.Errors:
		t.Fatal(err)
	case <-time.After(250 * time.Millisecond):
	}
}

func TestLoadRepairsUnsafeManagedStateModes(t *testing.T) {
	store := newStore(t)
	if err := os.Chmod(store.Dir, 0o755|os.ModeSticky); err != nil {
		t.Fatal(err)
	}
	paths := []string{store.StatePath(), store.lockPath()}
	for _, path := range paths {
		if err := os.Chmod(path, 0o644|os.ModeSetuid); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := store.Load(); err != nil {
		t.Fatal(err)
	}
	const managedModeMask = os.ModePerm | os.ModeSetuid | os.ModeSetgid | os.ModeSticky
	if info, err := os.Stat(store.Dir); err != nil || info.Mode()&managedModeMask != 0o700 {
		t.Fatalf("state directory mode=%v err=%v", info, err)
	}
	for _, path := range paths {
		if info, err := os.Stat(path); err != nil || info.Mode()&managedModeMask != 0o600 {
			t.Fatalf("managed path %s mode=%v err=%v", path, info, err)
		}
	}
}

func TestStateAndEventsNeverPersistSecrets(t *testing.T) {
	secret := "configured-secret-value"
	store := Store{Dir: t.TempDir(), RepoID: "repo-deadbeef", RepoPath: "/tmp/repo", Secrets: []string{secret}}
	if err := store.Initialize(); err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.Update("unsafe_result", 1, "run_1", map[string]string{"stderr": "Bearer abcdefghijklmnopqrstuvwxyz", "custom": secret}, func(value *Snapshot) error {
		value.Issues["1"] = &Issue{Number: 1, Title: "contains " + secret, LastError: "ghp_abcdefghijklmnopqrstuvwxyz123456"}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(snapshot.Issues["1"].Title, secret) || strings.Contains(snapshot.Issues["1"].LastError, "ghp_") {
		t.Fatalf("returned snapshot contains secret: %+v", snapshot.Issues["1"])
	}
	for _, path := range []string{store.StatePath(), store.EventsPath()} {
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if strings.Contains(string(data), secret) || strings.Contains(string(data), "ghp_") || strings.Contains(string(data), "Bearer abc") {
			t.Fatalf("secret persisted in %s: %s", path, data)
		}
		if info, statErr := os.Stat(path); statErr != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("unsafe mode for %s: info=%v err=%v", path, info, statErr)
		}
	}
	if info, err := os.Stat(store.Dir); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("unsafe state directory mode: info=%v err=%v", info, err)
	}
}

func TestUpdateIdentifiesIssueMutationFailure(t *testing.T) {
	store := newStore(t)
	base := errors.New("Issue lifecycle changed")
	_, err := store.Update("issue_changed", 42, "run-42", nil, func(*Snapshot) error { return base })
	var mutationErr IssueMutationError
	if !errors.As(err, &mutationErr) || mutationErr.IssueNumber != 42 || !errors.Is(err, base) {
		t.Fatalf("error=%T %v", err, err)
	}
}

func TestLegacyGoalSnapshotIsIgnoredWithoutLosingContinuationState(t *testing.T) {
	store := newStore(t)
	snapshot := store.emptySnapshot()
	snapshot.Issues["189"] = &Issue{
		Number: 189, Title: "legacy App Server run", Status: issuedomain.StatusNeedsInput,
		RunID: "run_189", Branch: "codex/issue-189", Worktree: "/tmp/issue-189",
		Workspace: &WorkerWorkspace{Path: "/tmp/issue-189", Branch: "codex/issue-189", RepoID: store.RepoID, Repository: "owner/repo", GitCommonDir: "/tmp/repo/.git", MainCheckout: "/tmp/repo", CapturedAt: time.Now().UTC()},
		SessionID: "session-189", Session: &WorkerSession{Backend: "codex", ID: "session-189"},
		Answers:  []AnswerRecord{{RequestID: "req_189", Question: "Continue?", Answer: "yes"}},
		Attempts: 2, Continuations: 1,
	}
	snapshot.PendingRequests["req_189"] = &Request{
		ID: "req_189", IssueNumber: 189, Question: "Continue?", Status: issuedomain.RequestStatusAnswered, Answer: "yes",
	}
	data, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	var legacy map[string]any
	if err := json.Unmarshal(data, &legacy); err != nil {
		t.Fatal(err)
	}
	issues := legacy["issues"].(map[string]any)
	issue := issues["189"].(map[string]any)
	issue["goal"] = map[string]any{
		"thread_id": "session-189", "objective": "finish", "status": "blocked", "tokens_used": 123,
	}
	if err := fsutil.WriteJSON(store.StatePath(), legacy, 0o600); err != nil {
		t.Fatal(err)
	}

	loaded, err := store.Load()
	if err != nil {
		t.Fatalf("legacy goal made state unreadable: %v", err)
	}
	item := loaded.Issues["189"]
	request := loaded.PendingRequests["req_189"]
	if item == nil || item.Worktree != "/tmp/issue-189" || item.SessionID != "session-189" || item.Session == nil || item.Session.ID != "session-189" || len(item.Answers) != 1 || item.Answers[0].Answer != "yes" || item.Attempts != 2 || item.Continuations != 1 || request == nil || request.Answer != "yes" {
		t.Fatalf("legacy goal load lost continuation state: %+v", item)
	}
	updated, err := store.Update("legacy_goal_ignored", 189, "run_189", nil, func(*Snapshot) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if updated.Issues["189"].SessionID != "session-189" || len(updated.Issues["189"].Answers) != 1 || updated.PendingRequests["req_189"].Answer != "yes" {
		t.Fatalf("state rewrite lost continuation state: %+v", updated.Issues["189"])
	}
	rewritten, err := os.ReadFile(store.StatePath())
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(rewritten, []byte(`"goal"`)) {
		t.Fatalf("legacy goal was rewritten into active state: %s", rewritten)
	}
}

func TestLegacyIssueCapabilityFieldsAreIgnoredWithoutLosingState(t *testing.T) {
	store := newStore(t)
	snapshot := store.emptySnapshot()
	snapshot.Issues["7"] = &Issue{
		Number: 7, Title: "legacy capability run", Status: issuedomain.StatusCompleted,
		RunID: "run_7", Attempts: 2, ExecutionProfile: "extended",
	}
	data, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	var legacy map[string]any
	if err := json.Unmarshal(data, &legacy); err != nil {
		t.Fatal(err)
	}
	issue := legacy["issues"].(map[string]any)["7"].(map[string]any)
	issue["capability_requirements"] = map[string]any{"version": 1, "profile": "standard", "network": "public"}
	issue["worker_capabilities"] = map[string]any{"version": 1, "profile": "standard", "network": "none"}
	if err := fsutil.WriteJSON(store.StatePath(), legacy, 0o600); err != nil {
		t.Fatal(err)
	}

	loaded, err := store.Load()
	if err != nil {
		t.Fatalf("legacy capability fields made state unreadable: %v", err)
	}
	item := loaded.Issues["7"]
	if item == nil || item.Title != "legacy capability run" || item.Status != issuedomain.StatusCompleted || item.RunID != "run_7" || item.Attempts != 2 || item.ExecutionProfile != "extended" {
		t.Fatalf("legacy capability load lost state: %+v", item)
	}
	if _, err := store.Update("legacy_capability_fields_ignored", 7, "run_7", nil, func(*Snapshot) error { return nil }); err != nil {
		t.Fatal(err)
	}
	persisted, err := os.ReadFile(store.StatePath())
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(persisted, []byte("capability_requirements")) || bytes.Contains(persisted, []byte("worker_capabilities")) {
		t.Fatalf("obsolete capability fields survived the next state write: %s", persisted)
	}
}

func TestFaultAttentionRevisionPersistsSnapshotAndEvent(t *testing.T) {
	store := newStore(t)
	snapshot, err := store.Update("supervisor_started", 0, "", map[string]string{"ok": "yes"}, func(s *Snapshot) error {
		s.Supervisor.State = "polling"
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.StateRevision != 1 {
		t.Fatalf("revision = %d", snapshot.StateRevision)
	}
	loaded, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Supervisor.State != "polling" || loaded.StateRevision != 1 {
		t.Fatalf("unexpected snapshot: %+v", loaded)
	}
	events, err := os.ReadFile(store.EventsPath())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(events), `"sequence":1`) || !strings.Contains(string(events), `"type":"supervisor_started"`) {
		t.Fatalf("unexpected events: %s", events)
	}
}

func TestLegacySessionIDIsNamespacedAsCodex(t *testing.T) {
	store := newStore(t)
	_, err := store.Update("legacy", 1, "run", nil, func(snapshot *Snapshot) error {
		snapshot.Issues["1"] = &Issue{Number: 1, SessionID: "legacy-session"}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	session := loaded.Issues["1"].Session
	if session == nil || session.Backend != "codex" || session.ID != "legacy-session" {
		t.Fatalf("session=%+v", session)
	}
}

func TestFaultAttentionRemainsStickyUntilAnswered(t *testing.T) {
	store := newStore(t)
	_, err := store.Update("input_requested", 7, "run", nil, func(s *Snapshot) error {
		s.Supervisor.State = "running"
		s.Issues["7"] = &Issue{Number: 7, RunID: "run", Status: issuedomain.StatusNeedsInput}
		s.PendingRequests["req_1"] = &Request{ID: "req_1", IssueNumber: 7, Status: issuedomain.RequestStatusPending}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if reason, ok := snapshot.Attention(false, true); !ok || reason != "needs_input" {
		t.Fatalf("reason=%q ok=%v", reason, ok)
	}
	_, err = store.Update("unrelated", 0, "", nil, func(s *Snapshot) error { s.Supervisor.State = "polling"; return nil })
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err = store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if reason, ok := snapshot.Attention(false, true); !ok || reason != "needs_input" {
		t.Fatalf("request was not sticky")
	}
}

func TestUntilIdleWaitsForPullRequestLifecycle(t *testing.T) {
	for _, status := range []string{"awaiting_checks", "awaiting_merge", "resolving_conflict"} {
		t.Run(status, func(t *testing.T) {
			snapshot := Snapshot{
				Supervisor: Supervisor{State: "polling"},
				Issues:     map[string]*Issue{"7": {Number: 7, Status: issuedomain.Status(status)}},
			}
			if reason, ok := snapshot.Attention(true, true); ok {
				t.Fatalf("reason=%q ok=%v", reason, ok)
			}
		})
	}
}

func TestDurableStateRejectsUnknownIssueStatus(t *testing.T) {
	const invalidStatus = "invented_status"
	snapshot := Snapshot{Issues: map[string]*Issue{
		"7": {Number: 7, Status: issuedomain.Status(invalidStatus)},
	}}
	if err := snapshot.Issues["7"].Status.Validate(); err == nil {
		t.Fatal("expected unknown durable Issue status to be rejected")
	}
}

func TestLifecycleAPIPreviousMinorNormalizesCompatibly(t *testing.T) {
	store := newStore(t)
	snapshot, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	legacy := strings.Replace(string(encoded), `"issue_lifecycle_api_version":"`+issuedomain.LifecycleAPICurrent+`"`, `"issue_lifecycle_api_version":"`+issuedomain.LifecycleAPIPreviousMinor+`"`, 1)
	var decoded Snapshot
	if err := json.Unmarshal([]byte(legacy), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.IssueLifecycleAPIVersion != issuedomain.LifecycleAPICurrent || decoded.Validate() != nil {
		t.Fatalf("version=%q validation=%v", decoded.IssueLifecycleAPIVersion, decoded.Validate())
	}
}

func TestAttentionReportsOneBlockedIssueWhileAnotherWorkerIsActive(t *testing.T) {
	snapshot := Snapshot{
		Supervisor: Supervisor{State: "running"},
		Issues: map[string]*Issue{
			"1": {Number: 1, Status: issuedomain.StatusRunning},
			"2": {Number: 2, Status: issuedomain.StatusBlocked},
		},
	}
	if reason, ok := snapshot.Attention(false, true); !ok || reason != "blocked" {
		t.Fatalf("reason=%q ok=%v", reason, ok)
	}
	if reason, ok := snapshot.Attention(true, true); !ok || reason != "blocked" {
		t.Fatalf("until-idle reason=%q ok=%v", reason, ok)
	}
}

func TestCanceledIssueIsTerminalWithoutStickyAttention(t *testing.T) {
	now := time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)
	snapshot := Snapshot{
		Supervisor: Supervisor{State: SupervisorStatePolling},
		Issues: map[string]*Issue{"93": {
			Number: 93, Status: issuedomain.StatusCanceled,
			Cancellation:      &Cancellation{Source: "github_not_planned", GitHubStateReason: "NOT_PLANNED", PreviousStatus: issuedomain.StatusBlocked, ExecutionReleaseResult: "not_present", CanceledAt: now},
			GitHubStateReason: "NOT_PLANNED",
		}},
		PendingEffects: map[string]*EffectIntent{}, PendingRequests: map[string]*Request{},
	}
	if reason, ok := snapshot.Attention(false, true); ok {
		t.Fatalf("reason=%q ok=%v", reason, ok)
	}
	if reason, ok := snapshot.Attention(true, true); !ok || reason != "idle" {
		t.Fatalf("until-idle reason=%q ok=%v", reason, ok)
	}
}

func TestNotPlannedCancellationReleasesMatchingRetainedExecution(t *testing.T) {
	store := newStore(t)
	now := time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)
	_, identity, err := store.StartExecution(ExecutionStart{
		IssueNumber: 93, Title: "Superseded", RunID: "run_93", StartedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := store.Update("issue_canceled", 93, identity.RunID, nil, func(snapshot *Snapshot) error {
		item := snapshot.Issues["93"]
		item.Status = issuedomain.StatusBlocked
		item.GitHubStateReason = "NOT_PLANNED"
		expected := *item
		releaseResult, applyErr := ApplyNotPlannedCancellation(snapshot, 93, &expected, now.Add(time.Minute))
		if applyErr != nil {
			return applyErr
		}
		if releaseResult != "released" {
			return fmt.Errorf("execution release result=%q", releaseResult)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.ActiveExecution != nil || result.Issues["93"].Status != issuedomain.StatusCanceled ||
		result.Issues["93"].Cancellation.ExecutionReleaseResult != "released" {
		t.Fatalf("issue=%+v active=%+v", result.Issues["93"], result.ActiveExecution)
	}
	if _, _, err := store.StartExecution(ExecutionStart{
		IssueNumber: 94, Title: "Next", RunID: "run_94", StartedAt: now.Add(2 * time.Minute),
	}); err != nil {
		t.Fatalf("next Issue was not admitted after ownership release: %v", err)
	}
}

func TestNotPlannedCancellationRejectsRetainedExecutionOwnerMismatch(t *testing.T) {
	now := time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)
	snapshot := Snapshot{
		ActiveExecution: &ActiveExecution{IssueNumber: 93, RunID: "other", Generation: 4, StartedAt: now},
		Issues: map[string]*Issue{"93": {
			Number: 93, Status: issuedomain.StatusBlocked, RunID: "run_93", Generation: 4,
			GitHubStateReason: "NOT_PLANNED",
		}},
		PendingEffects: map[string]*EffectIntent{}, PendingRequests: map[string]*Request{},
	}
	expected := *snapshot.Issues["93"]
	if _, err := ApplyNotPlannedCancellation(&snapshot, 93, &expected, now); err == nil || !strings.Contains(err.Error(), "ownership is inconsistent") {
		t.Fatalf("owner mismatch error=%v", err)
	}
	if snapshot.ActiveExecution == nil || snapshot.Issues["93"].Status != issuedomain.StatusBlocked {
		t.Fatalf("mismatched ownership was changed: issue=%+v active=%+v", snapshot.Issues["93"], snapshot.ActiveExecution)
	}
}

func TestFaultSnapshotWriteCrashRecoversEveryTransactionPoint(t *testing.T) {
	for _, crashPoint := range []string{"prepared", "event_appended", "snapshot_written"} {
		t.Run(crashPoint, func(t *testing.T) {
			store := newStore(t)
			base, err := store.Update("first", 0, "", nil, func(s *Snapshot) error {
				s.Supervisor.State = "polling"
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			next := base
			next.StateRevision++
			next.Supervisor.Message = "transaction completed"
			next.Supervisor.UpdatedAt = time.Now().UTC()
			event := Event{
				Version: CurrentVersion, EventID: "evt_transaction", Sequence: next.StateRevision,
				Timestamp: time.Now().UTC(), RepoID: store.RepoID, Type: "second",
			}
			if err := fsutil.WriteJSON(store.TransactionPath(), transaction{Version: CurrentVersion, Snapshot: next, Event: event}, 0o600); err != nil {
				t.Fatal(err)
			}
			if crashPoint == "event_appended" || crashPoint == "snapshot_written" {
				if err := store.appendEventUnlocked(event); err != nil {
					t.Fatal(err)
				}
			}
			if crashPoint == "snapshot_written" {
				if err := fsutil.WriteJSON(store.StatePath(), next, 0o600); err != nil {
					t.Fatal(err)
				}
			}

			loaded, err := store.Load()
			if err != nil {
				t.Fatal(err)
			}
			if loaded.StateRevision != 2 || loaded.Supervisor.Message != "transaction completed" {
				t.Fatalf("loaded=%+v", loaded)
			}
			if _, err := os.Stat(store.TransactionPath()); !os.IsNotExist(err) {
				t.Fatalf("transaction was not removed: %v", err)
			}
			events, _, partial, err := store.readEventsUnlocked()
			if err != nil || partial || len(events) != 2 || events[1].Type != "second" {
				t.Fatalf("events=%+v partial=%v err=%v", events, partial, err)
			}
		})
	}
}

func TestFaultNotPlannedCancellationRecoversOnceAtEveryTransactionPoint(t *testing.T) {
	for _, crashPoint := range []string{"prepared", "event_appended", "snapshot_written"} {
		t.Run(crashPoint, func(t *testing.T) {
			store := newStore(t)
			base, err := store.Update("blocked", 93, "run_93", nil, func(snapshot *Snapshot) error {
				snapshot.Issues["93"] = &Issue{Number: 93, Status: issuedomain.StatusBlocked, RunID: "run_93", LastError: "superseded"}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			next := base
			expected := *next.Issues["93"]
			next.Issues["93"].GitHubStateReason = "NOT_PLANNED"
			canceledAt := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
			if _, err := ApplyNotPlannedCancellation(&next, 93, &expected, canceledAt); err != nil {
				t.Fatal(err)
			}
			next.StateRevision++
			next.Supervisor.UpdatedAt = canceledAt
			if err := next.Validate(); err != nil {
				t.Fatal(err)
			}
			event := Event{
				Version: CurrentVersion, EventID: "evt_issue_canceled", Sequence: next.StateRevision, Timestamp: canceledAt,
				RepoID: store.RepoID, IssueNumber: 93, RunID: "run_93", Type: "issue_canceled",
			}
			if err := fsutil.WriteJSON(store.TransactionPath(), transaction{Version: CurrentVersion, Snapshot: next, Event: event}, 0o600); err != nil {
				t.Fatal(err)
			}
			if crashPoint == "event_appended" || crashPoint == "snapshot_written" {
				if err := store.appendEventUnlocked(event); err != nil {
					t.Fatal(err)
				}
			}
			if crashPoint == "snapshot_written" {
				if err := fsutil.WriteJSON(store.StatePath(), next, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			loaded, err := store.Load()
			if err != nil || loaded.Issues["93"].Status != issuedomain.StatusCanceled {
				t.Fatalf("loaded=%+v err=%v", loaded.Issues["93"], err)
			}
			if _, err := store.Load(); err != nil {
				t.Fatal(err)
			}
			events, _, partial, err := store.readEventsUnlocked()
			if err != nil || partial || len(events) != 2 || events[1].Type != "issue_canceled" {
				t.Fatalf("events=%+v partial=%v err=%v", events, partial, err)
			}
		})
	}
}

func TestFaultRetainedExecutionCancellationRecoversOnceAtEveryTransactionPoint(t *testing.T) {
	for _, crashPoint := range []string{"prepared", "event_appended", "snapshot_written"} {
		t.Run(crashPoint, func(t *testing.T) {
			store := newStore(t)
			canceledAt := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
			base, _, err := store.StartExecution(ExecutionStart{
				IssueNumber: 93, Title: "Superseded", RunID: "run_93", StartedAt: canceledAt.Add(-time.Hour),
			})
			if err != nil {
				t.Fatal(err)
			}
			next := base
			item := next.Issues["93"]
			item.Status = issuedomain.StatusBlocked
			item.GitHubStateReason = "NOT_PLANNED"
			expected := *item
			releaseResult, err := ApplyNotPlannedCancellation(&next, 93, &expected, canceledAt)
			if err != nil || releaseResult != "released" {
				t.Fatalf("release result=%q err=%v", releaseResult, err)
			}
			next.StateRevision++
			next.Supervisor.UpdatedAt = canceledAt
			event := Event{
				Version: CurrentVersion, EventID: "evt_issue_canceled_released", Sequence: next.StateRevision, Timestamp: canceledAt,
				RepoID: store.RepoID, IssueNumber: 93, RunID: "run_93", Type: "issue_canceled",
			}
			if err := fsutil.WriteJSON(store.TransactionPath(), transaction{Version: CurrentVersion, Snapshot: next, Event: event}, 0o600); err != nil {
				t.Fatal(err)
			}
			if crashPoint == "event_appended" || crashPoint == "snapshot_written" {
				if err := store.appendEventUnlocked(event); err != nil {
					t.Fatal(err)
				}
			}
			if crashPoint == "snapshot_written" {
				if err := fsutil.WriteJSON(store.StatePath(), next, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			loaded, err := store.Load()
			if err != nil || loaded.ActiveExecution != nil || loaded.Issues["93"].Status != issuedomain.StatusCanceled ||
				loaded.Issues["93"].Cancellation.ExecutionReleaseResult != "released" {
				t.Fatalf("loaded=%+v active=%+v err=%v", loaded.Issues["93"], loaded.ActiveExecution, err)
			}
			events, _, partial, err := store.readEventsUnlocked()
			if err != nil || partial || len(events) != 2 || events[1].Type != "issue_canceled" {
				t.Fatalf("events=%+v partial=%v err=%v", events, partial, err)
			}
		})
	}
}

func TestFaultPartialEventTailIsTruncatedAndRecorded(t *testing.T) {
	store := newStore(t)
	if _, err := store.Update("first", 0, "", nil, func(s *Snapshot) error { return nil }); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(store.EventsPath(), os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"version":4,"sequence":2`); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	loaded, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.StateRevision != 2 || loaded.Recovery != nil {
		t.Fatalf("loaded=%+v", loaded)
	}
	events, _, partial, err := store.readEventsUnlocked()
	if err != nil || partial || len(events) != 2 || events[1].Type != "event_log_tail_truncated" {
		t.Fatalf("events=%+v partial=%v err=%v", events, partial, err)
	}
}

func TestFaultRevisionMismatchIsQuarantined(t *testing.T) {
	store := newStore(t)
	if _, err := store.Update("first", 0, "", nil, func(s *Snapshot) error { return nil }); err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(map[string]string{"cause": "missing transaction"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.appendEventUnlocked(Event{
		Version: CurrentVersion, EventID: "evt_orphan", Sequence: 2, Timestamp: time.Now().UTC(),
		RepoID: store.RepoID, Type: "orphan", Payload: payload,
	}); err != nil {
		t.Fatal(err)
	}

	loaded, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Recovery == nil || loaded.Recovery.Status != "blocked" || loaded.Supervisor.State != "blocked" {
		t.Fatalf("loaded=%+v", loaded)
	}
	for _, name := range []string{"state.json", "events.jsonl"} {
		if _, err := os.Stat(filepath.Join(loaded.Recovery.BackupDir, name)); err != nil {
			t.Fatalf("missing recovery backup %s: %v", name, err)
		}
	}
	if _, err := store.Update("must_not_run", 0, "", nil, func(s *Snapshot) error { return nil }); err == nil {
		t.Fatal("recovery-blocked state accepted an update")
	}
	second, err := store.Load()
	if err != nil || second.Recovery == nil || second.StateRevision != 1 {
		t.Fatalf("second load=%+v err=%v", second, err)
	}
}

func TestFaultCorruptSnapshotIsQuarantined(t *testing.T) {
	store := newStore(t)
	if err := os.WriteFile(store.StatePath(), []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Recovery == nil || !strings.Contains(loaded.Recovery.Reason, "decode state") {
		t.Fatalf("loaded=%+v", loaded)
	}
}

func TestUnsupportedSchemaVersionIsRejectedWithoutQuarantine(t *testing.T) {
	for _, version := range []int{CurrentVersion - 1, CurrentVersion + 1} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			store := newStore(t)
			data, err := os.ReadFile(store.StatePath())
			if err != nil {
				t.Fatal(err)
			}
			var snapshot Snapshot
			if err := json.Unmarshal(data, &snapshot); err != nil {
				t.Fatal(err)
			}
			snapshot.Version = version
			modified, err := json.MarshalIndent(snapshot, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			modified = append(modified, '\n')
			if err := os.WriteFile(store.StatePath(), modified, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := store.Load(); err == nil {
				t.Fatal("unsupported schema was accepted")
			}
			after, err := os.ReadFile(store.StatePath())
			if err != nil || !bytes.Equal(after, modified) {
				t.Fatalf("unsupported state was modified: err=%v", err)
			}
			if _, err := os.Stat(filepath.Join(store.Dir, "recovery")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("unsupported state was quarantined: %v", err)
			}
		})
	}
}

func TestUnsupportedSemanticContractVersionIsRejectedWithoutQuarantine(t *testing.T) {
	store := newStore(t)
	if _, err := store.Update("completed", 7, "run_7", nil, func(snapshot *Snapshot) error {
		snapshot.Issues["7"] = &Issue{Number: 7, Status: issuedomain.StatusCompleted, RunID: "run_7", Attempts: 1}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(store.StatePath())
	if err != nil {
		t.Fatal(err)
	}
	var snapshot Snapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		t.Fatal(err)
	}
	snapshot.SemanticContractVersion--
	modified, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	var object map[string]any
	if err := json.Unmarshal(modified, &object); err != nil {
		t.Fatal(err)
	}
	issues := object["issues"].(map[string]any)
	issue := issues["7"].(map[string]any)
	issue["publication_failure"] = map[string]any{"code": "legacy"}
	modified, err = json.MarshalIndent(object, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	modified = append(modified, '\n')
	if err := os.WriteFile(store.StatePath(), modified, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(); err == nil {
		t.Fatal("unsupported semantic contract was accepted")
	} else {
		var versionErr SemanticContractVersionError
		if !errors.As(err, &versionErr) {
			t.Fatalf("error=%T %v", err, err)
		}
	}
	after, err := os.ReadFile(store.StatePath())
	if err != nil || !bytes.Equal(after, modified) {
		t.Fatalf("unsupported state was modified: err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(store.Dir, "recovery")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unsupported state was quarantined: %v", err)
	}
}

func TestUnsupportedLifecycleAPIVersionIsRejectedWithoutQuarantine(t *testing.T) {
	store := newStore(t)
	if _, err := store.Update("checkpoint", 0, "", nil, func(*Snapshot) error { return nil }); err != nil {
		t.Fatal(err)
	}
	stateData, err := os.ReadFile(store.StatePath())
	if err != nil {
		t.Fatal(err)
	}
	var snapshot Snapshot
	if err := json.Unmarshal(stateData, &snapshot); err != nil {
		t.Fatal(err)
	}
	snapshot.IssueLifecycleAPIVersion = "99.0"
	modifiedState, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	modifiedState = append(modifiedState, '\n')
	if err := os.WriteFile(store.StatePath(), modifiedState, 0o600); err != nil {
		t.Fatal(err)
	}
	transactionSnapshot := snapshot
	transactionSnapshot.StateRevision++
	prepared := transaction{Version: CurrentVersion, Snapshot: transactionSnapshot, Event: Event{
		Version: CurrentVersion, EventID: NewID("evt"), Sequence: transactionSnapshot.StateRevision,
		Timestamp: time.Now().UTC(), RepoID: store.RepoID, Type: "prepared_fixture",
	}}
	if err := fsutil.WriteJSON(store.TransactionPath(), prepared, 0o600); err != nil {
		t.Fatal(err)
	}
	modifiedTransaction, err := os.ReadFile(store.TransactionPath())
	if err != nil {
		t.Fatal(err)
	}
	eventsBefore, err := os.ReadFile(store.EventsPath())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(); err == nil {
		t.Fatal("unsupported lifecycle API was accepted")
	} else {
		var versionErr LifecycleAPIVersionError
		if !errors.As(err, &versionErr) || versionErr.Version != "99.0" || versionErr.Current != issuedomain.LifecycleAPICurrent {
			t.Fatalf("error=%T %v", err, err)
		}
	}
	for path, before := range map[string][]byte{
		store.StatePath():       modifiedState,
		store.EventsPath():      eventsBefore,
		store.TransactionPath(): modifiedTransaction,
	} {
		after, readErr := os.ReadFile(path)
		if readErr != nil || !bytes.Equal(after, before) {
			t.Fatalf("version mismatch modified %s: err=%v", filepath.Base(path), readErr)
		}
	}
	if _, err := os.Stat(filepath.Join(store.Dir, "recovery")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unsupported lifecycle state was quarantined: %v", err)
	}
}

func TestValidIDRejectsRunDirectoryTraversal(t *testing.T) {
	if !ValidID("run_abc-123", "run_") {
		t.Fatal("valid run ID was rejected")
	}
	for _, value := range []string{"run_", "../run_abc", "run_../../state", "resume_abc", "run_with space"} {
		if ValidID(value, "run_") {
			t.Fatalf("unsafe run ID was accepted: %q", value)
		}
	}
}

func TestFaultSecondSupervisorCannotAcquireLock(t *testing.T) {
	store := newStore(t)
	first, err := store.AcquireSupervisorLock()
	if err != nil {
		t.Fatal(err)
	}
	defer ReleaseSupervisorLock(first)
	if second, err := store.AcquireSupervisorLock(); err == nil {
		ReleaseSupervisorLock(second)
		t.Fatal("second supervisor acquired the repository lock")
	}
	ReleaseSupervisorLock(first)
	third, err := store.AcquireSupervisorLock()
	if err != nil {
		t.Fatalf("lock was not reusable after release: %v", err)
	}
	ReleaseSupervisorLock(third)
}

func TestInspectExclusiveBlocksConcurrentStateMutation(t *testing.T) {
	store := Store{Dir: t.TempDir(), RepoID: "repo-exclusive", RepoPath: "/tmp/repo-exclusive"}
	if err := store.Initialize(); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	inspectDone := make(chan error, 1)
	go func() {
		inspectDone <- store.InspectExclusive(func(Snapshot) error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered
	updateDone := make(chan error, 1)
	go func() {
		_, err := store.Update("concurrent_mutation", 0, "", nil, func(snapshot *Snapshot) error {
			snapshot.Supervisor.Message = "updated"
			return nil
		})
		updateDone <- err
	}()
	select {
	case err := <-updateDone:
		t.Fatalf("mutation crossed exclusive inspection: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if err := <-inspectDone; err != nil {
		t.Fatal(err)
	}
	if err := <-updateDone; err != nil {
		t.Fatal(err)
	}
}

func TestFaultEventRotationKeepsCheckpointAndRecoverySequence(t *testing.T) {
	store := Store{
		Dir: t.TempDir(), RepoID: "repo-deadbeef", RepoPath: "/tmp/repo",
		EventRetention: retention.Policy{MaxBytes: 1, MaxAge: time.Hour, Keep: 2},
	}
	if err := store.Initialize(); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 4; index++ {
		if _, err := store.Update("tick", 0, "", map[string]int{"index": index}, func(*Snapshot) error { return nil }); err != nil {
			t.Fatal(err)
		}
	}
	snapshot, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.StateRevision != 4 {
		t.Fatalf("revision=%d", snapshot.StateRevision)
	}
	events, _, partial, err := store.readEventsUnlocked()
	if err != nil || partial || len(events) == 0 || events[0].Type != "event_log_checkpoint" {
		t.Fatalf("events=%+v partial=%v err=%v", events, partial, err)
	}
	store.EventRetention.MaxBytes = 1 << 20
	if _, err := store.Update("after_rotation", 0, "", nil, func(*Snapshot) error { return nil }); err != nil {
		t.Fatal(err)
	}
	snapshot, err = store.Load()
	if err != nil || snapshot.StateRevision != 5 || snapshot.Recovery != nil {
		t.Fatalf("snapshot=%+v err=%v", snapshot, err)
	}
	events, _, partial, err = store.readEventsUnlocked()
	if err != nil || partial || len(events) != 2 || events[0].Sequence != 4 || events[1].Sequence != 5 || events[1].Type != "after_rotation" {
		t.Fatalf("events=%+v partial=%v err=%v", events, partial, err)
	}
	archives, err := filepath.Glob(store.EventsPath() + ".*.gz")
	if err != nil || len(archives) == 0 || len(archives) > 2 {
		t.Fatalf("archives=%v err=%v", archives, err)
	}
}

func TestFaultEventRotationFailurePreservesCommittedUpdate(t *testing.T) {
	store := Store{
		Dir: t.TempDir(), RepoID: "repo-deadbeef", RepoPath: "/tmp/repo",
		EventRetention: retention.Policy{MaxBytes: 1, MaxAge: time.Hour, Keep: 1},
	}
	archive := store.EventsPath() + ".000.gz"
	if err := os.Mkdir(archive, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(archive, "block-pruning"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previous) })
	snapshot, err := store.Update("tick", 0, "", nil, func(snapshot *Snapshot) error {
		snapshot.Supervisor.Message = "committed"
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.StateRevision != 1 || snapshot.Supervisor.Message != "committed" {
		t.Fatalf("snapshot=%+v", snapshot)
	}
	if !strings.Contains(logs.String(), "rotate event log") {
		t.Fatalf("rotation failure was not logged: %s", logs.String())
	}
	if _, err := os.Stat(store.TransactionPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("transaction remains: %v", err)
	}
	loaded, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.StateRevision != snapshot.StateRevision || loaded.Supervisor.Message != "committed" || loaded.Recovery != nil {
		t.Fatalf("loaded=%+v", loaded)
	}
}
