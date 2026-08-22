package supervisor

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"syscall"
	"time"

	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
	"github.com/ishii1648/codex-issue-loop/internal/state"
)

// ProcessGroupController is the OS boundary used to recover worker process
// groups left behind by a terminated supervisor. OwnsGroup must reject a
// reused PID or a group that cannot be tied to the saved worker identity.
type ProcessGroupController interface {
	ProcessInspector
	GroupAlive(pgid int) bool
	OwnsGroup(pid, pgid int) bool
	SignalGroup(pgid int, signal syscall.Signal) error
}

type OSProcessGroupController struct{}

func (OSProcessGroupController) Alive(pid int) bool { return osProcessInspector{}.Alive(pid) }

func (OSProcessGroupController) GroupAlive(pgid int) bool {
	if pgid <= 1 {
		return false
	}
	err := syscall.Kill(-pgid, 0)
	return err == nil || err == syscall.EPERM
}

func (c OSProcessGroupController) OwnsGroup(pid, pgid int) bool {
	if pid <= 1 || pgid <= 1 || pid != pgid || pgid == syscall.Getpgrp() {
		return false
	}
	actual, err := syscall.Getpgid(pid)
	if err == nil {
		return actual == pgid
	}
	// A process group remains identified by its original leader PID while
	// descendants are alive. Therefore a dead leader with pid == pgid is still
	// safely attributable to the saved group; a live reused PID was rejected
	// by Getpgid above.
	return !c.Alive(pid) && c.GroupAlive(pgid)
}

func (OSProcessGroupController) SignalGroup(pgid int, signal syscall.Signal) error {
	if pgid <= 1 {
		return fmt.Errorf("refuse to signal invalid process group %d", pgid)
	}
	err := syscall.Kill(-pgid, signal)
	if err == syscall.ESRCH {
		return nil
	}
	return err
}

type WorkerStop struct {
	IssueNumber int    `json:"issue_number"`
	RunID       string `json:"run_id"`
	PID         int    `json:"pid"`
	PGID        int    `json:"pgid"`
	Forced      bool   `json:"forced"`
}

type WorkerStopReport struct {
	Workers []WorkerStop `json:"workers"`
}

type workerStopTarget struct {
	issue      state.Issue
	stopped    WorkerStop
	alive      bool
	transition *issuedomain.Transition
}

