package github

import (
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
