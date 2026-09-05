package drain

import (
	"errors"
	"fmt"
	"os"

	"github.com/ishii1648/codex-issue-loop/internal/adapter/state"
	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
)

// Requested fails closed for an unreadable fence: a malformed or inaccessible
// control boundary must never reopen admission.
func Requested(paths ...string) bool {
	for _, path := range paths {
		if path == "" {
			continue
		}
		_, err := os.Lstat(path)
		if err == nil || !errors.Is(err, os.ErrNotExist) {
			return true
		}
	}
	return false
}

// Ready is the shared delivery/operator drain contract. Maintenance is only a
// durable checkpoint after the scheduler has finished every lifecycle job and
// no snapshot still owns a worker process.
func Ready(snapshot state.Snapshot) bool {
	if snapshot.Supervisor.State != state.SupervisorStateMaintenance {
		return false
	}
	if snapshot.ActiveExecution != nil {
		return false
	}
	return !HasWorker(snapshot)
}

func HasWorker(snapshot state.Snapshot) bool {
	for _, issue := range snapshot.Issues {
		if issue != nil && (issue.WorkerPID != 0 || issue.WorkerPGID != 0) {
			return true
		}
	}
	return false
}

// RecoverableUnstartedConflictLaunch only admits launch authority left by an
// earlier supervisor generation after the current supervisor entered drain.
func RecoverableUnstartedConflictLaunch(snapshot state.Snapshot) bool {
	if snapshot.Supervisor.State != state.SupervisorStateDraining && snapshot.Supervisor.State != state.SupervisorStateMaintenance {
		return false
	}
	active := snapshot.ActiveExecution
	if active == nil || snapshot.Supervisor.StartedAt.IsZero() || !active.StartedAt.Before(snapshot.Supervisor.StartedAt) {
		return false
	}
	issue := snapshot.Issues[fmt.Sprint(active.IssueNumber)]
	return issue != nil && issue.Status == issuedomain.StatusLaunching && issue.LaunchSource == issuedomain.StatusResolvingConflict &&
		issue.WorkerPID == 0 && issue.WorkerPGID == 0
}
