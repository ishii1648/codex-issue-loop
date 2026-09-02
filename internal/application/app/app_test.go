package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	gh "github.com/ishii1648/codex-issue-loop/internal/adapter/github"
	"github.com/ishii1648/codex-issue-loop/internal/adapter/state"
	"github.com/ishii1648/codex-issue-loop/internal/adapter/worker"
	"github.com/ishii1648/codex-issue-loop/internal/adapter/worktree"
	schema "github.com/ishii1648/codex-issue-loop/internal/application/migration"
	"github.com/ishii1648/codex-issue-loop/internal/application/observe"
	"github.com/ishii1648/codex-issue-loop/internal/application/supervisor"
	"github.com/ishii1648/codex-issue-loop/internal/platform/config"
	"github.com/ishii1648/codex-issue-loop/internal/platform/launchd"
	"github.com/ishii1648/codex-issue-loop/internal/platform/layout"
	"github.com/ishii1648/codex-issue-loop/internal/platform/registry"
	"github.com/ishii1648/codex-issue-loop/internal/platform/userrules"
)

type appProcessGroups struct {
	alive   map[int]bool
	signals map[int][]syscall.Signal
}

func TestHelpDoesNotCreateOrSecureManagementRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "absent-management-root")
	t.Setenv("AGENT_LOOP_HOME", root)
	var out bytes.Buffer
	if code := (App{Out: &out, Err: io.Discard}).Run(context.Background(), []string{"help"}); code != 0 || !strings.Contains(out.String(), "issue         Plan or resolve") {
		t.Fatalf("code=%d output=%q", code, out.String())
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("help changed management root: %v", err)
	}
}

func abandonLeaseForAppTest(store state.Store, issueNumber int, owner state.LeaseOwner) error {
	_, err := store.Update("lease_abandoned_fixture", issueNumber, owner.RunID, map[string]any{"owner": owner}, func(snapshot *state.Snapshot) error {
		item := snapshot.Issues[strconv.Itoa(issueNumber)]
		if item == nil || item.Lease == nil || item.Lease.Owner != owner {
			return fmt.Errorf("Issue #%d lease is not owned by fixture", issueNumber)
		}
		transition, transitionErr := issuedomain.NewTransition("abandon_fixture", item.Status, issuedomain.StatusFailed)
		if transitionErr != nil {
			return transitionErr
		}
		item.LastError = "test execution abandoned"
		return state.ApplyIssueTransition(item, transition)
	})
	return err
}

type resumedWorkspaceGitHub struct{ issue gh.Issue }

func testWorkerWorkspace(snapshot *state.Snapshot, path, branch string) *state.WorkerWorkspace {
	return &state.WorkerWorkspace{
		Path: path, Branch: branch, RepoID: snapshot.RepoID, Repository: "owner/repo",
		GitCommonDir: filepath.Join(snapshot.RepoPath, ".git"), MainCheckout: snapshot.RepoPath, CapturedAt: time.Now().UTC(),
	}
}

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

func TestDeliveryConfigureDefaultsToPreviewWithoutWritingConfigOrPlist(t *testing.T) {
	root := t.TempDir()
	managed := filepath.Join(root, "managed")
	launch := filepath.Join(root, "launch")
	configPath := filepath.Join(root, "delivery.yaml")
	t.Setenv("AGENT_LOOP_HOME", managed)
	t.Setenv("AGENT_LOOP_LAUNCH_AGENTS_DIR", launch)
	t.Setenv("AGENT_LOOP_SKILLS_DIR", filepath.Join(root, "skills"))
	var out, stderr bytes.Buffer
	code := (App{Out: &out, Err: &stderr}).Run(context.Background(), []string{"delivery", "configure", "--config", configPath, "--json"})
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	for _, path := range []string{configPath, filepath.Join(launch, "com.codex-issue-loop.delivery.plist")} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("preview wrote %s: %v", path, err)
		}
	}
	if !strings.Contains(out.String(), `"applied": false`) || !strings.Contains(out.String(), `"runtime_root"`) {
		t.Fatalf("output=%s", out.String())
	}
}

