package incidentloop

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ishii1648/codex-issue-loop/internal/platform/config"
)

func TestGitHubIssueAdapterSearchCreateAndReadbackAreExact(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gh")
	script := `#!/bin/sh
case "$1 $2" in
  "issue list") printf '%s' "$INCIDENT_GH_LIST" ;;
  "issue create") printf '%s\n' 'https://github.com/owner/repo/issues/42' ;;
  "issue view") printf '%s' "$INCIDENT_GH_VIEW" ;;
  *) exit 2 ;;
esac
`
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	fingerprint := strings.Repeat("a", 64)
	marker := "<!-- incident-fingerprint:" + fingerprint + " -->"
	t.Setenv("INCIDENT_GH_LIST", `[{"number":42,"url":"https://github.com/owner/repo/issues/42","body":"`+marker+`","createdAt":"2026-09-02T00:00:00Z","labels":[{"name":"codex-loop:ready"}]}]`)
	t.Setenv("INCIDENT_GH_VIEW", `{"number":42,"title":"incident","body":"`+marker+`","url":"https://github.com/owner/repo/issues/42","createdAt":"2026-09-02T00:00:00Z","state":"OPEN","labels":[{"name":"codex-loop:ready"}],"assignees":[],"milestone":null,"comments":[]}`)
	adapter := GitHubIssues{Path: path, Config: config.Config{GitHub: config.GitHub{Repo: "owner/repo"}}}
	found, err := adapter.FindByFingerprint(context.Background(), fingerprint)
	if err != nil || found == nil || found.Number != 42 || found.Fingerprint != fingerprint {
		t.Fatalf("found=%+v err=%v", found, err)
	}
	created, err := adapter.Create(context.Background(), IssueDraft{Title: "incident", Body: marker, Labels: []string{"codex-loop:ready"}, Fingerprint: fingerprint})
	if err != nil || created.Number != 42 {
		t.Fatalf("created=%+v err=%v", created, err)
	}
	readback, err := adapter.ReadBack(context.Background(), 42)
	if err != nil || readback.Fingerprint != fingerprint || !sameLabelSet(readback.Labels, []string{"codex-loop:ready"}) {
		t.Fatalf("readback=%+v err=%v", readback, err)
	}
}

func TestGitHubIssueAdapterRejectsAmbiguousFingerprint(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nprintf '%s' \"$INCIDENT_GH_LIST\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	fingerprint := strings.Repeat("b", 64)
	marker := "<!-- incident-fingerprint:" + fingerprint + " -->"
	t.Setenv("INCIDENT_GH_LIST", `[{"number":1,"body":"`+marker+`","labels":[]},{"number":2,"body":"`+marker+`","labels":[]}]`)
	adapter := GitHubIssues{Path: path, Config: config.Config{GitHub: config.GitHub{Repo: "owner/repo"}}}
	if _, err := adapter.FindByFingerprint(context.Background(), fingerprint); err == nil || !strings.Contains(err.Error(), "multiple Issues") {
		t.Fatalf("err=%v", err)
	}
}
