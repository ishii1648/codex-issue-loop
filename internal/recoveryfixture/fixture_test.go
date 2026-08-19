package recoveryfixture

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gh "github.com/ishii1648/codex-issue-loop/internal/github"
	"github.com/ishii1648/codex-issue-loop/internal/worktree"
)

func fixtureInput() Input {
	return Input{
		SourceSchemaVersion: 4,
		SourceVersion:       "v0.6.22",
		CapturedAt:          time.Date(2026, 8, 19, 1, 2, 3, 4, time.UTC),
		Repository:          "owner/repository",
		IssueNumber:         442,
		RepoID:              "repo_private",
		RepoPath:            "/Users/alice/private/repository",
		StateRevision:       3791,
		Issue: json.RawMessage(`{
          "number":442,"title":"private customer title","status":"blocked",
          "run_id":"run_original_442","lease_generation":2,
          "lease":{"owner":{"run_id":"run_original_442","generation":2},"slot":0,"declared_resources":["repo:*"],"resolved_resources":["repo:*"],"reserved_at":"2026-08-18T06:02:12.482456Z"},
          "branch":"codex/issue-442-private","worktree":"/Users/alice/private/worktrees/issue-442",
          "attempts":3,"continuations":0,"session_id":null,"session":null,
          "last_error":"worker workspace validation failed for /Users/alice/private/worktrees/issue-442: saved workspace provenance is missing",
          "blocked_cause":{"origin":"worker","kind":"environment","resumable":true,"reason":"worker workspace validation failed for /Users/alice/private/worktrees/issue-442: saved workspace provenance is missing","blocked_at":"2026-08-18T06:02:12Z"},
          "environment_resume":{"id":"resume_original_442","status":"running","confirmed_at":"2026-08-18T06:02:12.482456Z","previous_reason":"token ghp_abcdefghijklmnopqrstuvwxyz123456","base_sha":"1111111111111111111111111111111111111111","current_base_sha":"2222222222222222222222222222222222222222"},
          "updated_at":"2026-08-18T06:02:13Z"
        }`),
		PendingRequests: []json.RawMessage{json.RawMessage(`{"id":"req_original_442","issue_number":442,"question":"private question","allow_free_text":true,"run_id":"run_original_442","status":"answered","answer":"private answer","created_at":"2026-08-18T06:00:00Z"}`)},
		Events: []json.RawMessage{
			json.RawMessage(`{"version":4,"event_id":"event_original_1","sequence":10,"timestamp":"2026-08-18T06:02:12.482456Z","repo_id":"repo_private","issue_number":442,"run_id":"run_original_442","type":"environment_resume_requested","payload":{"resume_id":"resume_original_442","remote_head":null,"pull_requests":null}}`),
			json.RawMessage(`{"version":4,"event_id":"event_original_2","sequence":12,"timestamp":"2026-08-18T06:02:12.510889Z","repo_id":"repo_private","issue_number":442,"run_id":"run_original_442","type":"startup_reconciled","payload":{"inspection":{"RemoteHead":"","RemoteConsistent":false},"pull_requests":[]}}`),
		},
		Worktree: worktree.Inspection{Exists: true, Valid: true, Branch: "codex/issue-442-private", Head: "3333333333333333333333333333333333333333", Dirty: true, UnpushedCommits: true, LocalBranchExists: true},
		Remote: gh.RemoteState{Issue: gh.Issue{
			Number: 442, Title: "private customer title", Body: "private body", State: "OPEN", Labels: []string{"blocked"},
			Comments: []string{
				"<!-- codex-issue-loop:environment-resume:resume_original_442 -->\nprivate operator note",
				"<!-- codex-issue-loop:environment-resume:resume_original_442 -->\nanother private note",
			},
		}},
		Secrets: []string{"ghp_abcdefghijklmnopqrstuvwxyz123456"},
	}
}

