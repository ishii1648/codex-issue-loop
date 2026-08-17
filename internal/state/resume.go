package state

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/ishii1648/codex-issue-loop/internal/retention"
)

// EnvironmentResumeBaseSHA returns the publication base recorded by the
// matching write-ahead resume event. It is the recovery source for snapshots
// written by versions that stored the base only in the lease and event payload.
func (s Store) EnvironmentResumeBaseSHA(issueNumber int, runID, resumeID string) (string, error) {
	lock, err := s.lock(false)
	if err != nil {
		return "", err
	}
	defer unlock(lock)

	finder := &environmentResumeEventFinder{repoID: s.RepoID, issueNumber: issueNumber, runID: runID, resumeID: resumeID}
	if err := retention.WriteHistory(finder, s.EventsPath()); err != nil {
		return "", fmt.Errorf("read environment resume event history: %w", err)
	}
	if err := finder.finish(); err != nil {
		return "", err
	}
	if finder.baseSHA == "" {
		return "", fmt.Errorf("environment resume %s has no durable publication base SHA", resumeID)
	}
	return finder.baseSHA, nil
}

type environmentResumeEventFinder struct {
	repoID      string
	issueNumber int
	runID       string
	resumeID    string
	pending     []byte
	baseSHA     string
}

func (f *environmentResumeEventFinder) Write(data []byte) (int, error) {
	original := len(data)
	f.pending = append(f.pending, data...)
	for {
		index := bytes.IndexByte(f.pending, '\n')
		if index < 0 {
			break
		}
		line := append([]byte(nil), f.pending[:index]...)
		f.pending = f.pending[index+1:]
		if err := f.consume(line); err != nil {
			return 0, err
		}
	}
	return original, nil
}

func (f *environmentResumeEventFinder) finish() error {
	if len(bytes.TrimSpace(f.pending)) == 0 {
		return nil
	}
	return f.consume(f.pending)
}

func (f *environmentResumeEventFinder) consume(line []byte) error {
	if len(bytes.TrimSpace(line)) == 0 {
		return nil
	}
	var event Event
	if err := json.Unmarshal(line, &event); err != nil {
		return fmt.Errorf("decode environment resume event history: %w", err)
	}
	if event.Type != "environment_resume_requested" || event.RepoID != f.repoID || event.IssueNumber != f.issueNumber || event.RunID != f.runID {
		return nil
	}
	var payload struct {
		ResumeID string `json:"resume_id"`
		BaseSHA  string `json:"base_sha"`
	}
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		return fmt.Errorf("decode environment resume event payload at sequence %d: %w", event.Sequence, err)
	}
	if payload.ResumeID != f.resumeID {
		return nil
	}
	if payload.BaseSHA == "" {
		return fmt.Errorf("environment resume event at sequence %d has an empty publication base SHA", event.Sequence)
	}
	if f.baseSHA != "" && f.baseSHA != payload.BaseSHA {
		return fmt.Errorf("environment resume %s has conflicting publication base SHAs", f.resumeID)
	}
	f.baseSHA = payload.BaseSHA
	return nil
}
