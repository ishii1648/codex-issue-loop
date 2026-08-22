package state

import (
	"bytes"
	"encoding/json"
	"errors"
	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func predicateStatuses(report RecoveryPredicateReport) map[RecoveryPredicateCode]string {
	result := map[RecoveryPredicateCode]string{}
	for _, predicate := range report.Predicates {
		result[predicate.Code] = predicate.Status
	}
	return result
}

func interruptedWorkspaceResumeFixture(t *testing.T) (Store, Issue, []byte) {
	t.Helper()
	data, err := os.ReadFile("testdata/zeitreise-442-v0614-full-27-events.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	store := Store{Dir: t.TempDir(), RepoID: "repo_zeitreise"}
	if err := os.WriteFile(filepath.Join(store.Dir, "events.jsonl"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	stateData, err := os.ReadFile("testdata/zeitreise-442-v0614-full-27-state.json")
	if err != nil {
		t.Fatal(err)
	}
	var issue Issue
	if err := json.Unmarshal(stateData, &issue); err != nil {
		t.Fatal(err)
	}
	return store, issue, data
}

func TestInterruptedWorkspaceResumeEvidenceFromZeitreise442Full27EventFixture(t *testing.T) {
	store, issue, data := interruptedWorkspaceResumeFixture(t)
	evidence, err := store.InterruptedWorkspaceResumeEvidence(issue)
	if err != nil {
		t.Fatal(err)
	}
	if evidence.ResumeID != issue.EnvironmentResume.ID || evidence.PreviousReason != issue.EnvironmentResume.PreviousReason ||
		evidence.BaseSHA != issue.EnvironmentResume.BaseSHA || evidence.CurrentBaseSHA != issue.EnvironmentResume.CurrentBaseSHA ||
		evidence.WorktreeHead != "3333333333333333333333333333333333333333" || evidence.WorktreeHead == evidence.BaseSHA ||
		evidence.LeaseOwner != issue.Lease.Owner || evidence.LeaseSlot != issue.Lease.Slot || !evidence.LegacyLeaseRecovered ||
		issue.SessionID != "" || issue.Session != nil {
		t.Fatalf("evidence=%+v issue=%+v", evidence, issue)
	}
	var request Event
	if err := json.Unmarshal(fixtureEventLines(data)[21], &request); err != nil {
		t.Fatal(err)
	}
	if delay := request.Timestamp.Sub(issue.EnvironmentResume.ConfirmedAt); delay != 28*time.Millisecond+433*time.Microsecond {
		t.Fatalf("request timestamp delay=%s", delay)
	}
}

func TestInterruptedWorkspaceResumePredicateReportListsIndependentFullHistoryMismatches(t *testing.T) {
	store, issue, data := interruptedWorkspaceResumeFixture(t)
	lines := fixtureEventLines(data)
	// Keep all original positions evaluable while independently breaking event
	// count, request payload, marker identity, and reconciliation remote state.
	var extra Event
	if err := json.Unmarshal(lines[len(lines)-1], &extra); err != nil {
		t.Fatal(err)
	}
	extra.Sequence++
	extra.EventID = "event_sanitized_extra"
	extra.Timestamp = extra.Timestamp.Add(time.Second)
	extraRaw, err := json.Marshal(extra)
	if err != nil {
		t.Fatal(err)
	}
	lines = append(lines, extraRaw)
	lines[21] = bytes.Replace(lines[21], []byte(`"current_base_sha":"2222222222222222222222222222222222222222"`), []byte(`"current_base_sha":"ffffffffffffffffffffffffffffffffffffffff"`), 1)
	lines[23] = bytes.Replace(lines[23], []byte(`"resume_id":"resume_0733cc3d177d05f3",`), nil, 1)
	lines[19] = bytes.Replace(lines[19], []byte(`"RemoteBranchExists":false`), []byte(`"RemoteBranchExists":true`), 1)
	if err := os.WriteFile(store.EventsPath(), joinFixtureEventLines(lines), 0o600); err != nil {
		t.Fatal(err)
	}
	issue.SessionID = "session_private"
	issue.Session = &WorkerSession{Backend: "codex", ID: issue.SessionID}
	issue.EnvironmentResume.ConfirmedAt = issue.EnvironmentResume.ConfirmedAt.Add(-2 * time.Second)

	report, err := store.InterruptedWorkspaceResumePredicateReport(issue)
	if err != nil {
		t.Fatal(err)
	}
	if report.SchemaVersion != 1 || report.Operation != "resume-blocked" || report.Eligible {
		t.Fatalf("report header=%+v", report)
	}
	statuses := predicateStatuses(report)
	for _, code := range []RecoveryPredicateCode{
		RecoveryCodeEventCount, RecoveryCodePayloadShape, RecoveryCodeSessionIdentity,
		RecoveryCodeGitHubMarkers, RecoveryCodeTimestamps, RecoveryCodeRemoteIdentity,
	} {
		if statuses[code] != "fail" {
			t.Fatalf("predicate %s=%q report=%+v", code, statuses[code], report)
		}
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"session_private", issue.Worktree, issue.EnvironmentResume.PreviousReason} {
		if bytes.Contains(encoded, []byte(secret)) {
			t.Fatalf("report leaked sensitive evidence %q: %s", secret, encoded)
		}
	}
}

func TestInterruptedWorkspaceResumeMutationAndDiagnosisUseSamePredicateCode(t *testing.T) {
	store, issue, data := interruptedWorkspaceResumeFixture(t)
	data = bytes.Replace(data, []byte(`"current_base_sha":"2222222222222222222222222222222222222222"`), []byte(`"current_base_sha":"ffffffffffffffffffffffffffffffffffffffff"`), 1)
	if err := os.WriteFile(store.EventsPath(), data, 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := store.InterruptedWorkspaceResumePredicateReport(issue)
	if err != nil {
		t.Fatal(err)
	}
	if got := report.FirstFailure(); got == nil {
		t.Fatal("diagnosis unexpectedly passed")
	} else {
		var predicateErr RecoveryPredicateError
		if !errors.As(got, &predicateErr) || predicateErr.Code != RecoveryCodePayloadShape {
			t.Fatalf("diagnostic refusal=%v", got)
		}
	}
	_, mutationErr := store.InterruptedWorkspaceResumeEvidence(issue)
	var predicateErr RecoveryPredicateError
	if !errors.As(mutationErr, &predicateErr) || predicateErr.Code != RecoveryCodePayloadShape {
		t.Fatalf("mutating path refusal=%v", mutationErr)
	}
}

func TestReadRecoveryInputsDoesNotCreateLockOrChangeDurableFiles(t *testing.T) {
	store, issue, eventData := interruptedWorkspaceResumeFixture(t)
	snapshot := Snapshot{
		Version: CurrentVersion, RepoID: store.RepoID, RepoPath: "/sanitized/repository", StateRevision: 3791,
		Issues: map[string]*Issue{"442": &issue}, PendingRequests: map[string]*Request{},
	}
	stateData, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.StatePath(), stateData, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ReadRecoveryInputs(); err != nil {
		t.Fatal(err)
	}
	stateAfter, _ := os.ReadFile(store.StatePath())
	eventsAfter, _ := os.ReadFile(store.EventsPath())
	if !bytes.Equal(stateData, stateAfter) || !bytes.Equal(eventData, eventsAfter) {
		t.Fatal("read-only recovery input changed state or events")
	}
	if _, err := os.Stat(store.lockPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read-only recovery input created a lock file: %v", err)
	}
}

func TestInterruptedWorkspaceResumeEvidenceTimestampContract(t *testing.T) {
	tests := []struct {
		name      string
		timestamp string
		wantOK    bool
	}{
		{name: "equal compatibility", timestamp: "2026-08-18T06:02:12.482456Z", wantOK: true},
		{name: "before", timestamp: "2026-08-18T06:02:12.482455Z"},
		{name: "too late", timestamp: "2026-08-18T06:02:13.482457Z"},
		{name: "zero", timestamp: "0001-01-01T00:00:00Z"},
		{name: "invalid format", timestamp: "not-a-timestamp"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, issue, data := interruptedWorkspaceResumeFixture(t)
			data = bytes.Replace(data, []byte(`"timestamp":"2026-08-18T06:02:12.510889Z"`), []byte(`"timestamp":"`+test.timestamp+`"`), 1)
			if err := os.WriteFile(store.EventsPath(), data, 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := store.InterruptedWorkspaceResumeEvidence(issue)
			if test.wantOK && err != nil {
				t.Fatalf("compatible timestamp was rejected: %v", err)
			}
			if !test.wantOK && err == nil {
				t.Fatal("invalid timestamp was accepted")
			}
			if !test.wantOK && test.name != "invalid format" && !strings.Contains(err.Error(), "timestamp boundary") {
				t.Fatalf("timestamp diagnostic=%v", err)
			}
		})
	}
}

func TestInterruptedWorkspaceResumeEvidenceReportsFull27Boundary(t *testing.T) {
	tests := []struct {
		name string
		from string
		to   string
		want string
	}{
		{
			name: "timestamp", from: `"timestamp":"2026-08-18T06:02:12.510889Z"`,
			to: `"timestamp":"2026-08-18T06:02:13.482457Z"`, want: "timestamp boundary",
		},
		{
			name: "remote branch exists", from: `"RemoteBranchExists":false`,
			to: `"RemoteBranchExists":true`, want: "reconciliation remote boundary",
		},
		{
			name: "request payload", from: `"current_base_sha":"2222222222222222222222222222222222222222"`,
			to: `"current_base_sha":"ffffffffffffffffffffffffffffffffffffffff"`, want: "request payload boundary",
		},
		{
			name: "marker", from: `"resume_id":"resume_0733cc3d177d05f3","state":"environment_resume"`,
			to: `"state":"environment_resume"`, want: "marker boundary",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, issue, data := interruptedWorkspaceResumeFixture(t)
			data = bytes.Replace(data, []byte(test.from), []byte(test.to), 1)
			if err := os.WriteFile(store.EventsPath(), data, 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := store.InterruptedWorkspaceResumeEvidence(issue)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("boundary diagnostic=%v want=%q", err, test.want)
			}
		})
	}
}

func TestInterruptedWorkspaceResumeEvidenceRetainsSyntheticShortFixtureCompatibility(t *testing.T) {
	data, err := os.ReadFile("testdata/zeitreise-442-v0614-missing-workspace-resume-events.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	stateData, err := os.ReadFile("testdata/zeitreise-442-v0614-missing-workspace-resume-state.json")
	if err != nil {
		t.Fatal(err)
	}
	var issue Issue
	if err := json.Unmarshal(stateData, &issue); err != nil {
		t.Fatal(err)
	}
	store := Store{Dir: t.TempDir(), RepoID: "repo_zeitreise"}
	if err := os.WriteFile(store.EventsPath(), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.InterruptedWorkspaceResumeEvidence(issue); err != nil {
		t.Fatalf("v0.6.16 synthetic compatibility fixture was rejected: %v", err)
	}
}

func TestInterruptedWorkspaceResumeCandidateFailsClosedForOtherSupervisorBlocks(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Issue)
	}{
		{name: "workspace mismatch", mutate: func(issue *Issue) {
			issue.BlockedCause.Reason = "worker workspace validation failed for /tmp/zeitreise-442: saved workspace provenance does not match the launch target"
			issue.LastError = issue.BlockedCause.Reason
		}},
		{name: "symlink", mutate: func(issue *Issue) {
			issue.BlockedCause.Reason = "worker workspace validation failed for /tmp/zeitreise-442: worker worktree path must not contain a symbolic link"
			issue.LastError = issue.BlockedCause.Reason
		}},
		{name: "manual supervisor block", mutate: func(issue *Issue) { issue.BlockedCause.Kind = "manual" }},
		{name: "security supervisor block", mutate: func(issue *Issue) { issue.BlockedCause.Kind = "security" }},
		{name: "publication recovery", mutate: func(issue *Issue) { issue.PublicationRecovery = &PublicationRecovery{ID: "publication_442"} }},
		{name: "active worker", mutate: func(issue *Issue) { issue.WorkerPID = 442 }},
		{name: "active worker process group", mutate: func(issue *Issue) { issue.WorkerPGID = 442 }},
		{name: "workspace already saved", mutate: func(issue *Issue) { issue.Workspace = &WorkerWorkspace{Path: issue.Worktree} }},
		{name: "lease generation changed", mutate: func(issue *Issue) { issue.LeaseGeneration++ }},
		{name: "resume reason missing", mutate: func(issue *Issue) { issue.EnvironmentResume.PreviousReason = "" }},
		{name: "session ID only", mutate: func(issue *Issue) { issue.SessionID = "session_synthesized" }},
		{name: "session object only", mutate: func(issue *Issue) { issue.Session = &WorkerSession{Backend: "codex", ID: "session_synthesized"} }},
		{name: "resource park on running resume", mutate: func(issue *Issue) { issue.ResourcePark = &ResourceLeasePark{ID: "unexpected"} }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, issue, _ := interruptedWorkspaceResumeFixture(t)
			test.mutate(&issue)
			if MayHaveInterruptedWorkspaceResumeEvidence(&issue) {
				t.Fatalf("unexpected recovery candidate: %+v", issue)
			}
		})
	}
}

func TestInterruptedWorkspaceResumeEvidenceRejectsTamperedOrReorderedHistory(t *testing.T) {
	t.Run("changed previous reason", func(t *testing.T) {
		store, issue, _ := interruptedWorkspaceResumeFixture(t)
		issue.EnvironmentResume.PreviousReason = "different environment reason"
		if _, err := store.InterruptedWorkspaceResumeEvidence(issue); err == nil {
			t.Fatal("changed EnvironmentResume.PreviousReason was accepted")
		}
	})
	tests := []struct {
		name   string
		mutate func([]byte) []byte
	}{
		{name: "changed base", mutate: func(data []byte) []byte {
			return bytes.Replace(data, []byte("2222222222222222222222222222222222222222"), []byte("ffffffffffffffffffffffffffffffffffffffff"), 1)
		}},
		{name: "different rejection", mutate: func(data []byte) []byte {
			return bytes.Replace(data, []byte("saved workspace provenance is missing"), []byte("saved workspace provenance was modified"), 1)
		}},
		{name: "missing authority event", mutate: func(data []byte) []byte {
			return removeFixtureEvent(data, 0)
		}},
		{name: "missing process event", mutate: func(data []byte) []byte {
			return removeFixtureEvent(data, 7)
		}},
		{name: "missing retry event", mutate: func(data []byte) []byte {
			return removeFixtureEvent(data, 9)
		}},
		{name: "missing reconciliation", mutate: func(data []byte) []byte {
			return removeFixtureEvent(data, 17)
		}},
		{name: "missing resume sync", mutate: func(data []byte) []byte {
			return removeFixtureEvent(data, 22)
		}},
		{name: "full history compressed to synthetic type sequence", mutate: func(data []byte) []byte {
			return selectFixtureEvents(t, data, 0, 2, 13, 14, 15, 20, 21, 22, 23, 24, 25, 26)
		}},
		{name: "duplicate authority event", mutate: func(data []byte) []byte {
			return duplicateFixtureEvent(data, 13)
		}},
		{name: "reordered process and preflight", mutate: func(data []byte) []byte {
			return swapFixtureEvents(data, 11, 12)
		}},
		{name: "unknown event", mutate: func(data []byte) []byte {
			return bytes.Replace(data, []byte(`"type":"worker_preflight_completed"`), []byte(`"type":"unknown_preflight"`), 1)
		}},
		{name: "cross run reconciliation", mutate: func(data []byte) []byte {
			return replaceFixtureEventRun(t, data, 18, "run_other")
		}},
		{name: "request owner unexpectedly present", mutate: func(data []byte) []byte {
			return bytes.Replace(data, []byte(`"current_base_sha":"2222222222222222222222222222222222222222"`), []byte(`"current_base_sha":"2222222222222222222222222222222222222222","lease_owner":{"run_id":"run_adaf3142bd207b24","generation":2}`), 1)
		}},
		{name: "legacy recovery marker missing", mutate: func(data []byte) []byte {
			return bytes.Replace(data, []byte(`"legacy_lease_recovered":true`), []byte(`"legacy_lease_recovered":false`), 1)
		}},
		{name: "malformed legacy recovery marker", mutate: func(data []byte) []byte {
			return bytes.Replace(data, []byte(`"legacy_lease_recovered":true`), []byte(`"legacy_lease_recovered":"true"`), 1)
		}},
		{name: "resume ID missing from second sync", mutate: func(data []byte) []byte {
			return bytes.Replace(data, []byte(`"resume_id":"resume_0733cc3d177d05f3","state":"environment_resume"`), []byte(`"state":"environment_resume"`), 1)
		}},
		{name: "unexpected request marker", mutate: func(data []byte) []byte {
			return bytes.Replace(data, []byte(`"resume_id":"resume_0733cc3d177d05f3"}}`), []byte(`"resume_id":"resume_0733cc3d177d05f3","unexpected_marker":true}}`), 1)
		}},
		{name: "new initial worker field", mutate: func(data []byte) []byte {
			return bytes.Replace(data, []byte(`"identity":{"backend":"codex"`), []byte(`"expected_cwd":"/sanitized/worktrees/zeitreise/issue-442","identity":{"backend":"codex"`), 1)
		}},
		{name: "missing initial worker field", mutate: func(data []byte) []byte {
			return bytes.Replace(data, []byte(`"branch":"codex/issue-442-sanitized",`), nil, 1)
		}},
		{name: "missing legacy identity field", mutate: func(data []byte) []byte {
			return bytes.Replace(data, []byte(`,"runtime_version":"sanitized-version"`), nil, 1)
		}},
		{name: "new worker process field", mutate: func(data []byte) []byte {
			return bytes.Replace(data, []byte(`"payload":{"pgid":41001`), []byte(`"payload":{"expected_cwd":"/sanitized/worktrees/zeitreise/issue-442","pgid":41001`), 1)
		}},
		{name: "missing worker process field", mutate: func(data []byte) []byte {
			return bytes.Replace(data, []byte(`,"pid":41001`), nil, 1)
		}},
		{name: "reconciliation HEAD mismatch", mutate: func(data []byte) []byte {
			return bytes.Replace(data, []byte("3333333333333333333333333333333333333333"), []byte("4444444444444444444444444444444444444444"), 1)
		}},
		{name: "HEAD confused with publication base", mutate: func(data []byte) []byte {
			return bytes.ReplaceAll(data, []byte("3333333333333333333333333333333333333333"), []byte("1111111111111111111111111111111111111111"))
		}},
		{name: "remote fields appear too early", mutate: func(data []byte) []byte {
			return bytes.Replace(data, []byte(`"Head":"3333333333333333333333333333333333333333","Dirty"`), []byte(`"Head":"3333333333333333333333333333333333333333","RemoteHead":"3333333333333333333333333333333333333333","Dirty"`), 1)
		}},
		{name: "remote fields missing too late", mutate: func(data []byte) []byte {
			return bytes.Replace(data, []byte(`,"RemoteHead":""`), nil, 1)
		}},
		{name: "remote branch unexpectedly exists", mutate: func(data []byte) []byte {
			return bytes.Replace(data, []byte(`"RemoteBranchExists":false`), []byte(`"RemoteBranchExists":true`), 1)
		}},
		{name: "late remote branch consistent", mutate: func(data []byte) []byte {
			return bytes.Replace(data, []byte(`"RemoteConsistent":false`), []byte(`"RemoteConsistent":true`), 1)
		}},
		{name: "late remote HEAD non-empty", mutate: func(data []byte) []byte {
			return bytes.Replace(data, []byte(`"RemoteHead":""`), []byte(`"RemoteHead":"3333333333333333333333333333333333333333"`), 1)
		}},
		{name: "pull requests array", mutate: func(data []byte) []byte {
			return bytes.Replace(data, []byte(`"pull_requests":null`), []byte(`"pull_requests":[]`), 1)
		}},
		{name: "new reconciliation field", mutate: func(data []byte) []byte {
			return bytes.Replace(data, []byte(`"reason":"GitHub exclusion label was applied manually","status"`), []byte(`"reason":"GitHub exclusion label was applied manually","resource_park_created":false,"status"`), 1)
		}},
		{name: "missing reconciliation field", mutate: func(data []byte) []byte {
			return bytes.Replace(data, []byte(`"previous_status":"blocked",`), nil, 1)
		}},
		{name: "superseded after blocked sync", mutate: func(data []byte) []byte {
			return append(data, []byte(`{"version":4,"event_id":"event_sanitized_3792","sequence":3792,"timestamp":"2026-08-17T12:33:06Z","repo_id":"repo_zeitreise","issue_number":442,"run_id":"run_adaf3142bd207b24","type":"worker_completed","payload":{}}`+"\n")...)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, issue, data := interruptedWorkspaceResumeFixture(t)
			if err := os.WriteFile(store.EventsPath(), test.mutate(data), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := store.InterruptedWorkspaceResumeEvidence(issue); err == nil {
				t.Fatal("tampered interrupted resume history was accepted")
			}
		})
	}
}

func fixtureEventLines(data []byte) [][]byte {
	return bytes.Split(bytes.TrimSpace(data), []byte("\n"))
}

func joinFixtureEventLines(lines [][]byte) []byte {
	return append(bytes.Join(lines, []byte("\n")), '\n')
}

func removeFixtureEvent(data []byte, index int) []byte {
	lines := fixtureEventLines(data)
	return joinFixtureEventLines(append(lines[:index:index], lines[index+1:]...))
}

func duplicateFixtureEvent(data []byte, index int) []byte {
	lines := fixtureEventLines(data)
	result := append([][]byte(nil), lines[:index+1]...)
	result = append(result, append([]byte(nil), lines[index]...))
	result = append(result, lines[index+1:]...)
	return joinFixtureEventLines(result)
}

func selectFixtureEvents(t *testing.T, data []byte, indices ...int) []byte {
	t.Helper()
	lines := fixtureEventLines(data)
	selected := make([][]byte, 0, len(indices))
	for offset, index := range indices {
		var event Event
		if err := json.Unmarshal(lines[index], &event); err != nil {
			t.Fatal(err)
		}
		event.Sequence = uint64(3765 + offset)
		encoded, err := json.Marshal(event)
		if err != nil {
			t.Fatal(err)
		}
		selected = append(selected, encoded)
	}
	return joinFixtureEventLines(selected)
}

func swapFixtureEvents(data []byte, left, right int) []byte {
	lines := fixtureEventLines(data)
	lines[left], lines[right] = lines[right], lines[left]
	return joinFixtureEventLines(lines)
}

func replaceFixtureEventRun(t *testing.T, data []byte, index int, runID string) []byte {
	t.Helper()
	lines := fixtureEventLines(data)
	var event Event
	if err := json.Unmarshal(lines[index], &event); err != nil {
		t.Fatal(err)
	}
	event.RunID = runID
	encoded, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	lines[index] = encoded
	return joinFixtureEventLines(lines)
}

func TestInterruptedWorkspaceResumeEvidenceRejectsCurrentStateMismatches(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Issue)
	}{
		{name: "status requested alone", mutate: func(issue *Issue) { issue.EnvironmentResume.Status = issuedomain.EnvironmentResumeStatusRequested }},
		{name: "status github synced alone", mutate: func(issue *Issue) { issue.EnvironmentResume.Status = issuedomain.EnvironmentResumeStatusGitHubSynced }},
		{name: "lease generation", mutate: func(issue *Issue) { issue.Lease.Owner.Generation++; issue.LeaseGeneration++ }},
		{name: "lease slot", mutate: func(issue *Issue) { issue.Lease.Slot = 1 }},
		{name: "lease reservation time", mutate: func(issue *Issue) { issue.Lease.ReservedAt = issue.Lease.ReservedAt.Add(-1) }},
		{name: "base SHA", mutate: func(issue *Issue) {
			issue.Lease.BaseSHA = "ffffffffffffffffffffffffffffffffffffffff"
			issue.EnvironmentResume.BaseSHA = issue.Lease.BaseSHA
		}},
		{name: "current base SHA", mutate: func(issue *Issue) {
			issue.EnvironmentResume.CurrentBaseSHA = "ffffffffffffffffffffffffffffffffffffffff"
		}},
		{name: "resume ID", mutate: func(issue *Issue) { issue.EnvironmentResume.ID = "resume_other" }},
		{name: "worktree", mutate: func(issue *Issue) {
			issue.Worktree = "/tmp/other"
			issue.BlockedCause.Reason = "worker workspace validation failed for /tmp/other: saved workspace provenance is missing"
			issue.LastError = issue.BlockedCause.Reason
		}},
		{name: "branch", mutate: func(issue *Issue) { issue.Branch = "codex/other" }},
		{name: "session ID only", mutate: func(issue *Issue) { issue.SessionID = "session_other" }},
		{name: "session object only", mutate: func(issue *Issue) { issue.Session = &WorkerSession{Backend: "codex", ID: "session_other"} }},
		{name: "synthesized session pair", mutate: func(issue *Issue) {
			issue.SessionID = "session_other"
			issue.Session = &WorkerSession{Backend: "codex", ID: "session_other"}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, issue, _ := interruptedWorkspaceResumeFixture(t)
			test.mutate(&issue)
			if _, err := store.InterruptedWorkspaceResumeEvidence(issue); err == nil {
				t.Fatal("mismatched current state was accepted")
			}
		})
	}
}
