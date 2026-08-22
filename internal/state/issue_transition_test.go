package state

import (
	"testing"

	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
)

func TestApplyIssueTransitionFencesStaleDecision(t *testing.T) {
	transition, err := issuedomain.RetryConflict(issuedomain.StatusBlocked)
	if err != nil {
		t.Fatal(err)
	}
	item := &Issue{Status: issuedomain.StatusFailed}
	if err := ApplyIssueTransition(item, transition); err == nil {
		t.Fatal("expected stale transition to be rejected")
	}
	if item.Status != issuedomain.StatusFailed {
		t.Fatalf("stale transition changed status to %q", item.Status)
	}
}

func TestSetIssueStatusValidatesCompatibilityWrite(t *testing.T) {
	item := &Issue{}
	if err := SetIssueStatus(item, issuedomain.Status("misspelled")); err == nil {
		t.Fatal("expected unknown compatibility status to be rejected")
	}
	if item.Status != issuedomain.StatusUnset {
		t.Fatalf("invalid compatibility write changed status to %q", item.Status)
	}
}
