package supervisor

import (
	"context"
	"errors"
	"testing"
	"time"
)

type incidentRunnerFunc func(context.Context) error

func (f incidentRunnerFunc) RunIncidentOnce(ctx context.Context) error { return f(ctx) }

func TestIncidentAutomationDisabledUsesSchedulerOnly(t *testing.T) {
	want := errors.New("scheduler stopped")
	calls := 0
	loop := Loop{IncidentAutomation: incidentRunnerFunc(func(context.Context) error {
		calls++
		return nil
	})}
	err := loop.runWithIncidentAutomation(context.Background(), func(context.Context) error { return want })
	if !errors.Is(err, want) || calls != 0 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
}

func TestIncidentAutomationStartsImmediatelyAndWaitsForGracefulShutdown(t *testing.T) {
	want := errors.New("scheduler stopped")
	started := make(chan struct{})
	stopped := make(chan struct{})
	loop := Loop{IncidentAutomation: incidentRunnerFunc(func(ctx context.Context) error {
		close(started)
		<-ctx.Done()
		close(stopped)
		return ctx.Err()
	})}
	loop.Config.IncidentAutomation.Enabled = true
	loop.Config.IncidentAutomation.Interval.Duration = time.Hour
	err := loop.runWithIncidentAutomation(context.Background(), func(context.Context) error {
		<-started
		return want
	})
	if !errors.Is(err, want) {
		t.Fatalf("err=%v", err)
	}
	select {
	case <-stopped:
	default:
		t.Fatal("incident automation was not joined during shutdown")
	}
}

func TestIncidentAutomationFailureCancelsAndJoinsScheduler(t *testing.T) {
	want := errors.New("analysis store failed")
	stopped := make(chan struct{})
	loop := Loop{IncidentAutomation: incidentRunnerFunc(func(context.Context) error { return want })}
	loop.Config.IncidentAutomation.Enabled = true
	loop.Config.IncidentAutomation.Interval.Duration = time.Hour
	err := loop.runWithIncidentAutomation(context.Background(), func(ctx context.Context) error {
		<-ctx.Done()
		close(stopped)
		return nil
	})
	if !errors.Is(err, want) {
		t.Fatalf("err=%v", err)
	}
	select {
	case <-stopped:
	default:
		t.Fatal("scheduler was not joined after automation failure")
	}
}
