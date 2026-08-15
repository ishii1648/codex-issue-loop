package registry

import (
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
