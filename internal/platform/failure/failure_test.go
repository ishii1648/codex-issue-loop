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
