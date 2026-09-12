package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestVersionGate(t *testing.T) {
	for _, tc := range []struct {
		name            string
		base, head, max int
		changed, pass   bool
	}{
		{"unchanged", 6, 6, 6, false, true},
		{"contract without bump", 6, 6, 6, true, false},
		{"fresh version", 6, 7, 6, true, true},
		{"regression", 6, 5, 6, false, false},
		{"reuse", 6, 7, 7, true, false},
		{"older publication order", 6, 6, 7, false, false},
		{"legacy to unified", 5, 6, 5, true, true},
		{"revert unified contract", 5, 5, 5, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := checkVersion(tc.base, tc.head, tc.max, tc.changed); (err == nil) != tc.pass {
				t.Fatalf("%v", err)
			}
		})
	}
	for _, tc := range []struct {
		source string
		want   int
	}{
		{"const (\n CurrentVersion = 6\n CurrentSchemaVersion = CurrentVersion\n)", 6},
		{"const (\n CurrentVersion = 4\n CurrentSchemaVersion = 5\n)", 5},
		{"", 0}, {"CurrentSchemaVersion = unknown", 0},
		{"CurrentVersion = 6\nCurrentSchemaVersion = 7", 0},
	} {
		got, err := version([]byte(tc.source))
		if got != tc.want || (err == nil) != (tc.want > 0) {
			t.Fatalf("version=%d err=%v", got, err)
		}
	}
}

func TestReleaseSelection(t *testing.T) {
	now := time.Date(2026, 9, 13, 0, 0, 0, 0, time.UTC)
	valid := release{Tag: "v0.12.35", Published: now}
	later := release{Tag: "v0.12.36", Published: now.Add(time.Hour)}
	for _, tc := range []struct {
		name  string
		pages [][]release
		want  string
	}{
		{"pagination", [][]release{{valid}, {later}}, later.Tag},
		{"latest publication not highest tag", [][]release{{{Tag: "v0.13.0", Published: now.Add(-time.Hour)}, valid}}, valid.Tag},
		{"exclude candidates and drafts", [][]release{{valid, {Tag: "v0.12.36", Published: now, Prerelease: true}, {Tag: "v0.12.37", Published: now, Draft: true}, {Tag: "candidate-v0.12.38", Published: now}}}, valid.Tag},
		{"missing", nil, ""},
		{"before floor", [][]release{{{Tag: "v0.12.34", Published: now}}}, ""},
		{"missing publication", [][]release{{{Tag: valid.Tag}}}, ""},
		{"duplicate", [][]release{{valid, valid}}, ""},
		{"tied publication", [][]release{{valid, {Tag: later.Tag, Published: now}}}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data, _ := json.Marshal(tc.pages)
			got, err := releases(data)
			if tc.want == "" {
				if err == nil {
					t.Fatal("accepted unknown baseline")
				}
				return
			}
			if err != nil || got[0].Tag != tc.want {
				t.Fatalf("got=%v err=%v", got, err)
			}
		})
	}
	if _, err := releases([]byte("invalid")); err == nil {
		t.Fatal("accepted malformed API response")
	}
}

