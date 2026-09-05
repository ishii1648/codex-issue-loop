package store

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ishii1648/codex-issue-loop/monitor/internal/config"
	"github.com/ishii1648/codex-issue-loop/monitor/internal/model"
)

type Store struct{ Root string }

func (s Store) Ensure() error {
	if !filepath.IsAbs(s.Root) {
		return fmt.Errorf("monitor state root must be absolute")
	}
	if err := os.MkdirAll(filepath.Join(s.Root, "repositories"), 0o700); err != nil {
		return err
	}
	return os.Chmod(s.Root, 0o700)
}

func (s Store) Load(repository string) (*model.Snapshot, error) {
	data, err := os.ReadFile(s.currentPath(repository))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var snapshot model.Snapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return nil, fmt.Errorf("decode monitor current state: %w", err)
	}
	if snapshot.SchemaVersion != model.SchemaVersion || snapshot.Repository != repository {
		return nil, fmt.Errorf("monitor current state identity or schema mismatch")
	}
	return &snapshot, nil
}

func (s Store) Commit(snapshot model.Snapshot, closed *model.Interval) error {
	if err := s.Ensure(); err != nil {
		return err
	}
	dir := s.repoDir(snapshot.Repository)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	if closed != nil {
		history, err := s.History(snapshot.Repository)
		if err != nil {
			return err
		}
		exists := false
		for _, interval := range history {
			if interval.ID == closed.ID {
				exists = true
				break
			}
		}
		if !exists {
			history = append(history, *closed)
			sort.Slice(history, func(i, j int) bool { return history[i].StartedAt.Before(history[j].StartedAt) })
			if err := validateHistory(history); err != nil {
				return err
			}
			if err := writeJSONLines(s.historyPath(snapshot.Repository), history); err != nil {
				return err
			}
		}
	}
	return writeJSON(s.currentPath(snapshot.Repository), snapshot)
}

func (s Store) History(repository string) ([]model.Interval, error) {
	file, err := os.Open(s.historyPath(repository))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var intervals []model.Interval
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		if strings.TrimSpace(scanner.Text()) == "" {
			continue
		}
		var interval model.Interval
		if err := json.Unmarshal(scanner.Bytes(), &interval); err != nil {
			return nil, fmt.Errorf("decode monitor interval log: %w", err)
		}
		intervals = append(intervals, interval)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if err := validateHistory(intervals); err != nil {
		return nil, err
	}
	return intervals, nil
}

func (s Store) AllIntervals(repository string) ([]model.Interval, error) {
	intervals, err := s.History(repository)
	if err != nil {
		return nil, err
	}
	current, err := s.Load(repository)
	if err != nil {
		return nil, err
	}
	if current != nil {
		intervals = append(intervals, current.Current)
	}
	return intervals, nil
}

func validateHistory(intervals []model.Interval) error {
	ids := map[string]bool{}
	for index, interval := range intervals {
		if interval.ID == "" || interval.Repository == "" || interval.EndedAt.IsZero() || !interval.EndedAt.After(interval.StartedAt) {
			return fmt.Errorf("invalid finalized monitor interval")
		}
		if ids[interval.ID] {
			return fmt.Errorf("duplicate monitor interval %s", interval.ID)
		}
		ids[interval.ID] = true
		if index > 0 && interval.StartedAt.Before(intervals[index-1].EndedAt) {
			return fmt.Errorf("monitor intervals overlap")
		}
	}
	return nil
}

func (s Store) repoDir(repository string) string {
	return filepath.Join(s.Root, "repositories", config.RepoID(repository))
}

func (s Store) currentPath(repository string) string {
	return filepath.Join(s.repoDir(repository), "current.json")
}

func (s Store) historyPath(repository string) string {
	return filepath.Join(s.repoDir(repository), "intervals.jsonl")
}

func writeJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return writeFile(path, append(data, '\n'))
}

func writeJSONLines(path string, intervals []model.Interval) error {
	var data []byte
	for _, interval := range intervals {
		line, err := json.Marshal(interval)
		if err != nil {
			return err
		}
		data = append(data, line...)
		data = append(data, '\n')
	}
	return writeFile(path, data)
}

func writeFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	temporary, err := os.CreateTemp(dir, ".monitor-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}