func TestDeliveryRetryRollbackRequiresExplicitConfirmation(t *testing.T) {
	var output bytes.Buffer
	err := (App{Out: &output, Err: &output}).delivery(context.Background(), layout.Layout{Root: t.TempDir()}, []string{"retry-rollback", "--backup", "/tmp/not-authorized", "--json"})
	if err == nil || !strings.Contains(err.Error(), "--confirm-retained-fence") {
		t.Fatalf("err=%v output=%s", err, output.String())
	}
}

func TestDeliveryAssignmentRetryRollbackRequiresExplicitConfirmation(t *testing.T) {
	var output bytes.Buffer
	err := (App{Out: &output, Err: &output}).delivery(context.Background(), layout.Layout{Root: t.TempDir()}, []string{"assignment", "retry-rollback", "--repo", "/tmp/repo", "--json"})
	if err == nil || !strings.Contains(err.Error(), "--confirm-retained-fence") {
		t.Fatalf("err=%v output=%s", err, output.String())
	}
}

func TestDeliveryConfigureApplyWritesPrivateDefaultConfigAndSingleLaunchAgent(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("DELIVERY_LAUNCHD_STATE", filepath.Join(root, "loaded"))
	launchctl := filepath.Join(root, "launchctl")
	script := "#!/bin/sh\ncase \"$1\" in\n print) test -f \"$DELIVERY_LAUNCHD_STATE\" && printf 'state = running\\npid = 123\\n' ;;\n bootstrap) : > \"$DELIVERY_LAUNCHD_STATE\" ;;\n bootout) rm -f \"$DELIVERY_LAUNCHD_STATE\" ;;\n *) exit 2 ;;\nesac\n"
	if err := os.WriteFile(launchctl, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", root+":"+os.Getenv("PATH"))
	l := layout.Layout{Root: filepath.Join(root, "managed"), RegistryPath: filepath.Join(root, "managed", "registry.json"), ReposRoot: filepath.Join(root, "managed", "repos"), BinDir: filepath.Join(root, "managed", "bin"), SkillsDir: filepath.Join(root, "skills"), LaunchAgents: filepath.Join(root, "launch")}
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(l.BinDir, "agent-loop"), []byte("managed"), 0o700); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := (App{Out: &out, Err: &out}).deliveryConfigure(context.Background(), l, []string{"--apply", "--json"}); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(home, ".agent-loop-delivery.yaml")
	for _, path := range []string{configPath, l.DeliveryPlistPath()} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("path=%s info=%v err=%v", path, info, err)
		}
	}
	data, err := os.ReadFile(l.DeliveryPlistPath())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(data), "com.codex-issue-loop.delivery") != 1 || strings.Contains(string(data), "--config") {
		t.Fatalf("plist=%s", data)
	}
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
	t.Setenv("HOME", filepath.Join(root, "user"))
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
	config := `version: 5
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

func TestIncidentStatusReportsDisabledDryRunHealthWithoutNetwork(t *testing.T) {
	repo, l := testEnvironment(t)
	cfg, err := config.Load(repo)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (registry.Store{Path: l.RegistryPath}).Add(cfg); err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	code := (App{Out: &out, Err: &stderr}).Run(context.Background(), []string{"incident", "status", "--repo", repo, "--json"})
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	var status struct {
		Version int  `json:"version"`
		Enabled bool `json:"enabled"`
		DryRun  bool `json:"dry_run"`
		State   struct {
			Version int `json:"version"`
		} `json:"state"`
	}
	if err := json.Unmarshal(out.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.Version != 1 || status.State.Version != 1 || status.Enabled || !status.DryRun {
		t.Fatalf("status=%+v", status)
	}
}

func TestIncidentSeedCanaryRequiresConfirmationAndDedicatedRepository(t *testing.T) {
	repo, l := testEnvironment(t)
	cfg, err := config.Load(repo)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (registry.Store{Path: l.RegistryPath}).Add(cfg); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		args []string
		code int
		want string
	}{
		{name: "confirmation", args: []string{"incident", "seed-canary", "--repo", repo, "--id", "release-v1-2-3", "--json"}, code: 2, want: "--confirm-synthetic-evidence"},
		{name: "repository", args: []string{"incident", "seed-canary", "--repo", repo, "--id", "release-v1-2-3", "--confirm-synthetic-evidence", "--json"}, code: 1, want: "ends with -canary"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var out, stderr bytes.Buffer
			code := (App{Out: &out, Err: &stderr}).Run(context.Background(), test.args)
			if code != test.code || !strings.Contains(stderr.String(), test.want) {
				t.Fatalf("code=%d stderr=%s", code, stderr.String())
			}
		})
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
	issue.Status = issuedomain.StatusNeedsInput
	issue.Worktree = fmt.Sprintf("/tmp/issue-%d", issue.Number)
	issue.Branch = fmt.Sprintf("codex/issue-%d", issue.Number)
	issue.Workspace = &state.WorkerWorkspace{Path: issue.Worktree, Branch: issue.Branch, RepoID: snapshot.RepoID,
		Repository: "owner/repo", GitCommonDir: filepath.Join(snapshot.RepoPath, ".git"), MainCheckout: snapshot.RepoPath, CapturedAt: now}
	issue.LeaseGeneration = 1
	issue.Lease = &state.ResourceLease{
		Owner: owner, Slot: slot, DeclaredResources: []string{}, ResolvedResources: []string{state.RepositoryResource}, BaseSHA: "base-sha", ReservedAt: now,
	}
	parkID := "park_" + request.ID
	if err := state.CaptureContinuationLease(issue, owner, parkID, now); err != nil {
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
		addParkedNeedsInput(t, s, &state.Issue{Number: 4, RunID: "run_4"}, &state.Request{ID: "req_1", IssueNumber: 4, Question: "Choose", Status: issuedomain.RequestStatusPending}, 0)
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
	if snapshot.Issues["4"].Status != issuedomain.StatusResumePending || len(snapshot.Issues["4"].Answers) != 1 {
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
		AllowFreeText: true, Status: issuedomain.RequestStatusPending, CreatedAt: createdAt,
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
	if request := snapshot.PendingRequests[firstRequest.ID]; request.Status != issuedomain.RequestStatusAnswered || request.Answer != "safe" {
		t.Fatalf("answered request=%+v", request)
	}
	if issue := snapshot.Issues["89"]; issue.Status != issuedomain.StatusResumePending || len(issue.Answers) != 1 || issue.Answers[0].RequestID != firstRequest.ID {
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
		Status:  issuedomain.RequestStatusPending, CreatedAt: createdAt.Add(time.Minute),
	}
	_, err = store.Update("input_requested", 90, "run_90", nil, func(snapshot *state.Snapshot) error {
		snapshot.Issues["90"] = &state.Issue{Number: 90, RunID: "run_90", Status: issuedomain.StatusNeedsInput}
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
			runID := fmt.Sprintf("run_%d", number)
			snapshot.Issues[strconv.Itoa(number)] = &state.Issue{
				Number: number, RunID: runID, Status: issuedomain.StatusRunning, LeaseGeneration: 1,
				Lease: &state.ExecutionLease{Owner: state.LeaseOwner{RunID: runID, Generation: 1}, Slot: number - 1,
					DeclaredResources: []string{}, ResolvedResources: []string{fmt.Sprintf("fixture-%d", number)}, ReservedAt: time.Now().UTC()},
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
		if issue := snapshot.Issues[key]; issue.Status != issuedomain.StatusRetryWait || issue.WorkerPID != 0 || issue.WorkerPGID != 0 {
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
	configuration := fmt.Sprintf(`version: 5
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

