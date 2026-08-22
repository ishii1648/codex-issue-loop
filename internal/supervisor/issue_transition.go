package supervisor

import (
	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
	"github.com/ishii1648/codex-issue-loop/internal/state"
)

// applyIssueTransition is the application-layer commit boundary for domain
// lifecycle decisions. The domain decision is made before Store.Update; this
// function fences its application to the status observed by that decision.
func applyIssueTransition(item *state.Issue, transition issuedomain.Transition) error {
	return state.ApplyIssueTransition(item, transition)
}

// setIssueStatus is the temporary compatibility boundary for lifecycle paths
// that have not yet been promoted to a named domain decision. Keeping the only
// raw assignment here prevents new persistence closures from growing more
// lifecycle policy while those paths are extracted incrementally.
func setIssueStatus(item *state.Issue, status issuedomain.Status) error {
	return state.SetIssueStatus(item, status)
}
