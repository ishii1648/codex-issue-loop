package publish

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	gh "github.com/ishii1648/codex-issue-loop/internal/adapter/github"
	"github.com/ishii1648/codex-issue-loop/internal/domain/admission"
	"github.com/ishii1648/codex-issue-loop/internal/domain/publication"
	"github.com/ishii1648/codex-issue-loop/internal/platform/config"
)

func TestPublishCommitsPushesAndCreatesDraftPullRequestIdempotently(t *testing.T) {
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	repo := filepath.Join(root, "repo")
	runGit(t, root, "init", "--bare", remote)
	runGit(t, root, "init", repo)
	runGit(t, repo, "config", "user.name", "E2E Publisher")
	runGit(t, repo, "config", "user.email", "publisher@example.invalid")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", "README.md")
	runGit(t, repo, "commit", "-m", "base")
	runGit(t, repo, "branch", "-M", "main")
	runGit(t, repo, "remote", "add", "origin", remote)
	runGit(t, repo, "push", "-u", "origin", "main")
	baseSHA := runGit(t, repo, "rev-parse", "HEAD")
	runGit(t, repo, "config", "commit.gpgsign", "true")
	runGit(t, repo, "config", "gpg.format", "ssh")
	runGit(t, repo, "config", "user.signingkey", filepath.Join(root, "missing-signing-key"))
	branch := "codex/issue-1-publish"
	runGit(t, repo, "switch", "-c", branch)
	if err := os.MkdirAll(filepath.Join(repo, "results"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "results", "one.txt"), []byte("done\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	marker := filepath.Join(root, "pr-created")
	fakeGH := filepath.Join(root, "gh")
	script := `#!/bin/sh
case "$1 $2" in
  "pr list")
    if test -f "$PUBLISH_TEST_MARKER"; then
      head=$(git -C "$PUBLISH_TEST_REPO" rev-parse HEAD)
      printf '[{"url":"https://github.example/owner/repo/pull/1","state":"OPEN","mergedAt":null,"baseRefName":"main","baseRefOid":"%s","headRefName":"codex/issue-1-publish","headRefOid":"%s","headRepository":{"nameWithOwner":"owner/repo"}}]\n' "$PUBLISH_TEST_BASE" "$head"
    else
      printf '%s\n' '[]'
    fi
    ;;
  "pr create")
    : > "$PUBLISH_TEST_MARKER"
	printf '%s\n' 'Warning: 1 uncommitted change'
    printf '%s\n' 'https://github.example/owner/repo/pull/1'
    ;;
  *) exit 2 ;;
esac
`
	if err := os.WriteFile(fakeGH, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PUBLISH_TEST_MARKER", marker)
	t.Setenv("PUBLISH_TEST_REPO", repo)
	t.Setenv("PUBLISH_TEST_BASE", baseSHA)
	cfg := config.Defaults()
	cfg.GitHub.Repo = "owner/repo"
	manager := Manager{GitPath: "git", GHPath: fakeGH}
	issue := gh.Issue{Number: 1, Title: "Create marker"}

	first, audit, err := manager.Publish(context.Background(), cfg, issue, repo, branch, "", "implemented", baseSHA, []string{admission.RepositoryResource})
	if err != nil {
		t.Fatal(err)
	}
	if first.Branch != branch || first.Commit == "" || first.PullRequestURL != "https://github.example/owner/repo/pull/1" {
		t.Fatalf("unexpected first publish result: %+v", first)
	}
	if len(audit.ChangedPaths) != 1 || audit.ChangedPaths[0] != "results/one.txt" || len(audit.ActualResources) != 1 || audit.ActualResources[0] != admission.RepositoryResource {
		t.Fatalf("unexpected publication audit: %+v", audit)
	}
	if status := runGit(t, repo, "status", "--porcelain"); status != "" {
		t.Fatalf("published repository is dirty: %q", status)
	}
	if remoteCommit := runGit(t, repo, "rev-parse", "origin/"+branch); remoteCommit != first.Commit {
		t.Fatalf("remote commit=%s, want %s", remoteCommit, first.Commit)
	}

	second, _, err := manager.Publish(context.Background(), cfg, issue, repo, branch, first.PullRequestURL, "implemented", baseSHA, []string{admission.RepositoryResource})
	if err != nil {
		t.Fatal(err)
	}
	if second != first {
		t.Fatalf("idempotent publish result=%+v, want %+v", second, first)
	}
}

func TestPublishRefusesTrackedAndUntrackedResourcesOutsideClaimBeforeMutation(t *testing.T) {
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	repo := filepath.Join(root, "repo")
	runGit(t, root, "init", "--bare", remote)
	runGit(t, root, "init", repo)
	runGit(t, repo, "config", "user.name", "Audit Publisher")
	runGit(t, repo, "config", "user.email", "publisher@example.invalid")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", "README.md")
	runGit(t, repo, "commit", "-m", "base")
	runGit(t, repo, "branch", "-M", "main")
	runGit(t, repo, "remote", "add", "origin", remote)
	runGit(t, repo, "push", "-u", "origin", "main")
	baseSHA := runGit(t, repo, "rev-parse", "HEAD")
	branch := "codex/issue-2-audit"
	runGit(t, repo, "switch", "-c", branch)
	if err := os.MkdirAll(filepath.Join(repo, "docs"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "docs", "tracked.md"), []byte("tracked\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", "docs/tracked.md")
	runGit(t, repo, "commit", "-m", "worker committed docs")
	workerHead := runGit(t, repo, "rev-parse", "HEAD")
	if err := os.MkdirAll(filepath.Join(repo, "internal", "platform", "config"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "internal", "platform", "config", "untracked.go"), []byte("package config\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.GitHub.Repo = "owner/repo"
	cfg.Resources.Definitions = []config.ResourceDefinition{
		{Name: "config", Paths: []string{"internal/platform/config/**"}},
		{Name: "docs", Paths: []string{"docs/**"}},
	}
	manager := Manager{GitPath: "git", GHPath: filepath.Join(root, "gh-must-not-run")}
	_, audit, err := manager.Publish(context.Background(), cfg, gh.Issue{Number: 2, Title: "Audit"}, repo, branch, "", "implemented", baseSHA, []string{"config"})
	var mismatch publication.ClaimMismatchError
	if !errors.As(err, &mismatch) {
		t.Fatalf("claim mismatch not returned: audit=%+v err=%v", audit, err)
	}
	if strings.Join(audit.ChangedPaths, ",") != "docs/tracked.md,internal/platform/config/untracked.go" || strings.Join(audit.ActualResources, ",") != "config,docs" || audit.Reason != publication.ReasonResourceClaimMismatch {
		t.Fatalf("unexpected audit: %+v", audit)
	}
	if got := runGit(t, repo, "rev-parse", "HEAD"); got != workerHead {
		t.Fatalf("publisher changed HEAD before refusal: got=%s want=%s", got, workerHead)
	}
	if got := runGit(t, repo, "status", "--porcelain", "--untracked-files=all"); !strings.Contains(got, "?? internal/platform/config/untracked.go") {
		t.Fatalf("publisher staged or removed untracked work: %q", got)
	}
	if out, commandErr := exec.Command("git", "-C", repo, "rev-parse", "--verify", "origin/"+branch).CombinedOutput(); commandErr == nil {
		t.Fatalf("publisher pushed refused branch: %s", out)
	}
}

// Regression for the first quality-gate failures in Issues #65, #69, #92,
// and #93: workers may leave changed Go files syntactically valid but ungofmt'd.
func TestRegressionPublishFormatsWorkerGoFilesAndIsIdempotent(t *testing.T) {
	_, remote, repo, _ := setupPublishRepo(t)
	if err := os.WriteFile(filepath.Join(repo, "old.go"), []byte("package sample\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", "old.go")
	runGit(t, repo, "commit", "-m", "add rename source")
	runGit(t, repo, "push", "origin", "main")
	baseSHA := runGit(t, repo, "rev-parse", "HEAD")
	branch := "codex/issue-100-format"
	runGit(t, repo, "switch", "-c", branch)
	if err := os.MkdirAll(filepath.Join(repo, "with space"), 0o700); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "mv", "old.go", filepath.Join("with space", "renamed.go"))
	files := []string{"new.go", "line\nbreak.go", filepath.Join("with space", "renamed.go")}
	for _, name := range files {
		if err := os.WriteFile(filepath.Join(repo, name), []byte("package sample\n\nfunc value( )int{return 1}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfg := config.Defaults()
	cfg.GitHub.Repo = "owner/repo"
	cfg.Completion.CreateDraftPR = false
	cfg.Formatters.Go.Enabled = true
	manager := Manager{GitPath: "git", GofmtPath: filepath.Join(runtime.GOROOT(), "bin", "gofmt")}
	issue := gh.Issue{Number: 100, Title: "Format Go files"}

	first, audit, err := manager.Publish(context.Background(), cfg, issue, repo, branch, "", "formatted", baseSHA, []string{admission.RepositoryResource})
	if err != nil {
		t.Fatal(err)
	}
	if audit.Formatter.Result != "succeeded" || audit.Formatter.FileCount != 3 || !audit.Formatter.Changed {
		t.Fatalf("unexpected formatter audit: %+v", audit.Formatter)
	}
	for _, name := range files {
		data, readErr := os.ReadFile(filepath.Join(repo, name))
		if readErr != nil || strings.Contains(string(data), "value( )") || !strings.Contains(string(data), "func value() int") {
			t.Fatalf("file %q was not formatted: %s err=%v", name, data, readErr)
		}
	}
	second, secondAudit, err := manager.Publish(context.Background(), cfg, issue, repo, branch, "", "formatted", baseSHA, []string{admission.RepositoryResource})
	if err != nil {
		t.Fatal(err)
	}
	if second.Commit != first.Commit || secondAudit.Formatter.Changed {
		t.Fatalf("idempotent publish created a commit: first=%+v second=%+v audit=%+v", first, second, secondAudit.Formatter)
	}
	if remoteHead := runGit(t, remote, "rev-parse", branch); remoteHead != first.Commit {
		t.Fatalf("remote head=%s want=%s", remoteHead, first.Commit)
	}
}

func TestPublishRejectsGoSymlinkBeforeCommitOrPush(t *testing.T) {
	root, remote, repo, baseSHA := setupPublishRepo(t)
	branch := "codex/issue-100-symlink"
	runGit(t, repo, "switch", "-c", branch)
	outside := filepath.Join(root, "outside.go")
	original := []byte("package outside\nfunc value( )int{return 1}\n")
	if err := os.WriteFile(outside, original, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(repo, "linked.go")); err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.GitHub.Repo = "owner/repo"
	cfg.Completion.CreateDraftPR = false
	cfg.Formatters.Go.Enabled = true
	manager := Manager{GitPath: "git", GofmtPath: filepath.Join(runtime.GOROOT(), "bin", "gofmt")}
	_, audit, err := manager.Publish(context.Background(), cfg, gh.Issue{Number: 100}, repo, branch, "", "", baseSHA, []string{admission.RepositoryResource})
	var formatErr publication.FormatterError
	if !errors.As(err, &formatErr) || formatErr.Code != "path_unsafe" || audit.Reason != publication.ReasonFormatterFailed {
		t.Fatalf("unsafe symlink was not rejected: audit=%+v err=%v", audit, err)
	}
	if head := runGit(t, repo, "rev-parse", "HEAD"); head != baseSHA {
		t.Fatalf("HEAD changed after refusal: %s", head)
	}
	if data, readErr := os.ReadFile(outside); readErr != nil || !bytes.Equal(data, original) {
		t.Fatalf("publisher modified symlink target: data=%q err=%v", data, readErr)
	}
	if out, commandErr := exec.Command("git", "-C", remote, "rev-parse", "--verify", branch).CombinedOutput(); commandErr == nil {
		t.Fatalf("unsafe symlink branch was pushed: %s", out)
	}
}

func TestPublishFormatterFailureAndTimeoutDoNotCommitOrPush(t *testing.T) {
	// Leave enough headroom for process scheduling during cold parallel package
	// builds; the dedicated timeout case below still verifies the bounded path.
	for _, test := range []struct {
		name, script, code string
		timeout            time.Duration
		cancel             bool
	}{
		{name: "exit", script: "#!/bin/sh\nexit 42\n", code: "exit_failure", timeout: 5 * time.Second},
		{name: "timeout", script: "#!/bin/sh\nexec sleep 2\n", code: "timeout", timeout: 20 * time.Millisecond},
		{name: "canceled", script: "#!/bin/sh\nexec sleep 2\n", code: "canceled", timeout: 3 * time.Second, cancel: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			root, remote, repo, baseSHA := setupPublishRepo(t)
			branch := "codex/issue-100-" + test.name
			runGit(t, repo, "switch", "-c", branch)
			if err := os.WriteFile(filepath.Join(repo, "bad.go"), []byte("package bad\nfunc bad( ){}\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			formatter := filepath.Join(root, "gofmt")
			if err := os.WriteFile(formatter, []byte(test.script), 0o700); err != nil {
				t.Fatal(err)
			}
			cfg := config.Defaults()
			cfg.GitHub.Repo = "owner/repo"
			cfg.Completion.CreateDraftPR = false
			cfg.Formatters.Go.Enabled = true
			cfg.Formatters.Go.Timeout.Duration = test.timeout
			ctx := context.Background()
			var cancel context.CancelFunc
			if test.cancel {
				ctx, cancel = context.WithCancel(ctx)
				timer := time.AfterFunc(500*time.Millisecond, cancel)
				defer timer.Stop()
			}
			_, audit, err := (Manager{GitPath: "git", GofmtPath: formatter}).Publish(ctx, cfg, gh.Issue{Number: 100}, repo, branch, "", "", baseSHA, []string{admission.RepositoryResource})
			var formatErr publication.FormatterError
			if !errors.As(err, &formatErr) || formatErr.Code != test.code || audit.Formatter.FailureCode != test.code {
				t.Fatalf("formatter failure=%v audit=%+v", err, audit.Formatter)
			}
			if head := runGit(t, repo, "rev-parse", "HEAD"); head != baseSHA {
				t.Fatalf("formatter failure committed %s", head)
			}
			if out, commandErr := exec.Command("git", "-C", remote, "rev-parse", "--verify", branch).CombinedOutput(); commandErr == nil {
				t.Fatalf("formatter failure pushed branch: %s", out)
			}
		})
	}
}

func TestPublishFormatsCleanExistingPullRequestUsingAuthoritativeBase(t *testing.T) {
	root, remote, repo, baseSHA := setupPublishRepo(t)
	branch := "codex/issue-100-existing"
	runGit(t, repo, "switch", "-c", branch)
	if err := os.WriteFile(filepath.Join(repo, "existing.go"), []byte("package existing\nfunc value( )int{return 1}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", "existing.go")
	runGit(t, repo, "commit", "-m", "worker result")
	workerHead := runGit(t, repo, "rev-parse", "HEAD")
	runGit(t, repo, "push", "-u", "origin", branch)
	baseUpdate := filepath.Join(root, "base-update")
	runGit(t, root, "clone", "-b", "main", remote, baseUpdate)
	runGit(t, baseUpdate, "config", "user.name", "Base Updater")
	runGit(t, baseUpdate, "config", "user.email", "base@example.invalid")
	if err := os.WriteFile(filepath.Join(baseUpdate, "base-only.go"), []byte("package base\nfunc baseOnly( ){}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, baseUpdate, "add", "base-only.go")
	runGit(t, baseUpdate, "commit", "-m", "advance authoritative base")
	runGit(t, baseUpdate, "push", "origin", "main")
	authoritativeBase := runGit(t, baseUpdate, "rev-parse", "HEAD")
	fakeGH := filepath.Join(root, "gh")
	script := `#!/bin/sh
case "$*" in
  *headRepositoryOwner*) ;;
  *) exit 3 ;;
esac
printf '[{"url":"https://github.example/owner/repo/pull/100","state":"OPEN","mergedAt":null,"baseRefName":"main","baseRefOid":"%s","headRefName":"codex/issue-100-existing","headRefOid":"%s","headRepository":{"id":"R_kgDOTdsezg","name":"repo"},"headRepositoryOwner":{"login":"owner"}}]\n' "$PUBLISH_TEST_BASE" "$PUBLISH_TEST_HEAD"
`
	if err := os.WriteFile(fakeGH, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PUBLISH_TEST_BASE", authoritativeBase)
	t.Setenv("PUBLISH_TEST_HEAD", workerHead)
	cfg := config.Defaults()
	cfg.GitHub.Repo = "owner/repo"
	cfg.Formatters.Go.Enabled = true
	manager := Manager{GitPath: "git", GHPath: fakeGH, GofmtPath: filepath.Join(runtime.GOROOT(), "bin", "gofmt")}
	result, audit, err := manager.Publish(context.Background(), cfg, gh.Issue{Number: 100, Title: "Existing PR"}, repo, branch, "https://github.example/owner/repo/pull/100", "formatted", baseSHA, []string{admission.RepositoryResource})
	if err != nil {
		t.Fatal(err)
	}
	if result.Commit == workerHead || result.PullRequestURL == "" || audit.BaseSHA != authoritativeBase || !audit.Formatter.Changed || audit.Formatter.FileCount != 1 {
		t.Fatalf("existing PR was not updated: result=%+v audit=%+v", result, audit)
	}
	if remoteHead := runGit(t, remote, "rev-parse", branch); remoteHead != result.Commit {
		t.Fatalf("follow-up commit was not pushed: %s", remoteHead)
	}
}

func TestPublishRefusesUnsafeExistingPullRequestStateBeforeFormatting(t *testing.T) {
	for _, test := range []struct {
		name       string
		build      func(base, head, branch string) string
		wantDetail string
	}{
		{name: "closed-without-merge", build: func(base, head, branch string) string {
			return fmt.Sprintf(`[{"url":"https://github.example/owner/repo/pull/100","state":"CLOSED","mergedAt":null,"baseRefName":"main","baseRefOid":"%s","headRefName":"%s","headRefOid":"%s","headRepository":{"nameWithOwner":"owner/repo"}}]`, base, branch, head)
		}},
		{name: "multiple", build: func(base, head, branch string) string {
			one := fmt.Sprintf(`{"url":"https://github.example/owner/repo/pull/100","state":"OPEN","mergedAt":null,"baseRefName":"main","baseRefOid":"%s","headRefName":"%s","headRefOid":"%s","headRepository":{"nameWithOwner":"owner/repo"}}`, base, branch, head)
			return "[" + one + "," + strings.Replace(one, "/100\"", "/101\"", 1) + "]"
		}},
		{name: "wrong-branch", build: func(base, head, _ string) string {
			return fmt.Sprintf(`[{"url":"https://github.example/owner/repo/pull/100","state":"OPEN","mergedAt":null,"baseRefName":"main","baseRefOid":"%s","headRefName":"other-branch","headRefOid":"%s","headRepository":{"nameWithOwner":"owner/repo"}}]`, base, head)
		}},
		{name: "wrong-base", build: func(base, head, branch string) string {
			return fmt.Sprintf(`[{"url":"https://github.example/owner/repo/pull/100","state":"OPEN","mergedAt":null,"baseRefName":"release","baseRefOid":"%s","headRefName":"%s","headRefOid":"%s","headRepository":{"name":"repo"},"headRepositoryOwner":{"login":"owner"}}]`, base, branch, head)
		}},
		{name: "fork", wantDetail: "repository=attacker/repo", build: func(base, head, branch string) string {
			return fmt.Sprintf(`[{"url":"https://github.example/owner/repo/pull/100","state":"OPEN","mergedAt":null,"baseRefName":"main","baseRefOid":"%s","headRefName":"%s","headRefOid":"%s","headRepository":{"name":"repo"},"headRepositoryOwner":{"login":"attacker"}}]`, base, branch, head)
		}},
		{name: "different-repository", wantDetail: "repository=owner/other", build: func(base, head, branch string) string {
			return fmt.Sprintf(`[{"url":"https://github.example/owner/repo/pull/100","state":"OPEN","mergedAt":null,"baseRefName":"main","baseRefOid":"%s","headRefName":"%s","headRefOid":"%s","headRepository":{"name":"other"},"headRepositoryOwner":{"login":"owner"}}]`, base, branch, head)
		}},
		{name: "missing-owner", wantDetail: "repository= head=", build: func(base, head, branch string) string {
			return fmt.Sprintf(`[{"url":"https://github.example/owner/repo/pull/100","state":"OPEN","mergedAt":null,"baseRefName":"main","baseRefOid":"%s","headRefName":"%s","headRefOid":"%s","headRepository":{"name":"repo"}}]`, base, branch, head)
		}},
		{name: "missing-repository", wantDetail: "repository= head=", build: func(base, head, branch string) string {
			return fmt.Sprintf(`[{"url":"https://github.example/owner/repo/pull/100","state":"OPEN","mergedAt":null,"baseRefName":"main","baseRefOid":"%s","headRefName":"%s","headRefOid":"%s","headRepositoryOwner":{"login":"owner"}}]`, base, branch, head)
		}},
		{name: "missing-base-sha", wantDetail: "missing authoritative base or head SHA", build: func(_, head, branch string) string {
			return fmt.Sprintf(`[{"url":"https://github.example/owner/repo/pull/100","state":"OPEN","mergedAt":null,"baseRefName":"main","baseRefOid":"","headRefName":"%s","headRefOid":"%s","headRepository":{"name":"repo"},"headRepositoryOwner":{"login":"owner"}}]`, branch, head)
		}},
		{name: "missing-head-sha", wantDetail: "missing authoritative base or head SHA", build: func(base, _, branch string) string {
			return fmt.Sprintf(`[{"url":"https://github.example/owner/repo/pull/100","state":"OPEN","mergedAt":null,"baseRefName":"main","baseRefOid":"%s","headRefName":"%s","headRefOid":"","headRepository":{"name":"repo"},"headRepositoryOwner":{"login":"owner"}}]`, base, branch)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			root, _, repo, baseSHA := setupPublishRepo(t)
			branch := "codex/issue-100-pr-safety"
			runGit(t, repo, "switch", "-c", branch)
			original := []byte("package unsafe\nfunc value( )int{return 1}\n")
			if err := os.WriteFile(filepath.Join(repo, "unsafe.go"), original, 0o600); err != nil {
				t.Fatal(err)
			}
			runGit(t, repo, "add", "unsafe.go")
			runGit(t, repo, "commit", "-m", "unformatted worker result")
			head := runGit(t, repo, "rev-parse", "HEAD")
			runGit(t, repo, "push", "-u", "origin", branch)
			payload := test.build(baseSHA, head, branch)
			fakeGH := filepath.Join(root, "gh")
			script := "#!/bin/sh\nprintf '%s\\n' '" + payload + "'\n"
			if err := os.WriteFile(fakeGH, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			cfg := config.Defaults()
			cfg.GitHub.Repo = "owner/repo"
			cfg.Formatters.Go.Enabled = true
			manager := Manager{GitPath: "git", GHPath: fakeGH, GofmtPath: filepath.Join(runtime.GOROOT(), "bin", "gofmt")}
			_, audit, err := manager.Publish(context.Background(), cfg, gh.Issue{Number: 100}, repo, branch, "https://github.example/owner/repo/pull/100", "", baseSHA, []string{admission.RepositoryResource})
			var mismatch publication.PullRequestMismatchError
			if !errors.As(err, &mismatch) || audit.Reason != publication.ReasonPullRequestMismatch {
				t.Fatalf("unsafe PR was not refused: audit=%+v err=%v", audit, err)
			}
			if test.wantDetail != "" && !strings.Contains(mismatch.Detail, test.wantDetail) {
				t.Fatalf("mismatch detail=%q, want substring %q", mismatch.Detail, test.wantDetail)
			}
			data, readErr := os.ReadFile(filepath.Join(repo, "unsafe.go"))
			if readErr != nil || !bytes.Equal(data, original) || runGit(t, repo, "rev-parse", "HEAD") != head {
				t.Fatalf("publisher mutated refused PR: data=%q err=%v", data, readErr)
			}
		})
	}
}

func TestPublishFormatsConcurrentWorktreesWithoutCrossing(t *testing.T) {
	type fixture struct {
		repo, base, branch, want string
	}
	fixtures := make([]fixture, 0, 2)
	for index := 1; index <= 2; index++ {
		_, _, repo, base := setupPublishRepo(t)
		branch := fmt.Sprintf("codex/issue-10%d-concurrent", index)
		runGit(t, repo, "switch", "-c", branch)
		body := fmt.Sprintf("package concurrent\nfunc value( )int{return %d}\n", index)
		if err := os.WriteFile(filepath.Join(repo, "value.go"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		fixtures = append(fixtures, fixture{repo: repo, base: base, branch: branch, want: fmt.Sprintf("return %d", index)})
	}
	cfg := config.Defaults()
	cfg.GitHub.Repo = "owner/repo"
	cfg.Completion.CreateDraftPR = false
	cfg.Formatters.Go.Enabled = true
	manager := Manager{GitPath: "git", GofmtPath: filepath.Join(runtime.GOROOT(), "bin", "gofmt")}
	errorsByWorktree := make(chan error, len(fixtures))
	for index := range fixtures {
		item := fixtures[index]
		go func() {
			_, _, err := manager.Publish(context.Background(), cfg, gh.Issue{Number: 101}, item.repo, item.branch, "", "", item.base, []string{admission.RepositoryResource})
			errorsByWorktree <- err
		}()
	}
	for range fixtures {
		if err := <-errorsByWorktree; err != nil {
			t.Fatal(err)
		}
	}
	for _, item := range fixtures {
		data, err := os.ReadFile(filepath.Join(item.repo, "value.go"))
		if err != nil || !strings.Contains(string(data), item.want) || strings.Contains(string(data), "value( )") {
			t.Fatalf("crossed formatter output for %s: %q err=%v", item.repo, data, err)
		}
	}
}

func TestSafeRegularFileRejectsTraversal(t *testing.T) {
	root := t.TempDir()
	for _, path := range []string{"../outside.go", "/absolute.go", "dir/../file.go", "dir//file.go"} {
		if _, _, err := safeRegularFile(root, path); err == nil {
			t.Fatalf("unsafe path accepted: %q", path)
		}
	}
	outside := filepath.Join(t.TempDir(), "outside.go")
	if err := os.WriteFile(outside, []byte("package outside\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(outside, filepath.Join(root, "hardlink.go")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := safeRegularFile(root, "hardlink.go"); err == nil || !strings.Contains(err.Error(), "hard links") {
		t.Fatalf("external hard link accepted: %v", err)
	}
}

func TestParsePorcelainV1ZIsNULSafeForRenameAndWhitespace(t *testing.T) {
	paths, err := parsePorcelainV1Z(" M with space.go\x00?? line\nbreak.go\x00R  renamed.go\x00old.go\x00")
	if err != nil || strings.Join(paths, "|") != "with space.go|line\nbreak.go|renamed.go" {
		t.Fatalf("paths=%q err=%v", paths, err)
	}
	if _, err := parsePorcelainV1Z("R  missing-source.go\x00"); err == nil {
		t.Fatal("malformed rename status was accepted")
	}
}

func TestExtractPullRequestURLRejectsAmbiguousOutput(t *testing.T) {
	_, err := extractPullRequestURL("https://github.example/owner/repo/pull/1\nhttps://github.example/owner/repo/pull/2\n")
	if err == nil || !strings.Contains(err.Error(), "multiple Pull Request URLs") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	if len(args) > 0 && args[0] == "commit" {
		args = append([]string{"-c", "commit.gpgsign=false"}, args...)
	}
	command := exec.Command("git", args...)
	command.Dir = dir
	out, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func setupPublishRepo(t *testing.T) (root, remote, repo, baseSHA string) {
	t.Helper()
	root = t.TempDir()
	remote = filepath.Join(root, "remote.git")
	repo = filepath.Join(root, "repo")
	runGit(t, root, "init", "--bare", remote)
	runGit(t, root, "init", repo)
	runGit(t, repo, "config", "user.name", "Formatter Publisher")
	runGit(t, repo, "config", "user.email", "publisher@example.invalid")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", "README.md")
	runGit(t, repo, "commit", "-m", "base")
	runGit(t, repo, "branch", "-M", "main")
	runGit(t, repo, "remote", "add", "origin", remote)
	runGit(t, repo, "push", "-u", "origin", "main")
	baseSHA = runGit(t, repo, "rev-parse", "HEAD")
	return root, remote, repo, baseSHA
}
