package state

import issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
import contract "github.com/ishii1648/codex-issue-loop/internal/domain/snapshot"

type Supervisor = contract.Supervisor
type RateLimit = contract.RateLimit
type AnswerRecord = contract.AnswerRecord
type AnswerProvenance = contract.AnswerProvenance
type WorkerSession = contract.WorkerSession
type WorkerIdentity = contract.WorkerIdentity
type Cancellation = contract.Cancellation
type ExecutionIdentity = contract.ExecutionIdentity
type ActiveExecution = contract.ActiveExecution
type EffectIntent = contract.EffectIntent
type ContinuationCheckpoint = contract.ContinuationCheckpoint
type ContinuationEvidence = contract.ContinuationEvidence
type Suspension = contract.Suspension
type ConflictAttempt = contract.ConflictAttempt
type ConflictVerification = contract.ConflictVerification
type ConflictRecovery = contract.ConflictRecovery
type WorkerWorkspace = contract.WorkerWorkspace
type Issue = contract.Issue
type QuarantineRecord = contract.QuarantineRecord
type Option = contract.Option
type Request = contract.Request
type Recovery = contract.Recovery
type Snapshot = contract.Snapshot
type Event = contract.Event
type ConflictError = contract.ConflictError
type SemanticContractVersionError = contract.SemanticContractVersionError
type LifecycleAPIVersionError = contract.LifecycleAPIVersionError
type SemanticViolation = contract.SemanticViolation
type SemanticCompatibilityError = contract.SemanticCompatibilityError
type SupervisorState = contract.SupervisorState
type RecoveryState = contract.RecoveryState
type HumanNeed = contract.HumanNeed
type SchemaVersionError = contract.SchemaVersionError

const SemanticCodeCompatible = contract.SemanticCodeCompatible
const SemanticCodeContractVersionMismatch = contract.SemanticCodeContractVersionMismatch
const SemanticCodeWorkspaceProvenanceMissing = contract.SemanticCodeWorkspaceProvenanceMissing
const SemanticCodeWorkspaceProvenanceInvalid = contract.SemanticCodeWorkspaceProvenanceInvalid
const SemanticCodeExecutionAuthorityMissing = contract.SemanticCodeExecutionAuthorityMissing
const SemanticCodePreparedTransactionPresent = contract.SemanticCodePreparedTransactionPresent
const SupervisorStateStarting = contract.SupervisorStateStarting
const SupervisorStateRunning = contract.SupervisorStateRunning
const SupervisorStatePolling = contract.SupervisorStatePolling
const SupervisorStateRetryWait = contract.SupervisorStateRetryWait
const SupervisorStateBlocked = contract.SupervisorStateBlocked
const SupervisorStateStopped = contract.SupervisorStateStopped
const SupervisorStateMaintenance = contract.SupervisorStateMaintenance
const SupervisorStateDraining = contract.SupervisorStateDraining

func ValidID(value, prefix string) bool { return contract.ValidID(value, prefix) }
func validSHA256(value string) bool     { return contract.ValidSHA256(value) }
func validateAnswerObservation(s Snapshot, r *Request, p *AnswerProvenance) error {
	return contract.ValidateAnswerObservation(s, r, p)
}
func legacyWorkerLaunchSource(s *Snapshot, i *Issue) (issuedomain.Status, string, bool) {
	return contract.LegacyWorkerLaunchSource(s, i)
}
func crossedWorkerExecutionBoundary(i *Issue) bool      { return contract.CrossedWorkerExecutionBoundary(i) }
func SemanticViolations(s Snapshot) []SemanticViolation { return contract.SemanticViolations(s) }
func ValidateSemanticContract(s Snapshot) error         { return contract.ValidateSemanticContract(s) }

const RecoveryStateBlocked = contract.RecoveryStateBlocked

func validateRequestAggregate(s Snapshot, id string, r *Request) error {
	return contract.ValidateRequestAggregate(s, id, r)
}
func normalizeSnapshot(s *Snapshot) { contract.Normalize(s) }

type transaction = contract.Transaction

func (s Store) validateTransaction(txn transaction) error { return txn.Validate(s.RepoID) }
func validateEventSequence(s Snapshot, events []Event) error {
	return contract.ValidateEventSequence(s, events)
}
