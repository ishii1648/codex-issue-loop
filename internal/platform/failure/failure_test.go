package failure

import (
	"errors"
	"testing"
)

func TestWrapPreservesTypedClassification(t *testing.T) {
	base := errors.New("timeout")
	transient := Wrap(Transient, "list issues", base)
	wrappedAgain := Wrap(Issue, "outer", transient)
	if KindOf(wrappedAgain) != Transient || !errors.Is(wrappedAgain, base) {
		t.Fatalf("classification=%s error=%v", KindOf(wrappedAgain), wrappedAgain)
	}
}

func TestUnclassifiedErrorDefaultsToIssueIsolation(t *testing.T) {
	if got := KindOf(errors.New("unknown")); got != Issue {
		t.Fatalf("kind=%s", got)
	}
}

type scopedIssueError struct{ err error }

func (scopedIssueError) IssueScope() int { return 42 }
func (e scopedIssueError) Error() string { return e.err.Error() }
func (e scopedIssueError) Unwrap() error { return e.err }

func TestSupervisorWrapCannotPromoteIssueScopedError(t *testing.T) {
	base := errors.New("lifecycle changed")
	err := Wrap(Supervisor, "persist lifecycle", scopedIssueError{err: base})
	if got := KindOf(err); got != Issue || !errors.Is(err, base) {
		t.Fatalf("classification=%s error=%v", got, err)
	}
}
