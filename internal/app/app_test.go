package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/ishii1648/codex-issue-loop/internal/config"
	gh "github.com/ishii1648/codex-issue-loop/internal/github"
	"github.com/ishii1648/codex-issue-loop/internal/launchd"
	"github.com/ishii1648/codex-issue-loop/internal/layout"
	schema "github.com/ishii1648/codex-issue-loop/internal/migration"
	"github.com/ishii1648/codex-issue-loop/internal/observe"
	"github.com/ishii1648/codex-issue-loop/internal/publication"
	"github.com/ishii1648/codex-issue-loop/internal/publish"
	"github.com/ishii1648/codex-issue-loop/internal/recoveryfixture"
	"github.com/ishii1648/codex-issue-loop/internal/registry"
	"github.com/ishii1648/codex-issue-loop/internal/state"
	"github.com/ishii1648/codex-issue-loop/internal/supervisor"
	"github.com/ishii1648/codex-issue-loop/internal/userrules"
	"github.com/ishii1648/codex-issue-loop/internal/worker"
	"github.com/ishii1648/codex-issue-loop/internal/worktree"
)

type appProcessGroups struct {
	alive   map[int]bool
	signals map[int][]syscall.Signal
}

type resumedWorkspaceGitHub struct{ issue gh.Issue }

func (f resumedWorkspaceGitHub) ListReady(context.Context, config.Config) ([]gh.Issue, error) {
	return nil, nil
}
func (f resumedWorkspaceGitHub) Get(context.Context, config.Config, int) (gh.Issue, error) {
	return f.issue, nil
}
func (f resumedWorkspaceGitHub) Inspect(context.Context, config.Config, int, string) (gh.RemoteState, error) {
	return gh.RemoteState{Issue: f.issue}, nil
}
func (resumedWorkspaceGitHub) Claim(context.Context, config.Config, gh.Issue, string) error {
	return nil
}
func (resumedWorkspaceGitHub) MarkNeedsInput(context.Context, config.Config, int, string, string) error {
	return nil
}
func (resumedWorkspaceGitHub) MarkDone(context.Context, config.Config, int, string) error { return nil }
func (resumedWorkspaceGitHub) MarkFailed(context.Context, config.Config, int, string, bool) error {
	return nil
}
func (resumedWorkspaceGitHub) MarkRunning(context.Context, config.Config, int) error { return nil }
func (resumedWorkspaceGitHub) MarkConflictRetry(context.Context, config.Config, int, string) error {
	return nil
}
func (resumedWorkspaceGitHub) MarkPullRequestChecksRecovery(context.Context, config.Config, int, string) error {
	return nil
}
func (resumedWorkspaceGitHub) ReadyPullRequest(context.Context, config.Config, string) error {
	return nil
}
func (resumedWorkspaceGitHub) UpdatePullRequest(context.Context, config.Config, string) error {
	return nil
}
func (resumedWorkspaceGitHub) MergePullRequest(context.Context, config.Config, string) error {
	return nil
}

type resumedWorkspaceWorker struct {
	paths    []string
	prompts  []string
	sessions []string
	runs     int
	resumes  int
}

func (w *resumedWorkspaceWorker) Run(_ context.Context, cfg config.Config, _ gh.Issue, issue state.Issue, prompt string, _ worker.Started) (worker.Result, error) {
	w.paths = append(w.paths, cfg.RepoPath)
	w.prompts = append(w.prompts, prompt)
	w.sessions = append(w.sessions, issue.SessionID)
	w.runs++
	return worker.Result{Version: 1, Status: "retryable_failure", ExecutionProfile: "extended", Summary: "fixture retry", SessionID: "session_new_workspace_recovery", Identity: worker.Identity{Backend: "codex"}, Retry: &worker.Retry{Reason: "fixture retry"}}, nil
}

func (w *resumedWorkspaceWorker) Resume(_ context.Context, cfg config.Config, _ gh.Issue, issue state.Issue, prompt string, _ worker.Started) (worker.Result, error) {
	w.paths = append(w.paths, cfg.RepoPath)
	w.prompts = append(w.prompts, prompt)
	w.sessions = append(w.sessions, issue.SessionID)
	w.resumes++
	return worker.Result{Version: 1, Status: "retryable_failure", ExecutionProfile: "extended", Summary: "fixture retry", Retry: &worker.Retry{Reason: "fixture retry"}}, nil
}

func (f *appProcessGroups) Alive(pid int) bool           { return f.alive[pid] }
func (f *appProcessGroups) GroupAlive(pgid int) bool     { return f.alive[pgid] }
func (f *appProcessGroups) OwnsGroup(pid, pgid int) bool { return pid == pgid && f.alive[pgid] }
func (f *appProcessGroups) SignalGroup(pgid int, signal syscall.Signal) error {
	f.signals[pgid] = append(f.signals[pgid], signal)
	f.alive[pgid] = false
	return nil
}

// legacyResumeTestApp keeps older resume tests focused on lease/GitHub
// behavior even though those fixtures historically used the main checkout as
// their saved worktree. Strict workspace-boundary behavior has dedicated real
// linked-worktree tests below.
func legacyResumeTestApp(out, stderr *bytes.Buffer, controller supervisor.ProcessGroupController) App {
	return App{
		Out: out, Err: stderr, ProcessController: controller,
		validateResumeWorkspace: func(_ context.Context, _ worktree.Manager, cfg config.Config, path, branch string) (worktree.LaunchValidation, error) {
			return worktree.LaunchValidation{
				Valid: true, ExpectedCWD: path, CanonicalCWD: path, TopLevel: path, Branch: branch,
				CommonDir: filepath.Join(cfg.RepoPath, ".git"), MainCheckout: cfg.RepoPath,
				Checks: map[string]bool{"legacy_fixture": true},
			}, nil
		},
	}
}

