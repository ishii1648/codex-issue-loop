package supervisor

import (
	"fmt"
	"strings"
	"time"

	"github.com/ishii1648/codex-issue-loop/internal/adapter/state"
	"github.com/ishii1648/codex-issue-loop/internal/application/incidentloop"
	"github.com/ishii1648/codex-issue-loop/internal/domain/statecontract"
	"github.com/ishii1648/codex-issue-loop/internal/platform/failure"
	schemaversion "github.com/ishii1648/codex-issue-loop/internal/platform/schema"
)

type incidentSignalRecorder interface {
	Record(incidentloop.Signal) error
}

func (l *Loop) recordIncidentSignal(signal incidentloop.Signal) error {
	if l.IncidentSignals == nil {
		return nil
	}
	if signal.Version == 0 {
		signal.Version = incidentloop.SchemaVersion
	}
	if signal.ID == "" {
		signal.ID = state.NewID("sig")
	}
	if signal.Timestamp.IsZero() {
		signal.Timestamp = l.now()
	}
	if signal.Repository == "" {
		signal.Repository = l.Config.GitHub.Repo
	}
	if signal.CorrelationID == "" {
		if signal.RunID != "" {
			signal.CorrelationID = signal.RunID
		} else if signal.CycleID != "" {
			signal.CorrelationID = signal.CycleID
		} else {
			signal.CorrelationID = "repo-" + incidentloop.OpaqueScopeID(l.Store.RepoID)
		}
	}
	if signal.ScopeHash == "" {
		signal.ScopeHash = incidentloop.OpaqueScopeID(fmt.Sprintf("%s:%d:%s", l.Store.RepoID, signal.IssueNumber, signal.RunID))
	}
	return l.IncidentSignals.Record(signal)
}

func (l *Loop) recordRuntimeIdentity() error {
	version := l.ReleaseVersion
	if strings.TrimSpace(version) == "" {
		version = "unknown"
	}
	return l.recordIncidentSignal(incidentloop.Signal{
		Kind: "event", Name: "runtime_identity", Component: "supervisor", Phase: "startup",
		OutcomeCode: "observed", ReasonCode: "supervisor_started", ReleaseVersion: version,
		CommitSHA: l.ReleaseCommit, StateSchema: schemaversion.Current, SemanticContract: statecontract.CurrentVersion,
	})
}

func (l *Loop) recordFailureSignal(component, phase string, issueNumber int, runID, episodeID, code string, cause error, attempt int, retryAt *time.Time, human bool) error {
	kind := string(failure.KindOf(cause))
	if kind == "" {
		kind = "supervisor"
	}
	outcome := "failed"
	if retryAt != nil {
		outcome = "retrying"
	}
	return l.recordIncidentSignal(incidentloop.Signal{
		IssueNumber: issueNumber, RunID: runID, EpisodeID: episodeID,
		Kind: "event", Name: "failure_classified", Component: component, Phase: phase,
		OutcomeCode: outcome, ReasonCode: code, FailureKind: kind, FailureCode: code,
		Attempt: attempt, RetryAt: retryAt, HumanActionRequired: human,
		Evidence: []incidentloop.EvidenceRef{{Source: "state-event", Ref: code}},
	})
}