// StopWorkers terminates every saved worker group and records each Issue
// independently. Worktrees, sessions, branches, and leases are retained so a
// later start can reconcile and retry without losing valid work.
func StopWorkers(ctx context.Context, store state.Store, grace time.Duration, reason string, controller ProcessGroupController) (WorkerStopReport, error) {
	if controller == nil {
		controller = OSProcessGroupController{}
	}
	if grace <= 0 {
		return WorkerStopReport{}, fmt.Errorf("worker stop grace period must be positive")
	}
	snapshot, err := store.Load()
	if err != nil {
		return WorkerStopReport{}, err
	}
	issues := make([]state.Issue, 0, len(snapshot.Issues))
	for _, issue := range snapshot.Issues {
		if issue != nil && (issue.WorkerPID > 0 || issue.WorkerPGID > 0) {
			issues = append(issues, *issue)
		}
	}
	sort.Slice(issues, func(i, j int) bool { return issues[i].Number < issues[j].Number })

	targets := make([]workerStopTarget, 0, len(issues))
	for _, issue := range issues {
		pgid := issue.WorkerPGID
		if pgid <= 1 {
			pgid = issue.WorkerPID
		}
		stopped := WorkerStop{IssueNumber: issue.Number, RunID: issue.RunID, PID: issue.WorkerPID, PGID: pgid}
		alive := controller.Alive(issue.WorkerPID) || controller.GroupAlive(pgid)
		if alive {
			if !controller.OwnsGroup(issue.WorkerPID, pgid) {
				return WorkerStopReport{}, fmt.Errorf("Issue #%d saved worker PID %d does not own process group %d", issue.Number, issue.WorkerPID, pgid)
			}
		}
		var transition *issuedomain.Transition
		switch issue.Status {
		case issuedomain.StatusClaiming, issuedomain.StatusClaimed, issuedomain.StatusRunning:
			decision, transitionErr := issuedomain.InterruptExecution(issue.Status)
			if transitionErr != nil {
				return WorkerStopReport{}, transitionErr
			}
			transition = &decision
		}
		targets = append(targets, workerStopTarget{issue: issue, stopped: stopped, alive: alive, transition: transition})
	}

	// All groups receive cancellation before the pool-wide grace period starts.
	for index := range targets {
		if targets[index].alive {
			if err := controller.SignalGroup(targets[index].stopped.PGID, syscall.SIGTERM); err != nil {
				return WorkerStopReport{}, fmt.Errorf("stop Issue #%d worker process group %d: %w", targets[index].issue.Number, targets[index].stopped.PGID, err)
			}
		}
	}
	waitForGroups(ctx, controller, targets, grace)
	for index := range targets {
		if targets[index].alive && controller.GroupAlive(targets[index].stopped.PGID) {
			targets[index].stopped.Forced = true
			if err := controller.SignalGroup(targets[index].stopped.PGID, syscall.SIGKILL); err != nil {
				return WorkerStopReport{}, fmt.Errorf("kill Issue #%d worker process group %d: %w", targets[index].issue.Number, targets[index].stopped.PGID, err)
			}
		}
	}
	// Once SIGKILL has been sent, finish the bounded reap check even if the CLI
	// context was canceled; returning early here could leave an orphan group.
	waitForGroups(context.Background(), controller, targets, grace)
	for _, target := range targets {
		if target.alive && controller.GroupAlive(target.stopped.PGID) {
			return WorkerStopReport{}, fmt.Errorf("Issue #%d worker process group %d remained alive after SIGKILL", target.issue.Number, target.stopped.PGID)
		}
	}

	report := WorkerStopReport{Workers: make([]WorkerStop, 0, len(targets))}
	for _, target := range targets {
		issue, stopped := target.issue, target.stopped
		transition := target.transition
		now := time.Now().UTC()
		_, err := store.Update("worker_process_stopped", issue.Number, issue.RunID, stopped, func(current *state.Snapshot) error {
			item := current.Issues[strconv.Itoa(issue.Number)]
			if item == nil || item.RunID != issue.RunID {
				return fmt.Errorf("Issue #%d run changed while stopping worker", issue.Number)
			}
			if item.WorkerPID != issue.WorkerPID || item.WorkerPGID != issue.WorkerPGID {
				return fmt.Errorf("Issue #%d worker process identity changed while stopping", issue.Number)
			}
			item.WorkerPID, item.WorkerPGID = 0, 0
			switch item.Status {
			case issuedomain.StatusClaiming, issuedomain.StatusClaimed, issuedomain.StatusRunning:
				if transition == nil {
					return fmt.Errorf("Issue #%d active worker is missing its interruption decision", issue.Number)
				}
				if err := applyIssueTransition(item, *transition); err != nil {
					return err
				}
				item.RetryAfter = nil
				item.LastError = reason
			case issuedomain.StatusResolvingConflict:
				item.RetryAfter = nil
				item.LastError = reason
			}
			item.UpdatedAt = now
			return nil
		})
		if err != nil {
			return report, err
		}
		report.Workers = append(report.Workers, stopped)
	}
	return report, nil
}

func waitForGroups(ctx context.Context, controller ProcessGroupController, targets []workerStopTarget, grace time.Duration) {
	deadline := time.NewTimer(grace)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for groupsAlive(controller, targets) {
		select {
		case <-ctx.Done():
			return
		case <-deadline.C:
			return
		case <-ticker.C:
		}
	}
}

func groupsAlive(controller ProcessGroupController, targets []workerStopTarget) bool {
	for _, target := range targets {
		if target.alive && controller.GroupAlive(target.stopped.PGID) {
			return true
		}
	}
	return false
}
