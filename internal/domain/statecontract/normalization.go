package statecontract

import (
	queuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/queue"
)

func NormalizeSnapshot(snapshot *Snapshot) {
	if snapshot.Issues == nil {
		snapshot.Issues = map[string]*Issue{}
	}
	if snapshot.PendingRequests == nil {
		snapshot.PendingRequests = map[string]*Request{}
	}
	if snapshot.PendingEffects == nil {
		snapshot.PendingEffects = map[string]*EffectIntent{}
	}
	if snapshot.QuarantinedIssues == nil {
		snapshot.QuarantinedIssues = map[string]*QuarantineRecord{}
	}
	if snapshot.IntakeVerifications == nil {
		snapshot.IntakeVerifications = map[string]*queuedomain.AuthorVerification{}
	}
	for _, issue := range snapshot.Issues {
		if issue == nil {
			continue
		}
		if issue.Session == nil && issue.SessionID != "" {
			// session_id predates backend selection and was only ever produced by Codex.
			issue.Session = &WorkerSession{Backend: "codex", ID: issue.SessionID}
		}
		if issue.Session != nil && issue.SessionID == "" {
			issue.SessionID = issue.Session.ID
		}
	}
}
