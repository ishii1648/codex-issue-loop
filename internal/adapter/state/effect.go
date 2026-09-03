package state

import (
	"fmt"
	"strconv"
	"time"

	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
)

func PendingEffect(snapshot *Snapshot, issueNumber int) *EffectIntent {
	if snapshot == nil {
		return nil
	}
	return snapshot.PendingEffects[strconv.Itoa(issueNumber)]
}

func SetEffect(snapshot *Snapshot, issueNumber int, runID string, kind issuedomain.EffectKind, createdAt time.Time) error {
	if snapshot == nil || issueNumber < 1 || runID == "" || createdAt.IsZero() {
		return fmt.Errorf("effect identity is incomplete")
	}
	if kind == issuedomain.EffectNone {
		delete(snapshot.PendingEffects, strconv.Itoa(issueNumber))
		return nil
	}
	if err := kind.Validate(); err != nil {
		return err
	}
	key := strconv.Itoa(issueNumber)
	if current := snapshot.PendingEffects[key]; current != nil && current.IssueNumber == issueNumber && current.RunID == runID && current.Kind == kind {
		return nil
	}
	snapshot.PendingEffects[key] = &EffectIntent{
		ID: NewID("effect"), IssueNumber: issueNumber, RunID: runID, Kind: kind, CreatedAt: createdAt.UTC(),
	}
	return nil
}

func ClearEffect(snapshot *Snapshot, issueNumber int, effectID string) error {
	current := PendingEffect(snapshot, issueNumber)
	if current == nil {
		return nil
	}
	if current.ID != effectID {
		return fmt.Errorf("Issue #%d effect identity is stale", issueNumber)
	}
	delete(snapshot.PendingEffects, strconv.Itoa(issueNumber))
	return nil
}
