// Package gitops serializes repository-wide Git metadata phases.
package gitops

import (
	"context"
	"fmt"
)

// Phase identifies a repository-wide operation for diagnostics and tests.
type Phase string

const (
	Base     Phase = "base"
	Worktree Phase = "worktree"
	Publish  Phase = "publish"
	Conflict Phase = "conflict"
)

// Gate is a cancellable, repository-scoped binary semaphore. A single Gate
// must be shared by every manager that can update a repository's Git metadata.
type Gate struct {
	token chan struct{}
}

func NewGate() *Gate {
	gate := &Gate{token: make(chan struct{}, 1)}
	gate.token <- struct{}{}
	return gate
}

// Run waits for the repository phase gate, runs operation, and always releases
// the gate. A canceled waiter returns without entering the phase.
func (g *Gate) Run(ctx context.Context, phase Phase, operation func() error) error {
	if operation == nil {
		return fmt.Errorf("Git operation for phase %q is required", phase)
	}
	if g == nil {
		return operation()
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-g.token:
	}
	defer func() { g.token <- struct{}{} }()
	if err := ctx.Err(); err != nil {
		return err
	}
	return operation()
}
