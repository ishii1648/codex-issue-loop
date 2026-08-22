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

func TestUnclassifiedErrorDefaultsToSupervisor(t *testing.T) {
	if got := KindOf(errors.New("unknown")); got != Supervisor {
		t.Fatalf("kind=%s", got)
	}
}
