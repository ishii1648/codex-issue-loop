package github

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"github.com/ishii1648/codex-issue-loop/internal/platform/config"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ishii1648/codex-issue-loop/internal/adapter/state"
)

func TestRenderInputRequestIncludesCompleteHumanContract(t *testing.T) {
	created := time.Date(2026, 9, 5, 1, 2, 3, 0, time.UTC)
	payload := inputRequestMarkerPayload{
		Version: 1, RequestID: "req_1", IssueNumber: 239, RunID: "run_1", Question: "Choose a mode.",
		Reason: "It changes behavior.", RecommendedOption: "safe",
		Options:       []state.Option{{ID: "safe", Label: "Safe mode"}, {ID: "fast", Label: "Fast mode"}},
		AllowFreeText: true, CreatedAt: created,
	}
	body := renderInputRequest("<!-- versioned -->", "<!-- legacy -->", payload)
	for _, value := range []string{"<!-- versioned -->", "<!-- legacy -->", "Choose a mode.", "It changes behavior.", "`safe`: Safe mode", "`fast`: Fast mode", "Free-text allowed: `true`", created.Format(time.RFC3339Nano), "/agent-loop answer req_1 <answer>"} {
		if !strings.Contains(body, value) {
			t.Fatalf("rendered request omitted %q: %s", value, body)
		}
	}
}

func TestInputControlMarkersAndIdempotentRepair(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "gh")
	commentsPath, mutationsPath := filepath.Join(dir, "comments.json"), filepath.Join(dir, "mutations")
	bodyPath := filepath.Join(dir, "body.json")
	fake := fmt.Sprintf(`#!/bin/sh
case "$*" in
 *"--method POST"*|*"--method PATCH"*) printf '%%s\n' "$*" >> %q; cat > %q ;;
 *"--method DELETE"*) printf '%%s\n' "$*" >> %q ;;
 *"/user"*) printf '%%s\n' '{"login":"loop"}' ;;
 *"/comments?per_page=100"*) cat %q ;;
 *) exit 2 ;;
esac
`, mutationsPath, bodyPath, mutationsPath, commentsPath)
	if err := os.WriteFile(script, []byte(fake), 0700); err != nil {
		t.Fatal(err)
	}
	writeComments := func(comments []map[string]any) {
		t.Helper()
		data, err := json.Marshal([][]map[string]any{comments})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(commentsPath, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	comment := func(id int, actor, body string) map[string]any {
		return map[string]any{"id": id, "body": body, "user": map[string]string{"login": actor, "type": "User"}}
	}
	cfg := config.Defaults()
	cfg.GitHub.Repo = "owner/repo"
	client := CLI{Path: script, Secrets: []string{"sensitive-fixture-value"}}
	request := state.Request{ID: "req_1", IssueNumber: 1, Question: "Choose sensitive-fixture-value", Reason: "Reason", Recommended: "safe",
		Options: []state.Option{{ID: "safe", Label: "Safe"}}, CreatedAt: time.Now().UTC()}
	writeComments(nil)
	if err := client.SyncInputRequest(context.Background(), cfg, request); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(bodyPath)
	if err != nil {
		t.Fatal(err)
	}
	var posted struct {
		Body string `json:"body"`
	}
	if err := json.Unmarshal(data, &posted); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(posted.Body, "sensitive-fixture-value") {
		t.Fatal("question leaked secret")
	}
	marker := strings.Split(posted.Body, "\n")[0]
	encoded := strings.Split(strings.Split(marker, "payload=")[1], " ")[0]
	payload, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(payload)
	if !strings.Contains(marker, fmt.Sprintf("digest=%x -->", digest)) {
		t.Fatal("invalid payload digest")
	}
	var decoded inputRequestMarkerPayload
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.RequestID != request.ID || decoded.Version != 1 || decoded.IssueNumber != 1 || len(decoded.Options) != 1 {
		t.Fatalf("payload=%+v", decoded)
	}
	writeComments([]map[string]any{comment(1, "loop", posted.Body), comment(2, "attacker", posted.Body)})
	before, _ := os.ReadFile(mutationsPath)
	if err := client.SyncInputRequest(context.Background(), cfg, request); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(mutationsPath)
	if string(before) != string(after) {
		t.Fatal("identical marker was rewritten")
	}
	writeComments([]map[string]any{comment(1, "loop", marker+"\ntampered"), comment(2, "attacker", posted.Body), comment(3, "loop", posted.Body)})
	if err := client.SyncInputRequest(context.Background(), cfg, request); err != nil {
		t.Fatal(err)
	}
	after, _ = os.ReadFile(mutationsPath)
	if !strings.Contains(string(after), "--method PATCH") || !strings.Contains(string(after), "/issues/comments/3") || strings.Contains(string(after), "/issues/comments/2") {
		t.Fatalf("repair affected wrong comments: %s", after)
	}
	writeComments(nil)
	ack := InputAcknowledgement{RequestID: "req_1", CommentID: 42, Outcome: "accepted"}
	if err := client.SyncInputAcknowledgement(context.Background(), cfg, 1, ack); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(bodyPath)
	if err := json.Unmarshal(data, &posted); err != nil {
		t.Fatal(err)
	}
	writeComments([]map[string]any{comment(4, "loop", posted.Body)})
	before, _ = os.ReadFile(mutationsPath)
	if err := client.SyncInputAcknowledgement(context.Background(), cfg, 1, ack); err != nil {
		t.Fatal(err)
	}
	after, _ = os.ReadFile(mutationsPath)
	if string(before) != string(after) {
		t.Fatal("ack was duplicated")
	}
}

func TestInputActorRequiresCurrentPermissionEvenForOwnerAndAllowlist(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "gh")
	fake := `#!/bin/sh
case "$*" in
 *"/user"*) printf '%s\n' '{"login":"loop"}' ;;
 *"/collaborators/writer/permission"*) printf '%s\n' '{"permission":"write","user":{"login":"writer"}}' ;;
 *"/collaborators/owner/permission"*) printf '%s\n' '{"permission":"read","user":{"login":"owner"}}' ;;
 *"/collaborators/listed/permission"*) printf '%s\n' '{"permission":"read","user":{"login":"listed"}}' ;;
 *"/collaborators/mismatch/permission"*) printf '%s\n' '{"permission":"admin","user":{"login":"other"}}' ;;
 *) exit 2 ;;
esac
`
	if err := os.WriteFile(script, []byte(fake), 0700); err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.GitHub.Repo = "owner/repo"
	cfg.GitHub.TrustedIssueAuthors.AllowLogins = []string{"listed", "robot"}
	client := CLI{Path: script}
	for _, test := range []struct {
		actor, kind string
		trusted     bool
	}{
		{"writer", "User", true}, {"owner", "User", false}, {"listed", "User", false}, {"loop", "User", false}, {"robot", "Bot", false}, {"mismatch", "User", false},
	} {
		verification, err := client.VerifyInputActor(context.Background(), cfg, InputComment{Actor: test.actor, ActorType: test.kind})
		if err != nil || verification.Trusted != test.trusted {
			t.Fatalf("%s: %+v %v", test.actor, verification, err)
		}
	}
}
