package delivery

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/ishii1648/codex-issue-loop/internal/platform/fsutil"
)

type Paths struct {
	Root        string
	Transaction string
	Maintenance string
	Cache       string
	Log         string
	Lock        string
}

func RuntimePaths(managedRoot string) Paths {
	root := filepath.Join(managedRoot, "delivery")
	return Paths{Root: root, Transaction: filepath.Join(root, "transaction.json"), Maintenance: filepath.Join(root, "maintenance.json"), Cache: filepath.Join(root, "cache"), Log: filepath.Join(root, "delivery.log"), Lock: filepath.Join(root, "delivery.lock")}
}

func (p Paths) Ensure() error {
	for _, dir := range []string{p.Root, p.Cache} {
		if info, err := os.Lstat(dir); err == nil {
			if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
				return fmt.Errorf("delivery runtime path is not a regular directory: %s", dir)
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			return err
		}
	}
	return nil
}

type Phase string

const (
	PhaseIdle       Phase = "idle"
	PhaseDiscovered Phase = "discovered"
	PhaseDownloaded Phase = "downloaded"
	PhaseVerified   Phase = "verified"
	PhaseDraining   Phase = "draining"
	PhaseApplying   Phase = "applying"
	PhaseValidating Phase = "validating"
	PhaseSucceeded  Phase = "succeeded"
)

type VersionRef struct {
	Version string `json:"version,omitempty"`
	Commit  string `json:"commit,omitempty"`
}

type Transaction struct {
	Version               int           `json:"version"`
	Phase                 Phase         `json:"phase"`
	Attempt               int           `json:"attempt"`
	Current               VersionRef    `json:"current"`
	Desired               VersionRef    `json:"desired"`
	Previous              VersionRef    `json:"previous"`
	ArtifactDigest        string        `json:"artifact_digest,omitempty"`
	CandidateDir          string        `json:"candidate_dir,omitempty"`
	MaintenanceGeneration string        `json:"maintenance_generation,omitempty"`
	LoadedRepositories    []string      `json:"loaded_repositories,omitempty"`
	BackupPath            string        `json:"backup_path,omitempty"`
	LastResult            string        `json:"last_result,omitempty"`
	Reason                string        `json:"reason,omitempty"`
	Drain                 DrainProgress `json:"drain"`
	DrainStartedAt        time.Time     `json:"drain_started_at,omitempty"`
	DrainDeadline         time.Time     `json:"drain_deadline,omitempty"`
	StartedAt             time.Time     `json:"started_at,omitempty"`
	UpdatedAt             time.Time     `json:"updated_at"`
	LastCheckAt           time.Time     `json:"last_check_at,omitempty"`
	NextCheckAt           time.Time     `json:"next_check_at,omitempty"`
}

type Maintenance struct {
	Version     int        `json:"version"`
	Generation  string     `json:"generation"`
	Desired     VersionRef `json:"desired"`
	RequestedAt time.Time  `json:"requested_at"`
}

func LoadTransaction(path string) (Transaction, error) {
	var tx Transaction
	if info, statErr := os.Lstat(path); statErr == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return tx, fmt.Errorf("delivery transaction is not a regular file: %s", path)
		}
		if info.Mode().Perm()&0o077 != 0 {
			return tx, fmt.Errorf("delivery transaction is not owner-only: %s", path)
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return tx, statErr
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Transaction{Version: 1, Phase: PhaseIdle}, nil
	}
	if err != nil {
		return tx, err
	}
	if err := decodeStrictJSON(data, &tx); err != nil {
		return tx, fmt.Errorf("decode delivery transaction: %w", err)
	}
	if tx.Version != 1 {
		return tx, fmt.Errorf("unsupported delivery transaction version %d", tx.Version)
	}
	if !knownPhase(tx.Phase) {
		return tx, fmt.Errorf("unknown delivery transaction phase %q; keep maintenance fence and inspect %s", tx.Phase, path)
	}
	return tx, nil
}

func SaveTransaction(path string, tx Transaction) error {
	tx.Version = 1
	tx.UpdatedAt = time.Now().UTC()
	if !knownPhase(tx.Phase) {
		return fmt.Errorf("refuse to persist unknown delivery transaction phase %q", tx.Phase)
	}
	return fsutil.WriteJSON(path, tx, 0o600)
}

func WriteMaintenance(path string, value Maintenance) error {
	value.Version = 1
	return fsutil.WriteJSON(path, value, 0o600)
}

func knownPhase(phase Phase) bool {
	switch phase {
	case PhaseIdle, PhaseDiscovered, PhaseDownloaded, PhaseVerified, PhaseDraining, PhaseApplying, PhaseValidating, PhaseSucceeded:
		return true
	}
	return false
}

type Lock struct{ file *os.File }

func AcquireLock(path string) (*Lock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if info, statErr := os.Lstat(path); statErr == nil && (info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular()) {
		return nil, fmt.Errorf("delivery lock is not a regular file: %s", path)
	} else if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return nil, statErr
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errors.New("another delivery reconcile or apply is already running")
		}
		return nil, err
	}
	return &Lock{file: f}, nil
}

func (l *Lock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	_ = syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	return l.file.Close()
}
