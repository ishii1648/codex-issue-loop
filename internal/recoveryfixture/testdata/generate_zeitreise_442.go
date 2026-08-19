//go:build ignore

// This one-shot migration program records exactly how the pre-v1 zeitreise
// fixtures were combined. Run it from the repository root; review and commit
// the generated file only after the recovery-fixture runbook checks pass.
package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"time"

	gh "github.com/ishii1648/codex-issue-loop/internal/github"
	"github.com/ishii1648/codex-issue-loop/internal/recoveryfixture"
	"github.com/ishii1648/codex-issue-loop/internal/state"
	"github.com/ishii1648/codex-issue-loop/internal/worktree"
)

func main() {
	issue, err := os.ReadFile("internal/state/testdata/zeitreise-442-v0614-full-27-state.json")
	must(err)
	file, err := os.Open("internal/state/testdata/zeitreise-442-v0614-full-27-events.jsonl")
	must(err)
	defer file.Close()
	var events []json.RawMessage
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		events = append(events, append(json.RawMessage(nil), scanner.Bytes()...))
	}
	must(scanner.Err())
	var typed state.Issue
	must(json.Unmarshal(issue, &typed))
	originalReason := "worker blocked: " + typed.EnvironmentResume.PreviousReason
	workspaceReason := typed.BlockedCause.Reason
	resumeMarker := "<!-- codex-issue-loop:environment-resume:" + typed.EnvironmentResume.ID + " -->"
	bundle, err := recoveryfixture.Build(recoveryfixture.Input{
		SourceSchemaVersion: 4, SourceVersion: "v0.6.22", CapturedAt: time.Date(2026, 8, 18, 6, 3, 0, 0, time.UTC),
		Repository: "ishii1648/zeitreise", IssueNumber: 442, RepoID: "repo_zeitreise", RepoPath: "/sanitized/zeitreise",
		StateRevision: 3791, Issue: issue, Events: events,
		Worktree: worktree.Inspection{Exists: true, Valid: true, Branch: typed.Branch, Head: "3333333333333333333333333333333333333333", Dirty: true, UnpushedCommits: false, LocalBranchExists: true, RemoteBranchExists: false, RemoteConsistent: false},
		Remote: gh.RemoteState{Issue: gh.Issue{Number: 442, Title: typed.Title, State: "OPEN", Labels: []string{"blocked"}, Comments: []string{
			resumeMarker, resumeMarker, failureComment(442, originalReason), failureComment(442, workspaceReason),
		}}},
	})
	must(err)
	output, err := json.MarshalIndent(bundle, "", "  ")
	must(err)
	must(os.WriteFile("internal/recoveryfixture/testdata/zeitreise-442-full-history-v1.json", append(output, '\n'), 0o600))
}

func failureComment(number int, reason string) string {
	digest := sha256.Sum256([]byte(reason))
	return fmt.Sprintf("<!-- codex-issue-loop:failed:%d -->\n<!-- codex-issue-loop:failure:%s -->\nAutomation stopped: %s", number, hex.EncodeToString(digest[:8]), reason)
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}
