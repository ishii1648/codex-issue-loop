package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestSquashMergePreservesChangesFromPreviouslyMergedPullRequest(t *testing.T) {
	root := t.TempDir()
	repository := filepath.Join(root, "repository")
	remote := filepath.Join(root, "origin.git")
	runGit(t, root, "init", "--bare", remote)
	runGit(t, root, "init", repository)
	runGit(t, repository, "config", "user.name", "offline-contract")
	runGit(t, repository, "config", "user.email", "offline-contract@example.invalid")
	runGit(t, repository, "config", "commit.gpgsign", "false")
	runGit(t, repository, "remote", "add", "origin", remote)
	writeTestFile(t, filepath.Join(repository, "README.md"), "base\n")
	runGit(t, repository, "add", "README.md")
	runGit(t, repository, "commit", "-m", "base")
	runGit(t, repository, "branch", "-M", "main")
	runGit(t, repository, "push", "-u", "origin", "main")
	base := runGit(t, repository, "rev-parse", "HEAD")

	runGit(t, repository, "switch", "-c", "first")
	writeTestFile(t, filepath.Join(repository, "first.txt"), "first\n")
	runGit(t, repository, "add", "first.txt")
	runGit(t, repository, "commit", "-m", "first")
	first := runGit(t, repository, "rev-parse", "HEAD")
	runGit(t, repository, "push", "origin", "first")
	if _, err := squashMerge(remote, "main", first, 1); err != nil {
		t.Fatal(err)
	}

	runGit(t, repository, "switch", "--detach", base)
	runGit(t, repository, "switch", "-c", "second")
	writeTestFile(t, filepath.Join(repository, "second.txt"), "second\n")
	runGit(t, repository, "add", "second.txt")
	runGit(t, repository, "commit", "-m", "second")
	second := runGit(t, repository, "rev-parse", "HEAD")
	runGit(t, repository, "push", "origin", "second")
	if _, err := squashMerge(remote, "main", second, 2); err != nil {
		t.Fatal(err)
	}

	files := strings.Fields(runGit(t, root, "--git-dir", remote, "ls-tree", "-r", "--name-only", "main"))
	if strings.Join(files, ",") != "README.md,first.txt,second.txt" {
		t.Fatalf("merged files=%v", files)
	}
}

func writeTestFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

func runGit(t *testing.T, directory string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = directory
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, output)
	}
	return strings.TrimSpace(string(output))
}
