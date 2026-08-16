package app

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ishii1648/codex-issue-loop/internal/config"
)

func TestReadProposalRequiresStrictFields(t *testing.T) {
	valid := `{"version":1,"issue_number":69,"resources":[],"dependencies":[],"exclusive":true,"exclusive_reason":"unclear scope","confidence":"low","ambiguity_reasons":["paths unknown"]}`
	proposal, err := readProposal(strings.NewReader(valid), "-")
	if err != nil || proposal.IssueNumber != 69 || !proposal.Exclusive {
		t.Fatalf("proposal=%+v err=%v", proposal, err)
	}
	for _, invalid := range []string{
		`{"version":1,"issue_number":69,"resources":[],"dependencies":[],"confidence":"high","ambiguity_reasons":[]}`,
		`{"version":1,"issue_number":69,"resources":[],"dependencies":[],"exclusive":false,"exclusive_reason":"","confidence":"high","ambiguity_reasons":[],"extra":true}`,
		`{"version":1,"issue_number":69,"issue_number":70,"resources":[],"dependencies":[],"exclusive":false,"exclusive_reason":"","confidence":"high","ambiguity_reasons":[]}`,
		valid + `{}`,
	} {
		if _, err := readProposal(strings.NewReader(invalid), "-"); err == nil {
			t.Fatalf("invalid proposal accepted: %s", invalid)
		}
	}
}

func TestDesiredLabelsReplacesClaimsAndKeepsReadyOutOfPreview(t *testing.T) {
	cfg := producerConfigForApp()
	labels := desiredLabels([]string{"bug", "area:old", "codex-loop:ready"}, cfg, []string{"config", "docs"}, false)
	if strings.Join(labels, ",") != "area:config,area:docs,bug" {
		t.Fatalf("labels=%v", labels)
	}
}

func producerConfigForApp() config.Config {
	cfg := config.Defaults()
	cfg.GitHub.ReadyLabels = []string{"codex-loop:ready"}
	return cfg
}

func TestPrepareIssueApplyKeepsReadyLast(t *testing.T) {
	repo, _ := testEnvironment(t)
	root := filepath.Dir(repo)
	fakeGH := filepath.Join(root, "bin", "gh")
	countPath := filepath.Join(root, "get-count")
	logPath := filepath.Join(root, "prepare-calls")
	bodyPath := filepath.Join(root, "updated-body")
	script := `#!/bin/sh
case "$1 $2" in
  "issue view")
    count=0
    [ ! -f "$PREPARE_GET_COUNT" ] || count=$(cat "$PREPARE_GET_COUNT")
    count=$((count + 1))
    printf '%s' "$count" > "$PREPARE_GET_COUNT"
    if [ "$count" -eq 1 ]; then
      printf '%s\n' '{"number":69,"title":"scope","body":"scope","state":"OPEN","labels":[{"name":"codex-loop:ready"}]}'
    elif [ "$count" -eq 2 ]; then
      printf '%s\n' '{"number":69,"title":"scope","body":"scope","state":"OPEN","labels":[]}'
    elif [ "$count" -eq 3 ]; then
      printf '%s\n' '{"number":69,"title":"scope","body":"scope\n\n<!-- agent-loop:metadata\nversion: 1\ndepends_on: []\n-->\n","state":"OPEN","labels":[]}'
    else
      printf '%s\n' '{"number":69,"title":"scope","body":"scope\n\n<!-- agent-loop:metadata\nversion: 1\ndepends_on: []\n-->\n","state":"OPEN","labels":[{"name":"codex-loop:ready"}]}'
    fi
    ;;
  "issue edit")
    printf '%s\n' "$*" >> "$PREPARE_CALL_LOG"
    case "$*" in
      *"--body-file -"*) cat > "$PREPARE_BODY_LOG" ;;
    esac
    ;;
  *) exit 2 ;;
esac
`
	if err := os.WriteFile(fakeGH, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PREPARE_GET_COUNT", countPath)
	t.Setenv("PREPARE_CALL_LOG", logPath)
	t.Setenv("PREPARE_BODY_LOG", bodyPath)
	proposal := `{"version":1,"issue_number":69,"resources":[],"dependencies":[],"exclusive":true,"exclusive_reason":"repository-wide intake","confidence":"high","ambiguity_reasons":[]}`
	var stdout, stderr bytes.Buffer
	code := (App{In: strings.NewReader(proposal), Out: &stdout, Err: &stderr}).Run(context.Background(), []string{
		"prepare-issue", "--repo", repo, "--issue", "69", "--proposal", "-", "--apply", "--json",
	})
	if code != 0 {
		t.Fatalf("code=%d stderr=%s stdout=%s", code, stderr.String(), stdout.String())
	}
	var report struct {
		Applied bool `json:"applied"`
		Ready   bool `json:"ready"`
		Valid   bool `json:"valid"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil || !report.Applied || !report.Ready || !report.Valid {
		t.Fatalf("report=%+v err=%v stdout=%s", report, err, stdout.String())
	}
	calls, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	removeAt := strings.Index(string(calls), "--remove-label codex-loop:ready")
	bodyAt := strings.Index(string(calls), "--body-file -")
	addAt := strings.LastIndex(string(calls), "--add-label codex-loop:ready")
	if removeAt < 0 || bodyAt <= removeAt || addAt <= bodyAt {
		t.Fatalf("ready ordering was not remove/body/add: %s", calls)
	}
	body, err := os.ReadFile(bodyPath)
	if err != nil || !strings.Contains(string(body), "depends_on: []") {
		t.Fatalf("body=%q err=%v", body, err)
	}
}
