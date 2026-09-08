package app

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/ishii1648/codex-issue-loop/internal/adapter/state"
	"github.com/ishii1648/codex-issue-loop/internal/application/delivery"
	"github.com/ishii1648/codex-issue-loop/internal/platform/launchd"
	"github.com/ishii1648/codex-issue-loop/internal/platform/layout"
	"github.com/ishii1648/codex-issue-loop/internal/platform/registry"
)

func diagnosticRuntime(l layout.Layout, entry registry.Entry) error {
	path, err := delivery.ResolveConfigPath("")
	if err != nil {
		return err
	}
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	cfg, err := delivery.LoadConfig(path)
	if err != nil {
		return fmt.Errorf("STATE_RUNTIME_INCOMPATIBLE: cannot verify delivery assignment: %w", err)
	}
	assignment, ok := cfg.Assignments[entry.RepoID]
	if !ok {
		return fmt.Errorf("STATE_RUNTIME_INCOMPATIBLE: no assignment for %s", entry.RepoID)
	}
	tx, err := delivery.LoadAssignmentTransaction(l.DeliveryAssignmentTransactionPath(entry.RepoID))
	if err != nil {
		return fmt.Errorf("STATE_RUNTIME_INCOMPATIBLE: %w", err)
	}
	if tx.Phase == delivery.AssignmentValidating && tx.ExpectedGeneration == assignment.Generation {
		fence, err := delivery.LoadMaintenance(l.DeliveryAssignmentFencePath(entry.RepoID))
		if err != nil {
			return fmt.Errorf("STATE_RUNTIME_INCOMPATIBLE: %w", err)
		}
		program, err := (launchd.Manager{Layout: l}).Program(entry)
		if err != nil {
			return fmt.Errorf("STATE_RUNTIME_INCOMPATIBLE: %w", err)
		}
		if tx.RepositoryID != entry.RepoID || tx.Current != assignment.AssignmentRef ||
			fence.Generation != fmt.Sprintf("assignment-%d", tx.TargetGeneration) || fence.Desired.Version != tx.Desired.Version || fence.Desired.Commit != tx.Desired.Commit || program != tx.Desired.Slot {
			return fmt.Errorf("STATE_RUNTIME_INCOMPATIBLE: candidate assignment evidence differs")
		}
		assignment.AssignmentRef = tx.Desired
	}
	if err := delivery.VerifySlot(assignment.AssignmentRef); err != nil {
		return fmt.Errorf("STATE_RUNTIME_INCOMPATIBLE: %w", err)
	}
	if assignment.Version != Version || assignment.Commit != Commit {
		return fmt.Errorf("STATE_RUNTIME_INCOMPATIBLE: CLI %s/%s differs from repository assignment %s/%s; update the global CLI independently or use the verified assignment runtime after confirming it includes read-only diagnostics; no binary executed", Version, Commit, assignment.Version, assignment.Commit)
	}
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("STATE_RUNTIME_INCOMPATIBLE: %w", err)
	}
	digest, err := fileSHA256(executable)
	if err != nil {
		return fmt.Errorf("STATE_RUNTIME_INCOMPATIBLE: %w", err)
	}
	if digest != assignment.ArtifactSHA256 {
		return fmt.Errorf("STATE_RUNTIME_INCOMPATIBLE: CLI digest differs from verified repository assignment")
	}
	return nil
}

func stateDiagnostic(entry registry.Entry, err error) diagnostic {
	code := "STATE_CORRUPT"
	var schema state.SchemaVersionError
	var semantic state.SemanticContractVersionError
	var lifecycle state.LifecycleAPIVersionError
	switch {
	case errors.As(err, &schema), errors.As(err, &semantic), errors.As(err, &lifecycle):
		code = "STATE_VERSION_UNSUPPORTED"
	case strings.HasPrefix(err.Error(), "STATE_RUNTIME_INCOMPATIBLE:"):
		code = "STATE_RUNTIME_INCOMPATIBLE"
	case strings.HasPrefix(err.Error(), "STATE_UNCONFIRMED:"):
		code = "STATE_UNCONFIRMED"
	case strings.HasPrefix(err.Error(), "STATE_RECOVERY_REQUIRED:"):
		code = "STATE_RECOVERY_REQUIRED"
	case errors.Is(err, os.ErrNotExist):
		code = "STATE_MISSING"
	}
	return failedDiagnostic(code, "repository", entry.RepoID, "durable stateの正常性を確認できません（読み取りのみ）", err.Error(),
		instruction("対応する配備runtimeとsnapshot契約を確認してください"),
		instruction("durable filesを保存し、未確定なtransactionやrecoveryを診断操作で修復しないでください"))
}
