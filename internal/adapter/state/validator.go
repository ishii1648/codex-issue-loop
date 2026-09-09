package state

import "github.com/ishii1648/codex-issue-loop/internal/domain/statecontract"

type SemanticContractVersionError = statecontract.SemanticContractVersionError
type LifecycleAPIVersionError = statecontract.LifecycleAPIVersionError

func validateIssueAggregate(issue *Issue) error { return statecontract.ValidateIssueAggregate(issue) }
