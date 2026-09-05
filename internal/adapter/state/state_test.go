package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
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
	defer watcher.Close()
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

/*
Obsolete lease/resource-admission tests were replaced by execution_test.go.

	func TestLeaseReservationSurvivesRestartAndFencesStaleOwners(t *testing.T) {
		store := newStore(t)
		reservedAt := time.Date(2026, 8, 16, 10, 0, 0, 0, time.UTC)
		snapshot, owner, err := store.ReserveLease(LeaseReservation{
			IssueNumber: 7, Title: "durable", RunID: "run_7", Slot: 0,
			DeclaredResources: []string{"state", "docs"}, ResolvedResources: []string{"state", "docs"},
			BaseSHA: "abc123", ReservedAt: reservedAt,
		})
		if err != nil {
			t.Fatal(err)
		}
		lease := snapshot.Issues["7"].Lease
		if owner != (LeaseOwner{RunID: "run_7", Generation: 1}) || lease == nil || lease.BaseSHA != "abc123" || lease.ReservedAt != reservedAt {
			t.Fatalf("owner=%+v lease=%+v", owner, lease)
		}
		loaded, err := (Store{Dir: store.Dir, RepoID: store.RepoID, RepoPath: store.RepoPath}).Load()
		if err != nil || loaded.Issues["7"].Lease == nil || loaded.Issues["7"].Lease.Owner != owner {
			t.Fatalf("loaded=%+v err=%v", loaded.Issues["7"], err)
		}
		_, err = store.Update("publication_audited", 7, owner.RunID, nil, func(snapshot *Snapshot) error {
			issue := snapshot.Issues["7"]
			issue.ActualResources = []string{"docs", "state"}
			issue.Lease.ActualResources = []string{"docs", "state"}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		loaded, err = (Store{Dir: store.Dir, RepoID: store.RepoID, RepoPath: store.RepoPath}).Load()
		if err != nil || !reflect.DeepEqual(loaded.Issues["7"].DeclaredResources, []string{"docs", "state"}) || !reflect.DeepEqual(loaded.Issues["7"].ActualResources, []string{"docs", "state"}) {
			t.Fatalf("resource audit did not survive restart: issue=%+v err=%v", loaded.Issues["7"], err)
		}
		if err := abandonLeaseForTest(store, 7, LeaseOwner{RunID: "run_other", Generation: 1}); err == nil {
			t.Fatal("stale run released another run's lease")
		}
		if err := abandonLeaseForTest(store, 8, owner); err == nil {
			t.Fatal("owner released another Issue's lease")
		}
		loaded, err = store.Load()
		if err != nil || loaded.Issues["7"].Lease == nil {
			t.Fatalf("stale release changed lease: issue=%+v err=%v", loaded.Issues["7"], err)
		}
		if err := abandonLeaseForTest(store, 7, owner); err != nil {
			t.Fatal(err)
		}
		second, nextOwner, err := store.ReserveLease(LeaseReservation{IssueNumber: 7, RunID: "run_8", Slot: 0, ResolvedResources: []string{"state"}, ReservedAt: reservedAt.Add(time.Hour)})
		if err != nil || nextOwner.Generation != 2 || second.Issues["7"].LeaseGeneration != 2 {
			t.Fatalf("owner=%+v issue=%+v err=%v", nextOwner, second.Issues["7"], err)
		}
		if err := abandonLeaseForTest(store, 7, owner); err == nil {
			t.Fatal("old generation released replacement lease")
		}
	}

	func abandonLeaseForTest(store Store, issueNumber int, owner LeaseOwner) error {
		_, err := store.Update("lease_abandoned_fixture", issueNumber, owner.RunID, map[string]any{"owner": owner}, func(snapshot *Snapshot) error {
			item, ownedErr := ownedIssue(snapshot, issueNumber, owner)
			if ownedErr != nil {
				return ownedErr
			}
			transition, transitionErr := issuedomain.NewTransition("abandon_fixture", item.Status, issuedomain.StatusFailed)
			if transitionErr != nil {
				return transitionErr
			}
			item.LastError = "test execution abandoned"
			if err := ApplyIssueTransition(item, transition); err != nil {
				return err
			}
			item.ResourcePark.WorktreeSHA256 = strings.Repeat("0", 64)
			return nil
		})
		return err
	}

	func TestRepositoryAllowsOnlyOneActiveExecution(t *testing.T) {
		store := newStore(t)
		_, first, err := store.ReserveLease(LeaseReservation{IssueNumber: 1, RunID: "run_1", Slot: 0, ResolvedResources: []string{"state"}})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.ReserveLease(LeaseReservation{IssueNumber: 2, RunID: "run_2", Slot: 0, ResolvedResources: []string{"docs"}}); err == nil {
			t.Fatal("occupied slot was reserved twice")
		}
		if err := abandonLeaseForTest(store, 1, first); err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.ReserveLease(LeaseReservation{IssueNumber: 2, RunID: "run_2", Slot: 0, ResolvedResources: []string{"docs"}}); err != nil {
			t.Fatalf("next Issue did not acquire the released execution: %v", err)
		}
	}

	func TestFaultConcurrentLeaseReservationsNeverOverlapResources(t *testing.T) {
		store := newStore(t)
		const contenders = 16
		start := make(chan struct{})
		var wait sync.WaitGroup
		wait.Add(contenders)
		results := make(chan error, contenders)
		for number := 1; number <= contenders; number++ {
			go func(number int) {
				defer wait.Done()
				<-start
				_, _, err := store.ReserveLease(LeaseReservation{
					IssueNumber: number, RunID: fmt.Sprintf("run_%d", number), Slot: number - 1,
					ResolvedResources: []string{"scheduler"}, ReservedAt: time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC),
				})
				results <- err
			}(number)
		}
		close(start)
		wait.Wait()
		close(results)
		successes := 0
		for err := range results {
			if err == nil {
				successes++
			}
		}
		if successes != 1 {
			t.Fatalf("successful reservations=%d want=1", successes)
		}
		snapshot, err := store.Load()
		if err != nil {
			t.Fatal(err)
		}
		leases := 0
		for _, issue := range snapshot.Issues {
			if issue != nil && issue.Lease != nil {
				leases++
				if !reflect.DeepEqual(issue.Lease.ResolvedResources, []string{"scheduler"}) {
					t.Fatalf("unexpected lease=%+v", issue.Lease)
				}
			}
		}
		if leases != 1 {
			t.Fatalf("active leases=%d want=1", leases)
		}
	}

	func TestRetainedLeaseReleasesWorkerSlotButKeepsResourceConflict(t *testing.T) {
		store := newStore(t)
		reservedAt := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
		_, owner, err := store.ReserveLease(LeaseReservation{
			IssueNumber: 1, RunID: "run_1", Slot: 0,
			ResolvedResources: []string{"docs"}, ReservedAt: reservedAt,
		})
		if err != nil {
			t.Fatal(err)
		}
		_, err = store.Update("input_requested", 1, owner.RunID, nil, func(snapshot *Snapshot) error {
			issue := snapshot.Issues["1"]
			issue.Status = issuedomain.StatusNeedsInput
			setTestWorkspace(snapshot, issue)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.ReserveLease(LeaseReservation{
			IssueNumber: 2, RunID: "run_2", Slot: 0,
			ResolvedResources: []string{"scheduler"}, ReservedAt: reservedAt,
		}); err != nil {
			t.Fatalf("released worker slot was not reusable: %v", err)
		}
		if _, _, err := store.ReserveLease(LeaseReservation{
			IssueNumber: 3, RunID: "run_3", Slot: 1,
			ResolvedResources: []string{"docs"}, ReservedAt: reservedAt,
		}); err == nil {
			t.Fatal("retained resource lease stopped conflicting")
		}
	}

	func TestParkedLeaseReleasesAdmissionAndResumeUsesNewGeneration(t *testing.T) {
		store := newStore(t)
		reservedAt := time.Date(2026, 8, 17, 9, 0, 0, 0, time.UTC)
		parkedAt := reservedAt.Add(time.Minute)
		_, owner, err := store.ReserveLease(LeaseReservation{
			IssueNumber: 314, RunID: "run_314", Slot: 0,
			DeclaredResources: []string{RepositoryResource}, ResolvedResources: []string{RepositoryResource},
			BaseSHA: "base-314", ReservedAt: reservedAt,
		})
		if err != nil {
			t.Fatal(err)
		}
		_, err = store.Update("issue_blocked", 314, owner.RunID, nil, func(snapshot *Snapshot) error {
			issue := snapshot.Issues["314"]
			issue.Status = issuedomain.StatusBlocked
			issue.Branch = "codex/issue-314"
			issue.Worktree = "/tmp/issue-314"
			setTestWorkspace(snapshot, issue)
			issue.SessionID = "session-314"
			issue.Answers = []AnswerRecord{{RequestID: "req-314", Answer: "continue"}}
			if err := CaptureContinuationLease(issue, owner, "park_314", parkedAt); err != nil {
				return err
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		parked, err := store.Load()
		if err != nil {
			t.Fatal(err)
		}
		continuation := *parked.Issues["314"]
		if continuation.Lease != nil || continuation.ResourcePark == nil || continuation.ResourcePark.OriginalLease.Owner != owner {
			t.Fatalf("parked continuation=%+v", continuation)
		}
		_, nextOwner, err := store.ReserveLease(LeaseReservation{
			IssueNumber: 448, RunID: "run_448", Slot: 0,
			ResolvedResources: []string{RepositoryResource}, BaseSHA: "base-448", ReservedAt: parkedAt.Add(time.Minute),
		})
		if err != nil {
			t.Fatalf("parked resource still blocked the following queue: %v", err)
		}
		if _, err := store.Update("continuation_resume_requested", 314, owner.RunID, nil, func(snapshot *Snapshot) error {
			_, resumeErr := ResumeParkedLease(snapshot, 314, "park_314", 1, parkedAt.Add(2*time.Minute))
			return resumeErr
		}); err == nil || !strings.Contains(err.Error(), "Issue #448") {
			t.Fatalf("competing lease was not preserved: %v", err)
		}
		if err := abandonLeaseForTest(store, 448, nextOwner); err != nil {
			t.Fatal(err)
		}
		var resumedOwner LeaseOwner
		resumed, err := store.Update("continuation_resume_requested", 314, owner.RunID, nil, func(snapshot *Snapshot) error {
			var resumeErr error
			resumedOwner, resumeErr = ResumeParkedLease(snapshot, 314, "park_314", 0, parkedAt.Add(3*time.Minute))
			if resumeErr == nil {
				snapshot.Issues["314"].Status = issuedomain.StatusResumePending
			}
			return resumeErr
		})
		if err != nil {
			t.Fatal(err)
		}
		issue := resumed.Issues["314"]
		if resumedOwner.Generation != owner.Generation+1 || issue.Lease == nil || issue.Lease.Owner != resumedOwner || issue.ResourcePark.Status != issuedomain.ResourceParkStatusResuming {
			t.Fatalf("resumed issue=%+v owner=%+v", issue, resumedOwner)
		}
		if issue.RunID != continuation.RunID || issue.Worktree != continuation.Worktree || issue.Branch != continuation.Branch ||
			issue.SessionID != continuation.SessionID || !reflect.DeepEqual(issue.Answers, continuation.Answers) ||
			!reflect.DeepEqual(issue.ResourcePark.OriginalLease, continuation.ResourcePark.OriginalLease) {
			t.Fatalf("park/resume changed continuation boundary: before=%+v after=%+v", continuation, issue)
		}
	}

	func TestFaultConcurrentParkedLeaseResumeCreatesOneFencedOwner(t *testing.T) {
		store := newStore(t)
		now := time.Date(2026, 8, 17, 10, 0, 0, 0, time.UTC)
		_, owner, err := store.ReserveLease(LeaseReservation{IssueNumber: 1, RunID: "run_1", Slot: 0, ResolvedResources: []string{"scheduler"}, ReservedAt: now})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.Update("issue_blocked", 1, owner.RunID, nil, func(snapshot *Snapshot) error {
			issue := snapshot.Issues["1"]
			issue.Status = issuedomain.StatusBlocked
			setTestWorkspace(snapshot, issue)
			return CaptureContinuationLease(issue, owner, "park_1", now.Add(time.Minute))
		}); err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		results := make(chan LeaseOwner, 2)
		errors := make(chan error, 2)
		var wait sync.WaitGroup
		for range 2 {
			wait.Add(1)
			go func() {
				defer wait.Done()
				<-start
				var got LeaseOwner
				_, updateErr := store.Update("continuation_resume_requested", 1, owner.RunID, nil, func(snapshot *Snapshot) error {
					var resumeErr error
					got, resumeErr = ResumeParkedLease(snapshot, 1, "park_1", 0, now.Add(2*time.Minute))
					if resumeErr == nil {
						snapshot.Issues["1"].Status = issuedomain.StatusResumePending
					}
					return resumeErr
				})
				results <- got
				errors <- updateErr
			}()
		}
		close(start)
		wait.Wait()
		close(results)
		close(errors)
		for err := range errors {
			if err != nil {
				t.Fatal(err)
			}
		}
		var fenced LeaseOwner
		for got := range results {
			if fenced == (LeaseOwner{}) {
				fenced = got
			} else if got != fenced {
				t.Fatalf("concurrent resume owners differ: first=%+v second=%+v", fenced, got)
			}
		}
		loaded, err := store.Load()
		if err != nil {
			t.Fatal(err)
		}
		issue := loaded.Issues["1"]
		if fenced.Generation != 2 || issue.LeaseGeneration != 2 || issue.Lease == nil || issue.Lease.Owner != fenced || issue.ResourcePark.ResumeOwner == nil || *issue.ResourcePark.ResumeOwner != fenced {
			t.Fatalf("double resume was not fenced: owner=%+v issue=%+v", fenced, issue)
		}
	}

	func TestResumedResourceParkAllowsRetryLeaseTransferAndRelease(t *testing.T) {
		store := newStore(t)
		now := time.Date(2026, 8, 18, 9, 0, 0, 0, time.UTC)
		_, originalOwner, err := store.ReserveLease(LeaseReservation{
			IssueNumber: 146, RunID: "run_resumed", Slot: 0,
			DeclaredResources: []string{"scheduler"}, ResolvedResources: []string{"scheduler"},
			BaseSHA: "base-146", ReservedAt: now,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.Update("issue_blocked", 146, originalOwner.RunID, nil, func(snapshot *Snapshot) error {
			issue := snapshot.Issues["146"]
			issue.Status = issuedomain.StatusBlocked
			setTestWorkspace(snapshot, issue)
			return CaptureContinuationLease(issue, originalOwner, "park_146", now.Add(time.Minute))
		}); err != nil {
			t.Fatal(err)
		}
		var resumeOwner LeaseOwner
		if _, err := store.Update("continuation_started", 146, originalOwner.RunID, nil, func(snapshot *Snapshot) error {
			var resumeErr error
			resumeOwner, resumeErr = ResumeParkedLease(snapshot, 146, "park_146", 0, now.Add(2*time.Minute))
			if resumeErr != nil {
				return resumeErr
			}
			issue := snapshot.Issues["146"]
			issue.Status = issuedomain.StatusRetryWait
			issue.ResourcePark.Status = issuedomain.ResourceParkStatusResumed
			return nil
		}); err != nil {
			t.Fatal(err)
		}

		var retryOwner LeaseOwner
		if _, err := store.Update("worker_started", 146, "run_retry", nil, func(snapshot *Snapshot) error {
			issue := snapshot.Issues["146"]
			var transferErr error
			retryOwner, transferErr = TransferIssueLease(issue, resumeOwner, "run_retry")
			if transferErr != nil {
				return transferErr
			}
			issue.RunID = "run_retry"
			issue.Status = issuedomain.StatusRunning
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		transferred, err := store.Load()
		if err != nil {
			t.Fatal(err)
		}
		issue := transferred.Issues["146"]
		if retryOwner.Generation != resumeOwner.Generation+1 || issue.Lease == nil || issue.Lease.Owner != retryOwner ||
			issue.ResourcePark.Status != issuedomain.ResourceParkStatusResumed || issue.ResourcePark.OriginalLease.Owner != originalOwner ||
			issue.ResourcePark.ResumeOwner == nil || *issue.ResourcePark.ResumeOwner != resumeOwner || issue.Lease.BaseSHA != "base-146" {
			t.Fatalf("retry transfer lost resumed park provenance: owner=%+v issue=%+v", retryOwner, issue)
		}
		if err := abandonLeaseForTest(store, 146, retryOwner); err != nil {
			t.Fatal(err)
		}
		released, err := store.Load()
		if err != nil {
			t.Fatal(err)
		}
		issue = released.Issues["146"]
		if issue.Lease != nil || issue.ResourcePark == nil || issue.ResourcePark.Status != issuedomain.ResourceParkStatusParked ||
			issue.ResourcePark.OriginalLease.Owner != retryOwner || issue.ResourcePark.ResumeOwner != nil ||
			issue.Suspension == nil || issue.Suspension.Status != issuedomain.SuspensionActive {
			t.Fatalf("retry completion lost terminal park provenance: %+v", issue)
		}
	}

	func TestFaultConcurrentResumedParkRetryTransferCreatesOneFencedOwner(t *testing.T) {
		store := newStore(t)
		now := time.Date(2026, 8, 18, 10, 0, 0, 0, time.UTC)
		_, originalOwner, err := store.ReserveLease(LeaseReservation{
			IssueNumber: 146, RunID: "run_resumed", Slot: 0,
			ResolvedResources: []string{"scheduler"}, ReservedAt: now,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.Update("issue_blocked", 146, originalOwner.RunID, nil, func(snapshot *Snapshot) error {
			issue := snapshot.Issues["146"]
			issue.Status = issuedomain.StatusBlocked
			setTestWorkspace(snapshot, issue)
			return CaptureContinuationLease(issue, originalOwner, "park_146", now.Add(time.Minute))
		}); err != nil {
			t.Fatal(err)
		}
		var resumeOwner LeaseOwner
		if _, err := store.Update("continuation_started", 146, originalOwner.RunID, nil, func(snapshot *Snapshot) error {
			var resumeErr error
			resumeOwner, resumeErr = ResumeParkedLease(snapshot, 146, "park_146", 0, now.Add(2*time.Minute))
			if resumeErr != nil {
				return resumeErr
			}
			issue := snapshot.Issues["146"]
			issue.Status = issuedomain.StatusRetryWait
			issue.ResourcePark.Status = issuedomain.ResourceParkStatusResumed
			return nil
		}); err != nil {
			t.Fatal(err)
		}

		start := make(chan struct{})
		errors := make(chan error, 2)
		var wait sync.WaitGroup
		for _, runID := range []string{"run_retry_a", "run_retry_b"} {
			wait.Add(1)
			go func() {
				defer wait.Done()
				<-start
				_, updateErr := store.Update("worker_started", 146, runID, nil, func(snapshot *Snapshot) error {
					issue := snapshot.Issues["146"]
					if _, transferErr := TransferIssueLease(issue, resumeOwner, runID); transferErr != nil {
						return transferErr
					}
					issue.RunID = runID
					issue.Status = issuedomain.StatusRunning
					return nil
				})
				errors <- updateErr
			}()
		}
		close(start)
		wait.Wait()
		close(errors)
		succeeded := 0
		failed := 0
		for err := range errors {
			if err == nil {
				succeeded++
			} else if strings.Contains(err.Error(), "stale lease owner") {
				failed++
			} else {
				t.Fatalf("unexpected concurrent transfer error: %v", err)
			}
		}
		loaded, err := store.Load()
		if err != nil {
			t.Fatal(err)
		}
		issue := loaded.Issues["146"]
		if succeeded != 1 || failed != 1 || issue.LeaseGeneration != resumeOwner.Generation+1 || issue.Lease == nil ||
			issue.ResourcePark == nil || issue.ResourcePark.Status != issuedomain.ResourceParkStatusResumed || issue.ResourcePark.ResumeOwner == nil || *issue.ResourcePark.ResumeOwner != resumeOwner {
			t.Fatalf("concurrent retry transfer did not converge: succeeded=%d failed=%d issue=%+v", succeeded, failed, issue)
		}
	}

	func TestResourceParkValidationFailsClosed(t *testing.T) {
		now := time.Date(2026, 8, 17, 11, 0, 0, 0, time.UTC)
		valid := Issue{
			Number: 1, Status: issuedomain.StatusBlocked, RunID: "run_1", LeaseGeneration: 1,
			ResourcePark: &ContinuationCheckpoint{
				ID: "park_1", Status: issuedomain.ResourceParkStatusParked, ParkedAt: now,
				OriginalLease: ResourceLease{Owner: LeaseOwner{RunID: "run_1", Generation: 1}, Slot: 0, DeclaredResources: []string{}, ResolvedResources: []string{"scheduler"}, ReservedAt: now},
			},
		}
		for _, test := range []struct {
			name   string
			mutate func(*Issue)
		}{
			{name: "generation mismatch", mutate: func(issue *Issue) { issue.LeaseGeneration = 2 }},
			{name: "cross run", mutate: func(issue *Issue) { issue.ResourcePark.OriginalLease.Owner.RunID = "run_other" }},
			{name: "non canonical resources", mutate: func(issue *Issue) {
				issue.ResourcePark.OriginalLease.ResolvedResources = []string{"scheduler", "scheduler"}
			}},
			{name: "active parked lease", mutate: func(issue *Issue) { lease := issue.ResourcePark.OriginalLease; issue.Lease = &lease }},
		} {
			t.Run(test.name, func(t *testing.T) {
				issue := valid
				park := *valid.ResourcePark
				park.OriginalLease.DeclaredResources = append([]string(nil), valid.ResourcePark.OriginalLease.DeclaredResources...)
				park.OriginalLease.ResolvedResources = append([]string(nil), valid.ResourcePark.OriginalLease.ResolvedResources...)
				issue.ResourcePark = &park
				test.mutate(&issue)
				if err := validateResourceLeases(Snapshot{Issues: map[string]*Issue{"1": &issue}}); err == nil {
					t.Fatal("malformed resource park was accepted")
				}
			})
		}
	}

	func TestResumedResourceParkValidationFailsClosed(t *testing.T) {
		now := time.Date(2026, 8, 18, 11, 0, 0, 0, time.UTC)
		originalOwner := LeaseOwner{RunID: "run_resumed", Generation: 1}
		resumeOwner := LeaseOwner{RunID: "run_resumed", Generation: 2}
		valid := Issue{
			Number: 1, Status: issuedomain.StatusRetryWait, RunID: "run_resumed", LeaseGeneration: 2,
			Lease: &ResourceLease{
				Owner: resumeOwner, Slot: 0, DeclaredResources: []string{}, ResolvedResources: []string{"scheduler"}, ReservedAt: now.Add(2 * time.Minute),
			},
			ResourcePark: &ContinuationCheckpoint{
				ID: "park_1", Status: issuedomain.ResourceParkStatusResumed, ParkedAt: now, ResumedAt: now.Add(2 * time.Minute), ResumeOwner: &resumeOwner,
				OriginalLease: ResourceLease{Owner: originalOwner, Slot: 0, DeclaredResources: []string{}, ResolvedResources: []string{"scheduler"}, ReservedAt: now},
			},
		}
		transferred := valid
		transferred.RunID = "run_retry"
		transferred.LeaseGeneration = 3
		transferredLease := *valid.Lease
		transferredLease.Owner = LeaseOwner{RunID: "run_retry", Generation: 3}
		transferred.Lease = &transferredLease
		if err := validateResourceLeases(Snapshot{Issues: map[string]*Issue{"1": &transferred}}); err != nil {
			t.Fatalf("valid retry transfer was rejected: %v", err)
		}

		for _, test := range []struct {
			name   string
			mutate func(*Issue)
		}{
			{name: "resume owner changed run", mutate: func(issue *Issue) { issue.ResourcePark.ResumeOwner.RunID = "run_other" }},
			{name: "same generation changed owner", mutate: func(issue *Issue) {
				issue.RunID = "run_other"
				issue.Lease.Owner.RunID = "run_other"
			}},
			{name: "released same generation changed run", mutate: func(issue *Issue) {
				issue.RunID = "run_other"
				issue.Lease = nil
			}},
			{name: "active lease predates resume", mutate: func(issue *Issue) {
				issue.LeaseGeneration = 3
				issue.RunID = "run_retry"
				issue.Lease.Owner = LeaseOwner{RunID: "run_retry", Generation: 1}
			}},
		} {
			t.Run(test.name, func(t *testing.T) {
				issue := valid
				lease := *valid.Lease
				issue.Lease = &lease
				park := *valid.ResourcePark
				parkOriginal := valid.ResourcePark.OriginalLease
				park.OriginalLease = parkOriginal
				owner := *valid.ResourcePark.ResumeOwner
				park.ResumeOwner = &owner
				issue.ResourcePark = &park
				test.mutate(&issue)
				if err := validateResourceLeases(Snapshot{Issues: map[string]*Issue{"1": &issue}}); err == nil {
					t.Fatal("ambiguous resumed resource park was accepted")
				}
			})
		}
	}

	func TestResumedNeedsInputParkAllowsFencedConflictLeaseTransfer(t *testing.T) {
		now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
		originalOwner := LeaseOwner{RunID: "run_adaf3142bd207b24", Generation: 3}
		resumeOwner := LeaseOwner{RunID: originalOwner.RunID, Generation: 4}
		conflictOwner := LeaseOwner{RunID: "conflict_zeitreise_442", Generation: 5}
		request := &Request{
			ID: "req_b24ba2cd328c461f", IssueNumber: 442, RunID: originalOwner.RunID,
			ResourceParkID: "park_zeitreise_442", ReleasedOwner: &originalOwner, Status: issuedomain.RequestStatusAnswered,
		}
		issue := &Issue{
			Number: 442, Status: issuedomain.StatusResolvingConflict, RunID: conflictOwner.RunID, LeaseGeneration: conflictOwner.Generation,
			Lease: &ResourceLease{
				Owner: conflictOwner, Slot: 0, DeclaredResources: []string{}, ResolvedResources: []string{RepositoryResource}, ReservedAt: now.Add(3 * time.Minute),
			},
			ResourcePark: &ContinuationCheckpoint{
				ID: request.ResourceParkID, Kind: ResourceParkKindNeedsInput, RequestID: request.ID, Status: issuedomain.ResourceParkStatusResumed,
				OriginalLease: ResourceLease{Owner: originalOwner, Slot: 0, DeclaredResources: []string{}, ResolvedResources: []string{RepositoryResource}, ReservedAt: now},
				ParkedAt:      now.Add(time.Minute), ResumedAt: now.Add(2 * time.Minute), ResumeOwner: &resumeOwner,
			},
		}
		snapshot := Snapshot{
			Issues: map[string]*Issue{"442": issue}, PendingRequests: map[string]*Request{request.ID: request},
		}
		if err := ValidateNeedsInputPark(issue, request); err != nil {
			t.Fatalf("valid historical provenance was rejected: %v", err)
		}
		if err := validateResourceLeases(snapshot); err != nil {
			t.Fatalf("fenced conflict lease transfer was rejected: %v", err)
		}
	}

	func TestNeedsInputParkProvenanceMismatchesFailClosed(t *testing.T) {
		now := time.Date(2026, 8, 18, 12, 30, 0, 0, time.UTC)
		fixture := func() (Snapshot, *Issue, *Request) {
			originalOwner := LeaseOwner{RunID: "run_adaf3142bd207b24", Generation: 3}
			resumeOwner := LeaseOwner{RunID: originalOwner.RunID, Generation: 4}
			conflictOwner := LeaseOwner{RunID: "conflict_zeitreise_442", Generation: 5}
			request := &Request{
				ID: "req_b24ba2cd328c461f", IssueNumber: 442, RunID: originalOwner.RunID,
				ResourceParkID: "park_zeitreise_442", ReleasedOwner: &originalOwner, Status: issuedomain.RequestStatusAnswered,
			}
			issue := &Issue{
				Number: 442, Status: issuedomain.StatusResolvingConflict, RunID: conflictOwner.RunID, LeaseGeneration: conflictOwner.Generation,
				Lease: &ResourceLease{
					Owner: conflictOwner, Slot: 0, DeclaredResources: []string{}, ResolvedResources: []string{RepositoryResource}, ReservedAt: now.Add(3 * time.Minute),
				},
				ResourcePark: &ContinuationCheckpoint{
					ID: request.ResourceParkID, Kind: ResourceParkKindNeedsInput, RequestID: request.ID, Status: issuedomain.ResourceParkStatusResumed,
					OriginalLease: ResourceLease{Owner: originalOwner, Slot: 0, DeclaredResources: []string{}, ResolvedResources: []string{RepositoryResource}, ReservedAt: now},
					ParkedAt:      now.Add(time.Minute), ResumedAt: now.Add(2 * time.Minute), ResumeOwner: &resumeOwner,
				},
			}
			return Snapshot{Issues: map[string]*Issue{"442": issue}, PendingRequests: map[string]*Request{request.ID: request}}, issue, request
		}
		tests := []struct {
			name   string
			mutate func(*Snapshot, *Issue, *Request)
		}{
			{name: "request ID", mutate: func(_ *Snapshot, _ *Issue, request *Request) { request.ID = "req_other" }},
			{name: "Issue number", mutate: func(_ *Snapshot, _ *Issue, request *Request) { request.IssueNumber = 443 }},
			{name: "park ID", mutate: func(_ *Snapshot, _ *Issue, request *Request) { request.ResourceParkID = "park_other" }},
			{name: "source run ID", mutate: func(_ *Snapshot, _ *Issue, request *Request) { request.RunID = "run_other" }},
			{name: "released owner", mutate: func(_ *Snapshot, _ *Issue, request *Request) { request.ReleasedOwner.RunID = "run_other" }},
			{name: "released generation", mutate: func(_ *Snapshot, _ *Issue, request *Request) { request.ReleasedOwner.Generation++ }},
			{name: "resume generation", mutate: func(_ *Snapshot, issue *Issue, _ *Request) { issue.ResourcePark.ResumeOwner.Generation = 6 }},
			{name: "unfenced run transfer", mutate: func(_ *Snapshot, issue *Issue, _ *Request) {
				issue.LeaseGeneration = 4
				issue.Lease.Owner.Generation = 4
			}},
		}
		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				snapshot, issue, request := fixture()
				test.mutate(&snapshot, issue, request)
				if err := validateResourceLeases(snapshot); err == nil {
					t.Fatal("mismatched needs-input provenance was accepted")
				}
			})
		}
	}

	func TestActiveNeedsInputParkRemainsBoundToCurrentRun(t *testing.T) {
		now := time.Date(2026, 8, 18, 13, 0, 0, 0, time.UTC)
		for _, status := range []issuedomain.ResourceParkStatus{issuedomain.ResourceParkStatusParked, issuedomain.ResourceParkStatusResuming} {
			t.Run(string(status), func(t *testing.T) {
				originalOwner := LeaseOwner{RunID: "run_source", Generation: 3}
				resumeOwner := LeaseOwner{RunID: originalOwner.RunID, Generation: 4}
				request := &Request{ID: "req_1", IssueNumber: 1, RunID: originalOwner.RunID, ResourceParkID: "park_1", ReleasedOwner: &originalOwner, Status: issuedomain.RequestStatusAnswered}
				issue := &Issue{
					Number: 1, Status: issuedomain.StatusResumePending, RunID: originalOwner.RunID, LeaseGeneration: originalOwner.Generation,
					ResourcePark: &ContinuationCheckpoint{
						ID: request.ResourceParkID, Kind: ResourceParkKindNeedsInput, RequestID: request.ID, Status: status,
						OriginalLease: ResourceLease{Owner: originalOwner, Slot: 0, DeclaredResources: []string{}, ResolvedResources: []string{"scheduler"}, ReservedAt: now}, ParkedAt: now.Add(time.Minute),
					},
				}
				if status == issuedomain.ResourceParkStatusResuming {
					issue.LeaseGeneration = resumeOwner.Generation
					issue.Lease = &ResourceLease{Owner: resumeOwner, Slot: 0, DeclaredResources: []string{}, ResolvedResources: []string{"scheduler"}, ReservedAt: now.Add(2 * time.Minute)}
					issue.ResourcePark.ResumeOwner = &resumeOwner
					issue.ResourcePark.ResumedAt = now.Add(2 * time.Minute)
				}
				issue.RunID = "run_unfenced"
				if err := ValidateNeedsInputPark(issue, request); err == nil {
					t.Fatal("active needs-input claim accepted a different current run")
				}
			})
		}
	}

	func TestCrashPointsReplayPreparedLeaseTransaction(t *testing.T) {
		for _, appendEvent := range []bool{false, true} {
			t.Run(fmt.Sprintf("event_appended_%v", appendEvent), func(t *testing.T) {
				store := newStore(t)
				base, err := store.Load()
				if err != nil {
					t.Fatal(err)
				}
				reservedAt := time.Date(2026, 8, 16, 11, 0, 0, 0, time.UTC)
				base.StateRevision++
				base.Issues["9"] = &Issue{
					Number: 9, Status: issuedomain.StatusClaiming, RunID: "run_9", Attempts: 1, LeaseGeneration: 1, UpdatedAt: reservedAt,
					Lease: &ResourceLease{Owner: LeaseOwner{RunID: "run_9", Generation: 1}, Slot: 0, DeclaredResources: []string{}, ResolvedResources: []string{RepositoryResource}, ReservedAt: reservedAt},
				}
				event := Event{Version: CurrentVersion, EventID: "evt_lease", Sequence: base.StateRevision, Timestamp: reservedAt, RepoID: store.RepoID, IssueNumber: 9, RunID: "run_9", Type: "lease_reserved"}
				if err := fsutil.WriteJSON(store.TransactionPath(), transaction{Version: CurrentVersion, Snapshot: base, Event: event}, 0o600); err != nil {
					t.Fatal(err)
				}
				if appendEvent {
					if err := store.appendEventUnlocked(event); err != nil {
						t.Fatal(err)
					}
				}
				loaded, err := store.Load()
				if err != nil || loaded.Issues["9"] == nil || loaded.Issues["9"].Lease == nil {
					t.Fatalf("loaded=%+v err=%v", loaded, err)
				}
				if _, err := os.Stat(store.TransactionPath()); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("prepared transaction remains: %v", err)
				}
			})
		}
	}

	func newStore(t *testing.T) Store {
		t.Helper()
		store := Store{Dir: t.TempDir(), RepoID: "repo-deadbeef", RepoPath: "/tmp/repo"}
		if err := store.Initialize(); err != nil {
			t.Fatal(err)
		}
		return store
	}

	func setTestWorkspace(snapshot *Snapshot, issue *Issue) {
		if issue.Worktree == "" {
			issue.Worktree = fmt.Sprintf("/tmp/issue-%d", issue.Number)
		}
		if issue.Branch == "" {
			issue.Branch = fmt.Sprintf("codex/issue-%d", issue.Number)
		}
		issue.Workspace = &WorkerWorkspace{
			Path: issue.Worktree, Branch: issue.Branch, RepoID: snapshot.RepoID, Repository: "owner/repo",
			GitCommonDir: "/tmp/repo/.git", MainCheckout: "/tmp/repo", CapturedAt: time.Now().UTC(),
		}
	}
*/
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
	snapshot, _ := store.Load()
	if reason, ok := snapshot.Attention(false); !ok || reason != "needs_input" {
		t.Fatalf("reason=%q ok=%v", reason, ok)
	}
	_, err = store.Update("unrelated", 0, "", nil, func(s *Snapshot) error { s.Supervisor.State = "polling"; return nil })
	if err != nil {
		t.Fatal(err)
	}
	snapshot, _ = store.Load()
	if reason, ok := snapshot.Attention(false); !ok || reason != "needs_input" {
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
			if reason, ok := snapshot.Attention(true); ok {
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
	if reason, ok := snapshot.Attention(false); !ok || reason != "blocked" {
		t.Fatalf("reason=%q ok=%v", reason, ok)
	}
	if reason, ok := snapshot.Attention(true); !ok || reason != "blocked" {
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
	if reason, ok := snapshot.Attention(false); ok {
		t.Fatalf("reason=%q ok=%v", reason, ok)
	}
	if reason, ok := snapshot.Attention(true); !ok || reason != "idle" {
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
	payload, _ := json.Marshal(map[string]string{"cause": "missing transaction"})
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
	if err := store.rotateEventsUnlocked(snapshotForRotation(t, store)); err != nil {
		t.Fatal(err)
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
	archives, err := filepath.Glob(store.EventsPath() + ".*.gz")
	if err != nil || len(archives) == 0 || len(archives) > 2 {
		t.Fatalf("archives=%v err=%v", archives, err)
	}
}

func snapshotForRotation(t *testing.T, store Store) Snapshot {
	t.Helper()
	snapshot, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}
