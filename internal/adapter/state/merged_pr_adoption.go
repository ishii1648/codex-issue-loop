package state

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"

	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
	"github.com/ishii1648/codex-issue-loop/internal/platform/retention"
)

// MergedPullRequestAdoptionRecord is the durable event authority used to
// recover adoption metadata if an older concurrently running supervisor wrote
// a snapshot without the newly introduced optional field.
type MergedPullRequestAdoptionRecord struct {
	Adoption   MergedPullRequestAdoption
	LeaseOwner LeaseOwner
}

func (s Store) MergedPullRequestAdoptionRecord(issueNumber int, runID string) (MergedPullRequestAdoptionRecord, error) {
	lock, err := s.lock(false)
	if err != nil {
		return MergedPullRequestAdoptionRecord{}, err
	}
	defer unlock(lock)
	finder := &mergedPullRequestAdoptionFinder{repoID: s.RepoID, issueNumber: issueNumber, runID: runID}
	if err := retention.WriteHistory(finder, s.EventsPath()); err != nil {
		return MergedPullRequestAdoptionRecord{}, fmt.Errorf("read merged Pull Request adoption history: %w", err)
	}
	if err := finder.finish(); err != nil {
		return MergedPullRequestAdoptionRecord{}, err
	}
	if finder.record == nil {
		return MergedPullRequestAdoptionRecord{}, fmt.Errorf("Issue #%d has no durable merged Pull Request adoption event", issueNumber)
	}
	return *finder.record, nil
}

type mergedPullRequestAdoptionFinder struct {
	repoID      string
	issueNumber int
	runID       string
	pending     []byte
	record      *MergedPullRequestAdoptionRecord
	synced      map[string]bool
}

func (f *mergedPullRequestAdoptionFinder) Write(data []byte) (int, error) {
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

func (f *mergedPullRequestAdoptionFinder) finish() error {
	if len(bytes.TrimSpace(f.pending)) != 0 {
		if err := f.consume(f.pending); err != nil {
			return err
		}
	}
	if f.record != nil && f.synced[f.record.Adoption.ID] {
		f.record.Adoption.Status = issuedomain.MergedPullRequestAdoptionStatusSynced
	}
	return nil
}

func (f *mergedPullRequestAdoptionFinder) consume(line []byte) error {
	if len(bytes.TrimSpace(line)) == 0 {
		return nil
	}
	var event Event
	if err := json.Unmarshal(line, &event); err != nil {
		return fmt.Errorf("decode merged Pull Request adoption history: %w", err)
	}
	if event.RepoID != f.repoID || event.IssueNumber != f.issueNumber || event.RunID != f.runID {
		return nil
	}
	switch event.Type {
	case "merged_pull_request_adopted":
		var payload struct {
			AdoptionID        string             `json:"adoption_id"`
			Generation        int                `json:"generation"`
			OperatorConfirmed bool               `json:"operator_confirmed"`
			ConfirmedAt       *time.Time         `json:"confirmed_at"`
			AdoptedAt         time.Time          `json:"adopted_at"`
			PullRequestURL    string             `json:"pull_request_url"`
			PullRequestNumber int                `json:"pull_request_number"`
			HeadSHA           string             `json:"head_sha"`
			MergeSHA          string             `json:"merge_sha"`
			BaseBranch        string             `json:"base_branch"`
			PreviousStatus    issuedomain.Status `json:"previous_status"`
			PreviousReason    string             `json:"previous_reason"`
			Branch            string             `json:"branch"`
			LeaseOwner        LeaseOwner         `json:"lease_owner"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return fmt.Errorf("decode merged Pull Request adoption event at sequence %d: %w", event.Sequence, err)
		}
		if !payload.OperatorConfirmed || !ValidID(payload.AdoptionID, "merged_pr_adoption_") || payload.Generation < 1 ||
			payload.AdoptedAt.IsZero() || payload.PullRequestURL == "" || payload.PullRequestNumber < 1 || payload.HeadSHA == "" || payload.MergeSHA == "" ||
			payload.LeaseOwner.RunID != f.runID || payload.LeaseOwner.Generation == 0 {
			return fmt.Errorf("merged Pull Request adoption event at sequence %d is incomplete", event.Sequence)
		}
		if f.record != nil {
			return fmt.Errorf("Issue #%d has multiple merged Pull Request adoption events", f.issueNumber)
		}
		confirmedAt := payload.AdoptedAt
		if payload.ConfirmedAt != nil && !payload.ConfirmedAt.IsZero() {
			confirmedAt = payload.ConfirmedAt.UTC()
		}
		f.record = &MergedPullRequestAdoptionRecord{
			Adoption: MergedPullRequestAdoption{
				ID: payload.AdoptionID, Status: issuedomain.MergedPullRequestAdoptionStatusGitHubSyncPending, Generation: payload.Generation,
				ConfirmedAt: confirmedAt, AdoptedAt: payload.AdoptedAt.UTC(), PullRequestURL: payload.PullRequestURL,
				PullRequestNumber: payload.PullRequestNumber, PreviousStatus: payload.PreviousStatus, PreviousReason: payload.PreviousReason,
				Branch: payload.Branch, HeadSHA: payload.HeadSHA, MergeSHA: payload.MergeSHA, BaseBranch: payload.BaseBranch,
			},
			LeaseOwner: payload.LeaseOwner,
		}
	case "merged_pull_request_adoption_synced":
		var payload struct {
			AdoptionID string `json:"adoption_id"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return fmt.Errorf("decode merged Pull Request adoption sync event at sequence %d: %w", event.Sequence, err)
		}
		if payload.AdoptionID != "" {
			if f.synced == nil {
				f.synced = map[string]bool{}
			}
			f.synced[payload.AdoptionID] = true
		}
	}
	return nil
}