func TestBuildRoundTripPreservesShapeValuesTimestampsAndReferences(t *testing.T) {
	bundle, err := Build(fixtureInput())
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.MarshalIndent(bundle, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "fixture.json")
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Completeness.EventCount != 2 || !slicesEqual(loaded.Completeness.EventSequences, []uint64{10, 12}) ||
		!stringsEqual(loaded.Completeness.EventTypes, []string{"environment_resume_requested", "startup_reconciled"}) {
		t.Fatalf("completeness=%+v", loaded.Completeness)
	}
	var first map[string]any
	if err := json.Unmarshal(loaded.Capture.Events[0], &first); err != nil {
		t.Fatal(err)
	}
	payload := first["payload"].(map[string]any)
	if payload["remote_head"] != nil || payload["pull_requests"] != nil {
		t.Fatalf("null values changed: %#v", payload)
	}
	var second map[string]any
	_ = json.Unmarshal(loaded.Capture.Events[1], &second)
	secondPayload := second["payload"].(map[string]any)
	if values, ok := secondPayload["pull_requests"].([]any); !ok || len(values) != 0 {
		t.Fatalf("empty array changed: %#v", secondPayload["pull_requests"])
	}
}

func TestBuildRedactsSecretsPathsAndCommentTextWithoutBreakingIdentity(t *testing.T) {
	bundle, err := Build(fixtureInput())
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(bundle)
	for _, forbidden := range []string{"ghp_abcdefghijklmnopqrstuvwxyz123456", "/Users/alice", "private customer title", "private body", "private operator note", "private question", "private answer"} {
		if bytes.Contains(data, []byte(forbidden)) {
			t.Fatalf("sensitive value remained in fixture: %q", forbidden)
		}
	}
	var issue map[string]any
	_ = json.Unmarshal(bundle.Capture.Durable.Issue, &issue)
	runID := issue["run_id"].(string)
	resumeID := issue["environment_resume"].(map[string]any)["id"].(string)
	var event map[string]any
	_ = json.Unmarshal(bundle.Capture.Events[0], &event)
	if event["run_id"] != runID || event["payload"].(map[string]any)["resume_id"] != resumeID {
		t.Fatalf("run/resume identity changed: issue=%#v event=%#v", issue, event)
	}
	comments := bundle.Capture.Remote.Issue.Comments
	marker := "<!-- codex-issue-loop:environment-resume:" + resumeID + " -->"
	if len(comments) != 2 || comments[0] != marker || comments[1] != marker {
		t.Fatalf("marker cardinality or identity changed: %#v", comments)
	}
}

func TestBuildDoesNotTrustMarkerLikeUserCommentText(t *testing.T) {
	input := fixtureInput()
	input.Remote.Issue.Comments = append(input.Remote.Issue.Comments,
		"<!-- codex-issue-loop:operator-secret:ghp_abcdefghijklmnopqrstuvwxyz123456 -->")
	bundle, err := Build(input)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(bundle)
	if bytes.Contains(data, []byte("operator-secret")) || bytes.Contains(data, []byte("ghp_abcdefghijklmnopqrstuvwxyz123456")) {
		t.Fatalf("unrecognized marker-like comment crossed sanitization: %s", data)
	}
}

func TestValidateRejectsCompressionFieldBackfillScalarEditAndHashMismatch(t *testing.T) {
	baseline, err := Build(fixtureInput())
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		mutate func(*Bundle)
	}{
		{name: "compressed history", mutate: func(bundle *Bundle) { bundle.Capture.Events = bundle.Capture.Events[:1] }},
		{name: "backfilled field", mutate: func(bundle *Bundle) {
			bundle.Capture.Events[0] = bytes.Replace(bundle.Capture.Events[0], []byte(`"remote_head":null`), []byte(`"remote_head":null,"RemoteConsistent":true`), 1)
		}},
		{name: "synthetic success", mutate: func(bundle *Bundle) {
			bundle.Capture.Events[1] = bytes.Replace(bundle.Capture.Events[1], []byte(`"RemoteConsistent":false`), []byte(`"RemoteConsistent":true`), 1)
		}},
		{name: "manifest hash", mutate: func(bundle *Bundle) { bundle.Manifest.ContentSHA256 = strings.Repeat("0", 64) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			bundle := baseline
			bundle.Capture.Events = cloneRaw(baseline.Capture.Events)
			test.mutate(&bundle)
			if err := Validate(bundle); err == nil {
				t.Fatal("tampered fixture was accepted")
			}
		})
	}
}