func TestStartQuarantinesLegacySemanticStateBeforeLaunchdMutation(t *testing.T) {
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
	snapshot, err := store.Update("legacy_retry", 169, "run_169", nil, func(snapshot *state.Snapshot) error {
		issue := &state.Issue{Number: 169, Status: issuedomain.StatusRetryWait, RunID: "run_169", Worktree: "/tmp/legacy", Branch: "codex/issue-169", Attempts: 1}
		issue.Workspace = testWorkerWorkspace(snapshot, issue.Worktree, issue.Branch)
		snapshot.Issues["169"] = issue
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot.SemanticContractVersion = 0
	writeJSONFixture(t, store.StatePath(), snapshot)
	err = (App{Out: io.Discard, Err: io.Discard}).control(context.Background(), l, "start", []string{"--repo", repo, "--json"})
	if err == nil || !strings.Contains(err.Error(), "recovery-blocked") || !strings.Contains(err.Error(), "semantic contract version") {
		t.Fatalf("legacy semantic state was not rejected: %v", err)
	}
	blocked, loadErr := store.Load()
	if loadErr != nil || blocked.Recovery == nil || blocked.Recovery.Status != state.RecoveryStateBlocked || blocked.Recovery.BackupDir == "" {
		t.Fatalf("semantic quarantine did not preserve a recovery backup: snapshot=%+v err=%v", blocked, loadErr)
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
			request := &state.Request{ID: fmt.Sprintf("req_%d", number), IssueNumber: number, Question: fmt.Sprintf("%d?", number), Status: issuedomain.RequestStatusPending}
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
	if snapshot.PendingRequests["req_4"].Status != issuedomain.RequestStatusPending || snapshot.PendingRequests["req_4"].Answer != "" || snapshot.Issues["4"].Status != issuedomain.StatusNeedsInput || len(snapshot.Issues["4"].Answers) != 0 {
		t.Fatalf("unrelated request or Issue changed: request=%+v issue=%+v", snapshot.PendingRequests["req_4"], snapshot.Issues["4"])
	}
	if snapshot.PendingRequests["req_7"].Status != issuedomain.RequestStatusAnswered || snapshot.Issues["7"].Status != issuedomain.StatusResumePending || len(snapshot.Issues["7"].Answers) != 1 {
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
			ID: "req_4", IssueNumber: 4, Question: "Continue?", Status: issuedomain.RequestStatusPending, CreatedAt: now,
		}, 0)
		competingOwner = state.LeaseOwner{RunID: "run_5", Generation: 1}
		snapshot.Issues["5"] = &state.Issue{
			Number: 5, RunID: "run_5", Status: issuedomain.StatusRunning, LeaseGeneration: 1,
			Worktree: "/tmp/issue-5", Branch: "codex/issue-5",
			Workspace: &state.WorkerWorkspace{Path: "/tmp/issue-5", Branch: "codex/issue-5", RepoID: snapshot.RepoID,
				Repository: "owner/repo", GitCommonDir: filepath.Join(snapshot.RepoPath, ".git"), MainCheckout: snapshot.RepoPath, CapturedAt: now},
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
	if issue := afterFirst.Issues["4"]; issue.Status != issuedomain.StatusAnswerClaimWaiting || issue.Lease != nil || issue.ResourcePark.Status != issuedomain.ResourceParkStatusParked || len(issue.Answers) != 1 {
		t.Fatalf("answered Issue=%+v", issue)
	}
	if request := afterFirst.PendingRequests["req_4"]; request.Status != issuedomain.RequestStatusAnswered || request.Answer != "continue" {
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

func TestDeliveryBackupPathIsConfinedToManagedDirectChild(t *testing.T) {
	_, l := testEnvironment(t)
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(l.Root, "backups", "delivery-generation-v1.0.0")
	resolved, err := validateDeliveryBackupPath(l, want)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(resolved) != filepath.Base(want) {
		t.Fatalf("resolved=%s want=%s", resolved, want)
	}
	for _, path := range []string{filepath.Join(l.Root, "backups", "manual"), filepath.Join(l.Root, "backups", "delivery-ok", "nested"), filepath.Join(filepath.Dir(l.Root), "delivery-outside")} {
		if _, err := validateDeliveryBackupPath(l, path); err == nil {
			t.Fatalf("unsafe delivery backup accepted: %s", path)
		}
	}
	outside := t.TempDir()
	link := filepath.Join(l.Root, "backups", "delivery-link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if _, err := validateDeliveryBackupPath(l, link); err == nil {
		t.Fatal("symlink delivery backup accepted")
	}
}

func TestDeliveryBackupRetryPreservesCompletePreviousInstallation(t *testing.T) {
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
	backup := filepath.Join(l.Root, "backups", "delivery-generation-v1.0.0")
	if _, err := backupInstallationAt(l, backup); err != nil {
		t.Fatal(err)
	}
	if _, _, err := installArtifacts(l, newSource, "v1.1.0", "new"); err != nil {
		t.Fatal(err)
	}
	if _, err := backupInstallationAt(l, backup); err != nil {
		t.Fatal(err)
	}
	if err := restoreInstallation(l, backup); err != nil {
		t.Fatal(err)
	}
	restored, err := readInstallManifest(filepath.Join(l.Root, "install.json"))
	if err != nil || restored != oldManifest {
		t.Fatalf("retry overwrote previous backup: restored=%+v want=%+v err=%v", restored, oldManifest, err)
	}
}

func TestDeliveryBackupRetryRebuildsOnlyIncompleteBackupFromConsistentInstall(t *testing.T) {
	_, l := testEnvironment(t)
	source := filepath.Join(t.TempDir(), "agent-loop")
	if err := os.WriteFile(source, []byte("release"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, _, err := installArtifacts(l, source, "v1.0.0", "commit"); err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(l.Root, "backups", "delivery-generation-v1.0.0")
	if err := os.MkdirAll(backup, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(backup, "agent-loop"), []byte("partial"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := backupInstallationAt(l, backup); err != nil {
		t.Fatal(err)
	}
	if err := validateInstallationBackup(backup); err != nil {
		t.Fatalf("rebuilt backup is incomplete: %v", err)
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
	oldManifest.SchemaVersion = schema.CurrentVersion - 1
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
	if err != nil || restored.Version != "v0.1.0" || restored.SchemaVersion != schema.CurrentVersion-1 {
		t.Fatalf("restored=%+v err=%v", restored, err)
	}
}

func writeLegacySchemas(t *testing.T, repo string, l layout.Layout) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repo, config.FileName), []byte(fmt.Sprintf("version: %d\ngithub:\n  repo: owner/repo\nnotifications:\n  enabled: false\n", schema.CurrentVersion-1)), 0o600); err != nil {
		t.Fatal(err)
	}
	entry := registry.Entry{
		RepoID: "repo-v3", RepoPath: repo, GitHubRepo: "owner/repo",
		Commands: map[string]string{"launchctl": "/usr/bin/false"},
	}
	writeJSONFixture(t, l.RegistryPath, registry.Registry{Version: schema.CurrentVersion - 1, Repos: map[string]registry.Entry{entry.RepoID: entry}})
	if err := os.MkdirAll(l.RepoDir(entry.RepoID), 0o700); err != nil {
		t.Fatal(err)
	}
	writeJSONFixture(t, filepath.Join(l.RepoDir(entry.RepoID), "state.json"), state.Snapshot{
		Version: schema.CurrentVersion - 1, RepoID: entry.RepoID, RepoPath: repo,
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
