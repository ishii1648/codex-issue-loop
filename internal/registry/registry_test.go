package registry

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ishii1648/codex-issue-loop/internal/config"
)

func TestFaultRegistryAddResolveRemoveAndAmbiguity(t *testing.T) {
	root := t.TempDir()
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"git", "gh", "codex", "launchctl"} {
		if err := os.WriteFile(filepath.Join(binDir, name), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", binDir)
	store := Store{Path: filepath.Join(root, "registry.json")}
	firstRepo := filepath.Join(root, "first")
	secondRepo := filepath.Join(root, "second")
	for _, repo := range []string{firstRepo, secondRepo} {
		if err := os.MkdirAll(repo, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	first := testRegistryConfig(t, firstRepo, "owner/first")
	entry, err := store.Add(first)
	if err != nil {
		t.Fatal(err)
	}
	for name, command := range entry.Commands {
		if !filepath.IsAbs(command) || !strings.HasPrefix(command, binDir+string(filepath.Separator)) {
			t.Fatalf("command %s=%q", name, command)
		}
	}
	resolved, err := store.Resolve(firstRepo, "")
	if err != nil || resolved.RepoID != entry.RepoID {
		t.Fatalf("resolved=%+v err=%v", resolved, err)
	}
	readded, err := store.Add(first)
	if err != nil || !readded.RegisteredAt.Equal(entry.RegisteredAt) {
		t.Fatalf("readded=%+v err=%v", readded, err)
	}
	if _, err := store.Add(testRegistryConfig(t, secondRepo, "owner/second")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Resolve("", ""); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("expected ambiguity, got %v", err)
	}
	if err := store.Remove(entry.RepoID); err != nil {
		t.Fatal(err)
	}
	registry, err := store.Load()
	if err != nil || len(registry.Repos) != 1 {
		t.Fatalf("registry=%+v err=%v", registry, err)
	}
}

func TestRegistryRejectsSymbolicLink(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target.json")
	if err := os.WriteFile(target, []byte(`{"version":4,"repos":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "registry.json")
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if _, err := (Store{Path: path}).Load(); err == nil {
		t.Fatal("symbolic-link registry was accepted")
	}
}

func TestRegistryResolvesSelectedBackendCommand(t *testing.T) {
	root := t.TempDir()
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"git", "gh", "opencode", "launchctl"} {
		body := "#!/bin/sh\nexit 0\n"
		if name == "opencode" {
			body = "#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then echo 'opencode 1.1.1'; fi\n"
		}
		if err := os.WriteFile(filepath.Join(binDir, name), []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", binDir)
	repo := filepath.Join(root, "repo")
	if err := os.Mkdir(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := testRegistryConfig(t, repo, "owner/repo")
	cfg.Worker.Backend, cfg.Worker.Model = "opencode", "opencode-go/test"
	entry, err := (Store{Path: filepath.Join(root, "registry.json")}).Add(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if entry.WorkerBackend != "opencode" || entry.Commands["opencode"] != filepath.Join(binDir, "opencode") || entry.Commands["codex"] != "" {
		t.Fatalf("entry=%+v", entry)
	}
}

func TestRegistryRejectsLegacyAndFutureSchemaWithActionableErrors(t *testing.T) {
	for _, test := range []struct {
		version int
		want    string
	}{
		{version: 3, want: "migration required"},
		{version: CurrentVersion + 1, want: "unsupported registry version"},
	} {
		path := filepath.Join(t.TempDir(), "registry.json")
		if err := os.WriteFile(path, []byte(fmt.Sprintf("{\"version\":%d,\"repos\":{}}\n", test.version)), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := (Store{Path: path}).Load(); err == nil || !strings.Contains(err.Error(), test.want) {
			t.Fatalf("version %d: %v", test.version, err)
		}
	}
}

func testRegistryConfig(t *testing.T, repoPath, githubRepo string) config.Config {
	t.Helper()
	canonical, err := config.CanonicalRepoPath(repoPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.RepoPath = canonical
	cfg.GitHub.Repo = githubRepo
	return cfg
}
