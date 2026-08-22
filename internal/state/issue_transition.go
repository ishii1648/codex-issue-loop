package state

import (
	"fmt"

	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
)

// ApplyIssueTransition is the persistence commit boundary for a domain
// lifecycle decision. It fences the write to the status observed when the
// decision was made; ownership fields remain fenced by the surrounding
// transaction.
func ApplyIssueTransition(item *Issue, transition issuedomain.Transition) error {
	if item == nil {
		return fmt.Errorf("cannot apply Issue transition %s to a missing Issue", transition.Name)
	}
	if err := transition.ValidateCommit(item.Status); err != nil {
		return err
	}
	item.Status = transition.To
	return nil
}

// SetIssueStatus is the compatibility commit boundary for lifecycle paths
// that do not yet have a named domain decision. It centralizes validation and
// keeps raw status assignment out of Store.Update closures.
func SetIssueStatus(item *Issue, status issuedomain.Status) error {
	if item == nil {
		return fmt.Errorf("cannot set status on a missing Issue")
	}
	if err := status.Validate(); err != nil {
		return err
	}
	item.Status = status
	return nil
}
