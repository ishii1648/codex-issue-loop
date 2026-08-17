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
	"github.com/ishii1648/codex-issue-loop/internal/registry"
	"github.com/ishii1648/codex-issue-loop/internal/state"
	"github.com/ishii1648/codex-issue-loop/internal/userrules"
	"github.com/ishii1648/codex-issue-loop/internal/worktree"
)

type appProcessGroups struct {
	alive   map[int]bool
	signals map[int][]syscall.Signal
}

func (f *appProcessGroups) Alive(pid int) bool           { return f.alive[pid] }
func (f *appProcessGroups) GroupAlive(pgid int) bool     { return f.alive[pgid] }
func (f *appProcessGroups) OwnsGroup(pid, pgid int) bool { return pid == pgid && f.alive[pgid] }
func (f *appProcessGroups) SignalGroup(pgid int, signal syscall.Signal) error {
	f.signals[pgid] = append(f.signals[pgid], signal)
	f.alive[pgid] = false
	return nil
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
		s.Issues["4"] = &state.Issue{Number: 4, Status: "needs_input"}
		s.PendingRequests["req_1"] = &state.Request{ID: "req_1", IssueNumber: 4, Question: "Choose", Status: "pending"}
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
		snapshot.Issues["89"] = &state.Issue{Number: 89, RunID: "run_89", Status: "needs_input"}
		snapshot.PendingRequests[firstRequest.ID] = firstRequest
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
			snapshot.Issues[strconv.Itoa(number)] = &state.Issue{Number: number, RunID: fmt.Sprintf("run_%d", number), Status: "needs_input"}
		}
		snapshot.PendingRequests["req_4"] = &state.Request{ID: "req_4", IssueNumber: 4, Question: "Four?", Status: "pending"}
		snapshot.PendingRequests["req_7"] = &state.Request{ID: "req_7", IssueNumber: 7, Question: "Seven?", Status: "pending"}
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
	_, err = store.Update("issue_blocked", 8, "run_8", nil, func(s *state.Snapshot) error {
		s.Issues["8"] = &state.Issue{
			Number: 8, Status: "blocked", RunID: "run_8", Branch: branch, Worktree: repo,
			SessionID: "session-8", Session: &state.WorkerSession{Backend: "codex", ID: "session-8"},
			DeclaredResources: []string{state.RepositoryResource}, ActualResources: []string{state.RepositoryResource},
			Answers:     []state.AnswerRecord{{RequestID: "req-8", Question: "Continue?", Answer: "yes", AnsweredAt: time.Now().UTC()}},
			FailureKind: "issue", LastError: "issue: worker blocked: localhost bind denied", UpdatedAt: time.Now().UTC(),
		}
		return nil
	})
	if err != nil {
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
		a := App{Out: &out, Err: &stderr, ProcessController: &appProcessGroups{alive: map[int]bool{}, signals: map[int][]syscall.Signal{}}}
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
	if item.BlockedCause == nil || item.BlockedCause.Origin != "worker" || item.BlockedCause.Kind != "environment" || item.Lease.ResolvedResources[0] != state.RepositoryResource {
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
	if !strings.Contains(string(calls), "--remove-label blocked") || strings.Contains(string(calls), "--remove-label do-not-automate") || !strings.Contains(string(calls), "codex-issue-loop:environment-resume:") {
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
	a := App{Out: &out, Err: &stderr, ProcessController: &appProcessGroups{alive: map[int]bool{}, signals: map[int][]syscall.Signal{}}}
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

func TestResumeBlockedFailsClosedWhenConfiguredBaseSHAIsUnavailable(t *testing.T) {
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
	// Only the worker branch exists remotely. In particular, origin/main is
	// unavailable and must not be replaced with HEAD as a publication base.
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
	blockedAt := time.Now().UTC()
	_, err = store.Update("issue_blocked", 9, "run_9", nil, func(snapshot *state.Snapshot) error {
		snapshot.Issues["9"] = &state.Issue{
			Number: 9, Status: "blocked", RunID: "run_9", Branch: branch, Worktree: cfg.RepoPath,
			SessionID: "session-9", FailureKind: "issue", LastError: "issue: worker blocked: legacy environment", UpdatedAt: blockedAt,
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	before, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	a := App{Out: &out, Err: &stderr, ProcessController: &appProcessGroups{alive: map[int]bool{}, signals: map[int][]syscall.Signal{}}}
	code := a.Run(context.Background(), []string{"resume-blocked", "--repo", cfg.RepoPath, "--issue", "9", "--confirm-prerequisite-resolved", "--json"})
	if code == 0 || !strings.Contains(stderr.String(), "resolve configured base branch") {
		t.Fatalf("missing configured base was not rejected: code=%d stdout=%s stderr=%s", code, out.String(), stderr.String())
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
	a := App{Out: &out, Err: &stderr, ProcessController: &appProcessGroups{alive: map[int]bool{}, signals: map[int][]syscall.Signal{}}}
	if code := a.Run(context.Background(), []string{"resume-blocked", "--repo", cfg.RepoPath, "--issue", "10", "--confirm-prerequisite-resolved", "--json"}); code != 0 {
		t.Fatalf("code=%d stdout=%s stderr=%s", code, out.String(), stderr.String())
	}
	snapshot, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	item := snapshot.Issues["10"]
	if item.Lease.BaseSHA != baseSHA || item.Lease.BaseSHA == newBaseSHA || item.LeaseGeneration != 7 || item.Lease.Owner != originalLease.Owner || item.Lease.Slot != originalLease.Slot || !reflect.DeepEqual(item.Lease.DeclaredResources, originalLease.DeclaredResources) || !reflect.DeepEqual(item.Lease.ResolvedResources, originalLease.ResolvedResources) || !item.Lease.ReservedAt.Equal(reservedAt) {
		t.Fatalf("existing lease metadata was overwritten: old=%+v new=%+v configured_base=%s", originalLease, item.Lease, newBaseSHA)
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
