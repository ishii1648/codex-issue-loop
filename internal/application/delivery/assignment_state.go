package delivery

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/ishii1648/codex-issue-loop/internal/platform/fsutil"
)

type AssignmentPhase string
type AssignmentOperation string

const (
	AssignmentOperationApply    AssignmentOperation = "apply"
	AssignmentOperationRollback AssignmentOperation = "rollback"
)

const (
	AssignmentPlanned        AssignmentPhase = "planned"
	AssignmentDraining       AssignmentPhase = "draining"
	AssignmentApplying       AssignmentPhase = "applying"
	AssignmentValidating     AssignmentPhase = "validating"
	AssignmentRollingBack    AssignmentPhase = "rolling_back"
	AssignmentSucceeded      AssignmentPhase = "succeeded"
	AssignmentRolledBack     AssignmentPhase = "rolled_back"
	AssignmentRollbackFailed AssignmentPhase = "rollback_failed"
)

type AssignmentTransaction struct {
	Version            int                 `json:"version"`
	RepositoryID       string              `json:"repository_id"`
	Operation          AssignmentOperation `json:"operation"`
	Phase              AssignmentPhase     `json:"phase"`
	ExpectedGeneration uint64              `json:"expected_generation"`
	TargetGeneration   uint64              `json:"target_generation"`
	Current            AssignmentRef       `json:"current"`
	Desired            AssignmentRef       `json:"desired"`
	WasLoaded          bool                `json:"was_loaded"`
	Result             string              `json:"result,omitempty"`
	Reason             string              `json:"reason,omitempty"`
	StartedAt          time.Time           `json:"started_at"`
	UpdatedAt          time.Time           `json:"updated_at"`
}

func LoadAssignmentTransaction(path string) (AssignmentTransaction, error) {
	var tx AssignmentTransaction
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return tx, nil
	}
	if err != nil {
		return tx, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return tx, errors.New("assignment transaction must be an owner-only regular file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return tx, err
	}
	if err := decodeStrictJSON(data, &tx); err != nil {
		return tx, fmt.Errorf("decode assignment transaction: %w", err)
	}
	if tx.Version != 1 || tx.RepositoryID == "" || !knownAssignmentPhase(tx.Phase) {
		return tx, errors.New("assignment transaction is incomplete or unsupported")
	}
	if err := validateAssignmentTransaction(tx); err != nil {
		return tx, err
	}
	return tx, nil
}

func validateAssignmentTransaction(tx AssignmentTransaction) error {
	if tx.RepositoryID == "" || !knownAssignmentPhase(tx.Phase) {
		return errors.New("assignment transaction is incomplete or unsupported")
	}
	if tx.Operation != AssignmentOperationApply && tx.Operation != AssignmentOperationRollback {
		return errors.New("assignment transaction operation is incomplete or unsupported")
	}
	if tx.ExpectedGeneration == 0 || tx.TargetGeneration != tx.ExpectedGeneration+1 {
		return errors.New("assignment transaction generation is invalid")
	}
	if tx.StartedAt.IsZero() {
		return errors.New("assignment transaction start time is missing")
	}
	if err := validateAssignmentRef(tx.Current); err != nil {
		return fmt.Errorf("assignment transaction current: %w", err)
	}
	if err := validateAssignmentRef(tx.Desired); err != nil {
		return fmt.Errorf("assignment transaction desired: %w", err)
	}
	if tx.Current == tx.Desired {
		return errors.New("assignment transaction does not change the assignment")
	}
	return nil
}

func SaveAssignmentTransaction(path string, tx AssignmentTransaction) error {
	if err := validateAssignmentTransaction(tx); err != nil {
		return err
	}
	if err := ensurePrivateDirectory(filepath.Dir(path)); err != nil {
		return err
	}
	tx.Version = 1
	tx.UpdatedAt = time.Now().UTC()
	return fsutil.WriteJSON(path, tx, 0o600)
}

func knownAssignmentPhase(phase AssignmentPhase) bool {
	switch phase {
	case AssignmentPlanned, AssignmentDraining, AssignmentApplying, AssignmentValidating, AssignmentRollingBack, AssignmentSucceeded, AssignmentRolledBack, AssignmentRollbackFailed:
		return true
	}
	return false
}
