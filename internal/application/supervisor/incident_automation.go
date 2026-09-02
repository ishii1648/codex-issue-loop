package supervisor

import (
	"context"
	"errors"
	"fmt"
	"time"
)

type incidentAutomationRunner interface {
	RunIncidentOnce(context.Context) error
}

func (l *Loop) runWithIncidentAutomation(ctx context.Context, runScheduler func(context.Context) error) error {
	if !l.Config.IncidentAutomation.Enabled || l.IncidentAutomation == nil {
		return runScheduler(ctx)
	}
	child, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make(chan error, 2)
	go func() { results <- runScheduler(child) }()
	go func() { results <- l.runIncidentAutomation(child) }()
	first := <-results
	cancel()
	second := <-results
	for _, err := range []error{first, second} {
		if err != nil && !errors.Is(err, context.Canceled) {
			return err
		}
	}
	return nil
}

func (l *Loop) runIncidentAutomation(ctx context.Context) error {
	interval := l.Config.IncidentAutomation.Interval.Duration
	for {
		if err := l.IncidentAutomation.RunIncidentOnce(ctx); err != nil {
			return fmt.Errorf("incident automation cycle: %w", err)
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return nil
		case <-timer.C:
		}
	}
}
