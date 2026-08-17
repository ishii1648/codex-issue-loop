package webhook

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/ishii1648/codex-issue-loop/internal/fsutil"
)

type SweepState struct {
	Version        int       `json:"version"`
	ETag           string    `json:"etag,omitempty"`
	LastModified   string    `json:"last_modified,omitempty"`
	LastStatus     int       `json:"last_status,omitempty"`
	LastSuccessful time.Time `json:"last_successful,omitempty"`
	NotModified304 uint64    `json:"not_modified_304"`
	REST200        uint64    `json:"rest_200"`
	RateRemaining  string    `json:"rate_remaining,omitempty"`
	RateReset      string    `json:"rate_reset,omitempty"`
}

func LoadSweepState(repoStateDir string) (SweepState, error) {
	state := SweepState{Version: InboxVersion}
	data, err := os.ReadFile(filepath.Join(repoStateDir, "webhook-sweep.json"))
	if errors.Is(err, os.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		return SweepState{}, err
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return SweepState{}, err
	}
	if state.Version != InboxVersion {
		return SweepState{}, errors.New("unsupported webhook sweep state version")
	}
	return state, nil
}

func SaveSweepState(repoStateDir string, state SweepState) error {
	state.Version = InboxVersion
	return fsutil.WriteJSON(filepath.Join(repoStateDir, "webhook-sweep.json"), state, 0o600)
}
