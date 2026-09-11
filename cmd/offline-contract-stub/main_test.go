package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
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

func TestGHRESTIssueComments(t *testing.T) {
	t.Setenv("OFFLINE_CONTRACT_STATE", t.TempDir())
	t.Setenv("OFFLINE_CONTRACT_REMOTE", "unused")
	if err := runHarness([]string{"seed"}); err != nil {
		t.Fatal(err)
	}
	saved := make([]string, 201)
	for i := range saved {
		saved[i] = fmt.Sprintf("comment %d\n<!-- codex-issue-loop:marker-%d -->", i, i)
		if err := runGH([]string{"issue", "comment", "1", "--body", saved[i]}); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name, number, query string
		want                []string
		wantError           bool
	}{
		{"first", "1", "per_page=100&page=1", saved[:100], false},
		{"second", "1", "per_page=100&page=2", saved[100:200], false},
		{"last", "1", "per_page=100&page=3", saved[200:], false},
		{"past end", "1", "per_page=100&page=4", nil, false},
		{"empty issue", "2", "per_page=100&page=1", nil, false},
		{"custom size", "1", "page=2&per_page=2", saved[2:4], false},
		{"exact boundary", "1", "per_page=3&page=68", nil, false},
		{"missing issue", "3", "per_page=100&page=1", nil, true},
		{"zero page", "1", "per_page=100&page=0", nil, true},
		{"invalid size", "1", "per_page=0&page=1", nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			output, err := os.CreateTemp(t.TempDir(), "stdout")
			if err != nil {
				t.Fatal(err)
			}
			defer output.Close()
			original := os.Stdout
			os.Stdout = output
			err = runGH([]string{"api", "--method", "GET", "-H", "Accept: application/vnd.github+json", "/repos/offline/repository/issues/" + tc.number + "/comments?" + tc.query})
			os.Stdout = original
			if (err != nil) != tc.wantError {
				t.Fatalf("error = %v", err)
			}
			if tc.wantError {
				return
			}
			data, err := os.ReadFile(output.Name())
			if err != nil {
				t.Fatal(err)
			}
			var got []map[string]string
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, comments(tc.want)) {
				t.Fatalf("comments = %s, want %v", data, tc.want)
			}
		})
	}
}
