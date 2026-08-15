package github

import (
	"testing"

	"github.com/ishii1648/codex-issue-loop/internal/config"
)

func TestEligibleRequiresReadyAndRejectsStateLabels(t *testing.T) {
	cfg := config.Defaults().GitHub
	if !Eligible([]string{"codex-loop:ready"}, cfg) {
		t.Fatal("ready Issue was rejected")
	}
	if Eligible([]string{"codex-loop:ready", "blocked"}, cfg) {
		t.Fatal("excluded Issue was accepted")
	}
	if Eligible([]string{"codex-loop:ready", "codex-loop:running"}, cfg) {
		t.Fatal("running Issue was accepted")
	}
}

func TestSelectReadyIsDeterministic(t *testing.T) {
	issues := []Issue{{Number: 9}, {Number: 2}, {Number: 5}}
	selected, ok := SelectReady(issues, map[string]string{"2": "completed"})
	if !ok || selected.Number != 5 {
		t.Fatalf("selected=%+v ok=%v", selected, ok)
	}
}
