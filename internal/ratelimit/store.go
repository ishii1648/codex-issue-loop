package ratelimit

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/ishii1648/codex-issue-loop/internal/fsutil"
)

var processStoreMu sync.Mutex

// Cooldown is shared by every repository supervisor below one managed root.
// GitHub's primary GraphQL quota belongs to the authenticated user rather than
// an individual repository, so a per-repository retry deadline is insufficient.
type Cooldown struct {
	Resource             string    `json:"resource"`
	ResetAt              time.Time `json:"reset_at"`
	Source               string    `json:"source"`
	ObservedAt           time.Time `json:"observed_at"`
	SuppressedRetryCount uint64    `json:"suppressed_retry_count"`
}

func (c Cooldown) Active(now time.Time) bool {
	return !c.ResetAt.IsZero() && c.ResetAt.After(now)
}

// Store persists the user-quota cooldown outside repository state. Path must
// be the same for all supervisors managed by one agent-loop installation.
type Store struct {
	Path string
}

func (s Store) Current(now time.Time) (Cooldown, bool, error) {
	if s.Path == "" {
		return Cooldown{}, false, nil
	}
	var current Cooldown
	err := s.withLock(func() error {
		var err error
		current, err = s.loadUnlocked()
		return err
	})
	if err != nil {
		return Cooldown{}, false, err
	}
	return current, current.Active(now), nil
}

// Suppress atomically records one request that the shared cooldown prevented.
func (s Store) Suppress(now time.Time) (Cooldown, bool, error) {
	if s.Path == "" {
		return Cooldown{}, false, nil
	}
	var current Cooldown
	active := false
	err := s.withLock(func() error {
		var err error
		current, err = s.loadUnlocked()
		if err != nil || !current.Active(now) {
			return err
		}
		active = true
		current.SuppressedRetryCount++
		return fsutil.WriteJSON(s.Path, current, 0o600)
	})
	return current, active, err
}

// Observe extends, but never shortens, an active cooldown. A later reset from
// another repository wins so concurrent observations converge safely.
func (s Store) Observe(observed Cooldown, now time.Time) (Cooldown, error) {
	if observed.Resource == "" {
		observed.Resource = "graphql"
	}
	observed.ResetAt = observed.ResetAt.UTC()
	observed.ObservedAt = now.UTC()
	if s.Path == "" {
		return observed, nil
	}
	var result Cooldown
	err := s.withLock(func() error {
		current, err := s.loadUnlocked()
		if err != nil {
			return err
		}
		if current.Active(now) && !observed.ResetAt.After(current.ResetAt) {
			result = current
			return nil
		}
		if current.Active(now) {
			observed.SuppressedRetryCount = current.SuppressedRetryCount
		}
		result = observed
		return fsutil.WriteJSON(s.Path, result, 0o600)
	})
	return result, err
}

// Replace writes an externally revalidated cooldown even when it shortens the
// currently active deadline. Callers must only use this after an authoritative
// quota status reports that requests are available again.
func (s Store) Replace(observed Cooldown, now time.Time) (Cooldown, error) {
	if observed.Resource == "" {
		observed.Resource = "graphql"
	}
	observed.ResetAt = observed.ResetAt.UTC()
	observed.ObservedAt = now.UTC()
	if s.Path == "" {
		return observed, nil
	}
	err := s.withLock(func() error {
		return fsutil.WriteJSON(s.Path, observed, 0o600)
	})
	return observed, err
}

func (s Store) withLock(fn func() error) error {
	processStoreMu.Lock()
	defer processStoreMu.Unlock()
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o700); err != nil {
		return fmt.Errorf("create rate-limit directory: %w", err)
	}
	lockPath := s.Path + ".lock"
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open rate-limit lock: %w", err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("lock rate-limit state: %w", err)
	}
	defer func() { _ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN) }()
	return fn()
}

func (s Store) loadUnlocked() (Cooldown, error) {
	info, err := os.Lstat(s.Path)
	if errors.Is(err, os.ErrNotExist) {
		return Cooldown{}, nil
	}
	if err != nil {
		return Cooldown{}, fmt.Errorf("inspect rate-limit state: %w", err)
	}
	if !info.Mode().IsRegular() {
		return Cooldown{}, fmt.Errorf("rate-limit state is not a regular file: %s", s.Path)
	}
	if err := os.Chmod(s.Path, 0o600); err != nil {
		return Cooldown{}, fmt.Errorf("secure rate-limit state: %w", err)
	}
	data, err := os.ReadFile(s.Path)
	if err != nil {
		return Cooldown{}, fmt.Errorf("read rate-limit state: %w", err)
	}
	var current Cooldown
	if err := json.Unmarshal(data, &current); err != nil {
		return Cooldown{}, fmt.Errorf("decode rate-limit state: %w", err)
	}
	return current, nil
}