func TestContractTreeDetectsWholeHistoryAndPaths(t *testing.T) {
	dir := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		out, err := command(dir, "git", args...)
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q")
	git("config", "user.email", "fixture@example.invalid")
	git("config", "user.name", "Fixture")
	path := filepath.Join(dir, contractPaths[0])
	if err := os.MkdirAll(path, 0700); err != nil {
		t.Fatal(err)
	}
	write := func(name, data string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(path, name), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	commit := func() string {
		t.Helper()
		git("add", ".")
		git("-c", "commit.gpgsign=false", "commit", "-qm", "fixture")
		return git("rev-parse", "HEAD")
	}
	write("contract.go", "original")
	base := commit()
	original, err := contractTree(dir, base)
	if err != nil {
		t.Fatal(err)
	}
	write("contract.go", "changed")
	commit()
	write("contract_test.go", "test only")
	head := commit()
	current, err := contractTree(dir, head)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(original, current) {
		t.Fatal("missed earlier contract commit")
	}
	write("contract_test.go", "more tests")
	if err := os.WriteFile(filepath.Join(dir, "implementation.go"), []byte("implementation only"), 0600); err != nil {
		t.Fatal(err)
	}
	testsOnly := commit()
	next, err := contractTree(dir, testsOnly)
	if err != nil || !bytes.Equal(current, next) {
		t.Fatal("tests changed contract", err)
	}
	if err := os.Rename(filepath.Join(path, "contract.go"), filepath.Join(path, "moved.go")); err != nil {
		t.Fatal(err)
	}
	moved := commit()
	next, err = contractTree(dir, moved)
	if err != nil || bytes.Equal(current, next) {
		t.Fatal("missed move", err)
	}
	if err := os.Remove(filepath.Join(path, "moved.go")); err != nil {
		t.Fatal(err)
	}
	deleted := commit()
	if _, err := contractTree(dir, deleted); err == nil {
		t.Fatal("accepted missing contract")
	}
	if _, err := contractTree(dir, "missing-ref"); err == nil {
		t.Fatal("accepted missing ref")
	}
}

func TestCompatibilityWithCurrentWritersAndHistoricalAnswerRegression(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	oldDir, newDir := filepath.Join(tmp, "old"), filepath.Join(tmp, "new")
	for _, dir := range []string{oldDir, newDir} {
		if err := archive(root, "HEAD", dir); err != nil {
			t.Fatal(err)
		}
	}
	if err := compatibility(root, oldDir, newDir, tmp, true); err != nil {
		t.Fatal(err)
	}
	sourcePath := filepath.Join(oldDir, "internal/domain/snapshot/snapshot.go")
	source, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	signature := "func (snapshot Snapshot) Validate() error {"
	regression := signature + `
 for _,request := range snapshot.PendingRequests {
  if request.Status == "answered" {
   item := snapshot.Issues[strconv.Itoa(request.IssueNumber)]
   if item != nil && item.Continuation != nil && request.CheckpointID != item.Continuation.ID {
    return fmt.Errorf("answered checkpoint mismatch")
   }
  }
 }
`
	if !bytes.Contains(source, []byte(signature)) {
		t.Fatal("validator signature missing")
	}
	if err := os.WriteFile(sourcePath, bytes.Replace(source, []byte(signature), []byte(regression), 1), 0600); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(tmp, "regressed-reader")
	if _, err := command(oldDir, "go", "build", "-o", binary, "./snapshot-contract-probe"); err != nil {
		t.Fatal(err)
	}
	// The historical #439 rule rejects an answer retained across a newer checkpoint.
	fixtureDir := filepath.Join(tmp, "regression-fixture")
	if _, err := command("", filepath.Join(tmp, "new-probe"), "write", fixtureDir, filepath.Join(root, "internal/adapter/state/testdata/issue-439-answered-next-checkpoint.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := readFixture(binary, fixtureDir, true, false); err == nil {
		t.Fatal("same-version acceptance difference escaped the gate")
	}
}

func TestGateProcessFailsClosed(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	binary := filepath.Join(tmp, "gate")
	if _, err := command(root, "go", "build", "-o", binary, "./scripts/snapshot-contract"); err != nil {
		t.Fatal(err)
	}
	fake := filepath.Join(tmp, "gh")
	t.Setenv("PATH", tmp+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, scenario := range []struct{ name, body string }{
		{"API unavailable", "exit 1"},
		{"malformed", "printf invalid"},
		{"missing release", "printf '[]'"},
		{"missing tag", `case "$*" in
   *releases*) printf '[[{"tag_name":"v999.0.0","published_at":"2026-09-13T00:00:00Z"}]]' ;;
   *) printf '{"ref":"refs/tags/v999.0.0","object":{"sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}' ;;
  esac`},
		{"tag mismatch", `case "$*" in
   *releases*) printf '[[{"tag_name":"v0.12.35","published_at":"2026-09-13T00:00:00Z"}]]' ;;
   *) printf '{"ref":"refs/tags/v0.12.35","object":{"sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}' ;;
  esac`},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			if err := os.WriteFile(fake, []byte("#!/bin/sh\n"+scenario.body+"\n"), 0700); err != nil {
				t.Fatal(err)
			}
			out, err := command(root, binary)
			if err == nil || !strings.Contains(err.Error(), "snapshot-contract: FAIL") || strings.Contains(string(out), "PASS") {
				t.Fatalf("exit/output mismatch: %s %v", out, err)
			}
		})
	}
}
