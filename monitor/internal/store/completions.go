package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/ishii1648/codex-issue-loop/monitor/internal/model"
)

func (s Store) Completions(repository string) (*model.CompletionHistory, error) {
	data, err := os.ReadFile(filepath.Join(s.repoDir(repository), "completions.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var history model.CompletionHistory
	if err := json.Unmarshal(data, &history); err != nil {
		return nil, fmt.Errorf("decode completion history: %w", err)
	}
	if history.Repository != repository {
		return nil, fmt.Errorf("completion repository mismatch")
	}
	if err := history.Validate(); err != nil {
		return nil, err
	}
	return &history, nil
}

func (s Store) CommitCompletions(history model.CompletionHistory) error {
	if err := history.Validate(); err != nil {
		return err
	}
	if err := s.Ensure(); err != nil {
		return err
	}
	dir := s.repoDir(history.Repository)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(dir, "completions.json"), history); err != nil {
		return err
	}
	directory, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
