package state

import "github.com/ishii1648/codex-issue-loop/internal/domain/statecontract"

type Supervisor = statecontract.Supervisor
type RateLimit = statecontract.RateLimit
type AnswerRecord = statecontract.AnswerRecord
type AnswerProvenance = statecontract.AnswerProvenance
type WorkerSession = statecontract.WorkerSession
type WorkerIdentity = statecontract.WorkerIdentity
type Cancellation = statecontract.Cancellation
type ExecutionIdentity = statecontract.ExecutionIdentity
type ActiveExecution = statecontract.ActiveExecution
type EffectIntent = statecontract.EffectIntent
type ContinuationCheckpoint = statecontract.ContinuationCheckpoint
type ContinuationEvidence = statecontract.ContinuationEvidence
type Suspension = statecontract.Suspension
type ConflictAttempt = statecontract.ConflictAttempt
type ConflictVerification = statecontract.ConflictVerification
type ConflictRecovery = statecontract.ConflictRecovery
type WorkerWorkspace = statecontract.WorkerWorkspace
type Issue = statecontract.Issue
type QuarantineRecord = statecontract.QuarantineRecord
type Option = statecontract.Option
type Request = statecontract.Request
type Recovery = statecontract.Recovery
type Snapshot = statecontract.Snapshot
type Event = statecontract.Event