func TestZeitreise442FullHistoryFixtureRetainsEveryLegacyMismatch(t *testing.T) {
	bundle, err := Load("testdata/zeitreise-442-full-history-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	replay, err := bundle.Replay()
	if err != nil {
		t.Fatal(err)
	}
	issue := replay.Snapshot.Issues["442"]
	if issue == nil || len(replay.Events) != 27 || bundle.Completeness.EventCount != 27 {
		t.Fatalf("full history was compressed: issue=%+v events=%d", issue, len(replay.Events))
	}
	var rawIssue map[string]json.RawMessage
	if err := json.Unmarshal(bundle.Capture.Durable.Issue, &rawIssue); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"session_id", "session"} {
		if !bytes.Equal(bytes.TrimSpace(rawIssue[key]), []byte("null")) {
			t.Fatalf("%s must remain explicit null: %s", key, rawIssue[key])
		}
	}
	if issue.SessionID != "" || issue.Session != nil || issue.EnvironmentResume == nil {
		t.Fatalf("session/resume provenance changed: %+v", issue)
	}
	request := replay.Events[21]
	if delay := request.Timestamp.Sub(issue.EnvironmentResume.ConfirmedAt); delay != 28*time.Millisecond+433*time.Microsecond {
		t.Fatalf("resume writer timestamp relation changed: %s", delay)
	}
	for index := 15; index <= 20; index++ {
		var payload struct {
			PullRequests json.RawMessage            `json:"pull_requests"`
			Worktree     map[string]json.RawMessage `json:"worktree"`
		}
		if err := json.Unmarshal(replay.Events[index].Payload, &payload); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(bytes.TrimSpace(payload.PullRequests), []byte("null")) {
			t.Fatalf("event %d changed pull_requests null", index)
		}
		_, hasRemoteHead := payload.Worktree["RemoteHead"]
		_, hasRemoteConsistent := payload.Worktree["RemoteConsistent"]
		wantRemoteFields := index >= 19
		if hasRemoteHead != wantRemoteFields || hasRemoteConsistent != wantRemoteFields {
			t.Fatalf("event %d remote field evolution changed: %#v", index, payload.Worktree)
		}
		if wantRemoteFields && (!bytes.Equal(payload.Worktree["RemoteHead"], []byte(`""`)) || !bytes.Equal(payload.Worktree["RemoteConsistent"], []byte("false"))) {
			t.Fatalf("event %d unpublished remote values changed", index)
		}
	}
	resumeMarker := "<!-- codex-issue-loop:environment-resume:" + issue.EnvironmentResume.ID + " -->"
	resumeMarkers, failureMarkers := 0, 0
	for _, comment := range replay.Remote.Issue.Comments {
		resumeMarkers += strings.Count(comment, resumeMarker)
		failureMarkers += strings.Count(comment, "<!-- codex-issue-loop:failed:442 -->")
	}
	if resumeMarkers != 2 || failureMarkers != 2 || replay.Remote.PullRequests != nil ||
		bundle.Capture.Worktree.RemoteBranchExists || bundle.Capture.Worktree.RemoteHead != "" || bundle.Capture.Worktree.RemoteConsistent {
		t.Fatalf("marker/PR/remote cardinality changed: resume=%d failure=%d remote=%+v worktree=%+v", resumeMarkers, failureMarkers, replay.Remote, bundle.Capture.Worktree)
	}
}

func TestBlessedReleaseFixturesMatchReviewedByteLock(t *testing.T) {
	lock, err := os.ReadFile("testdata/blessed-fixtures.sha256")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(lock)), "\n")
	if len(lines) == 0 {
		t.Fatal("release fixture lock is empty")
	}
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) != 2 || len(fields[0]) != 64 || strings.Contains(fields[1], "/") {
			t.Fatalf("invalid release fixture lock entry %q", line)
		}
		path := filepath.Join("testdata", fields[1])
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(data)
		if got := hex.EncodeToString(digest[:]); got != fields[0] {
			t.Fatalf("reviewed fixture %s was hand-edited: got %s want %s", path, got, fields[0])
		}
		if _, err := Load(path); err != nil {
			t.Fatalf("reviewed fixture %s is incomplete: %v", path, err)
		}
	}
}
