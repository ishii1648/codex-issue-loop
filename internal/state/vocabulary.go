package state

import "fmt"

type SupervisorState string

const (
	SupervisorStateStarting    SupervisorState = "starting"
	SupervisorStateRunning     SupervisorState = "running"
	SupervisorStatePolling     SupervisorState = "polling"
	SupervisorStateRetryWait   SupervisorState = "retry_wait"
	SupervisorStateBlocked     SupervisorState = "blocked"
	SupervisorStateStopped     SupervisorState = "stopped"
	SupervisorStateMaintenance SupervisorState = "maintenance"
	SupervisorStateDraining    SupervisorState = "draining"
)

type RecoveryState string

const RecoveryStateBlocked RecoveryState = "blocked"

func (s SupervisorState) Validate() error {
	if s == "" {
		return nil
	}
	switch s {
	case SupervisorStateStarting, SupervisorStateRunning, SupervisorStatePolling, SupervisorStateRetryWait,
		SupervisorStateBlocked, SupervisorStateStopped, SupervisorStateMaintenance, SupervisorStateDraining:
		return nil
	default:
		return fmt.Errorf("unknown supervisor state %q", s)
	}
}

func (s RecoveryState) Validate() error {
	if s == RecoveryStateBlocked {
		return nil
	}
	return fmt.Errorf("unknown recovery state %q", s)
}
