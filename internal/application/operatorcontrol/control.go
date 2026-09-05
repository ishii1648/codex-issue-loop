package operatorcontrol

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/ishii1648/codex-issue-loop/internal/platform/fsutil"
)

type Operation string

const (
	OperationStop    Operation = "stop"
	OperationRestart Operation = "restart"
)

type Phase string

const (
	PhaseDraining           Phase = "draining"
	PhaseStopping           Phase = "stopping"
	PhaseStarting           Phase = "starting"
	PhaseStoppingBroker     Phase = "stopping_broker"
	PhaseStartingBroker     Phase = "starting_broker"
	PhaseStartingRepository Phase = "starting_repository"
	PhaseHealthCheck        Phase = "health_check"
	PhaseSucceeded          Phase = "succeeded"
	PhaseTimedOut           Phase = "timed_out"
)

type Transaction struct {
	Version       int       `json:"version"`
	Generation    string    `json:"generation"`
	Operation     Operation `json:"operation"`
	Phase         Phase     `json:"phase"`
	RequestedAt   time.Time `json:"requested_at"`
	DrainDeadline time.Time `json:"drain_deadline"`
	UpdatedAt     time.Time `json:"updated_at"`
	CompletedAt   time.Time `json:"completed_at,omitempty"`
	Reason        string    `json:"reason,omitempty"`
	SupervisorPID int       `json:"supervisor_pid,omitempty"`
	RestartBroker bool      `json:"restart_broker"`
}

type Fence struct {
	Version     int       `json:"version"`
	Generation  string    `json:"generation"`
	Operation   Operation `json:"operation"`
	RequestedAt time.Time `json:"requested_at"`
}

func (tx Transaction) Active() bool {
	return tx.Generation != "" && tx.Phase != PhaseSucceeded && tx.Phase != PhaseTimedOut
}

func Load(path string) (Transaction, error) {
	var tx Transaction
	data, err := readPrivateRegular(path, "operator control transaction")
	if errors.Is(err, os.ErrNotExist) {
		return Transaction{Version: 1}, nil
	}
	if err != nil {
		return tx, err
	}
	if err := decode(data, &tx); err != nil {
		return tx, fmt.Errorf("decode operator control transaction: %w", err)
	}
	if tx.Version != 1 || tx.Generation == "" || !knownOperation(tx.Operation) || !knownPhase(tx.Phase) || tx.RequestedAt.IsZero() || tx.DrainDeadline.IsZero() {
		return tx, errors.New("operator control transaction is incomplete or unsupported")
	}
	return tx, nil
}

func Save(path string, tx Transaction) error {
	tx.Version = 1
	if tx.Generation == "" || !knownOperation(tx.Operation) || !knownPhase(tx.Phase) || tx.RequestedAt.IsZero() || tx.DrainDeadline.IsZero() {
		return errors.New("refuse to persist incomplete operator control transaction")
	}
	return fsutil.WriteJSON(path, tx, 0o600)
}

func WriteFence(path string, value Fence) error {
	value.Version = 1
	if value.Generation == "" || !knownOperation(value.Operation) || value.RequestedAt.IsZero() {
		return errors.New("refuse to persist incomplete operator maintenance fence")
	}
	return fsutil.WriteJSON(path, value, 0o600)
}

func LoadFence(path string) (Fence, error) {
	var value Fence
	data, err := readPrivateRegular(path, "operator maintenance fence")
	if err != nil {
		return value, err
	}
	if err := decode(data, &value); err != nil {
		return value, fmt.Errorf("decode operator maintenance fence: %w", err)
	}
	if value.Version != 1 || value.Generation == "" || !knownOperation(value.Operation) || value.RequestedAt.IsZero() {
		return value, errors.New("operator maintenance fence is incomplete or unsupported")
	}
	return value, nil
}

func ClearFence(path string) error {
	err := os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func readPrivateRegular(path, name string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file: %s", name, path)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s is not owner-only: %s", name, path)
	}
	return os.ReadFile(path)
}

func decode(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return errors.New("trailing JSON data")
	}
	return nil
}

func knownOperation(operation Operation) bool {
	return operation == OperationStop || operation == OperationRestart
}

func knownPhase(phase Phase) bool {
	switch phase {
	case PhaseDraining, PhaseStopping, PhaseStarting, PhaseStoppingBroker, PhaseStartingBroker, PhaseStartingRepository, PhaseHealthCheck, PhaseSucceeded, PhaseTimedOut:
		return true
	}
	return false
}