func persistInterruptedMissingWorkspaceResume(t *testing.T, store state.Store, number int, runID, worktreePath, branch, baseSHA, currentBaseSHA, resumeID string) state.LeaseOwner {
	t.Helper()
	confirmedAt := time.Now().UTC().Add(-time.Minute)
	_, originalOwner, err := store.ReserveLease(state.LeaseReservation{
		IssueNumber: number, Title: "Interrupted missing workspace", RunID: runID, Slot: 0,
		DeclaredResources: []string{state.RepositoryResource}, ResolvedResources: []string{state.RepositoryResource},
		BaseSHA: baseSHA, ReservedAt: confirmedAt.Add(-time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	previousReason := "localhost listen denied"
	if _, err := store.Update("worker_started", number, runID, map[string]string{"worktree": worktreePath, "branch": branch}, func(snapshot *state.Snapshot) error {
		item := snapshot.Issues[strconv.Itoa(number)]
		item.Status = "running"
		item.Worktree = worktreePath
		item.Branch = branch
		item.SessionID = "session_interrupted_workspace"
		item.Session = &state.WorkerSession{Backend: "codex", ID: item.SessionID}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	blockedAt := confirmedAt.Add(-time.Minute)
	if _, err := store.Update("issue_blocked", number, runID, map[string]string{"error": "worker blocked: " + previousReason, "failure_kind": "issue"}, func(snapshot *state.Snapshot) error {
		item := snapshot.Issues[strconv.Itoa(number)]
		if err := state.ReleaseIssueLease(item, originalOwner); err != nil {
			return err
		}
		item.Status = "blocked"
		item.FailureKind = "issue"
		item.LastError = "worker blocked: " + previousReason
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update("github_state_synced", number, runID, map[string]string{"state": "blocked"}, func(*state.Snapshot) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update("startup_reconciled", number, runID, map[string]string{"previous_status": "blocked", "status": "blocked", "reason": "GitHub exclusion label was applied manually"}, func(*state.Snapshot) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update("startup_reconciled", number, runID, map[string]string{"previous_status": "blocked", "status": "blocked", "reason": "supervisor-owned worker environment block provenance preserved"}, func(snapshot *state.Snapshot) error {
		item := snapshot.Issues[strconv.Itoa(number)]
		item.BlockedCause = &state.BlockedCause{Origin: "worker", Kind: "environment", Resumable: true, Reason: previousReason, BlockedAt: blockedAt}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	resumePayload := map[string]any{
		"resume_id": resumeID, "previous_reason": previousReason, "resource_park_id": "",
		"parked_lease_reacquired": false, "legacy_worker_block": true, "legacy_lease_recovered": true,
		"interrupted_resume": false, "base_sha": baseSHA, "current_base_sha": currentBaseSHA,
	}
	var resumeOwner state.LeaseOwner
	_, err = store.Update("environment_resume_requested", number, runID, resumePayload, func(snapshot *state.Snapshot) error {
		item := snapshot.Issues[strconv.Itoa(number)]
		item.LeaseGeneration++
		resumeOwner = state.LeaseOwner{RunID: runID, Generation: item.LeaseGeneration}
		item.Lease = &state.ResourceLease{
			Owner: resumeOwner, Slot: 0, DeclaredResources: []string{}, ResolvedResources: []string{state.RepositoryResource},
			BaseSHA: baseSHA, ReservedAt: confirmedAt,
		}
		item.Status = "environment_resume_pending"
		item.EnvironmentResume = &state.EnvironmentResume{
			ID: resumeID, Status: "requested", ConfirmedAt: confirmedAt, PreviousReason: previousReason,
			BaseSHA: baseSHA, CurrentBaseSHA: currentBaseSHA,
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update("github_state_synced", number, runID, map[string]string{"state": "environment_resume"}, func(*state.Snapshot) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update("github_state_synced", number, runID, map[string]string{"state": "environment_resume", "resume_id": resumeID}, func(snapshot *state.Snapshot) error {
		snapshot.Issues[strconv.Itoa(number)].EnvironmentResume.Status = "github_synced"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update("worker_started", number, runID, map[string]string{"mode": "environment_block_resume"}, func(snapshot *state.Snapshot) error {
		item := snapshot.Issues[strconv.Itoa(number)]
		item.Status = "running"
		item.EnvironmentResume.Status = "running"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	reason := fmt.Sprintf("worker workspace validation failed for %s: saved workspace provenance is missing", worktreePath)
	checks := map[string]bool{
		"run_id": true, "session_id": true, "saved_path": true, "saved_branch_state": true, "lease_owner_generation": true,
		"managed_root": true, "no_symlink_components": true, "canonical_path": true, "not_main_checkout": true,
		"git_top_level": true, "repository_identity": true, "saved_branch": true,
	}
	rejection := map[string]any{
		"expected_cwd": worktreePath, "error": reason, "run_id": runID,
		"validation": worktree.LaunchValidation{
			Valid: true, ExpectedCWD: worktreePath, CanonicalCWD: worktreePath, TopLevel: worktreePath,
			Branch: branch, Checks: checks,
		},
	}
	if _, err := store.Update("worker_workspace_rejected", number, runID, rejection, func(snapshot *state.Snapshot) error {
		item := snapshot.Issues[strconv.Itoa(number)]
		item.Status = "blocked"
		item.FailureKind = "issue"
		item.LastError = reason
		item.WorkerPID = 0
		item.WorkerPGID = 0
		item.BlockedCause = &state.BlockedCause{
			Origin: "supervisor", Kind: "worker_workspace", Resumable: false, Reason: reason, BlockedAt: time.Now().UTC(),
		}
		// The affected durable record retained the running explicit resume marker.
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update("github_state_synced", number, runID, map[string]string{"state": "blocked"}, func(*state.Snapshot) error { return nil }); err != nil {
		t.Fatal(err)
	}
	return resumeOwner
}

func persistZeitreise442Full27EventResumeFixture(t *testing.T, store state.Store, number int, runID, worktreePath, branch, baseSHA, worktreeHead, currentBaseSHA, resumeID string) state.LeaseOwner {
	t.Helper()
	bundle, err := recoveryfixture.Load("../recoveryfixture/testdata/zeitreise-442-full-history-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	replay, err := bundle.Replay()
	if err != nil {
		t.Fatal(err)
	}
	original := replay.Snapshot.Issues[strconv.Itoa(bundle.Manifest.IssueNumber)]
	if original == nil {
		t.Fatal("unified fixture has no target Issue")
	}
	issueData := append([]byte(nil), bundle.Capture.Durable.Issue...)
	var eventLines [][]byte
	for _, raw := range bundle.Capture.Events {
		eventLines = append(eventLines, append([]byte(nil), raw...))
	}
	replacements := [][2]string{
		{original.Worktree, worktreePath},
		{original.Branch, branch},
		{original.EnvironmentResume.BaseSHA, baseSHA},
		{bundle.Capture.Worktree.Head, worktreeHead},
		{original.EnvironmentResume.CurrentBaseSHA, currentBaseSHA},
		{original.RunID, runID},
		{original.EnvironmentResume.ID, resumeID},
		{bundle.Capture.Durable.RepoID, store.RepoID},
	}
	for _, replacement := range replacements {
		issueData = bytes.ReplaceAll(issueData, []byte(replacement[0]), []byte(replacement[1]))
		for index := range eventLines {
			eventLines[index] = bytes.ReplaceAll(eventLines[index], []byte(replacement[0]), []byte(replacement[1]))
		}
	}
	for index := range eventLines {
		var event state.Event
		if err := json.Unmarshal(eventLines[index], &event); err != nil {
			t.Fatal(err)
		}
		event.IssueNumber = number
		event.RepoID = store.RepoID
		eventLines[index], err = json.Marshal(event)
		if err != nil {
			t.Fatal(err)
		}
	}
	events := append(bytes.Join(eventLines, []byte("\n")), '\n')
	checkpoint := fmt.Sprintf(`{"version":4,"event_id":"event_fixture_checkpoint","sequence":3764,"timestamp":"2026-08-17T12:19:59Z","repo_id":%q,"type":"event_log_checkpoint","payload":{"archived_through":3764}}`+"\n", store.RepoID)
	events = append([]byte(checkpoint), events...)
	var issue state.Issue
	if err := json.Unmarshal(issueData, &issue); err != nil {
		t.Fatal(err)
	}
	issue.Number = number
	snapshot, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	snapshot.StateRevision = 3791
	snapshot.Issues[strconv.Itoa(number)] = &issue
	writeJSONFixture(t, store.StatePath(), snapshot)
	if err := os.WriteFile(store.EventsPath(), events, 0o600); err != nil {
		t.Fatal(err)
	}
	return issue.Lease.Owner
}

func testEnvironment(t *testing.T) (string, layout.Layout) {
	t.Helper()
	root := t.TempDir()
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	fakeCodex := filepath.Join(binDir, "codex")
	if err := os.WriteFile(fakeCodex, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("AGENT_LOOP_HOME", filepath.Join(root, "home"))
	t.Setenv("AGENT_LOOP_SKILLS_DIR", filepath.Join(root, "skills"))
	t.Setenv("AGENT_LOOP_LAUNCH_AGENTS_DIR", filepath.Join(root, "launchagents"))
	l, err := layout.New()
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "init", "-q", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	config := `version: 4
github:
  repo: owner/repo
watch:
  reconcile_interval: 20ms
`
	if err := os.WriteFile(filepath.Join(repo, ".agent-loop.yaml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	return repo, l
}

func TestInstallAndRegister(t *testing.T) {
	repo, l := testEnvironment(t)
	var out, stderr bytes.Buffer
	a := App{In: bytes.NewBuffer(nil), Out: &out, Err: &stderr}
	if code := a.Run(context.Background(), []string{"install", "--json"}); code != 0 {
		t.Fatalf("install code=%d stderr=%s", code, stderr.String())
	}
	if _, err := os.Stat(filepath.Join(l.BinDir, "agent-loop")); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	stderr.Reset()
	if code := a.Run(context.Background(), []string{"register", "--repo", repo, "--json"}); code != 0 {
		t.Fatalf("register code=%d stderr=%s", code, stderr.String())
	}
	r, err := (registry.Store{Path: l.RegistryPath}).Load()
	if err != nil || len(r.Repos) != 1 {
		t.Fatalf("registry=%+v err=%v", r, err)
	}
	for _, entry := range r.Repos {
		if _, err := os.Stat(l.PlistPath(entry.RepoID)); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(l.RepoDir(entry.RepoID), "state.json")); err != nil {
			t.Fatal(err)
		}
	}
}

func mustConfig(t *testing.T, repo string) config.Config {
	t.Helper()
	cfg, err := config.Load(repo)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func addParkedNeedsInput(t *testing.T, snapshot *state.Snapshot, issue *state.Issue, request *state.Request, slot int) {
	t.Helper()
	if issue.RunID == "" {
		issue.RunID = fmt.Sprintf("run_%d", issue.Number)
	}
	now := request.CreatedAt
	if now.IsZero() {
		now = time.Now().UTC()
	}
	owner := state.LeaseOwner{RunID: issue.RunID, Generation: 1}
	issue.Status = "needs_input"
	issue.LeaseGeneration = 1
	issue.Lease = &state.ResourceLease{
		Owner: owner, Slot: slot, DeclaredResources: []string{}, ResolvedResources: []string{state.RepositoryResource}, BaseSHA: "base-sha", ReservedAt: now,
	}
	parkID := "park_" + request.ID
	if err := state.ParkIssueLease(issue, owner, parkID, now); err != nil {
		t.Fatal(err)
	}
	issue.ResourcePark.Kind = state.ResourceParkKindNeedsInput
	issue.ResourcePark.RequestID = request.ID
	request.RunID = issue.RunID
	request.ResourceParkID = parkID
	request.ReleasedOwner = &owner
	snapshot.Issues[strconv.Itoa(issue.Number)] = issue
	snapshot.PendingRequests[request.ID] = request
}

func TestAnswerIsRecordedAndIdempotent(t *testing.T) {
	repo, l := testEnvironment(t)
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	entry, err := (registry.Store{Path: l.RegistryPath}).Add(mustConfig(t, repo))
	if err != nil {
		t.Fatal(err)
	}
	store := state.Store{Dir: l.RepoDir(entry.RepoID), RepoID: entry.RepoID, RepoPath: repo}
	if err := store.Initialize(); err != nil {
		t.Fatal(err)
	}
	_, err = store.Update("input_requested", 4, "run", nil, func(s *state.Snapshot) error {
		s.Supervisor.State = "running"
		addParkedNeedsInput(t, s, &state.Issue{Number: 4, RunID: "run_4"}, &state.Request{ID: "req_1", IssueNumber: 4, Question: "Choose", Status: "pending"}, 0)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		var out, stderr bytes.Buffer
		a := App{In: bytes.NewBuffer(nil), Out: &out, Err: &stderr}
		code := a.Run(context.Background(), []string{"answer", "--repo", repo, "--request-id", "req_1", "--message", "A", "--json"})
		if code != 0 {
			t.Fatalf("answer %d code=%d stderr=%s", i, code, stderr.String())
		}
		var result map[string]any
		if err := json.Unmarshal(out.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
	}
	snapshot, _ := store.Load()
	if snapshot.Issues["4"].Status != "resume_pending" || len(snapshot.Issues["4"].Answers) != 1 {
		t.Fatalf("issue=%+v", snapshot.Issues["4"])
	}
}

func TestWatchAnswerReconnectRoundTripPreservesQuestionContract(t *testing.T) {
	repo, l := testEnvironment(t)
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	entry, err := (registry.Store{Path: l.RegistryPath}).Add(mustConfig(t, repo))
	if err != nil {
		t.Fatal(err)
	}
	store := state.Store{Dir: l.RepoDir(entry.RepoID), RepoID: entry.RepoID, RepoPath: repo}
	if err := store.Initialize(); err != nil {
		t.Fatal(err)
	}
	createdAt := time.Date(2026, 8, 17, 1, 2, 3, 0, time.UTC)
	firstRequest := &state.Request{
		ID: "req_desktop_1", IssueNumber: 89, Question: "Which behavior should be used?",
		Reason: "The choice changes user-visible behavior.", Recommended: "safe",
		Options:       []state.Option{{ID: "safe", Label: "Keep durable state"}, {ID: "fast", Label: "Use transient signal only"}},
		AllowFreeText: true, Status: "pending", CreatedAt: createdAt,
	}
	_, err = store.Update("input_requested", 89, "run_89", nil, func(snapshot *state.Snapshot) error {
		snapshot.Supervisor.State = "running"
		addParkedNeedsInput(t, snapshot, &state.Issue{Number: 89, RunID: "run_89"}, firstRequest, 0)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	runWatch := func(ctx context.Context) (observe.Result, int, string) {
		t.Helper()
		var out, stderr bytes.Buffer
		app := App{In: bytes.NewBuffer(nil), Out: &out, Err: &stderr}
		code := app.Run(ctx, []string{"watch", "--repo", repo, "--until-attention", "--json"})
		var result observe.Result
		if code == 0 {
			if err := json.Unmarshal(out.Bytes(), &result); err != nil {
				t.Fatalf("decode watch output: %v; output=%s", err, out.String())
			}
		}
		return result, code, stderr.String()
	}
	assertRequest := func(result observe.Result, expected *state.Request) {
		t.Helper()
		if result.Reason != "needs_input" || len(result.PendingRequests) != 1 {
			t.Fatalf("watch result=%+v", result)
		}
		if !reflect.DeepEqual(result.PendingRequests[0], expected) {
			t.Fatalf("request changed across watch: got=%+v want=%+v", result.PendingRequests[0], expected)
		}
	}

	// A newly connected Desktop monitor and a reconnected monitor must both
	// receive the complete pending request immediately from durable state.
	for attempt := 0; attempt < 2; attempt++ {
		result, code, stderr := runWatch(context.Background())
		if code != 0 {
			t.Fatalf("watch attempt %d code=%d stderr=%s", attempt, code, stderr)
		}
		assertRequest(result, firstRequest)
	}

	// Repeating the same answer is safe and creates only one durable answer.
	var firstAnswerRevision uint64
	for attempt := 0; attempt < 2; attempt++ {
		var out, stderr bytes.Buffer
		app := App{In: strings.NewReader("safe\n"), Out: &out, Err: &stderr}
		code := app.Run(context.Background(), []string{"answer", "--repo", repo, "--request-id", firstRequest.ID, "--message-file", "-", "--json"})
		if code != 0 {
			t.Fatalf("answer attempt %d code=%d stderr=%s", attempt, code, stderr.String())
		}
		if attempt == 0 {
			snapshot, err := store.Load()
			if err != nil {
				t.Fatal(err)
			}
			firstAnswerRevision = snapshot.StateRevision
		}
	}
	snapshot, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if request := snapshot.PendingRequests[firstRequest.ID]; request.Status != "answered" || request.Answer != "safe" {
		t.Fatalf("answered request=%+v", request)
	}
	if issue := snapshot.Issues["89"]; issue.Status != "resume_pending" || len(issue.Answers) != 1 || issue.Answers[0].RequestID != firstRequest.ID {
		t.Fatalf("issue after answer=%+v", issue)
	}
	if snapshot.StateRevision != firstAnswerRevision {
		t.Fatalf("idempotent answer created a second durable revision: first=%d final=%d", firstAnswerRevision, snapshot.StateRevision)
	}

	// The same monitor can return to one blocking watch. A later durable
	// request wakes it without any caller-side polling.
	type watchOutcome struct {
		result observe.Result
		code   int
		stderr string
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	outcomeCh := make(chan watchOutcome, 1)
	go func() {
		result, code, stderr := runWatch(ctx)
		outcomeCh <- watchOutcome{result: result, code: code, stderr: stderr}
	}()
	secondRequest := &state.Request{
		ID: "req_desktop_2", IssueNumber: 90, Question: "Continue?", Recommended: "continue",
		Options: []state.Option{{ID: "continue", Label: "Continue"}, {ID: "stop", Label: "Stop"}},
		Status:  "pending", CreatedAt: createdAt.Add(time.Minute),
	}
	_, err = store.Update("input_requested", 90, "run_90", nil, func(snapshot *state.Snapshot) error {
		snapshot.Issues["90"] = &state.Issue{Number: 90, RunID: "run_90", Status: "needs_input"}
		snapshot.PendingRequests[secondRequest.ID] = secondRequest
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	outcome := <-outcomeCh
	if outcome.code != 0 {
		t.Fatalf("blocking watch code=%d stderr=%s", outcome.code, outcome.stderr)
	}
	assertRequest(outcome.result, secondRequest)
}

func TestStopCancelsEverySavedWorkerBeforeRecordingSupervisorStopped(t *testing.T) {
	repo, l := testEnvironment(t)
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	entry, err := (registry.Store{Path: l.RegistryPath}).Add(mustConfig(t, repo))
	if err != nil {
		t.Fatal(err)
	}
	entry.Commands["launchctl"] = "/usr/bin/false"
	writeJSONFixture(t, l.RegistryPath, registry.Registry{Version: registry.CurrentVersion, Repos: map[string]registry.Entry{entry.RepoID: entry}})
	store := state.Store{Dir: l.RepoDir(entry.RepoID), RepoID: entry.RepoID, RepoPath: repo}
	if err := store.Initialize(); err != nil {
		t.Fatal(err)
	}
	_, err = store.Update("workers_running", 0, "", nil, func(snapshot *state.Snapshot) error {
		for _, number := range []int{1, 2} {
			snapshot.Issues[strconv.Itoa(number)] = &state.Issue{
				Number: number, RunID: fmt.Sprintf("run_%d", number), Status: "running",
				WorkerPID: 100 + number, WorkerPGID: 100 + number,
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	groups := &appProcessGroups{alive: map[int]bool{101: true, 102: true}, signals: map[int][]syscall.Signal{}}
	var out, stderr bytes.Buffer
	a := App{Out: &out, Err: &stderr, ProcessController: groups}
	if code := a.Run(context.Background(), []string{"stop", "--repo", repo, "--json"}); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	snapshot, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Supervisor.State != "stopped" || len(groups.signals) != 2 {
		t.Fatalf("supervisor=%+v signals=%v", snapshot.Supervisor, groups.signals)
	}
	for _, key := range []string{"1", "2"} {
		if issue := snapshot.Issues[key]; issue.Status != "retry_wait" || issue.WorkerPID != 0 || issue.WorkerPGID != 0 {
			t.Fatalf("Issue %s=%+v", key, issue)
		}
	}
}

func TestStartRecoversOnlyUnloadedSharedBrokerWhenSupervisorIsLoaded(t *testing.T) {
	repo, l := testEnvironment(t)
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(t.TempDir(), "webhook-secret")
	if err := os.WriteFile(secret, []byte("fixture-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	configuration := fmt.Sprintf(`version: 4
github:
  repo: owner/repo
  repository_id: 1234
webhook:
  mode: webhook
  listener_address: 127.0.0.1:8787
  public_url_identifier: fixture.example/webhook
  secret_source:
    file: %q
  installation_ids: [99]
`, secret)
	if err := os.WriteFile(filepath.Join(repo, config.FileName), []byte(configuration), 0o600); err != nil {
		t.Fatal(err)
	}
	binDir := strings.Split(os.Getenv("PATH"), string(os.PathListSeparator))[0]
	launchctl := filepath.Join(binDir, "launchctl")
	script := `#!/bin/sh
case "$1" in
  print)
    case "$2" in
      *broker*) test -f "$START_TEST_BROKER" && printf 'state = running\npid = 222\n' ;;
      *) test -f "$START_TEST_REPO" && printf 'state = running\npid = 111\n' ;;
    esac
    ;;
  bootstrap)
    case "$3" in
      *broker*) : > "$START_TEST_BROKER"; printf 'bootstrap broker\n' >> "$START_TEST_LOG" ;;
      *) : > "$START_TEST_REPO"; printf 'bootstrap repo\n' >> "$START_TEST_LOG" ;;
    esac
    ;;
  bootout) printf 'bootout\n' >> "$START_TEST_LOG" ;;
esac
`
	if err := os.WriteFile(launchctl, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	repoState := filepath.Join(t.TempDir(), "repo-loaded")
	brokerState := filepath.Join(t.TempDir(), "broker-loaded")
	logPath := filepath.Join(t.TempDir(), "launchctl.log")
	if err := os.WriteFile(repoState, []byte("loaded"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("START_TEST_REPO", repoState)
	t.Setenv("START_TEST_BROKER", brokerState)
	t.Setenv("START_TEST_LOG", logPath)
	cfg := mustConfig(t, repo)
	entry, err := (registry.Store{Path: l.RegistryPath}).Add(cfg)
	if err != nil {
		t.Fatal(err)
	}
	store := state.Store{Dir: l.RepoDir(entry.RepoID), RepoID: entry.RepoID, RepoPath: repo}
	if err := store.Initialize(); err != nil {
		t.Fatal(err)
	}
	manager := launchd.Manager{Layout: l, Launchctl: launchctl}
	if err := manager.WritePlist(entry, filepath.Join(binDir, "codex")); err != nil {
		t.Fatal(err)
	}
	if err := manager.WriteBrokerPlist(filepath.Join(binDir, "codex"), entry.EnvironmentPath); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := (App{Out: &out, Err: &out}).control(context.Background(), l, "start", []string{"--repo", repo, "--json"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(brokerState); err != nil {
		t.Fatalf("broker was not recovered: %v", err)
	}
	logData, err := os.ReadFile(logPath)
	if err != nil || string(logData) != "bootstrap broker\n" {
		t.Fatalf("unexpected shared restart activity: log=%q err=%v", logData, err)
	}
}

func TestAnswerChangesOnlyTheRequestAndIssueNamedByRequestID(t *testing.T) {
	repo, l := testEnvironment(t)
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	entry, err := (registry.Store{Path: l.RegistryPath}).Add(mustConfig(t, repo))
	if err != nil {
		t.Fatal(err)
	}
	store := state.Store{Dir: l.RepoDir(entry.RepoID), RepoID: entry.RepoID, RepoPath: repo}
	if err := store.Initialize(); err != nil {
		t.Fatal(err)
	}
	_, err = store.Update("input_requested", 0, "", nil, func(snapshot *state.Snapshot) error {
		for _, number := range []int{4, 7} {
			request := &state.Request{ID: fmt.Sprintf("req_%d", number), IssueNumber: number, Question: fmt.Sprintf("%d?", number), Status: "pending"}
			addParkedNeedsInput(t, snapshot, &state.Issue{Number: number, RunID: fmt.Sprintf("run_%d", number)}, request, number-4)
		}
		snapshot.PendingRequests["req_4"].Question = "Four?"
		snapshot.PendingRequests["req_7"].Question = "Seven?"
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	a := App{In: bytes.NewBuffer(nil), Out: &out, Err: &stderr}
	if code := a.Run(context.Background(), []string{"answer", "--repo", repo, "--request-id", "req_7", "--message", "seven", "--json"}); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	snapshot, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.PendingRequests["req_4"].Status != "pending" || snapshot.PendingRequests["req_4"].Answer != "" || snapshot.Issues["4"].Status != "needs_input" || len(snapshot.Issues["4"].Answers) != 0 {
		t.Fatalf("unrelated request or Issue changed: request=%+v issue=%+v", snapshot.PendingRequests["req_4"], snapshot.Issues["4"])
	}
	if snapshot.PendingRequests["req_7"].Status != "answered" || snapshot.Issues["7"].Status != "resume_pending" || len(snapshot.Issues["7"].Answers) != 1 {
		t.Fatalf("target request or Issue not updated: request=%+v issue=%+v", snapshot.PendingRequests["req_7"], snapshot.Issues["7"])
	}
}

func TestAnswerDurablyWaitsWithoutStealingConflictingLease(t *testing.T) {
	repo, l := testEnvironment(t)
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	entry, err := (registry.Store{Path: l.RegistryPath}).Add(mustConfig(t, repo))
	if err != nil {
		t.Fatal(err)
	}
	store := state.Store{Dir: l.RepoDir(entry.RepoID), RepoID: entry.RepoID, RepoPath: repo}
	if err := store.Initialize(); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 18, 5, 0, 0, 0, time.UTC)
	var competingOwner state.LeaseOwner
	_, err = store.Update("fixture", 0, "", nil, func(snapshot *state.Snapshot) error {
		addParkedNeedsInput(t, snapshot, &state.Issue{Number: 4, RunID: "run_4"}, &state.Request{
			ID: "req_4", IssueNumber: 4, Question: "Continue?", Status: "pending", CreatedAt: now,
		}, 0)
		competingOwner = state.LeaseOwner{RunID: "run_5", Generation: 1}
		snapshot.Issues["5"] = &state.Issue{
			Number: 5, RunID: "run_5", Status: "running", LeaseGeneration: 1,
			Lease: &state.ResourceLease{Owner: competingOwner, Slot: 0, DeclaredResources: []string{}, ResolvedResources: []string{state.RepositoryResource}, BaseSHA: "base-5", ReservedAt: now},
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	a := App{In: bytes.NewBuffer(nil), Out: &out, Err: &stderr}
	args := []string{"answer", "--repo", repo, "--request-id", "req_4", "--message", "continue", "--json"}
	if code := a.Run(context.Background(), args); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(out.String(), `"claim_waiting": true`) || !strings.Contains(out.String(), `"issue_number": 5`) {
		t.Fatalf("structured waiting output=%s", out.String())
	}
	afterFirst, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if issue := afterFirst.Issues["4"]; issue.Status != "answer_claim_waiting" || issue.Lease != nil || issue.ResourcePark.Status != "parked" || len(issue.Answers) != 1 {
		t.Fatalf("answered Issue=%+v", issue)
	}
	if request := afterFirst.PendingRequests["req_4"]; request.Status != "answered" || request.Answer != "continue" {
		t.Fatalf("request=%+v", request)
	}
	if lease := afterFirst.Issues["5"].Lease; lease == nil || lease.Owner != competingOwner {
		t.Fatalf("competing lease was changed: %+v", lease)
	}
	revision := afterFirst.StateRevision
	out.Reset()
	stderr.Reset()
	if code := a.Run(context.Background(), args); code != 0 {
		t.Fatalf("idempotent code=%d stderr=%s", code, stderr.String())
	}
	afterSecond, _ := store.Load()
	if afterSecond.StateRevision != revision || len(afterSecond.Issues["4"].Answers) != 1 || afterSecond.Issues["5"].Lease.Owner != competingOwner {
		t.Fatalf("idempotent answer changed state: before=%d after=%d", revision, afterSecond.StateRevision)
	}
}

func TestRetryConflictResumesLegacyBlockedIssueWithoutReplacingBranchOrPullRequest(t *testing.T) {
	repo, l := testEnvironment(t)
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	runGitApp(t, repo, "config", "user.name", "Test User")
	runGitApp(t, repo, "config", "user.email", "test@example.com")
	runGitApp(t, repo, "config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitApp(t, repo, "add", "README.md")
	runGitApp(t, repo, "commit", "-m", "initial")
	branch := "codex/issue-4-test"
	runGitApp(t, repo, "checkout", "-b", branch)
	remote := filepath.Join(filepath.Dir(repo), "remote.git")
	runGitApp(t, filepath.Dir(repo), "init", "--bare", remote)
	runGitApp(t, repo, "remote", "add", "origin", remote)
	runGitApp(t, repo, "push", "-u", "origin", branch)

	binDir := filepath.Join(filepath.Dir(repo), "bin")
	fakeGH := filepath.Join(binDir, "gh")
	logPath := filepath.Join(filepath.Dir(repo), "gh-calls.log")
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$AGENT_LOOP_TEST_GH_LOG"
case "$1 $2" in
  "issue view")
    if printf '%s\n' "$*" | grep -q -- '--jq'; then printf '%s\n' ''; else printf '%s\n' '{"number":4,"title":"Conflict","body":"","url":"https://example.test/issues/4","state":"OPEN","labels":[{"name":"blocked"}],"assignees":[],"milestone":null,"comments":[]}'; fi
    ;;
  "pr list") printf '%s\n' '[{"number":9,"url":"https://example.test/pull/9","state":"OPEN","isDraft":true,"mergedAt":null,"headRefName":"codex/issue-4-test","mergeStateStatus":"DIRTY","statusCheckRollup":[]}]' ;;
  "issue edit"|"issue comment") exit 0 ;;
  *) exit 2 ;;
esac
`
	if err := os.WriteFile(fakeGH, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENT_LOOP_TEST_GH_LOG", logPath)
	configFile, err := os.OpenFile(filepath.Join(repo, config.FileName), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintf(configFile, "git:\n  worktree_root: %q\n", filepath.Dir(repo)); err != nil {
		t.Fatal(err)
	}
	if err := configFile.Close(); err != nil {
		t.Fatal(err)
	}
	cfg := mustConfig(t, repo)
	repo = cfg.RepoPath
	entry := registry.Entry{
		RepoID: registry.RepoID(cfg.GitHub.Repo, repo), RepoPath: repo, GitHubRepo: cfg.GitHub.Repo,
		Commands: map[string]string{"git": "/usr/bin/git", "gh": fakeGH},
	}
	writeJSONFixture(t, l.RegistryPath, registry.Registry{Version: registry.CurrentVersion, Repos: map[string]registry.Entry{entry.RepoID: entry}})
	store := state.Store{Dir: l.RepoDir(entry.RepoID), RepoID: entry.RepoID, RepoPath: repo}
	if err := store.Initialize(); err != nil {
		t.Fatal(err)
	}
	prURL := "https://example.test/pull/9"
	_, err = store.Update("blocked", 4, "run_4", nil, func(s *state.Snapshot) error {
		s.Issues["4"] = &state.Issue{
			Number: 4, Status: "blocked", RunID: "run_4", Branch: branch, Worktree: repo,
			PullRequestURL: prURL, LastError: "Pull Request lifecycle: Pull Request has merge conflicts",
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	a := App{Out: &out, Err: &stderr}
	if code := a.Run(context.Background(), []string{"retry", "--repo", repo, "--issue", "4", "--json"}); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	snapshot, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	item := snapshot.Issues["4"]
	if item.Status != "resolving_conflict" || item.GitHubSync != "" || item.Branch != branch || item.PullRequestURL != prURL || item.ConflictRecovery == nil {
		t.Fatalf("item=%+v", item)
	}
	calls, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(calls), "--remove-label blocked") || strings.Contains(string(calls), "--remove-label do-not-automate") || !strings.Contains(string(calls), "codex-issue-loop:conflict-retry:") {
		t.Fatalf("calls=%s", calls)
	}
}

func TestResumeBlockedEnvironmentPreservesWorktreeBranchSessionAndDirtyChanges(t *testing.T) {
	repo, l := testEnvironment(t)
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	runGitApp(t, repo, "config", "user.name", "Test User")
	runGitApp(t, repo, "config", "user.email", "test@example.com")
	runGitApp(t, repo, "config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitApp(t, repo, "add", "README.md")
	runGitApp(t, repo, "commit", "-m", "initial")
	runGitApp(t, repo, "branch", "-M", "main")
	remotePath := filepath.Join(filepath.Dir(repo), "environment-remote.git")
	runGitApp(t, filepath.Dir(repo), "init", "--bare", remotePath)
	runGitApp(t, repo, "remote", "add", "origin", remotePath)
	runGitApp(t, repo, "push", "-u", "origin", "main")
	baseSHA := runGitOutputApp(t, repo, "rev-parse", "HEAD")
	branch := "codex/issue-8-network"
	runGitApp(t, repo, "checkout", "-b", branch)
	runGitApp(t, repo, "push", "-u", "origin", branch)
	if err := os.WriteFile(filepath.Join(repo, "dirty-evidence.txt"), []byte("preserve me\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	fakeGH := filepath.Join(filepath.Dir(repo), "bin", "gh-resume")
	logPath := filepath.Join(filepath.Dir(repo), "resume-gh-calls.log")
	failOncePath := filepath.Join(filepath.Dir(repo), "resume-gh-failed-once")
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$AGENT_LOOP_TEST_GH_LOG"
case "$1 $2" in
  "issue view") printf '%s\n' '{"number":8,"title":"Network","body":"","url":"https://example.test/issues/8","state":"OPEN","labels":[{"name":"blocked"}],"assignees":[],"milestone":null,"comments":[]}' ;;
  "pr list") printf '%s\n' '[]' ;;
  "pr create") printf '%s\n' 'https://example.test/pull/8' ;;
  "issue edit") if [ ! -e "$AGENT_LOOP_TEST_GH_FAIL_ONCE" ]; then : > "$AGENT_LOOP_TEST_GH_FAIL_ONCE"; exit 1; fi; exit 0 ;;
  "issue comment") exit 0 ;;
  *) exit 2 ;;
esac
`
	if err := os.WriteFile(fakeGH, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENT_LOOP_TEST_GH_LOG", logPath)
	t.Setenv("AGENT_LOOP_TEST_GH_FAIL_ONCE", failOncePath)
	configFile, err := os.OpenFile(filepath.Join(repo, config.FileName), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintf(configFile, "git:\n  worktree_root: %q\n", filepath.Dir(repo)); err != nil {
		t.Fatal(err)
	}
	if err := configFile.Close(); err != nil {
		t.Fatal(err)
	}
	cfg := mustConfig(t, repo)
	repo = cfg.RepoPath
	entry := registry.Entry{
		RepoID: registry.RepoID(cfg.GitHub.Repo, repo), RepoPath: repo, GitHubRepo: cfg.GitHub.Repo,
		Commands: map[string]string{"git": "/usr/bin/git", "gh": fakeGH},
	}
	writeJSONFixture(t, l.RegistryPath, registry.Registry{Version: registry.CurrentVersion, Repos: map[string]registry.Entry{entry.RepoID: entry}})
	store := state.Store{Dir: l.RepoDir(entry.RepoID), RepoID: entry.RepoID, RepoPath: repo}
	if err := store.Initialize(); err != nil {
		t.Fatal(err)
	}
	_, owner, err := store.ReserveLease(state.LeaseReservation{
		IssueNumber: 8, Title: "Network", RunID: "run_8", Slot: 0,
		DeclaredResources: []string{state.RepositoryResource}, ResolvedResources: []string{state.RepositoryResource},
		BaseSHA: baseSHA, ReservedAt: time.Now().UTC().Add(-time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Update("worker_started", 8, owner.RunID, map[string]string{"worktree": repo, "branch": branch}, func(s *state.Snapshot) error {
		item := s.Issues["8"]
		item.Status = "running"
		item.Branch = branch
		item.Worktree = repo
		item.SessionID = "session-8"
		item.Session = &state.WorkerSession{Backend: "codex", ID: "session-8"}
		item.DeclaredResources = []string{state.RepositoryResource}
		item.ActualResources = []string{state.RepositoryResource}
		item.Answers = []state.AnswerRecord{{RequestID: "req-8", Question: "Continue?", Answer: "yes", AnsweredAt: time.Now().UTC()}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	legacyError := "worker blocked: localhost bind denied"
	_, err = store.Update("issue_blocked", 8, "run_8", map[string]string{"error": legacyError, "failure_kind": "issue"}, func(s *state.Snapshot) error {
		item := s.Issues["8"]
		item.Status = "blocked"
		item.FailureKind = "issue"
		item.LastError = legacyError
		item.UpdatedAt = time.Now().UTC()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Update("github_state_synced", 8, "run_8", map[string]string{"state": "blocked"}, func(*state.Snapshot) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Update("startup_reconciled", 8, "run_8", map[string]string{
		"previous_status": "blocked", "status": "blocked", "reason": "GitHub exclusion label was applied manually",
	}, func(snapshot *state.Snapshot) error {
		item := snapshot.Issues["8"]
		item.LastError = "startup reconciliation blocked: GitHub exclusion label was applied manually"
		return state.ReleaseIssueLease(item, owner)
	})
	if err != nil {
		t.Fatal(err)
	}
	typedCause := &state.BlockedCause{Origin: "worker", Kind: "environment", Resumable: true, Reason: "localhost bind denied"}
	legacySnapshot, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	historyCause, err := store.LegacyWorkerBlockProvenance(*legacySnapshot.Issues["8"])
	if err != nil {
		t.Fatal(err)
	}
	typedCause.BlockedAt = historyCause.BlockedAt
	_, err = store.Update("startup_reconciled", 8, "run_8", map[string]string{
		"previous_status": "blocked", "status": "blocked", "reason": "supervisor-owned worker environment block provenance preserved",
	}, func(snapshot *state.Snapshot) error {
		item := snapshot.Issues["8"]
		item.LastError = legacyError
		item.BlockedCause = typedCause
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	_, competingOwner, err := store.ReserveLease(state.LeaseReservation{
		IssueNumber: 99, Title: "Competing", RunID: "run_99", Slot: 1,
		ResolvedResources: []string{"host"}, BaseSHA: baseSHA, ReservedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	conflicted, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	var conflictOut, conflictErr bytes.Buffer
	conflictApp := legacyResumeTestApp(&conflictOut, &conflictErr, &appProcessGroups{alive: map[int]bool{}, signals: map[int][]syscall.Signal{}})
	if code := conflictApp.Run(context.Background(), []string{"resume-blocked", "--repo", repo, "--issue", "8", "--confirm-prerequisite-resolved", "--json"}); code == 0 || !strings.Contains(conflictErr.String(), "cannot recover repo:*") {
		t.Fatalf("competing lease was accepted: code=%d stdout=%s stderr=%s", code, conflictOut.String(), conflictErr.String())
	}
	afterConflict, err := store.Load()
	if err != nil || afterConflict.StateRevision != conflicted.StateRevision || afterConflict.Issues["8"].Lease != nil {
		t.Fatalf("rejected competing lease changed recovery state: before=%d after=%d issue=%+v err=%v", conflicted.StateRevision, afterConflict.StateRevision, afterConflict.Issues["8"], err)
	}
	if _, err := store.ReleaseLease(99, competingOwner, "test conflict cleared"); err != nil {
		t.Fatal(err)
	}
	blockedSnapshot, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	blockedRevision := blockedSnapshot.StateRevision
	var durableResumeID, durableBaseSHA string
	var durableGeneration uint64
	for attempt := 0; attempt < 3; attempt++ {
		var out, stderr bytes.Buffer
		a := legacyResumeTestApp(&out, &stderr, &appProcessGroups{alive: map[int]bool{}, signals: map[int][]syscall.Signal{}})
		code := a.Run(context.Background(), []string{"resume-blocked", "--repo", repo, "--issue", "8", "--confirm-prerequisite-resolved", "--json"})
		if attempt == 0 {
			if code == 0 {
				t.Fatal("injected GitHub synchronization failure was not reported")
			}
			pending, loadErr := store.Load()
			if loadErr != nil || pending.StateRevision != blockedRevision+1 || pending.Issues["8"].Status != "environment_resume_pending" || pending.Issues["8"].GitHubSync != "environment_resume" {
				t.Fatalf("write-ahead resume was not retained: issue=%+v err=%v", pending.Issues["8"], loadErr)
			}
			durableResumeID = pending.Issues["8"].EnvironmentResume.ID
			durableBaseSHA = pending.Issues["8"].Lease.BaseSHA
			durableGeneration = pending.Issues["8"].LeaseGeneration
			continue
		}
		if code != 0 {
			t.Fatalf("attempt=%d code=%d stderr=%s", attempt, code, stderr.String())
		}
	}
	snapshot, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	item := snapshot.Issues["8"]
	if item.Status != "environment_resume_pending" || item.GitHubSync != "" || item.RunID != "run_8" || item.Branch != branch || item.Worktree != repo || item.SessionID != "session-8" || item.Lease == nil {
		t.Fatalf("item=%+v", item)
	}
	if item.BlockedCause == nil || item.BlockedCause.Origin != "worker" || item.BlockedCause.Kind != "environment" || item.BlockedCause.Reason != "localhost bind denied" || item.BlockedCause.BlockedAt.IsZero() || item.EnvironmentResume.PreviousReason != "localhost bind denied" || item.Lease.ResolvedResources[0] != state.RepositoryResource {
		t.Fatalf("legacy worker block was not normalized conservatively: %+v", item)
	}
	if item.Lease.BaseSHA != baseSHA || item.Lease.BaseSHA != durableBaseSHA || item.LeaseGeneration != durableGeneration || item.EnvironmentResume.ID != durableResumeID {
		t.Fatalf("resume metadata was not durable and idempotent: issue=%+v base=%s resume=%s generation=%d", item, durableBaseSHA, durableResumeID, durableGeneration)
	}
	if item.Session == nil || item.Session.ID != "session-8" || len(item.Answers) != 1 || item.Answers[0].Answer != "yes" || strings.Join(item.DeclaredResources, ",") != state.RepositoryResource || strings.Join(item.ActualResources, ",") != state.RepositoryResource {
		t.Fatalf("session, answers, or resource metadata changed: %+v", item)
	}
	if data, err := os.ReadFile(filepath.Join(repo, "dirty-evidence.txt")); err != nil || string(data) != "preserve me\n" {
		t.Fatalf("dirty changes were lost: data=%q err=%v", data, err)
	}
	calls, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(calls), "--remove-label blocked") || strings.Contains(string(calls), "--remove-label do-not-automate") || strings.Count(string(calls), "codex-issue-loop:environment-resume:") != 1 {
		t.Fatalf("calls=%s", calls)
	}
	result, audit, err := (publish.Manager{GitPath: "/usr/bin/git", GHPath: fakeGH}).Publish(
		context.Background(), cfg, gh.Issue{Number: 8, Title: "Network"}, repo, branch, "", "implemented", item.Lease.BaseSHA, item.DeclaredResources,
	)
	if err != nil {
		t.Fatalf("publish resumed dirty worktree: %v (audit=%+v)", err, audit)
	}
	if result.PullRequestURL != "https://example.test/pull/8" || result.Commit == "" || result.Commit == baseSHA || audit.BaseSHA != baseSHA || !reflect.DeepEqual(audit.ChangedPaths, []string{".agent-loop.yaml", "dirty-evidence.txt"}) {
		t.Fatalf("resumed publication did not audit, commit, push, and open a Pull Request: result=%+v audit=%+v", result, audit)
	}
	if remoteHead := runGitOutputApp(t, repo, "rev-parse", "origin/"+branch); remoteHead != result.Commit {
		t.Fatalf("remote branch=%s, want published commit %s", remoteHead, result.Commit)
	}
}

func TestFaultResumeBlockedBackfillsMissingWorkspaceProvenanceForDirtyBehindManagedWorktree(t *testing.T) {
	repo, l := testEnvironment(t)
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	root := filepath.Dir(repo)
	managedRoot := filepath.Join(root, "managed-worktrees")
	if err := os.MkdirAll(managedRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	managedRoot, err := filepath.EvalSymlinks(managedRoot)
	if err != nil {
		t.Fatal(err)
	}
	configFile, err := os.OpenFile(filepath.Join(repo, config.FileName), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintf(configFile, "git:\n  worktree_root: %q\n", managedRoot); err != nil {
		t.Fatal(err)
	}
	if err := configFile.Close(); err != nil {
		t.Fatal(err)
	}
	runGitApp(t, repo, "config", "user.name", "Test User")
	runGitApp(t, repo, "config", "user.email", "test@example.com")
	runGitApp(t, repo, "config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitApp(t, repo, "add", ".agent-loop.yaml", "README.md")
	runGitApp(t, repo, "commit", "-m", "base")
	runGitApp(t, repo, "branch", "-M", "main")
	remotePath := filepath.Join(root, "workspace-recovery-remote.git")
	runGitApp(t, root, "init", "--bare", remotePath)
	runGitApp(t, repo, "remote", "add", "origin", remotePath)
	runGitApp(t, repo, "push", "-u", "origin", "main")
	baseSHA := runGitOutputApp(t, repo, "rev-parse", "HEAD")
	branch := "codex/issue-148-workspace-recovery"
	managedWorktree := filepath.Join(managedRoot, "repo-fixture", "issue-148")
	if err := os.MkdirAll(filepath.Dir(managedWorktree), 0o700); err != nil {
		t.Fatal(err)
	}
	runGitApp(t, repo, "worktree", "add", "-b", branch, managedWorktree, baseSHA)
	runGitApp(t, managedWorktree, "push", "-u", "origin", branch)
	if err := os.WriteFile(filepath.Join(managedWorktree, "dirty-unpublished.txt"), []byte("preserve exactly\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "main-ahead.txt"), []byte("new base work\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitApp(t, repo, "add", "main-ahead.txt")
	runGitApp(t, repo, "commit", "-m", "main advances")
	runGitApp(t, repo, "push", "origin", "main")
	currentBaseSHA := runGitOutputApp(t, repo, "rev-parse", "HEAD")

	fakeGH := filepath.Join(root, "bin", "gh-workspace-recovery")
	failOncePath := filepath.Join(root, "workspace-recovery-failed-once")
	script := `#!/bin/sh
case "$1 $2" in
  "issue view") printf '%s\n' '{"number":148,"title":"Workspace recovery","body":"","url":"https://example.test/issues/148","state":"OPEN","labels":[{"name":"blocked"}],"assignees":[],"milestone":null,"comments":[]}' ;;
  "pr list") printf '%s\n' '[]' ;;
  "issue edit") if [ ! -e "$AGENT_LOOP_TEST_GH_FAIL_ONCE" ]; then : > "$AGENT_LOOP_TEST_GH_FAIL_ONCE"; exit 1; fi; exit 0 ;;
  "issue comment") exit 0 ;;
  *) exit 2 ;;
esac
`
	if err := os.WriteFile(fakeGH, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENT_LOOP_TEST_GH_FAIL_ONCE", failOncePath)
	cfg := mustConfig(t, repo)
	entry := registry.Entry{
		RepoID: registry.RepoID(cfg.GitHub.Repo, cfg.RepoPath), RepoPath: cfg.RepoPath, GitHubRepo: cfg.GitHub.Repo,
		Commands: map[string]string{"git": "/usr/bin/git", "gh": fakeGH},
	}
	writeJSONFixture(t, l.RegistryPath, registry.Registry{Version: registry.CurrentVersion, Repos: map[string]registry.Entry{entry.RepoID: entry}})
	store := state.Store{Dir: l.RepoDir(entry.RepoID), RepoID: entry.RepoID, RepoPath: cfg.RepoPath}
	if err := store.Initialize(); err != nil {
		t.Fatal(err)
	}
	_, owner, err := store.ReserveLease(state.LeaseReservation{
		IssueNumber: 148, Title: "Workspace recovery", RunID: "run_workspace_recovery", Slot: 0,
		ResolvedResources: []string{state.RepositoryResource}, BaseSHA: baseSHA, ReservedAt: time.Now().UTC().Add(-time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	blockedAt := time.Now().UTC().Add(-time.Minute)
	_, err = store.Update("issue_blocked", 148, owner.RunID, nil, func(snapshot *state.Snapshot) error {
		item := snapshot.Issues["148"]
		item.Status = "blocked"
		item.Worktree = managedWorktree
		item.Branch = branch
		item.SessionID = "session-workspace-recovery"
		item.Session = &state.WorkerSession{Backend: "codex", ID: item.SessionID}
		item.BlockedCause = &state.BlockedCause{Origin: "worker", Kind: "environment", Resumable: true, Reason: "network unavailable", BlockedAt: blockedAt}
		item.LastError = "worker blocked: network unavailable"
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	before, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if before.Issues["148"].Workspace != nil {
		t.Fatalf("fixture unexpectedly has workspace provenance: %+v", before.Issues["148"].Workspace)
	}

	args := []string{"resume-blocked", "--repo", repo, "--issue", "148", "--confirm-prerequisite-resolved", "--json"}
	controller := &appProcessGroups{alive: map[int]bool{}, signals: map[int][]syscall.Signal{}}
	var out, stderr bytes.Buffer
	raceApp := App{
		Out: &out, Err: &stderr, ProcessController: controller,
		validateResumeWorkspace: func(ctx context.Context, manager worktree.Manager, cfg config.Config, path, branch string) (worktree.LaunchValidation, error) {
			validation, validateErr := manager.ValidateLaunch(ctx, cfg, path, branch)
			if validateErr != nil {
				return validation, validateErr
			}
			_, updateErr := store.Update("test_concurrent_workspace_recovery", 148, owner.RunID, nil, func(snapshot *state.Snapshot) error {
				snapshot.Issues["148"].UpdatedAt = time.Now().UTC()
				return nil
			})
			return validation, updateErr
		},
	}
	if code := raceApp.Run(context.Background(), args); code == 0 || !strings.Contains(stderr.String(), "durable state changed") {
		t.Fatalf("concurrent state change was not fenced: code=%d stdout=%s stderr=%s", code, out.String(), stderr.String())
	}
	afterRace, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if afterRace.Issues["148"].Status != "blocked" || afterRace.Issues["148"].Workspace != nil || afterRace.Issues["148"].EnvironmentResume != nil || afterRace.Issues["148"].Lease.Owner != owner {
		t.Fatalf("fenced recovery partially mutated lifecycle/provenance: %+v", afterRace.Issues["148"])
	}
	out.Reset()
	stderr.Reset()
	if code := (App{Out: &out, Err: &stderr, ProcessController: controller}).Run(context.Background(), args); code == 0 || !strings.Contains(stderr.String(), "durable resume remains pending") {
		t.Fatalf("injected synchronization fault did not preserve recovery: code=%d stdout=%s stderr=%s", code, out.String(), stderr.String())
	}
	pending, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	item := pending.Issues["148"]
	if item.Status != "environment_resume_pending" || item.GitHubSync != "environment_resume" || item.Workspace == nil || item.Lease == nil {
		t.Fatalf("workspace and lifecycle were not persisted together: %+v", item)
	}
	validation, err := (worktree.Manager{StateRoot: l.Root, GitPath: "/usr/bin/git"}).ValidateLaunch(context.Background(), cfg, managedWorktree, branch)
	if err != nil || !item.Workspace.Matches(validation.CanonicalCWD, validation.Branch, entry.RepoID, cfg.GitHub.Repo, cfg.GitHub.RepositoryID, validation.CommonDir, validation.MainCheckout) {
		t.Fatalf("backfill does not satisfy spawn validator: workspace=%+v validation=%+v err=%v", item.Workspace, validation, err)
	}
	if item.Lease.BaseSHA != baseSHA || item.EnvironmentResume.CurrentBaseSHA != currentBaseSHA || item.Worktree != managedWorktree || item.Branch != branch {
		t.Fatalf("resume changed original base/worktree/branch: %+v", item)
	}
	if got := runGitOutputApp(t, managedWorktree, "rev-parse", "HEAD"); got != baseSHA {
		t.Fatalf("dirty behind-main worktree was rebased or moved: got=%s want=%s", got, baseSHA)
	}
	if data, err := os.ReadFile(filepath.Join(managedWorktree, "dirty-unpublished.txt")); err != nil || string(data) != "preserve exactly\n" {
		t.Fatalf("dirty worktree content changed: data=%q err=%v", data, err)
	}
	events, err := os.ReadFile(store.EventsPath())
	if err != nil {
		t.Fatal(err)
	}
	eventText := string(events)
	for _, evidence := range []string{
		`"type":"environment_resume_requested"`, `"old_provenance_missing":true`, `"confirm_prerequisite_resolved":true`,
		`"expected"`, `"actual"`, `"git_common_dir"`, `"repository_identity":true`, `"not_main_checkout":true`,
	} {
		if !strings.Contains(eventText, evidence) {
			t.Fatalf("workspace recovery audit is missing %s: %s", evidence, eventText)
		}
	}

	out.Reset()
	stderr.Reset()
	if code := (App{Out: &out, Err: &stderr, ProcessController: controller}).Run(context.Background(), args); code != 0 {
		t.Fatalf("restart convergence failed: code=%d stdout=%s stderr=%s", code, out.String(), stderr.String())
	}
	converged, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	revision := converged.StateRevision
	ownerAfterRecovery := converged.Issues["148"].Lease.Owner
	out.Reset()
	stderr.Reset()
	if code := (App{Out: &out, Err: &stderr, ProcessController: controller}).Run(context.Background(), args); code != 0 || !strings.Contains(out.String(), `"idempotent": true`) {
		t.Fatalf("idempotent retry failed: code=%d stdout=%s stderr=%s", code, out.String(), stderr.String())
	}
	idempotent, err := store.Load()
	if err != nil || idempotent.StateRevision != revision || idempotent.Issues["148"].Lease.Owner != ownerAfterRecovery {
		t.Fatalf("idempotent retry duplicated lifecycle/lease: before=%d after=%d issue=%+v err=%v", revision, idempotent.StateRevision, idempotent.Issues["148"], err)
	}

	_, err = store.Update("test_workspace_mismatch", 148, owner.RunID, nil, func(snapshot *state.Snapshot) error {
		snapshot.Issues["148"].Workspace.GitCommonDir = filepath.Join(t.TempDir(), ".git")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	mismatched, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	out.Reset()
	stderr.Reset()
	if code := (App{Out: &out, Err: &stderr, ProcessController: controller}).Run(context.Background(), args); code == 0 || !strings.Contains(stderr.String(), "does not match") {
		t.Fatalf("existing provenance mismatch was accepted: code=%d stdout=%s stderr=%s", code, out.String(), stderr.String())
	}
	afterMismatch, err := store.Load()
	if err != nil || afterMismatch.StateRevision != mismatched.StateRevision || afterMismatch.Issues["148"].Workspace.GitCommonDir != mismatched.Issues["148"].Workspace.GitCommonDir {
		t.Fatalf("mismatch rejection changed state: before=%d after=%d issue=%+v err=%v", mismatched.StateRevision, afterMismatch.StateRevision, afterMismatch.Issues["148"], err)
	}
}

func TestFaultZeitreise442Full27EventHistoryBackfillsAndSpawnsSameWorktree(t *testing.T) {
	repo, l := testEnvironment(t)
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	root := filepath.Dir(repo)
	managedRoot := filepath.Join(root, "interrupted-managed-worktrees")
	if err := os.MkdirAll(managedRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	managedRoot, err := filepath.EvalSymlinks(managedRoot)
	if err != nil {
		t.Fatal(err)
	}
	configFile, err := os.OpenFile(filepath.Join(repo, config.FileName), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintf(configFile, "git:\n  worktree_root: %q\n", managedRoot); err != nil {
		t.Fatal(err)
	}
	if err := configFile.Close(); err != nil {
		t.Fatal(err)
	}
	for _, pair := range [][2]string{{"user.name", "Test User"}, {"user.email", "test@example.com"}, {"commit.gpgsign", "false"}} {
		runGitApp(t, repo, "config", pair[0], pair[1])
	}
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitApp(t, repo, "add", ".agent-loop.yaml", "README.md")
	runGitApp(t, repo, "commit", "-m", "base")
	runGitApp(t, repo, "branch", "-M", "main")
	remotePath := filepath.Join(root, "interrupted-workspace-remote.git")
	runGitApp(t, root, "init", "--bare", remotePath)
	runGitApp(t, repo, "remote", "add", "origin", remotePath)
	runGitApp(t, repo, "push", "-u", "origin", "main")
	baseSHA := runGitOutputApp(t, repo, "rev-parse", "HEAD")
	branch := "codex/issue-150-interrupted-workspace"
	managedWorktree := filepath.Join(managedRoot, "repo-fixture", "issue-150")
	if err := os.MkdirAll(filepath.Dir(managedWorktree), 0o700); err != nil {
		t.Fatal(err)
	}
	runGitApp(t, repo, "worktree", "add", "-b", branch, managedWorktree, baseSHA)
	if err := os.WriteFile(filepath.Join(managedWorktree, "worker-head.txt"), []byte("local worker head\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitApp(t, managedWorktree, "add", "worker-head.txt")
	runGitApp(t, managedWorktree, "commit", "-m", "worker head")
	worktreeHead := runGitOutputApp(t, managedWorktree, "rev-parse", "HEAD")
	if worktreeHead == baseSHA {
		t.Fatal("fixture worktree HEAD must differ from the original publication base")
	}
	if err := os.WriteFile(filepath.Join(managedWorktree, "dirty-v0614.txt"), []byte("preserve interrupted resume\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	inspection, err := (worktree.Manager{GitPath: "/usr/bin/git"}).Inspect(context.Background(), mustConfig(t, repo), managedWorktree, branch)
	if err != nil || !inspection.Valid || !inspection.LocalBranchExists || inspection.RemoteBranchExists || !inspection.Dirty || inspection.Head != worktreeHead {
		t.Fatalf("fixture must remain the exact dirty local-only branch: inspection=%+v err=%v", inspection, err)
	}
	if err := os.WriteFile(filepath.Join(repo, "main-advanced.txt"), []byte("new main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitApp(t, repo, "add", "main-advanced.txt")
	runGitApp(t, repo, "commit", "-m", "advance main")
	runGitApp(t, repo, "push", "origin", "main")
	currentBaseSHA := runGitOutputApp(t, repo, "rev-parse", "HEAD")

	resumeID := "resume_0733cc3d177d05f3"
	fakeGH := filepath.Join(root, "bin", "gh-interrupted-workspace")
	failOncePath := filepath.Join(root, "interrupted-workspace-gh-failed-once")
	missingResumeMarkerPath := filepath.Join(root, "interrupted-workspace-missing-resume-marker")
	missingFailureMarkerPath := filepath.Join(root, "interrupted-workspace-missing-failure-marker")
	extraMarkerPath := filepath.Join(root, "interrupted-workspace-extra-marker")
	markerOverridePath := filepath.Join(root, "interrupted-workspace-marker-override.json")
	resumeComment := strconv.Quote("<!-- codex-issue-loop:environment-resume:" + resumeID + " -->")
	originalFailureComment := strconv.Quote(failureComment(150, "worker blocked: localhost listen denied"))
	workspaceFailureComment := strconv.Quote(failureComment(150, fmt.Sprintf("worker workspace validation failed for %s: saved workspace provenance is missing", managedWorktree)))
	script := fmt.Sprintf(`#!/bin/sh
case "$1 $2" in
  "issue view") if [ -e "$AGENT_LOOP_TEST_INTERRUPTED_MARKER_OVERRIDE" ]; then /bin/cat "$AGENT_LOOP_TEST_INTERRUPTED_MARKER_OVERRIDE"; elif [ -e "$AGENT_LOOP_TEST_INTERRUPTED_MISSING_RESUME_MARKER" ]; then printf '%%s\n' '{"number":150,"title":"Interrupted workspace","body":"","url":"https://example.test/issues/150","state":"OPEN","labels":[{"name":"blocked"}],"assignees":[],"milestone":null,"comments":[{"body":%s},{"body":%s},{"body":%s}]}'; elif [ -e "$AGENT_LOOP_TEST_INTERRUPTED_MISSING_FAILURE_MARKER" ]; then printf '%%s\n' '{"number":150,"title":"Interrupted workspace","body":"","url":"https://example.test/issues/150","state":"OPEN","labels":[{"name":"blocked"}],"assignees":[],"milestone":null,"comments":[{"body":%s},{"body":%s},{"body":%s}]}'; elif [ -e "$AGENT_LOOP_TEST_INTERRUPTED_EXTRA_MARKER" ]; then printf '%%s\n' '{"number":150,"title":"Interrupted workspace","body":"","url":"https://example.test/issues/150","state":"OPEN","labels":[{"name":"blocked"}],"assignees":[],"milestone":null,"comments":[{"body":%s},{"body":%s},{"body":%s},{"body":%s},{"body":%s}]}'; else printf '%%s\n' '{"number":150,"title":"Interrupted workspace","body":"","url":"https://example.test/issues/150","state":"OPEN","labels":[{"name":"blocked"}],"assignees":[],"milestone":null,"comments":[{"body":%s},{"body":%s},{"body":%s},{"body":%s}]}'; fi ;;
  "pr list") printf '%%s\n' '[]' ;;
  "issue edit") if [ ! -e "$AGENT_LOOP_TEST_INTERRUPTED_GH_FAIL_ONCE" ]; then : > "$AGENT_LOOP_TEST_INTERRUPTED_GH_FAIL_ONCE"; exit 1; fi; exit 0 ;;
  "issue comment") exit 0 ;;
  *) exit 2 ;;
esac
`, resumeComment, originalFailureComment, workspaceFailureComment,
		resumeComment, resumeComment, originalFailureComment,
		resumeComment, resumeComment, resumeComment, originalFailureComment, workspaceFailureComment,
		resumeComment, resumeComment, originalFailureComment, workspaceFailureComment)
	if err := os.WriteFile(fakeGH, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENT_LOOP_TEST_INTERRUPTED_GH_FAIL_ONCE", failOncePath)
	t.Setenv("AGENT_LOOP_TEST_INTERRUPTED_MISSING_RESUME_MARKER", missingResumeMarkerPath)
	t.Setenv("AGENT_LOOP_TEST_INTERRUPTED_MISSING_FAILURE_MARKER", missingFailureMarkerPath)
	t.Setenv("AGENT_LOOP_TEST_INTERRUPTED_EXTRA_MARKER", extraMarkerPath)
	t.Setenv("AGENT_LOOP_TEST_INTERRUPTED_MARKER_OVERRIDE", markerOverridePath)
	cfg := mustConfig(t, repo)
	entry := registry.Entry{
		RepoID: registry.RepoID(cfg.GitHub.Repo, cfg.RepoPath), RepoPath: cfg.RepoPath, GitHubRepo: cfg.GitHub.Repo,
		Commands: map[string]string{"git": "/usr/bin/git", "gh": fakeGH},
	}
	writeJSONFixture(t, l.RegistryPath, registry.Registry{Version: registry.CurrentVersion, Repos: map[string]registry.Entry{entry.RepoID: entry}})
	store := state.Store{Dir: l.RepoDir(entry.RepoID), RepoID: entry.RepoID, RepoPath: cfg.RepoPath}
	if err := store.Initialize(); err != nil {
		t.Fatal(err)
	}
	owner := persistZeitreise442Full27EventResumeFixture(t, store, 150, "run_interrupted_workspace", managedWorktree, branch, baseSHA, worktreeHead, currentBaseSHA, resumeID)
	seeded, err := store.Load()
	if err != nil || seeded.Issues["150"] == nil {
		t.Fatalf("full 27-event fixture was not installed: issue=%+v err=%v", seeded.Issues["150"], err)
	}

	args := []string{"resume-blocked", "--repo", repo, "--issue", "150", "--confirm-prerequisite-resolved", "--json"}
	controller := &appProcessGroups{alive: map[int]bool{}, signals: map[int][]syscall.Signal{}}
	if err := os.WriteFile(filepath.Join(managedWorktree, "later-head.txt"), []byte("head moved after reconciliation\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitApp(t, managedWorktree, "add", "later-head.txt")
	runGitApp(t, managedWorktree, "commit", "-m", "move worker head after reconciliation")
	newWorktreeHead := runGitOutputApp(t, managedWorktree, "rev-parse", "HEAD")
	var headOut, headErr bytes.Buffer
	if code := (App{Out: &headOut, Err: &headErr, ProcessController: controller}).Run(context.Background(), args); code == 0 || !strings.Contains(headErr.String(), "worktree HEAD changed") {
		t.Fatalf("changed worktree HEAD was accepted: code=%d stdout=%s stderr=%s", code, headOut.String(), headErr.String())
	}
	afterHeadMismatch, err := store.Load()
	if err != nil || afterHeadMismatch.StateRevision != seeded.StateRevision || afterHeadMismatch.Issues["150"].Workspace != nil || afterHeadMismatch.Issues["150"].Lease.Owner != owner {
		t.Fatalf("worktree HEAD rejection changed state: before=%d after=%d issue=%+v err=%v", seeded.StateRevision, afterHeadMismatch.StateRevision, afterHeadMismatch.Issues["150"], err)
	}
	worktreeHead = newWorktreeHead
	owner = persistZeitreise442Full27EventResumeFixture(t, store, 150, "run_interrupted_workspace", managedWorktree, branch, baseSHA, worktreeHead, currentBaseSHA, resumeID)
	if err := os.WriteFile(missingResumeMarkerPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	beforeMarkerMismatch, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	var markerOut, markerErr bytes.Buffer
	if code := (App{Out: &markerOut, Err: &markerErr, ProcessController: controller}).Run(context.Background(), args); code == 0 || !strings.Contains(markerErr.String(), "GitHub state does not prove") {
		t.Fatalf("missing GitHub resume marker was accepted: code=%d stdout=%s stderr=%s", code, markerOut.String(), markerErr.String())
	}
	afterMarkerMismatch, err := store.Load()
	if err != nil || afterMarkerMismatch.StateRevision != beforeMarkerMismatch.StateRevision || afterMarkerMismatch.Issues["150"].Workspace != nil || afterMarkerMismatch.Issues["150"].Lease.Owner != owner {
		t.Fatalf("GitHub marker rejection changed state: before=%d after=%d issue=%+v err=%v", beforeMarkerMismatch.StateRevision, afterMarkerMismatch.StateRevision, afterMarkerMismatch.Issues["150"], err)
	}
	if err := os.Remove(missingResumeMarkerPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(missingFailureMarkerPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	markerOut.Reset()
	markerErr.Reset()
	if code := (App{Out: &markerOut, Err: &markerErr, ProcessController: controller}).Run(context.Background(), args); code == 0 || !strings.Contains(markerErr.String(), "GitHub state does not prove") {
		t.Fatalf("missing GitHub failure marker was accepted: code=%d stdout=%s stderr=%s", code, markerOut.String(), markerErr.String())
	}
	afterFailureMarkerMismatch, err := store.Load()
	if err != nil || afterFailureMarkerMismatch.StateRevision != beforeMarkerMismatch.StateRevision || afterFailureMarkerMismatch.Issues["150"].Workspace != nil || afterFailureMarkerMismatch.Issues["150"].Lease.Owner != owner {
		t.Fatalf("GitHub failure marker rejection changed state: before=%d after=%d issue=%+v err=%v", beforeMarkerMismatch.StateRevision, afterFailureMarkerMismatch.StateRevision, afterFailureMarkerMismatch.Issues["150"], err)
	}
	if err := os.Remove(missingFailureMarkerPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(extraMarkerPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	markerOut.Reset()
	markerErr.Reset()
	if code := (App{Out: &markerOut, Err: &markerErr, ProcessController: controller}).Run(context.Background(), args); code == 0 || !strings.Contains(markerErr.String(), "GitHub state does not prove") {
		t.Fatalf("extra GitHub resume marker was accepted: code=%d stdout=%s stderr=%s", code, markerOut.String(), markerErr.String())
	}
	afterExtraMarker, err := store.Load()
	if err != nil || afterExtraMarker.StateRevision != beforeMarkerMismatch.StateRevision || afterExtraMarker.Issues["150"].Workspace != nil || afterExtraMarker.Issues["150"].Lease.Owner != owner {
		t.Fatalf("extra GitHub marker rejection changed state: before=%d after=%d issue=%+v err=%v", beforeMarkerMismatch.StateRevision, afterExtraMarker.StateRevision, afterExtraMarker.Issues["150"], err)
	}
	if err := os.Remove(extraMarkerPath); err != nil {
		t.Fatal(err)
	}
	marker := "<!-- codex-issue-loop:environment-resume:" + resumeID + " -->"
	originalFailure := failureComment(150, "worker blocked: localhost listen denied")
	workspaceFailure := failureComment(150, fmt.Sprintf("worker workspace validation failed for %s: saved workspace provenance is missing", managedWorktree))
	writeMarkerOverride := func(t *testing.T, comments ...string) {
		t.Helper()
		commentObjects := make([]map[string]string, 0, len(comments))
		for _, comment := range comments {
			commentObjects = append(commentObjects, map[string]string{"body": comment})
		}
		fixture := map[string]any{
			"number": 150, "title": "Interrupted workspace", "body": "", "url": "https://example.test/issues/150", "state": "OPEN",
			"labels": []map[string]string{{"name": "blocked"}}, "assignees": []any{}, "milestone": nil, "comments": commentObjects,
		}
		data, marshalErr := json.Marshal(fixture)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		if writeErr := os.WriteFile(markerOverridePath, append(data, '\n'), 0o600); writeErr != nil {
			t.Fatal(writeErr)
		}
	}
	markerCases := []struct {
		name     string
		comments []string
	}{
		{name: "three failure markers", comments: []string{marker, marker, originalFailure, workspaceFailure, failureComment(150, "unexpected third reason")}},
		{name: "extra unique failure marker", comments: []string{marker, marker, originalFailure, workspaceFailure, failureIDMarker("unexpected marker without base marker")}},
		{name: "different resume ID", comments: []string{marker, marker, "<!-- codex-issue-loop:environment-resume:resume_other -->", originalFailure, workspaceFailure}},
		{name: "failure reason mismatch", comments: []string{marker, marker, originalFailure, failureComment(150, "wrong workspace rejection reason")}},
	}
	for _, markerCase := range markerCases {
		t.Run(markerCase.name, func(t *testing.T) {
			writeMarkerOverride(t, markerCase.comments...)
			defer os.Remove(markerOverridePath)
			markerOut.Reset()
			markerErr.Reset()
			if code := (App{Out: &markerOut, Err: &markerErr, ProcessController: controller}).Run(context.Background(), args); code == 0 || !strings.Contains(markerErr.String(), "GitHub state does not prove") {
				t.Fatalf("invalid GitHub markers were accepted: code=%d stdout=%s stderr=%s", code, markerOut.String(), markerErr.String())
			}
			after, loadErr := store.Load()
			if loadErr != nil || after.StateRevision != beforeMarkerMismatch.StateRevision || after.Issues["150"].Workspace != nil || after.Issues["150"].Lease.Owner != owner {
				t.Fatalf("GitHub marker rejection changed state: before=%d after=%d issue=%+v err=%v", beforeMarkerMismatch.StateRevision, after.StateRevision, after.Issues["150"], loadErr)
			}
		})
	}
	var ready sync.WaitGroup
	ready.Add(2)
	release := make(chan struct{})
	validator := func(ctx context.Context, manager worktree.Manager, cfg config.Config, path, branch string) (worktree.LaunchValidation, error) {
		validation, validateErr := manager.ValidateLaunch(ctx, cfg, path, branch)
		ready.Done()
		<-release
		return validation, validateErr
	}
	type recoveryResult struct {
		code   int
		stdout string
		stderr string
	}
	results := make(chan recoveryResult, 2)
	for range 2 {
		go func() {
			var localOut, localErr bytes.Buffer
			code := (App{Out: &localOut, Err: &localErr, ProcessController: controller, validateResumeWorkspace: validator}).Run(context.Background(), args)
			results <- recoveryResult{code: code, stdout: localOut.String(), stderr: localErr.String()}
		}()
	}
	ready.Wait()
	close(release)
	succeeded := 0
	durableFailure := 0
	revisionFailure := 0
	for range 2 {
		result := <-results
		if result.code == 0 {
			succeeded++
		} else if strings.Contains(result.stderr, "durable resume remains pending") {
			durableFailure++
		} else if strings.Contains(result.stderr, "durable state changed") {
			revisionFailure++
		} else {
			t.Fatalf("parallel recovery failed without a revision fence: %+v", result)
		}
	}
	if succeeded != 0 || durableFailure != 1 || revisionFailure != 1 {
		t.Fatalf("parallel crash boundary results: success=%d durable-failure=%d revision-failure=%d", succeeded, durableFailure, revisionFailure)
	}
	pending, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	pendingItem := pending.Issues["150"]
	pendingOwner := state.LeaseOwner{RunID: owner.RunID, Generation: owner.Generation + 1}
	if pendingItem.Status != "environment_resume_pending" || pendingItem.GitHubSync != "environment_resume" || pendingItem.Workspace == nil ||
		pendingItem.EnvironmentResume == nil || pendingItem.EnvironmentResume.Status != "requested" || pendingItem.Lease == nil ||
		pendingItem.Lease.Owner != pendingOwner || pendingItem.LeaseGeneration != pendingOwner.Generation {
		t.Fatalf("exact v0.6.14 crash boundary partially persisted recovery: %+v", pendingItem)
	}
	var resumeOut, resumeErr bytes.Buffer
	if code := (App{Out: &resumeOut, Err: &resumeErr, ProcessController: controller}).Run(context.Background(), args); code != 0 {
		t.Fatalf("crash-boundary retry failed: code=%d stdout=%s stderr=%s", code, resumeOut.String(), resumeErr.String())
	}
	recovered, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	item := recovered.Issues["150"]
	recoveredOwner := pendingOwner
	if item.Status != "environment_resume_pending" || item.GitHubSync != "" || item.Workspace == nil ||
		item.EnvironmentResume == nil || item.EnvironmentResume.ID != resumeID || item.EnvironmentResume.Status != "github_synced" ||
		item.Lease == nil || item.Lease.Owner != recoveredOwner || item.LeaseGeneration != recoveredOwner.Generation || item.Lease.BaseSHA != baseSHA || worktreeHead == baseSHA ||
		item.SessionID != "" || item.Session != nil ||
		item.BlockedCause == nil || item.BlockedCause.Origin != "worker" || item.BlockedCause.Kind != "environment" ||
		!item.BlockedCause.Resumable || item.BlockedCause.Reason != item.EnvironmentResume.PreviousReason {
		t.Fatalf("interrupted workspace recovery did not converge atomically: %+v", item)
	}
	if got := runGitOutputApp(t, managedWorktree, "rev-parse", "HEAD"); got != worktreeHead {
		t.Fatalf("worker HEAD changed during recovery: got=%s want=%s base=%s", got, worktreeHead, baseSHA)
	}
	if data, err := os.ReadFile(filepath.Join(managedWorktree, "dirty-v0614.txt")); err != nil || string(data) != "preserve interrupted resume\n" {
		t.Fatalf("dirty worktree changed: data=%q err=%v", data, err)
	}
	revision := recovered.StateRevision
	var out, stderr bytes.Buffer
	if code := (App{Out: &out, Err: &stderr, ProcessController: controller}).Run(context.Background(), args); code != 0 || !strings.Contains(out.String(), `"idempotent": true`) {
		t.Fatalf("idempotent retry code=%d stdout=%s stderr=%s", code, out.String(), stderr.String())
	}
	retried, err := store.Load()
	if err != nil || retried.StateRevision != revision || retried.Issues["150"].Lease.Owner != recoveredOwner {
		t.Fatalf("idempotent retry duplicated recovery: before=%d after=%d issue=%+v err=%v", revision, retried.StateRevision, retried.Issues["150"], err)
	}

	runtime := &resumedWorkspaceWorker{}
	loop := &supervisor.Loop{
		Config: cfg, Store: store,
		GitHub:    resumedWorkspaceGitHub{issue: gh.Issue{Number: 150, Title: "Interrupted workspace", State: "OPEN", Labels: []string{cfg.GitHub.RunningLabel}}},
		Worktrees: worktree.Manager{StateRoot: l.Root, GitPath: "/usr/bin/git"}, Worker: runtime,
		DiskAvailable: func(string) (uint64, error) { return ^uint64(0), nil },
	}
	if worked, err := loop.RunOnce(context.Background()); err != nil || !worked {
		t.Fatalf("same-worktree spawn worked=%v err=%v", worked, err)
	}
	if len(runtime.paths) != 1 || runtime.paths[0] != managedWorktree || len(runtime.prompts) != 1 || !strings.Contains(runtime.prompts[0], "localhost listen denied") ||
		runtime.runs != 1 || runtime.resumes != 0 || len(runtime.sessions) != 1 || runtime.sessions[0] != "" {
		t.Fatalf("worker did not start a fresh session in the original workspace: paths=%v sessions=%v runs=%d resumes=%d prompts=%v", runtime.paths, runtime.sessions, runtime.runs, runtime.resumes, runtime.prompts)
	}
	spawned, err := store.Load()
	if err != nil || spawned.Issues["150"].SessionID != "session_new_workspace_recovery" || spawned.Issues["150"].Session == nil ||
		spawned.Issues["150"].Session.ID != "session_new_workspace_recovery" {
		t.Fatalf("fresh worker session provenance was not saved: issue=%+v err=%v", spawned.Issues["150"], err)
	}
	events, err := os.ReadFile(store.EventsPath())
	if err != nil || strings.Count(string(events), `"type":"environment_resume_recovered"`) != 1 ||
		strings.Count(string(events), `"type":"worker_workspace_validated"`) != 1 {
		t.Fatalf("recovery/spawn audit was duplicated or missing: err=%v events=%s", err, events)
	}
}

func TestFaultResumeBlockedRecoversLeaseLostByInterruptedReconciliation(t *testing.T) {
	repo, l := testEnvironment(t)
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	runGitApp(t, repo, "config", "user.name", "Test User")
	runGitApp(t, repo, "config", "user.email", "test@example.com")
	runGitApp(t, repo, "config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitApp(t, repo, "add", "README.md")
	runGitApp(t, repo, "commit", "-m", "base")
	runGitApp(t, repo, "branch", "-M", "main")
	remotePath := filepath.Join(filepath.Dir(repo), "interrupted-resume-remote.git")
	runGitApp(t, filepath.Dir(repo), "init", "--bare", remotePath)
	runGitApp(t, repo, "remote", "add", "origin", remotePath)
	runGitApp(t, repo, "push", "-u", "origin", "main")
	baseSHA := runGitOutputApp(t, repo, "rev-parse", "HEAD")
	branch := "codex/issue-8-interrupted-resume"
	runGitApp(t, repo, "checkout", "-b", branch)
	runGitApp(t, repo, "push", "-u", "origin", branch)
	if err := os.WriteFile(filepath.Join(repo, "dirty-evidence.txt"), []byte("keep interrupted work\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	fakeGH := filepath.Join(filepath.Dir(repo), "bin", "gh-interrupted-resume")
	manualExclusion := filepath.Join(filepath.Dir(repo), "manual-exclusion")
	if err := os.WriteFile(manualExclusion, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENT_LOOP_TEST_MANUAL_EXCLUSION", manualExclusion)
	script := `#!/bin/sh
case "$1 $2" in
  "issue view")
    if [ -e "$AGENT_LOOP_TEST_MANUAL_EXCLUSION" ]; then
      printf '%s\n' '{"number":8,"title":"Interrupted resume","body":"","url":"https://example.test/issues/8","state":"OPEN","labels":[{"name":"blocked"},{"name":"do-not-automate"}],"assignees":[],"milestone":null,"comments":[]}'
    else
      printf '%s\n' '{"number":8,"title":"Interrupted resume","body":"","url":"https://example.test/issues/8","state":"OPEN","labels":[{"name":"blocked"}],"assignees":[],"milestone":null,"comments":[]}'
    fi ;;
  "pr list") printf '%s\n' '[]' ;;
  "issue edit"|"issue comment") exit 0 ;;
  *) exit 2 ;;
esac
`
	if err := os.WriteFile(fakeGH, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	configFile, err := os.OpenFile(filepath.Join(repo, config.FileName), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintf(configFile, "git:\n  worktree_root: %q\n", filepath.Dir(repo)); err != nil {
		t.Fatal(err)
	}
	if err := configFile.Close(); err != nil {
		t.Fatal(err)
	}
	cfg := mustConfig(t, repo)
	entry := registry.Entry{
		RepoID: registry.RepoID(cfg.GitHub.Repo, cfg.RepoPath), RepoPath: cfg.RepoPath, GitHubRepo: cfg.GitHub.Repo,
		Commands: map[string]string{"git": "/usr/bin/git", "gh": fakeGH},
	}
	writeJSONFixture(t, l.RegistryPath, registry.Registry{Version: registry.CurrentVersion, Repos: map[string]registry.Entry{entry.RepoID: entry}})
	store := state.Store{Dir: l.RepoDir(entry.RepoID), RepoID: entry.RepoID, RepoPath: cfg.RepoPath}
	if err := store.Initialize(); err != nil {
		t.Fatal(err)
	}
	_, owner, err := store.ReserveLease(state.LeaseReservation{
		IssueNumber: 8, Title: "Interrupted resume", RunID: "run_8", Slot: 0,
		DeclaredResources: []string{"host"}, ResolvedResources: []string{"host"}, BaseSHA: baseSHA, ReservedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	resumeID := "resume_interrupted"
	_, err = store.Update("environment_resume_requested", 8, owner.RunID, map[string]string{"resume_id": resumeID, "base_sha": baseSHA}, func(snapshot *state.Snapshot) error {
		item := snapshot.Issues["8"]
		item.Status = "blocked"
		item.Branch = branch
		item.Worktree = cfg.RepoPath
		item.SessionID = "session-8"
		item.BlockedCause = &state.BlockedCause{Origin: "worker", Kind: "environment", Resumable: true, Reason: "network unavailable", BlockedAt: time.Now().UTC()}
		item.EnvironmentResume = &state.EnvironmentResume{ID: resumeID, Status: "requested", ConfirmedAt: time.Now().UTC(), PreviousReason: "network unavailable"}
		item.LastError = "startup reconciliation blocked: GitHub exclusion label was applied manually"
		return state.ReleaseIssueLease(item, owner)
	})
	if err != nil {
		t.Fatal(err)
	}

	var out, stderr bytes.Buffer
	a := legacyResumeTestApp(&out, &stderr, &appProcessGroups{alive: map[int]bool{}, signals: map[int][]syscall.Signal{}})
	args := []string{"resume-blocked", "--repo", cfg.RepoPath, "--issue", "8", "--confirm-prerequisite-resolved", "--json"}
	broken, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if code := a.Run(context.Background(), args); code == 0 || !strings.Contains(stderr.String(), "manual exclusion") {
		t.Fatalf("manual exclusion did not fail closed: code=%d stdout=%s stderr=%s", code, out.String(), stderr.String())
	}
	rejected, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if rejected.StateRevision != broken.StateRevision || rejected.Issues["8"].Lease != nil || rejected.Issues["8"].Status != "blocked" {
		t.Fatalf("manual exclusion changed interrupted state: before=%d after=%d issue=%+v", broken.StateRevision, rejected.StateRevision, rejected.Issues["8"])
	}
	if err := os.Remove(manualExclusion); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	stderr.Reset()
	if code := a.Run(context.Background(), args); code != 0 {
		t.Fatalf("code=%d stdout=%s stderr=%s", code, out.String(), stderr.String())
	}
	loaded, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	item := loaded.Issues["8"]
	if item.Status != "environment_resume_pending" || item.GitHubSync != "" || item.Lease == nil || item.Lease.BaseSHA != baseSHA || item.EnvironmentResume == nil || item.EnvironmentResume.ID != resumeID || item.EnvironmentResume.Status != "github_synced" || item.EnvironmentResume.BaseSHA != baseSHA {
		t.Fatalf("interrupted resume did not converge: %+v", item)
	}
	if item.Lease.Owner.RunID != "run_8" || item.Lease.Owner.Generation != owner.Generation+1 || !reflect.DeepEqual(item.Lease.ResolvedResources, []string{state.RepositoryResource}) {
		t.Fatalf("lease was not conservatively reacquired: %+v", item.Lease)
	}
	if data, err := os.ReadFile(filepath.Join(repo, "dirty-evidence.txt")); err != nil || string(data) != "keep interrupted work\n" {
		t.Fatalf("dirty worktree was not preserved: data=%q err=%v", data, err)
	}
	revision := loaded.StateRevision
	out.Reset()
	stderr.Reset()
	if code := a.Run(context.Background(), args); code != 0 {
		t.Fatalf("idempotent retry code=%d stdout=%s stderr=%s", code, out.String(), stderr.String())
	}
	retried, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if retried.StateRevision != revision || retried.Issues["8"].EnvironmentResume.ID != resumeID || retried.Issues["8"].Lease.Owner != item.Lease.Owner {
		t.Fatalf("idempotent retry changed durable state: before=%d after=%d issue=%+v", revision, retried.StateRevision, retried.Issues["8"])
	}
}

func TestResumeBlockedFailsClosedWhenRecoveredLeaseBaseSHAIsUnavailable(t *testing.T) {
	repo, l := testEnvironment(t)
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	runGitApp(t, repo, "config", "user.name", "Test User")
	runGitApp(t, repo, "config", "user.email", "test@example.com")
	runGitApp(t, repo, "config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitApp(t, repo, "add", "README.md")
	runGitApp(t, repo, "commit", "-m", "initial")
	runGitApp(t, repo, "branch", "-M", "main")
	branch := "codex/issue-9-missing-base"
	runGitApp(t, repo, "checkout", "-b", branch)
	remotePath := filepath.Join(filepath.Dir(repo), "missing-base-remote.git")
	runGitApp(t, filepath.Dir(repo), "init", "--bare", remotePath)
	runGitApp(t, repo, "remote", "add", "origin", remotePath)
	// Only the worker branch exists remotely. Recovery must use the invalid
	// durable lease base below rather than silently replacing it with HEAD.
	runGitApp(t, repo, "push", "-u", "origin", branch)

	ghLog := filepath.Join(filepath.Dir(repo), "missing-base-gh-calls.log")
	fakeGH := filepath.Join(filepath.Dir(repo), "bin", "gh-missing-base")
	if err := os.WriteFile(fakeGH, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$AGENT_LOOP_TEST_GH_LOG\"\nexit 2\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENT_LOOP_TEST_GH_LOG", ghLog)
	configFile, err := os.OpenFile(filepath.Join(repo, config.FileName), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintf(configFile, "git:\n  worktree_root: %q\n", filepath.Dir(repo)); err != nil {
		t.Fatal(err)
	}
	if err := configFile.Close(); err != nil {
		t.Fatal(err)
	}
	cfg := mustConfig(t, repo)
	entry := registry.Entry{
		RepoID: registry.RepoID(cfg.GitHub.Repo, cfg.RepoPath), RepoPath: cfg.RepoPath, GitHubRepo: cfg.GitHub.Repo,
		Commands: map[string]string{"git": "/usr/bin/git", "gh": fakeGH},
	}
	writeJSONFixture(t, l.RegistryPath, registry.Registry{Version: registry.CurrentVersion, Repos: map[string]registry.Entry{entry.RepoID: entry}})
	store := state.Store{Dir: l.RepoDir(entry.RepoID), RepoID: entry.RepoID, RepoPath: cfg.RepoPath}
	if err := store.Initialize(); err != nil {
		t.Fatal(err)
	}
	missingBaseSHA := strings.Repeat("0", 40)
	_, owner, err := store.ReserveLease(state.LeaseReservation{
		IssueNumber: 9, Title: "Missing base", RunID: "run_9", Slot: 0,
		ResolvedResources: []string{state.RepositoryResource}, BaseSHA: missingBaseSHA, ReservedAt: time.Now().UTC().Add(-time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Update("worker_started", 9, owner.RunID, map[string]string{"worktree": cfg.RepoPath, "branch": branch}, func(snapshot *state.Snapshot) error {
		item := snapshot.Issues["9"]
		item.Status = "running"
		item.Branch = branch
		item.Worktree = cfg.RepoPath
		item.SessionID = "session-9"
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	blockedAt := time.Now().UTC()
	legacyError := "issue: worker blocked: legacy environment"
	_, err = store.Update("issue_blocked", 9, "run_9", map[string]string{"error": legacyError, "failure_kind": "issue"}, func(snapshot *state.Snapshot) error {
		item := snapshot.Issues["9"]
		item.Status = "blocked"
		item.FailureKind = "issue"
		item.LastError = legacyError
		item.UpdatedAt = blockedAt
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Update("github_state_synced", 9, "run_9", map[string]string{"state": "blocked"}, func(*state.Snapshot) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Update("startup_reconciled", 9, owner.RunID, map[string]string{
		"previous_status": "blocked", "status": "blocked", "reason": "GitHub exclusion label was applied manually",
	}, func(snapshot *state.Snapshot) error {
		item := snapshot.Issues["9"]
		item.LastError = "startup reconciliation blocked: GitHub exclusion label was applied manually"
		return state.ReleaseIssueLease(item, owner)
	})
	if err != nil {
		t.Fatal(err)
	}
	before, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.LegacyWorkerBlockRecoveryEvidence(*before.Issues["9"]); err != nil {
		t.Fatalf("legacy recovery fixture is invalid: %v", err)
	}
	var out, stderr bytes.Buffer
	a := legacyResumeTestApp(&out, &stderr, &appProcessGroups{alive: map[int]bool{}, signals: map[int][]syscall.Signal{}})
	code := a.Run(context.Background(), []string{"resume-blocked", "--repo", cfg.RepoPath, "--issue", "9", "--confirm-prerequisite-resolved", "--json"})
	if code == 0 || !strings.Contains(stderr.String(), "verify publication base SHA") || !strings.Contains(stderr.String(), missingBaseSHA) {
		t.Fatalf("missing recovered lease base was not rejected: code=%d stdout=%s stderr=%s", code, out.String(), stderr.String())
	}
	after, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	item := after.Issues["9"]
	if after.StateRevision != before.StateRevision || item.Status != "blocked" || item.Lease != nil || item.EnvironmentResume != nil || item.GitHubSync != "" {
		t.Fatalf("failed resume changed durable state: before_revision=%d after_revision=%d issue=%+v", before.StateRevision, after.StateRevision, item)
	}
	if calls, err := os.ReadFile(ghLog); err == nil && strings.TrimSpace(string(calls)) != "" {
		t.Fatalf("failed resume changed or inspected GitHub after base failure: %s", calls)
	} else if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

func TestResumeBlockedPreservesExistingLeaseBaseSHA(t *testing.T) {
	repo, l := testEnvironment(t)
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	runGitApp(t, repo, "config", "user.name", "Test User")
	runGitApp(t, repo, "config", "user.email", "test@example.com")
	runGitApp(t, repo, "config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitApp(t, repo, "add", "README.md")
	runGitApp(t, repo, "commit", "-m", "base")
	runGitApp(t, repo, "branch", "-M", "main")
	remotePath := filepath.Join(filepath.Dir(repo), "existing-base-remote.git")
	runGitApp(t, filepath.Dir(repo), "init", "--bare", remotePath)
	runGitApp(t, repo, "remote", "add", "origin", remotePath)
	runGitApp(t, repo, "push", "-u", "origin", "main")
	baseSHA := runGitOutputApp(t, repo, "rev-parse", "HEAD")
	branch := "codex/issue-10-existing-base"
	runGitApp(t, repo, "checkout", "-b", branch)
	runGitApp(t, repo, "push", "-u", "origin", branch)
	runGitApp(t, repo, "checkout", "main")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("new base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitApp(t, repo, "add", "README.md")
	runGitApp(t, repo, "commit", "-m", "advance base")
	runGitApp(t, repo, "push", "origin", "main")
	newBaseSHA := runGitOutputApp(t, repo, "rev-parse", "HEAD")
	runGitApp(t, repo, "checkout", branch)

	fakeGH := filepath.Join(filepath.Dir(repo), "bin", "gh-existing-base")
	script := `#!/bin/sh
case "$1 $2" in
  "issue view") printf '%s\n' '{"number":10,"title":"Existing base","body":"","url":"https://example.test/issues/10","state":"OPEN","labels":[{"name":"blocked"}],"assignees":[],"milestone":null,"comments":[]}' ;;
  "pr list") printf '%s\n' '[]' ;;
  "issue edit"|"issue comment") exit 0 ;;
  *) exit 2 ;;
esac
`
	if err := os.WriteFile(fakeGH, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	configFile, err := os.OpenFile(filepath.Join(repo, config.FileName), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintf(configFile, "git:\n  worktree_root: %q\n", filepath.Dir(repo)); err != nil {
		t.Fatal(err)
	}
	if err := configFile.Close(); err != nil {
		t.Fatal(err)
	}
	cfg := mustConfig(t, repo)
	entry := registry.Entry{
		RepoID: registry.RepoID(cfg.GitHub.Repo, cfg.RepoPath), RepoPath: cfg.RepoPath, GitHubRepo: cfg.GitHub.Repo,
		Commands: map[string]string{"git": "/usr/bin/git", "gh": fakeGH},
	}
	writeJSONFixture(t, l.RegistryPath, registry.Registry{Version: registry.CurrentVersion, Repos: map[string]registry.Entry{entry.RepoID: entry}})
	store := state.Store{Dir: l.RepoDir(entry.RepoID), RepoID: entry.RepoID, RepoPath: cfg.RepoPath}
	if err := store.Initialize(); err != nil {
		t.Fatal(err)
	}
	reservedAt := time.Now().UTC().Add(-time.Hour)
	originalLease := &state.ResourceLease{
		Owner: state.LeaseOwner{RunID: "run_10", Generation: 7}, Slot: 1,
		DeclaredResources: []string{"worker"}, ResolvedResources: []string{"worker"}, BaseSHA: baseSHA, ReservedAt: reservedAt,
	}
	_, err = store.Update("issue_blocked", 10, "run_10", nil, func(snapshot *state.Snapshot) error {
		snapshot.Issues["10"] = &state.Issue{
			Number: 10, Status: "blocked", RunID: "run_10", LeaseGeneration: 7, Lease: originalLease,
			Branch: branch, Worktree: cfg.RepoPath, SessionID: "session-10",
			BlockedCause: &state.BlockedCause{Origin: "worker", Kind: "environment", Resumable: true, Reason: "environment", BlockedAt: reservedAt},
			LastError:    "environment", UpdatedAt: reservedAt,
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	a := legacyResumeTestApp(&out, &stderr, &appProcessGroups{alive: map[int]bool{}, signals: map[int][]syscall.Signal{}})
	if code := a.Run(context.Background(), []string{"resume-blocked", "--repo", cfg.RepoPath, "--issue", "10", "--confirm-prerequisite-resolved", "--json"}); code != 0 {
		t.Fatalf("code=%d stdout=%s stderr=%s", code, out.String(), stderr.String())
	}
	snapshot, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	item := snapshot.Issues["10"]
	if item.Lease.BaseSHA != baseSHA || item.Lease.BaseSHA == newBaseSHA || item.EnvironmentResume == nil || item.EnvironmentResume.CurrentBaseSHA != newBaseSHA || item.LeaseGeneration != 7 || item.Lease.Owner != originalLease.Owner || item.Lease.Slot != originalLease.Slot || !reflect.DeepEqual(item.Lease.DeclaredResources, originalLease.DeclaredResources) || !reflect.DeepEqual(item.Lease.ResolvedResources, originalLease.ResolvedResources) || !item.Lease.ReservedAt.Equal(reservedAt) {
		t.Fatalf("existing lease metadata was overwritten: old=%+v new=%+v configured_base=%s", originalLease, item.Lease, newBaseSHA)
	}
	if !strings.Contains(out.String(), `"current_base_sha": "`+newBaseSHA+`"`) {
		t.Fatalf("current base was not exposed in CLI output: %s", out.String())
	}
}

func TestFaultResumeBlockedReacquiresParkedLeaseOnceAcrossGitHubSyncFailure(t *testing.T) {
	repo, l := testEnvironment(t)
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	runGitApp(t, repo, "config", "user.name", "Test User")
	runGitApp(t, repo, "config", "user.email", "test@example.com")
	runGitApp(t, repo, "config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitApp(t, repo, "add", "README.md")
	runGitApp(t, repo, "commit", "-m", "base")
	runGitApp(t, repo, "branch", "-M", "main")
	remotePath := filepath.Join(filepath.Dir(repo), "parked-resume-remote.git")
	runGitApp(t, filepath.Dir(repo), "init", "--bare", remotePath)
	runGitApp(t, repo, "remote", "add", "origin", remotePath)
	runGitApp(t, repo, "push", "-u", "origin", "main")
	baseSHA := runGitOutputApp(t, repo, "rev-parse", "HEAD")
	branch := "codex/issue-11-parked"
	runGitApp(t, repo, "checkout", "-b", branch)
	runGitApp(t, repo, "push", "-u", "origin", branch)
	if err := os.WriteFile(filepath.Join(repo, "parked-dirty.txt"), []byte("keep parked work\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	fakeGH := filepath.Join(filepath.Dir(repo), "bin", "gh-parked-resume")
	logPath := filepath.Join(filepath.Dir(repo), "parked-resume-gh.log")
	failOncePath := filepath.Join(filepath.Dir(repo), "parked-resume-failed-once")
	closedPath := filepath.Join(filepath.Dir(repo), "parked-resume-closed")
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$AGENT_LOOP_TEST_GH_LOG"
case "$1 $2" in
  "issue view") issue_state=OPEN; if [ -e "$AGENT_LOOP_TEST_GH_CLOSED" ]; then issue_state=CLOSED; fi; printf '{"number":11,"title":"Parked","body":"","url":"https://example.test/issues/11","state":"%s","labels":[{"name":"blocked"}],"assignees":[],"milestone":null,"comments":[]}\n' "$issue_state" ;;
  "pr list") printf '%s\n' '[]' ;;
  "issue edit") if [ ! -e "$AGENT_LOOP_TEST_GH_FAIL_ONCE" ]; then : > "$AGENT_LOOP_TEST_GH_FAIL_ONCE"; exit 1; fi; exit 0 ;;
  "issue comment") exit 0 ;;
  *) exit 2 ;;
esac
`
	if err := os.WriteFile(fakeGH, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENT_LOOP_TEST_GH_LOG", logPath)
	t.Setenv("AGENT_LOOP_TEST_GH_FAIL_ONCE", failOncePath)
	t.Setenv("AGENT_LOOP_TEST_GH_CLOSED", closedPath)
	configFile, err := os.OpenFile(filepath.Join(repo, config.FileName), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintf(configFile, "git:\n  worktree_root: %q\n", filepath.Dir(repo)); err != nil {
		t.Fatal(err)
	}
	if err := configFile.Close(); err != nil {
		t.Fatal(err)
	}
	cfg := mustConfig(t, repo)
	entry := registry.Entry{
		RepoID: registry.RepoID(cfg.GitHub.Repo, cfg.RepoPath), RepoPath: cfg.RepoPath, GitHubRepo: cfg.GitHub.Repo,
		Commands: map[string]string{"git": "/usr/bin/git", "gh": fakeGH},
	}
	writeJSONFixture(t, l.RegistryPath, registry.Registry{Version: registry.CurrentVersion, Repos: map[string]registry.Entry{entry.RepoID: entry}})
	store := state.Store{Dir: l.RepoDir(entry.RepoID), RepoID: entry.RepoID, RepoPath: cfg.RepoPath}
	if err := store.Initialize(); err != nil {
		t.Fatal(err)
	}
	reservedAt := time.Now().UTC().Add(-time.Hour)
	_, owner, err := store.ReserveLease(state.LeaseReservation{
		IssueNumber: 11, Title: "Parked", RunID: "run_11", Slot: 0,
		DeclaredResources: []string{"scheduler"}, ResolvedResources: []string{"scheduler"}, BaseSHA: baseSHA, ReservedAt: reservedAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	parkedAt := reservedAt.Add(time.Minute)
	_, err = store.Update("issue_blocked", 11, owner.RunID, nil, func(snapshot *state.Snapshot) error {
		item := snapshot.Issues["11"]
		item.Status = "blocked"
		item.Branch = branch
		item.Worktree = cfg.RepoPath
		item.SessionID = "session-11"
		item.Session = &state.WorkerSession{Backend: "codex", ID: "session-11"}
		item.Goal = &state.WorkerGoal{ThreadID: "thread-11", Objective: "finish Issue #11", Status: "blocked"}
		item.Answers = []state.AnswerRecord{{RequestID: "req-11", Answer: "approved", AnsweredAt: parkedAt}}
		item.Attempts = 2
		item.Continuations = 1
		item.BlockedCause = &state.BlockedCause{Origin: "worker", Kind: "environment", Resumable: true, Reason: "public network unavailable", BlockedAt: parkedAt}
		item.LastError = "worker blocked: public network unavailable"
		item.FailureKind = "issue"
		return state.ParkIssueLease(item, owner, "park_11", parkedAt)
	})
	if err != nil {
		t.Fatal(err)
	}
	_, competitor, err := store.ReserveLease(state.LeaseReservation{
		IssueNumber: 12, Title: "Competing", RunID: "run_12", Slot: 1,
		ResolvedResources: []string{"scheduler"}, BaseSHA: baseSHA, ReservedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update("input_requested", 12, competitor.RunID, nil, func(snapshot *state.Snapshot) error {
		snapshot.Issues["12"].Status = "needs_input"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	beforeConflict, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	args := []string{"resume-blocked", "--repo", cfg.RepoPath, "--issue", "11", "--confirm-prerequisite-resolved", "--json"}
	controller := &appProcessGroups{alive: map[int]bool{}, signals: map[int][]syscall.Signal{}}
	var out, stderr bytes.Buffer
	if code := legacyResumeTestApp(&out, &stderr, controller).Run(context.Background(), args); code == 0 || !strings.Contains(stderr.String(), "Issue #12") {
		t.Fatalf("competing parked resume was accepted: code=%d stdout=%s stderr=%s", code, out.String(), stderr.String())
	}
	afterConflict, err := store.Load()
	if err != nil || afterConflict.StateRevision != beforeConflict.StateRevision || afterConflict.Issues["11"].Lease != nil || afterConflict.Issues["11"].ResourcePark.Status != "parked" {
		t.Fatalf("conflict changed parked state: before=%d after=%d issue=%+v err=%v", beforeConflict.StateRevision, afterConflict.StateRevision, afterConflict.Issues["11"], err)
	}
	if _, err := store.ReleaseLease(12, competitor, "test competitor finished"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update("test_worker_alive", 11, owner.RunID, nil, func(snapshot *state.Snapshot) error {
		snapshot.Issues["11"].WorkerPID = 4311
		snapshot.Issues["11"].WorkerPGID = 4311
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	controller.alive[4311] = true
	out.Reset()
	stderr.Reset()
	if code := legacyResumeTestApp(&out, &stderr, controller).Run(context.Background(), args); code == 0 || !strings.Contains(stderr.String(), "active worker") {
		t.Fatalf("active worker was accepted: code=%d stderr=%s", code, stderr.String())
	}
	controller.alive[4311] = false
	if _, err := store.Update("test_pending_request", 11, owner.RunID, nil, func(snapshot *state.Snapshot) error {
		snapshot.Issues["11"].WorkerPID = 0
		snapshot.Issues["11"].WorkerPGID = 0
		snapshot.PendingRequests["req-pending-11"] = &state.Request{ID: "req-pending-11", IssueNumber: 11, Status: "pending"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	stderr.Reset()
	if code := legacyResumeTestApp(&out, &stderr, controller).Run(context.Background(), args); code == 0 || !strings.Contains(stderr.String(), "pending manual answer") {
		t.Fatalf("pending request was accepted: code=%d stderr=%s", code, stderr.String())
	}
	if _, err := store.Update("test_request_answered", 11, owner.RunID, nil, func(snapshot *state.Snapshot) error {
		snapshot.PendingRequests["req-pending-11"].Status = "answered"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(closedPath, []byte("closed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	stderr.Reset()
	if code := legacyResumeTestApp(&out, &stderr, controller).Run(context.Background(), args); code == 0 || !strings.Contains(stderr.String(), "is not open") {
		t.Fatalf("closed Issue was accepted: code=%d stderr=%s", code, stderr.String())
	}
	if err := os.Remove(closedPath); err != nil {
		t.Fatal(err)
	}

	out.Reset()
	stderr.Reset()
	if code := legacyResumeTestApp(&out, &stderr, controller).Run(context.Background(), args); code == 0 || !strings.Contains(stderr.String(), "durable resume remains pending") {
		t.Fatalf("GitHub sync fault was not retained: code=%d stdout=%s stderr=%s", code, out.String(), stderr.String())
	}
	pending, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	pendingIssue := pending.Issues["11"]
	if pendingIssue.Status != "environment_resume_pending" || pendingIssue.GitHubSync != "environment_resume" || pendingIssue.Lease == nil || pendingIssue.Lease.Owner.Generation != owner.Generation+1 || pendingIssue.ResourcePark.Status != "resuming" {
		t.Fatalf("write-ahead parked resume is inconsistent: %+v", pendingIssue)
	}
	resumeOwner := pendingIssue.Lease.Owner
	resumeID := pendingIssue.EnvironmentResume.ID

	for attempt := 0; attempt < 2; attempt++ {
		out.Reset()
		stderr.Reset()
		if code := legacyResumeTestApp(&out, &stderr, controller).Run(context.Background(), args); code != 0 {
			t.Fatalf("resume retry %d failed: code=%d stdout=%s stderr=%s", attempt, code, out.String(), stderr.String())
		}
	}
	completed, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	item := completed.Issues["11"]
	if item.Lease == nil || item.Lease.Owner != resumeOwner || item.LeaseGeneration != resumeOwner.Generation || item.EnvironmentResume.ID != resumeID || item.ResourcePark.ResumeOwner == nil || *item.ResourcePark.ResumeOwner != resumeOwner {
		t.Fatalf("parked resume was not idempotent: %+v", item)
	}
	if item.RunID != "run_11" || item.Branch != branch || item.Worktree != cfg.RepoPath || item.SessionID != "session-11" || item.Session == nil || item.Session.ID != "session-11" || item.Goal == nil || item.Goal.ThreadID != "thread-11" || len(item.Answers) != 1 || item.Attempts != 2 || item.Continuations != 1 {
		t.Fatalf("continuation metadata changed: %+v", item)
	}
	if item.ResourcePark.OriginalLease.Owner != owner || item.ResourcePark.OriginalLease.BaseSHA != baseSHA || !item.ResourcePark.OriginalLease.ReservedAt.Equal(reservedAt) || item.Lease.BaseSHA != baseSHA {
		t.Fatalf("resource provenance changed: park=%+v lease=%+v", item.ResourcePark, item.Lease)
	}
	if data, err := os.ReadFile(filepath.Join(repo, "parked-dirty.txt")); err != nil || string(data) != "keep parked work\n" {
		t.Fatalf("dirty work was lost: data=%q err=%v", data, err)
	}
	if !strings.Contains(out.String(), `"idempotent": true`) || !strings.Contains(out.String(), `"resource_park_id": "park_11"`) {
		t.Fatalf("idempotent CLI output lacks park audit: %s", out.String())
	}
	calls, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(calls), "codex-issue-loop:environment-resume:") != 1 {
		t.Fatalf("environment resume comment was duplicated: %s", calls)
	}
}

func TestResumeBlockedRejectsUnconfirmedAndNonEnvironmentBlocks(t *testing.T) {
	repo, l := testEnvironment(t)
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	entry, err := (registry.Store{Path: l.RegistryPath}).Add(mustConfig(t, repo))
	if err != nil {
		t.Fatal(err)
	}
	store := state.Store{Dir: l.RepoDir(entry.RepoID), RepoID: entry.RepoID, RepoPath: repo}
	if err := store.Initialize(); err != nil {
		t.Fatal(err)
	}
	_, err = store.Update("manual_block", 9, "run_9", nil, func(s *state.Snapshot) error {
		s.Issues["9"] = &state.Issue{Number: 9, Status: "blocked", RunID: "run_9", Worktree: repo, Branch: "manual", LastError: "manual exclusion"}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"resume-blocked", "--repo", repo, "--issue", "9", "--json"},
		{"resume-blocked", "--repo", repo, "--issue", "9", "--confirm-prerequisite-resolved", "--json"},
	} {
		var out, stderr bytes.Buffer
		if code := (App{Out: &out, Err: &stderr}).Run(context.Background(), args); code == 0 {
			t.Fatalf("unsafe resume accepted: %v output=%s", args, out.String())
		}
	}
	for _, test := range []struct {
		name  string
		issue state.Issue
	}{
		{name: "conflict", issue: state.Issue{Number: 9, Status: "blocked", RunID: "run_9", BlockedCause: &state.BlockedCause{Origin: "worker", Kind: "environment", Resumable: true}, ConflictRecovery: &state.ConflictRecovery{PullRequestURL: "https://example.test/pr/9"}}},
		{name: "running", issue: state.Issue{Number: 9, Status: "running", RunID: "run_9", BlockedCause: &state.BlockedCause{Origin: "worker", Kind: "environment", Resumable: true}}},
		{name: "failed", issue: state.Issue{Number: 9, Status: "failed", RunID: "run_9", BlockedCause: &state.BlockedCause{Origin: "worker", Kind: "environment", Resumable: true}}},
		{name: "completed", issue: state.Issue{Number: 9, Status: "completed", RunID: "run_9", BlockedCause: &state.BlockedCause{Origin: "worker", Kind: "environment", Resumable: true}}},
		{name: "security", issue: state.Issue{Number: 9, Status: "blocked", RunID: "run_9", BlockedCause: &state.BlockedCause{Origin: "supervisor", Kind: "security", Resumable: false}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := store.Update("replace_block", 9, "run_9", nil, func(s *state.Snapshot) error {
				copy := test.issue
				s.Issues["9"] = &copy
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			var out, stderr bytes.Buffer
			args := []string{"resume-blocked", "--repo", repo, "--issue", "9", "--confirm-prerequisite-resolved", "--json"}
			if code := (App{Out: &out, Err: &stderr}).Run(context.Background(), args); code == 0 {
				t.Fatalf("unsafe %s resume accepted: %s", test.name, out.String())
			}
		})
	}
}

func TestRecoverPublicationResumesLegacyMissingBaseInPlaceAndIsIdempotent(t *testing.T) {
	repo, l := testEnvironment(t)
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	runGitApp(t, repo, "config", "user.name", "Test User")
	runGitApp(t, repo, "config", "user.email", "test@example.com")
	runGitApp(t, repo, "config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitApp(t, repo, "add", "README.md")
	runGitApp(t, repo, "commit", "-m", "base")
	runGitApp(t, repo, "branch", "-M", "main")
	remotePath := filepath.Join(filepath.Dir(repo), "publication-recovery-remote.git")
	runGitApp(t, filepath.Dir(repo), "init", "--bare", remotePath)
	runGitApp(t, repo, "remote", "add", "origin", remotePath)
	runGitApp(t, repo, "push", "-u", "origin", "main")
	baseSHA := runGitOutputApp(t, repo, "rev-parse", "HEAD")
	branch := "codex/issue-102-legacy-publication"
	runGitApp(t, repo, "checkout", "-b", branch)
	if err := os.WriteFile(filepath.Join(repo, "implementation.txt"), []byte("preserve dirty implementation\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	fakeGH := filepath.Join(filepath.Dir(repo), "bin", "gh-publication-recovery")
	ghLog := filepath.Join(filepath.Dir(repo), "publication-recovery-gh.log")
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$AGENT_LOOP_TEST_GH_LOG"
case "$1 $2" in
  "issue view") printf '%s\n' '{"number":102,"title":"Legacy publication","body":"","url":"https://example.test/issues/102","state":"OPEN","labels":[{"name":"codex-loop:failed"}],"assignees":[],"milestone":null,"comments":[]}' ;;
  "pr list") printf '%s\n' '[]' ;;
  "issue edit"|"issue comment") exit 0 ;;
  *) exit 2 ;;
esac
`
	if err := os.WriteFile(fakeGH, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENT_LOOP_TEST_GH_LOG", ghLog)
	configFile, err := os.OpenFile(filepath.Join(repo, config.FileName), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintf(configFile, "git:\n  worktree_root: %q\n", filepath.Dir(repo)); err != nil {
		t.Fatal(err)
	}
	if err := configFile.Close(); err != nil {
		t.Fatal(err)
	}
	cfg := mustConfig(t, repo)
	entry := registry.Entry{
		RepoID: registry.RepoID(cfg.GitHub.Repo, cfg.RepoPath), RepoPath: cfg.RepoPath, GitHubRepo: cfg.GitHub.Repo,
		Commands: map[string]string{"git": "/usr/bin/git", "gh": fakeGH},
	}
	writeJSONFixture(t, l.RegistryPath, registry.Registry{Version: registry.CurrentVersion, Repos: map[string]registry.Entry{entry.RepoID: entry}})
	store := state.Store{Dir: l.RepoDir(entry.RepoID), RepoID: entry.RepoID, RepoPath: cfg.RepoPath}
	if err := store.Initialize(); err != nil {
		t.Fatal(err)
	}
	runID := "run_legacy_102"
	runDir := filepath.Join(store.Dir, "runs", runID)
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		t.Fatal(err)
	}
	completed := `{"version":1,"status":"completed","execution_profile":"extended","summary":"implementation verified","question":null,"tests":[{"command":"go test ./...","result":"pass"}],"git":null,"retry":null}`
	if err := os.WriteFile(filepath.Join(runDir, "result-1.json"), []byte(completed), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	_, err = store.Update("issue_failed", 102, runID, nil, func(s *state.Snapshot) error {
		s.Issues["102"] = &state.Issue{
			Number: 102, Title: "Legacy publication", Status: "failed", RunID: runID,
			Branch: branch, Worktree: cfg.RepoPath, Attempts: cfg.Queue.MaxAttempts,
			SessionID: "session-102", Session: &state.WorkerSession{Backend: "codex", ID: "session-102"},
			Answers:           []state.AnswerRecord{{RequestID: "req-102", Question: "Continue?", Answer: "yes", AnsweredAt: now}},
			DeclaredResources: []string{state.RepositoryResource},
			BlockedCause:      &state.BlockedCause{Origin: "worker", Kind: "environment", Resumable: true, Reason: "legacy localhost prerequisite", BlockedAt: now},
			EnvironmentResume: &state.EnvironmentResume{ID: "resume-102", Status: "running", ConfirmedAt: now},
			PublicationAudit:  &publication.Audit{BaseSHA: "", DeclaredResources: []string{state.RepositoryResource}},
			FailureKind:       "issue", LastError: "issue: worker retry limit reached: publish completed work: inspect publish changes: durable base SHA is missing",
			UpdatedAt: now,
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	assertRefused := func(name string, app App, args []string) {
		t.Helper()
		var out, stderr bytes.Buffer
		app.Out, app.Err = &out, &stderr
		if code := app.Run(context.Background(), args); code == 0 {
			t.Fatalf("%s recovery unexpectedly succeeded: %s", name, out.String())
		}
	}
	baseArgs := []string{"recover-publication", "--repo", cfg.RepoPath, "--issue", "102", "--json"}
	assertRefused("confirmation", App{}, baseArgs)
	_, err = store.Update("fault_active_worker", 102, runID, nil, func(s *state.Snapshot) error {
		s.Issues["102"].WorkerPID = 4242
		s.Issues["102"].WorkerPGID = 4242
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	assertRefused("active worker", App{ProcessController: &appProcessGroups{alive: map[int]bool{4242: true}, signals: map[int][]syscall.Signal{}}}, append(baseArgs, "--confirm-prerequisite-resolved"))
	_, err = store.Update("fault_pending_request", 102, runID, nil, func(s *state.Snapshot) error {
		s.Issues["102"].WorkerPID, s.Issues["102"].WorkerPGID = 0, 0
		s.PendingRequests["req-pending-102"] = &state.Request{ID: "req-pending-102", IssueNumber: 102, Status: "pending"}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	assertRefused("pending request", App{ProcessController: &appProcessGroups{alive: map[int]bool{}, signals: map[int][]syscall.Signal{}}}, append(baseArgs, "--confirm-prerequisite-resolved"))
	_, err = store.Update("faults_repaired", 102, runID, nil, func(s *state.Snapshot) error {
		delete(s.PendingRequests, "req-pending-102")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var recoveryID string
	for attempt := 0; attempt < 2; attempt++ {
		var out, stderr bytes.Buffer
		a := App{Out: &out, Err: &stderr, ProcessController: &appProcessGroups{alive: map[int]bool{}, signals: map[int][]syscall.Signal{}}}
		code := a.Run(context.Background(), []string{"recover-publication", "--repo", cfg.RepoPath, "--issue", "102", "--confirm-prerequisite-resolved", "--json"})
		if code != 0 {
			t.Fatalf("attempt=%d code=%d stdout=%s stderr=%s", attempt, code, out.String(), stderr.String())
		}
		snapshot, loadErr := store.Load()
		if loadErr != nil {
			t.Fatal(loadErr)
		}
		item := snapshot.Issues["102"]
		if attempt == 0 {
			recoveryID = item.PublicationRecovery.ID
		}
		if item.Status != "publication_recovery_pending" || item.GitHubSync != "" || item.Lease == nil || item.Lease.BaseSHA != baseSHA || item.PublicationRecovery.ID != recoveryID || item.PublicationRecovery.Attempts != 0 {
			t.Fatalf("recovery was not durable/idempotent: %+v", item)
		}
		if item.Attempts != cfg.Queue.MaxAttempts || item.SessionID != "session-102" || item.Session == nil || len(item.Answers) != 1 || item.BlockedCause == nil || item.EnvironmentResume == nil {
			t.Fatalf("worker history or metadata changed: %+v", item)
		}
	}
	if data, err := os.ReadFile(filepath.Join(repo, "implementation.txt")); err != nil || string(data) != "preserve dirty implementation\n" {
		t.Fatalf("dirty implementation changed: data=%q err=%v", data, err)
	}
	calls, err := os.ReadFile(ghLog)
	if err != nil || !strings.Contains(string(calls), "--remove-label codex-loop:failed") || !strings.Contains(string(calls), "codex-issue-loop:publication-recovery:") {
		t.Fatalf("GitHub recovery sync missing: calls=%s err=%v", calls, err)
	}
}

func TestPublicationRecoveryEligibilityAndPullRequestsFailClosed(t *testing.T) {
	typed := &publication.FailureProvenance{
		Origin: publication.FailureOriginPublisher, Phase: publication.FailurePhasePrePublication,
		Code: publication.FailureCodeDurableBaseMissing, Recoverable: true,
	}
	if !eligibleTypedBaseFailure(typed) {
		t.Fatal("typed missing-base failure was rejected")
	}
	for _, mutation := range []func(*publication.FailureProvenance){
		func(value *publication.FailureProvenance) { value.Origin = "worker" },
		func(value *publication.FailureProvenance) { value.Phase = "worker_execution" },
		func(value *publication.FailureProvenance) { value.Code = "unknown" },
		func(value *publication.FailureProvenance) { value.Recoverable = false },
	} {
		copy := *typed
		mutation(&copy)
		if eligibleTypedBaseFailure(&copy) {
			t.Fatalf("unsafe provenance was accepted: %+v", copy)
		}
	}
	legacy := &state.Issue{
		Attempts: 3, FailureKind: "issue",
		LastError:        "issue: worker retry limit reached: publish completed work: inspect publish changes: durable base SHA is missing",
		PublicationAudit: &publication.Audit{},
	}
	if !eligibleLegacyBaseFailure(legacy, 3) {
		t.Fatal("strict legacy fixture was rejected")
	}
	legacy.LastError = "worker implementation failed"
	if eligibleLegacyBaseFailure(legacy, 3) {
		t.Fatal("generic worker failure was accepted")
	}

	current := &state.Issue{Number: 7, Branch: "codex/issue-7-test"}
	inspection := worktree.Inspection{RemoteBranchExists: true}
	if err := validateRecoveryPullRequests(current, gh.RemoteState{PullRequests: []gh.PullRequest{{URL: "https://example.test/pr/7", State: "OPEN"}}}, inspection, "main"); err == nil {
		t.Fatal("unrecorded Pull Request was accepted")
	}
	current.PullRequestURL = "https://example.test/pr/7"
	for _, pr := range []gh.PullRequest{
		{URL: current.PullRequestURL, State: "CLOSED", HeadRefName: current.Branch, BaseRefName: "main"},
		{URL: "https://example.test/pr/other", State: "OPEN", HeadRefName: current.Branch, BaseRefName: "main"},
		{URL: current.PullRequestURL, State: "OPEN", HeadRefName: "other-branch", BaseRefName: "main"},
		{URL: current.PullRequestURL, State: "OPEN", HeadRefName: current.Branch, BaseRefName: "release"},
	} {
		if err := validateRecoveryPullRequests(current, gh.RemoteState{PullRequests: []gh.PullRequest{pr}}, inspection, "main"); err == nil {
			t.Fatalf("inconsistent Pull Request was accepted: %+v", pr)
		}
	}

	syncCurrent := &state.Issue{Number: 7, Branch: "codex/issue-7-test"}
	syncRemote := gh.RemoteState{Issue: gh.Issue{Number: 7, State: "OPEN", Labels: []string{"codex-loop:failed"}}}
	cfg := config.Defaults()
	if err := validatePublicationRecoverySyncState(cfg, syncCurrent, syncRemote); err != nil {
		t.Fatalf("synchronized failed state was rejected: %v", err)
	}
	syncRemote.Issue.Labels = []string{"codex-loop:running"}
	if err := validatePublicationRecoverySyncState(cfg, syncCurrent, syncRemote); err != nil {
		t.Fatalf("idempotent running transition was rejected: %v", err)
	}
	for _, labels := range [][]string{
		{"codex-loop:failed", "codex-loop:running"},
		{"codex-loop:failed", "do-not-automate"},
		{"codex-loop:failed", "codex-loop:ready"},
	} {
		syncRemote.Issue.Labels = labels
		if err := validatePublicationRecoverySyncState(cfg, syncCurrent, syncRemote); err == nil {
			t.Fatalf("unsafe synchronization labels were accepted: %v", labels)
		}
	}
}

func TestRecoverChecksReusesExternallyFixedBranchAndIsIdempotent(t *testing.T) {
	repo, l := testEnvironment(t)
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	binDir := strings.Split(os.Getenv("PATH"), string(os.PathListSeparator))[0]
	fakeGH := filepath.Join(binDir, "gh")
	ghLog := filepath.Join(t.TempDir(), "gh.log")

	gitPath := repo
	runGit := func(args ...string) string {
		t.Helper()
		command := exec.Command("git", append([]string{"-C", gitPath}, args...)...)
		out, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	runGit("config", "user.name", "Test")
	runGit("config", "user.email", "test@example.invalid")
	runGit("config", "commit.gpgsign", "false")
	runGit("add", ".agent-loop.yaml")
	runGit("commit", "-m", "base")
	runGit("branch", "-M", "main")
	remoteDir := filepath.Join(t.TempDir(), "remote.git")
	if out, err := exec.Command("git", "init", "--bare", "-q", remoteDir).CombinedOutput(); err != nil {
		t.Fatalf("git init --bare: %v: %s", err, out)
	}
	runGit("remote", "add", "origin", remoteDir)
	runGit("push", "-u", "origin", "main")
	branch := "codex/issue-102-checks"
	canonicalRoot, err := filepath.EvalSymlinks(l.Root)
	if err != nil {
		t.Fatal(err)
	}
	managedWorktree := filepath.Join(canonicalRoot, "worktrees", "repo-test", "issue-102")
	if err := os.MkdirAll(filepath.Dir(managedWorktree), 0o700); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", repo, "worktree", "add", "-b", branch, managedWorktree).CombinedOutput(); err != nil {
		t.Fatalf("git worktree add: %v: %s", err, out)
	}
	gitPath = managedWorktree
	fixture := filepath.Join(managedWorktree, "format.ts")
	if err := os.WriteFile(fixture, []byte("const value = 'deno-2.9.5'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit("add", "format.ts")
	runGit("commit", "-m", "worker formatter output")
	runGit("push", "-u", "origin", branch)
	oldHead := runGit("rev-parse", "HEAD")
	if err := os.WriteFile(fixture, []byte("const value = \"deno-2.7.14\";\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit("add", "format.ts")
	runGit("commit", "-m", "apply pinned formatter")
	runGit("push", "origin", branch)
	newHead := runGit("rev-parse", "HEAD")

	issueJSON := `{"number":102,"title":"checks","body":"","url":"https://example.test/issues/102","state":"OPEN","labels":[{"name":"codex-loop:failed"}],"assignees":[],"milestone":null,"comments":[]}`
	writeFakeGH := func(conclusion string) {
		t.Helper()
		prJSON := fmt.Sprintf(`[{"number":447,"url":"https://example.test/pr/447","state":"OPEN","isDraft":true,"mergedAt":null,"headRefName":%q,"baseRefName":"main","headRefOid":%q,"headRepository":{"name":"repo"},"headRepositoryOwner":{"login":"owner"},"mergeStateStatus":"CLEAN","statusCheckRollup":[{"__typename":"CheckRun","status":"COMPLETED","conclusion":%q}]}]`, branch, newHead, conclusion)
		script := fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$*" >> %q
case "$1 $2" in
  "issue view") printf '%%s\n' '%s' ;;
  "pr list") printf '%%s\n' '%s' ;;
  "issue edit"|"issue comment") exit 0 ;;
  *) exit 2 ;;
esac
`, ghLog, issueJSON, prJSON)
		if err := os.WriteFile(fakeGH, []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeFakeGH("FAILURE")
	cfg := mustConfig(t, repo)
	entry, err := (registry.Store{Path: l.RegistryPath}).Add(cfg)
	if err != nil {
		t.Fatal(err)
	}
	store := state.Store{Dir: l.RepoDir(entry.RepoID), RepoID: entry.RepoID, RepoPath: repo}
	if err := store.Initialize(); err != nil {
		t.Fatal(err)
	}
	runID := "run_checks_102"
	_, _, err = store.ReserveLease(state.LeaseReservation{
		IssueNumber: 102, Title: "checks", RunID: runID, Slot: 0,
		DeclaredResources: []string{state.RepositoryResource}, ResolvedResources: []string{state.RepositoryResource},
		ReservedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Update("pull_request_checks_retry_exhausted", 102, runID, nil, func(snapshot *state.Snapshot) error {
		item := snapshot.Issues["102"]
		item.Status = "failed"
		item.Worktree = managedWorktree
		item.Branch = branch
		item.PullRequestURL = "https://example.test/pr/447"
		item.PullRequestNumber = 447
		item.HeadSHA = oldHead
		item.Attempts = cfg.Queue.MaxAttempts
		item.Continuations = cfg.Worker.Profiles["extended"].MaxContinuations
		item.FailureKind = "issue"
		item.LastError = "issue: worker retry limit reached: Pull Request checks failed"
		item.PullRequestChecksFailure = &state.PullRequestChecksFailure{
			Origin: state.ChecksFailureOriginPullRequest, Phase: state.ChecksFailurePhaseRequired,
			Code: state.ChecksFailureCodeRetryExhausted, Recoverable: true, RetryExhausted: true,
			PullRequestURL: item.PullRequestURL, PullRequestNumber: 447, Branch: branch,
			HeadSHA: oldHead, ChecksStatus: "failure", FailedAt: time.Now().UTC(),
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	baseArgs := []string{"recover-checks", "--repo", repo, "--issue", "102", "--json"}
	assertRefused := func(name string, app App, args []string) {
		t.Helper()
		var out, stderr bytes.Buffer
		app.Out, app.Err = &out, &stderr
		if code := app.Run(context.Background(), args); code == 0 {
			t.Fatalf("%s unexpectedly succeeded: %s", name, out.String())
		}
	}
	assertRefused("missing confirmation", App{}, baseArgs)
	_, err = store.Update("fault_active_worker", 102, runID, nil, func(snapshot *state.Snapshot) error {
		snapshot.Issues["102"].WorkerPID = 4242
		snapshot.Issues["102"].WorkerPGID = 4242
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	assertRefused("active worker", App{ProcessController: &appProcessGroups{alive: map[int]bool{4242: true}, signals: map[int][]syscall.Signal{}}}, append(baseArgs, "--confirm-external-fix"))
	_, err = store.Update("fault_pending_request", 102, runID, nil, func(snapshot *state.Snapshot) error {
		snapshot.Issues["102"].WorkerPID = 0
		snapshot.Issues["102"].WorkerPGID = 0
		snapshot.PendingRequests["req-102"] = &state.Request{ID: "req-102", IssueNumber: 102, Status: "pending"}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	assertRefused("pending request", App{ProcessController: &appProcessGroups{alive: map[int]bool{}, signals: map[int][]syscall.Signal{}}}, append(baseArgs, "--confirm-external-fix"))
	_, err = store.Update("faults_repaired", 102, runID, nil, func(snapshot *state.Snapshot) error {
		delete(snapshot.PendingRequests, "req-102")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fixture, []byte("const value = \"dirty\";\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	assertRefused("dirty worktree", App{ProcessController: &appProcessGroups{alive: map[int]bool{}, signals: map[int][]syscall.Signal{}}}, append(baseArgs, "--confirm-external-fix"))
	if err := os.WriteFile(fixture, []byte("const value = \"deno-2.7.14\";\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var failedOut, failedErr bytes.Buffer
	failedApp := App{Out: &failedOut, Err: &failedErr, ProcessController: &appProcessGroups{alive: map[int]bool{}, signals: map[int][]syscall.Signal{}}}
	if code := failedApp.Run(context.Background(), append(baseArgs, "--confirm-external-fix")); code != 0 {
		t.Fatalf("failed checks observation code=%d stdout=%s stderr=%s", code, failedOut.String(), failedErr.String())
	}
	failedSnapshot, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	failedItem := failedSnapshot.Issues["102"]
	if failedItem.Status != "failed" || failedItem.Lease == nil || failedItem.PullRequestChecksRecovery == nil || failedItem.PullRequestChecksRecovery.Status != "checks_failed" {
		t.Fatalf("failed replacement checks did not remain terminal: %+v", failedItem)
	}
	writeFakeGH("SUCCESS")

	var recoveryID string
	for attempt := 0; attempt < 2; attempt++ {
		var out, stderr bytes.Buffer
		a := App{Out: &out, Err: &stderr, ProcessController: &appProcessGroups{alive: map[int]bool{}, signals: map[int][]syscall.Signal{}}}
		code := a.Run(context.Background(), []string{"recover-checks", "--repo", repo, "--issue", "102", "--confirm-external-fix", "--json"})
		if code != 0 {
			t.Fatalf("attempt=%d code=%d stdout=%s stderr=%s", attempt, code, out.String(), stderr.String())
		}
		snapshot, loadErr := store.Load()
		if loadErr != nil {
			t.Fatal(loadErr)
		}
		item := snapshot.Issues["102"]
		if attempt == 0 {
			recoveryID = item.PullRequestChecksRecovery.ID
		}
		if item.Status != "awaiting_checks" || item.GitHubSync != "" || item.Lease == nil || item.RunID != runID ||
			item.Attempts != cfg.Queue.MaxAttempts || item.Continuations != cfg.Worker.Profiles["extended"].MaxContinuations ||
			item.PullRequestChecksRecovery == nil || item.PullRequestChecksRecovery.ID != recoveryID ||
			item.PullRequestChecksRecovery.OldHeadSHA != oldHead || item.PullRequestChecksRecovery.NewHeadSHA != newHead ||
			item.PullRequestChecksRecovery.Status != "resumed" {
			t.Fatalf("recovery was not durable/idempotent: %+v", item)
		}
	}
	calls, err := os.ReadFile(ghLog)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(calls), "issue comment") != 1 || !strings.Contains(string(calls), "--remove-label codex-loop:failed") || !strings.Contains(string(calls), "codex-issue-loop:checks-recovery:") {
		t.Fatalf("GitHub recovery was not idempotent:\n%s", calls)
	}
}

func TestRecoverChecksAuthoritativeStateValidationFailsClosed(t *testing.T) {
	cfg := config.Defaults()
	cfg.GitHub.Repo = "owner/repo"
	current := &state.Issue{
		Number: 102, Branch: "codex/issue-102-checks", PullRequestURL: "https://example.test/pr/447", PullRequestNumber: 447,
	}
	inspection := worktree.Inspection{Head: "new-head", RemoteHead: "new-head"}
	baseline := gh.RemoteState{
		Issue: gh.Issue{Number: 102, State: "OPEN", Labels: []string{cfg.GitHub.FailedLabel}},
		PullRequests: []gh.PullRequest{{
			Number: 447, URL: current.PullRequestURL, State: "OPEN", HeadRefName: current.Branch,
			BaseRefName: cfg.Git.BaseBranch, HeadSHA: "new-head", ChecksStatus: "success", HeadRepository: cfg.GitHub.Repo,
		}},
	}
	clone := func() gh.RemoteState {
		result := baseline
		result.Issue.Labels = append([]string(nil), baseline.Issue.Labels...)
		result.PullRequests = append([]gh.PullRequest(nil), baseline.PullRequests...)
		return result
	}
	tests := []struct {
		name   string
		mutate func(*gh.RemoteState)
	}{
		{name: "manual exclusion", mutate: func(remote *gh.RemoteState) { remote.Issue.Labels = append(remote.Issue.Labels, "do-not-automate") }},
		{name: "conflicting running label", mutate: func(remote *gh.RemoteState) {
			remote.Issue.Labels = append(remote.Issue.Labels, cfg.GitHub.RunningLabel)
		}},
		{name: "closed Issue", mutate: func(remote *gh.RemoteState) { remote.Issue.State = "CLOSED" }},
		{name: "multiple Pull Requests", mutate: func(remote *gh.RemoteState) {
			remote.PullRequests = append(remote.PullRequests, remote.PullRequests[0])
		}},
		{name: "closed Pull Request", mutate: func(remote *gh.RemoteState) { remote.PullRequests[0].State = "CLOSED" }},
		{name: "changed branch", mutate: func(remote *gh.RemoteState) { remote.PullRequests[0].HeadRefName = "other" }},
		{name: "changed head", mutate: func(remote *gh.RemoteState) { remote.PullRequests[0].HeadSHA = "other-head" }},
		{name: "changed base", mutate: func(remote *gh.RemoteState) { remote.PullRequests[0].BaseRefName = "release" }},
		{name: "fork", mutate: func(remote *gh.RemoteState) { remote.PullRequests[0].HeadRepository = "attacker/repo" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			remote := clone()
			test.mutate(&remote)
			if _, err := validatePullRequestChecksRecovery(cfg, current, remote, inspection, true); err == nil {
				t.Fatal("unsafe authoritative state was accepted")
			}
		})
	}
	if _, err := validatePullRequestChecksRecovery(cfg, current, baseline, inspection, true); err != nil {
		t.Fatalf("aligned failed state was rejected: %v", err)
	}
}

func runGitApp(t *testing.T, dir string, args ...string) {
	t.Helper()
	if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
}

func runGitOutputApp(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestAdoptMergedPullRequestReleasesLeaseAndIsIdempotent(t *testing.T) {
	repo, l := testEnvironment(t)
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	binDir := strings.Split(os.Getenv("PATH"), string(os.PathListSeparator))[0]
	fakeGH := filepath.Join(binDir, "gh")
	ghLog := filepath.Join(t.TempDir(), "gh.log")

	runGitApp(t, repo, "config", "user.name", "Test")
	runGitApp(t, repo, "config", "user.email", "test@example.invalid")
	runGitApp(t, repo, "config", "commit.gpgsign", "false")
	runGitApp(t, repo, "add", ".agent-loop.yaml")
	runGitApp(t, repo, "commit", "-m", "base")
	runGitApp(t, repo, "branch", "-M", "main")
	remoteDir := filepath.Join(t.TempDir(), "remote.git")
	if out, err := exec.Command("git", "init", "--bare", "-q", remoteDir).CombinedOutput(); err != nil {
		t.Fatalf("git init --bare: %v: %s", err, out)
	}
	runGitApp(t, repo, "remote", "add", "origin", remoteDir)
	runGitApp(t, repo, "push", "-u", "origin", "main")
	baseSHA := runGitOutputApp(t, repo, "rev-parse", "HEAD")

	branch := "codex/issue-102-manual"
	canonicalRoot, err := filepath.EvalSymlinks(l.Root)
	if err != nil {
		t.Fatal(err)
	}
	managedWorktree := filepath.Join(canonicalRoot, "worktrees", "repo-test", "issue-102")
	if err := os.MkdirAll(filepath.Dir(managedWorktree), 0o700); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", repo, "worktree", "add", "-b", branch, managedWorktree).CombinedOutput(); err != nil {
		t.Fatalf("git worktree add: %v: %s", err, out)
	}
	fixture := filepath.Join(managedWorktree, "manual.txt")
	if err := os.WriteFile(fixture, []byte("published outside supervisor\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitApp(t, managedWorktree, "add", "manual.txt")
	runGitApp(t, managedWorktree, "commit", "-m", "manual publication")
	runGitApp(t, managedWorktree, "push", "-u", "origin", branch)
	headSHA := runGitOutputApp(t, managedWorktree, "rev-parse", "HEAD")
	runGitApp(t, repo, "merge", "--no-ff", "--no-edit", branch)
	runGitApp(t, repo, "push", "origin", "main")
	mergeSHA := runGitOutputApp(t, repo, "rev-parse", "HEAD")

	issueJSON := `{"number":102,"title":"manual publication","body":"","url":"https://example.test/issues/102","state":"CLOSED","labels":[{"name":"blocked"}],"assignees":[],"milestone":null,"comments":[{"body":"<!-- codex-issue-loop:failed:102 -->"}]}`
	prJSON := fmt.Sprintf(`[{"number":132,"url":"https://example.test/pr/132","state":"MERGED","isDraft":false,"mergedAt":"2026-08-18T00:00:00Z","headRefName":%q,"baseRefName":"main","headRefOid":%q,"mergeCommit":{"oid":%q},"headRepository":{"name":"repo"},"headRepositoryOwner":{"login":"owner"},"mergeStateStatus":"CLEAN","statusCheckRollup":[]}]`, branch, headSHA, mergeSHA)
	script := fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$*" >> %q
case "$1 $2" in
  "issue view") printf '%%s\n' '%s' ;;
  "pr list") printf '%%s\n' '%s' ;;
  "issue edit"|"issue comment"|"issue close") exit 0 ;;
  *) exit 2 ;;
esac
`, ghLog, issueJSON, prJSON)
	if err := os.WriteFile(fakeGH, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := mustConfig(t, repo)
	entry, err := (registry.Store{Path: l.RegistryPath}).Add(cfg)
	if err != nil {
		t.Fatal(err)
	}
	store := state.Store{Dir: l.RepoDir(entry.RepoID), RepoID: entry.RepoID, RepoPath: repo}
	if err := store.Initialize(); err != nil {
		t.Fatal(err)
	}
	runID := "run_adopt_102"
	_, _, err = store.ReserveLease(state.LeaseReservation{
		IssueNumber: 102, Title: "manual publication", RunID: runID, Slot: 0,
		DeclaredResources: []string{state.RepositoryResource}, ResolvedResources: []string{state.RepositoryResource},
		BaseSHA: baseSHA, ReservedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	answer := state.AnswerRecord{RequestID: "req-old", Question: "continue?", Answer: "yes", AnsweredAt: time.Now().UTC()}
	_, err = store.Update("worker_blocked", 102, runID, nil, func(snapshot *state.Snapshot) error {
		item := snapshot.Issues["102"]
		item.Status = "blocked"
		item.Worktree = managedWorktree
		item.Branch = branch
		item.Attempts = 3
		item.Continuations = 2
		item.SessionID = "session-102"
		item.Session = &state.WorkerSession{Backend: "codex", ID: "session-102"}
		item.Answers = []state.AnswerRecord{answer}
		item.FailureKind = "issue"
		item.LastError = "issue: worker environment blocked after manual publication"
		item.BlockedCause = &state.BlockedCause{Origin: "worker", Kind: "environment", Resumable: true, Reason: "legacy publication gap", BlockedAt: time.Now().UTC()}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	controller := &appProcessGroups{alive: map[int]bool{}, signals: map[int][]syscall.Signal{}}
	baseArgs := []string{"adopt-merged-pr", "--repo", repo, "--issue", "102", "--confirm-merged-pr-adoption", "--json"}
	run := func(args []string) (int, string, string) {
		t.Helper()
		var out, stderr bytes.Buffer
		app := App{Out: &out, Err: &stderr, ProcessController: controller}
		return app.Run(context.Background(), args), out.String(), stderr.String()
	}
	if code, _, _ := run(baseArgs[:len(baseArgs)-2]); code == 0 {
		t.Fatal("missing explicit confirmation was accepted")
	}
	_, err = store.Update("test_worker_alive", 102, runID, nil, func(snapshot *state.Snapshot) error {
		snapshot.Issues["102"].WorkerPID = 4321
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	controller.alive[4321] = true
	if code, _, _ := run(baseArgs); code == 0 {
		t.Fatal("active worker was accepted")
	}
	controller.alive[4321] = false
	_, err = store.Update("test_pending_request", 102, runID, nil, func(snapshot *state.Snapshot) error {
		snapshot.Issues["102"].WorkerPID = 0
		snapshot.PendingRequests["req-pending"] = &state.Request{ID: "req-pending", IssueNumber: 102, Status: "pending"}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if code, _, _ := run(baseArgs); code == 0 {
		t.Fatal("pending manual answer was accepted")
	}
	_, err = store.Update("test_request_answered", 102, runID, nil, func(snapshot *state.Snapshot) error {
		snapshot.PendingRequests["req-pending"].Status = "answered"
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	dirty := filepath.Join(managedWorktree, "dirty.txt")
	if err := os.WriteFile(dirty, []byte("unsafe\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := run(baseArgs); code == 0 {
		t.Fatal("dirty worktree was accepted")
	}
	if err := os.Remove(dirty); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := run(baseArgs)
	if code != 0 {
		t.Fatalf("code=%d stdout=%s stderr=%s", code, stdout, stderr)
	}
	var output map[string]any
	if err := json.Unmarshal([]byte(stdout), &output); err != nil {
		t.Fatal(err)
	}
	if output["status"] != "completed" || output["adoption_status"] != "synced" || output["lease_released"] != true {
		t.Fatalf("output=%v", output)
	}
	snapshot, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	item := snapshot.Issues["102"]
	if item.Lease != nil || item.Status != "completed" || !item.PullRequestMerged || item.PullRequestNumber != 132 || item.GitHubSync != "" ||
		item.MergedPullRequestAdoption == nil || item.MergedPullRequestAdoption.MergeSHA != mergeSHA || item.MergedPullRequestAdoption.Status != "synced" {
		t.Fatalf("adopted Issue=%+v", item)
	}
	if item.Attempts != 3 || item.Continuations != 2 || item.SessionID != "session-102" || !reflect.DeepEqual(item.Answers, []state.AnswerRecord{answer}) {
		t.Fatalf("worker history changed: %+v", item)
	}
	if code, stdout, stderr := run(baseArgs); code != 0 || !strings.Contains(stdout, `"idempotent": true`) {
		t.Fatalf("idempotent rerun code=%d stdout=%s stderr=%s", code, stdout, stderr)
	}
	if _, _, err := store.ReserveLease(state.LeaseReservation{
		IssueNumber: 130, Title: "next", RunID: "run_next_130", Slot: 0,
		DeclaredResources: []string{state.RepositoryResource}, ResolvedResources: []string{state.RepositoryResource}, BaseSHA: mergeSHA,
	}); err != nil {
		t.Fatalf("released repository lease did not unblock next Issue: %v", err)
	}
	logBytes, err := os.ReadFile(ghLog)
	if err != nil {
		t.Fatal(err)
	}
	logText := string(logBytes)
	for _, command := range []string{"issue edit", "issue comment", "issue close"} {
		if strings.Count(logText, command) != 1 {
			t.Fatalf("%s calls were not exactly once:\n%s", command, logText)
		}
	}
}

func TestEvaluateSleepSettings(t *testing.T) {
	output := `Battery Power:
 sleep 1
AC Power:
 sleep 0
 displaysleep 10
`
	ok, detail := evaluateSleepSettings(output, nil)
	if !ok || detail == "" {
		t.Fatalf("ok=%v detail=%q", ok, detail)
	}
	ok, _ = evaluateSleepSettings("AC Power:\n sleep 1\n", nil)
	if ok {
		t.Fatal("enabled sleep was accepted")
	}
}

func TestBootstrapLabelsCommandRequiresApplyToMutate(t *testing.T) {
	repo, _ := testEnvironment(t)
	binDir := filepath.Join(filepath.Dir(repo), "bin")
	logPath := filepath.Join(filepath.Dir(repo), "label-calls.log")
	fakeGH := filepath.Join(binDir, "gh")
	script := "#!/bin/sh\ncase \"$1 $2\" in\n  \"label list\") printf '[]\\n' ;;\n  \"label create\") printf '%s\\n' \"$*\" >> \"$AGENT_LOOP_LABEL_LOG\" ;;\n  *) exit 2 ;;\nesac\n"
	if err := os.WriteFile(fakeGH, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENT_LOOP_LABEL_LOG", logPath)
	for _, test := range []struct {
		name  string
		apply bool
	}{
		{name: "preview"},
		{name: "apply", apply: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var out, stderr bytes.Buffer
			args := []string{"bootstrap-labels", "--repo", repo, "--json"}
			if test.apply {
				args = append(args, "--apply")
			}
			if code := (App{Out: &out, Err: &stderr}).Run(context.Background(), args); code != 0 {
				t.Fatalf("code=%d stderr=%s", code, stderr.String())
			}
			var result gh.LabelBootstrapResult
			if err := json.Unmarshal(out.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if result.Applied != test.apply {
				t.Fatalf("result=%+v", result)
			}
			if !test.apply {
				if _, err := os.Stat(logPath); !os.IsNotExist(err) {
					t.Fatalf("preview mutated labels: %v", err)
				}
			}
		})
	}
	calls, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(calls), "label create") != len(gh.RequiredLabelSpecs(mustConfig(t, repo))) {
		t.Fatalf("calls=%s", calls)
	}
}

func TestInitCommandPreviewIsReadOnlyAndAgentsCanBeLimited(t *testing.T) {
	root := t.TempDir()
	codexHome := filepath.Join(root, "codex")
	claudeDir := filepath.Join(root, "claude")
	agentLoopHome := filepath.Join(root, "agent-loop")
	t.Setenv("CODEX_HOME", codexHome)
	t.Setenv("CLAUDE_CONFIG_DIR", claudeDir)
	t.Setenv("AGENT_LOOP_HOME", agentLoopHome)

	var out, stderr bytes.Buffer
	a := App{Out: &out, Err: &stderr}
	if code := a.Run(context.Background(), []string{"init", "--agents", "claude", "--json"}); code != 0 {
		t.Fatalf("preview code=%d stderr=%s", code, stderr.String())
	}
	var preview userrules.Report
	if err := json.Unmarshal(out.Bytes(), &preview); err != nil {
		t.Fatal(err)
	}
	if preview.Apply || preview.Changed || len(preview.Targets) != 1 || preview.Targets[0].Agent != userrules.AgentClaude || preview.Targets[0].Status != userrules.StatusMissing {
		t.Fatalf("preview=%+v", preview)
	}
	for _, path := range []string{codexHome, claudeDir, agentLoopHome} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("preview created %s: %v", path, err)
		}
	}

	out.Reset()
	stderr.Reset()
	if code := a.Run(context.Background(), []string{"init", "--agents", "claude", "--apply", "--json"}); code != 0 {
		t.Fatalf("apply code=%d stderr=%s", code, stderr.String())
	}
	var applied userrules.Report
	if err := json.Unmarshal(out.Bytes(), &applied); err != nil {
		t.Fatal(err)
	}
	if !applied.Apply || !applied.Changed || applied.Targets[0].ApplyResult != userrules.ResultCreated {
		t.Fatalf("applied=%+v", applied)
	}
	if _, err := os.Stat(filepath.Join(claudeDir, "rules", "codex-issue-loop.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(codexHome, "AGENTS.md")); !os.IsNotExist(err) {
		t.Fatalf("limited apply changed Codex target: %v", err)
	}
}

func TestInitCommandRejectsUnknownAgentWithoutChangingFiles(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(root, "claude"))
	t.Setenv("AGENT_LOOP_HOME", filepath.Join(root, "agent-loop"))
	var out, stderr bytes.Buffer
	if code := (App{Out: &out, Err: &stderr}).Run(context.Background(), []string{"init", "--agents", "cursor", "--apply", "--json"}); code != 2 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
		t.Fatalf("unexpected changes=%v err=%v", entries, err)
	}
}

func TestRecordSupervisorControlReplacesStaleStoppedState(t *testing.T) {
	store := state.Store{Dir: t.TempDir(), RepoID: "repo-id", RepoPath: "/tmp/repo"}
	if err := store.Initialize(); err != nil {
		t.Fatal(err)
	}
	_, err := store.Update("failed", 0, "", nil, func(snapshot *state.Snapshot) error {
		snapshot.Supervisor.State = "stopped"
		snapshot.Supervisor.PID = 42
		snapshot.Supervisor.Message = "old failure"
		snapshot.Supervisor.FailureKind = "transient"
		snapshot.Supervisor.ConsecutiveFailures = 3
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := recordSupervisorControl(store, "starting", "restart requested"); err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	got := snapshot.Supervisor
	if got.State != "starting" || got.PID != 0 || got.Message != "restart requested" || got.FailureKind != "" || got.ConsecutiveFailures != 0 {
		t.Fatalf("unexpected supervisor state: %+v", got)
	}
}

func TestInstallArtifactsAreIdempotentAndVersioned(t *testing.T) {
	_, l := testEnvironment(t)
	source := filepath.Join(t.TempDir(), "agent-loop")
	if err := os.WriteFile(source, []byte("release-binary"), 0o700); err != nil {
		t.Fatal(err)
	}
	first, changed, err := installArtifacts(l, source, "v1.2.3", "abc123")
	if err != nil || !changed {
		t.Fatalf("first=%+v changed=%v err=%v", first, changed, err)
	}
	second, changed, err := installArtifacts(l, source, "v1.2.3", "abc123")
	if err != nil || changed || second != first {
		t.Fatalf("second=%+v changed=%v err=%v", second, changed, err)
	}
	version, err := os.ReadFile(filepath.Join(l.SkillsDir, "agent-loop", "VERSION"))
	if err != nil || string(version) != "v1.2.3\n" {
		t.Fatalf("version=%q err=%v", version, err)
	}
	match, err := installationMatches(l, source, "v1.2.3", "abc123")
	if err != nil || !match {
		t.Fatalf("match=%v err=%v", match, err)
	}
}

func TestUninstallPreservesLegacyCredentialFile(t *testing.T) {
	_, l := testEnvironment(t)
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(t.TempDir(), "agent-loop")
	if err := os.WriteFile(source, []byte("release-binary"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, _, err := installArtifacts(l, source, "v1.2.3", "abc123"); err != nil {
		t.Fatal(err)
	}
	legacyPath := filepath.Join(l.RepoDir("legacy-repo"), "notification-token")
	if err := os.MkdirAll(filepath.Dir(legacyPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyPath, []byte("retained-legacy-credential\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := (App{Out: &out, Err: &out}).uninstall(context.Background(), l, []string{"--json"}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(legacyPath)
	if err != nil || string(data) != "retained-legacy-credential\n" {
		t.Fatalf("uninstall modified legacy credential: err=%v data=%q", err, data)
	}
}

func TestUpdateBackupCanRestoreBinarySkillAndManifest(t *testing.T) {
	_, l := testEnvironment(t)
	oldSource := filepath.Join(t.TempDir(), "old-agent-loop")
	newSource := filepath.Join(t.TempDir(), "new-agent-loop")
	if err := os.WriteFile(oldSource, []byte("old-release"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newSource, []byte("new-release"), 0o700); err != nil {
		t.Fatal(err)
	}
	oldManifest, _, err := installArtifacts(l, oldSource, "v1.0.0", "old")
	if err != nil {
		t.Fatal(err)
	}
	backup, err := backupInstallation(l)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := installArtifacts(l, newSource, "v1.1.0", "new"); err != nil {
		t.Fatal(err)
	}
	if err := restoreInstallation(l, backup); err != nil {
		t.Fatal(err)
	}
	restored, err := readInstallManifest(filepath.Join(l.Root, "install.json"))
	if err != nil || restored != oldManifest {
		t.Fatalf("restored=%+v want=%+v err=%v", restored, oldManifest, err)
	}
	resolved, err := validateBackupPath(l, backup)
	expectedBackup, resolveErr := filepath.EvalSymlinks(backup)
	if err != nil || resolveErr != nil || resolved != expectedBackup {
		t.Fatalf("resolved=%q expected=%q err=%v resolveErr=%v", resolved, expectedBackup, err, resolveErr)
	}
	if _, err := validateBackupPath(l, filepath.Dir(l.Root)); err == nil {
		t.Fatal("outside backup path accepted")
	}
}

func TestSchemaChangingUpdateRequiresStoppedMigrationAndPairedRollback(t *testing.T) {
	repo, l := testEnvironment(t)
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	oldSource := filepath.Join(t.TempDir(), "old-agent-loop")
	if err := os.WriteFile(oldSource, []byte("old-v3-release"), 0o700); err != nil {
		t.Fatal(err)
	}
	oldManifest, _, err := installArtifacts(l, oldSource, "v0.1.0", "old")
	if err != nil {
		t.Fatal(err)
	}
	oldManifest.SchemaVersion = 3
	writeJSONFixture(t, filepath.Join(l.Root, "install.json"), oldManifest)
	writeLegacySchemas(t, repo, l)

	oldVersion, oldCommit := Version, Commit
	Version, Commit = "v0.2.0-test", "candidate"
	t.Cleanup(func() { Version, Commit = oldVersion, oldCommit })
	var out, stderr bytes.Buffer
	a := App{Out: &out, Err: &stderr}
	if err := a.update(context.Background(), l, []string{"--json"}); err != nil {
		t.Fatalf("update: %v stderr=%s", err, stderr.String())
	}
	var updateResult struct {
		Backup                  string `json:"backup"`
		SchemaMigrationRequired bool   `json:"schema_migration_required"`
	}
	if err := json.Unmarshal(out.Bytes(), &updateResult); err != nil || updateResult.Backup == "" || !updateResult.SchemaMigrationRequired {
		t.Fatalf("update result=%+v err=%v output=%s", updateResult, err, out.String())
	}

	migrationResult, err := (schema.Migrator{Layout: l}).Apply()
	if err != nil || migrationResult.Backup == "" {
		t.Fatalf("migration=%+v err=%v", migrationResult, err)
	}
	if err := a.rollback(context.Background(), l, []string{"--backup", updateResult.Backup, "--json"}); err == nil || !strings.Contains(err.Error(), "restore the matching migration backup first") {
		t.Fatalf("installation rollback crossed schema boundary: %v", err)
	}
	if _, err := (schema.Migrator{Layout: l}).Restore(migrationResult.Backup); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := a.rollback(context.Background(), l, []string{"--backup", updateResult.Backup, "--json"}); err != nil {
		t.Fatalf("paired installation rollback: %v", err)
	}
	restored, err := readInstallManifest(filepath.Join(l.Root, "install.json"))
	if err != nil || restored.Version != "v0.1.0" || restored.SchemaVersion != 3 {
		t.Fatalf("restored=%+v err=%v", restored, err)
	}
}

func writeLegacySchemas(t *testing.T, repo string, l layout.Layout) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repo, config.FileName), []byte("version: 3\ngithub:\n  repo: owner/repo\nnotifications:\n  enabled: false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	entry := registry.Entry{
		RepoID: "repo-v3", RepoPath: repo, GitHubRepo: "owner/repo",
		Commands: map[string]string{"launchctl": "/usr/bin/false"},
	}
	writeJSONFixture(t, l.RegistryPath, registry.Registry{Version: 3, Repos: map[string]registry.Entry{entry.RepoID: entry}})
	if err := os.MkdirAll(l.RepoDir(entry.RepoID), 0o700); err != nil {
		t.Fatal(err)
	}
	writeJSONFixture(t, filepath.Join(l.RepoDir(entry.RepoID), "state.json"), state.Snapshot{
		Version: 3, RepoID: entry.RepoID, RepoPath: repo,
		Supervisor: state.Supervisor{State: "stopped"}, Issues: map[string]*state.Issue{}, PendingRequests: map[string]*state.Request{},
	})
}

func writeJSONFixture(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}
