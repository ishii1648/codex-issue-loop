package gitops

import (
	"context"
	"errors"
	"testing"
)

func TestGateReleasesAfterFailureAndCancelsWaiter(t *testing.T) {
	gate := NewGate()
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- gate.Run(context.Background(), Worktree, func() error {
			close(entered)
			<-release
			return errors.New("injected failure")
		})
	}()
	<-entered

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	if err := gate.Run(ctx, Publish, func() error { called = true; return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled waiter error=%v", err)
	}
	if called {
		t.Fatal("canceled waiter entered the repository phase")
	}

	close(release)
	if err := <-done; err == nil || err.Error() != "injected failure" {
		t.Fatalf("first phase error=%v", err)
	}
	if err := gate.Run(context.Background(), Base, func() error { return nil }); err != nil {
		t.Fatalf("gate remained locked after failure: %v", err)
	}
}
