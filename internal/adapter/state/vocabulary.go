package state

import "github.com/ishii1648/codex-issue-loop/internal/domain/statecontract"

type SupervisorState = statecontract.SupervisorState
type RecoveryState = statecontract.RecoveryState

const SupervisorStateStarting = statecontract.SupervisorStateStarting
const SupervisorStateRunning = statecontract.SupervisorStateRunning
const SupervisorStatePolling = statecontract.SupervisorStatePolling
const SupervisorStateRetryWait = statecontract.SupervisorStateRetryWait
const SupervisorStateBlocked = statecontract.SupervisorStateBlocked
const SupervisorStateStopped = statecontract.SupervisorStateStopped
const SupervisorStateMaintenance = statecontract.SupervisorStateMaintenance
const SupervisorStateDraining = statecontract.SupervisorStateDraining

const RecoveryStateBlocked = statecontract.RecoveryStateBlocked
