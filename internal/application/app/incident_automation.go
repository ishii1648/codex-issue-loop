package app

import (
	"context"
	"flag"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	assets "github.com/ishii1648/codex-issue-loop"
	"github.com/ishii1648/codex-issue-loop/internal/adapter/state"
	"github.com/ishii1648/codex-issue-loop/internal/application/incidentloop"
	"github.com/ishii1648/codex-issue-loop/internal/platform/config"
	"github.com/ishii1648/codex-issue-loop/internal/platform/layout"
	"github.com/ishii1648/codex-issue-loop/internal/platform/retention"
)

func newIncidentAutomation(l layout.Layout, repoID string, cfg config.Config, source state.Store, workerPath, ghPath string, secrets []string, policy retention.Policy) (incidentloop.Automation, incidentloop.Store) {
	incidentStore := incidentloop.Store{Dir: filepath.Join(l.RepoDir(repoID), "incidents"), Secrets: secrets, Retention: policy}
	automation := incidentloop.Automation{
		Collector: incidentloop.StateEventCollector{Repository: cfg.GitHub.Repo, CloseIssue: cfg.Completion.CloseIssue, Source: source, Target: incidentStore},
		Pipeline: incidentloop.Pipeline{
			Store:    incidentStore,
			Analyzer: incidentloop.CodexAnalyzer{Path: workerPath, RepoPath: cfg.RepoPath, StateDir: incidentStore.Dir, Model: cfg.Worker.Model, Timeout: cfg.IncidentAutomation.AnalyzerTimeout.Duration, Secrets: secrets},
			Issues:   incidentloop.GitHubIssues{Path: ghPath, Secrets: secrets, Config: cfg},
			Config: incidentloop.PipelineConfig{
				Repository: cfg.GitHub.Repo, RulesJSON: assets.IncidentRules, ReadyLabels: append([]string(nil), cfg.GitHub.ReadyLabels...),
				DryRun: cfg.IncidentAutomation.DryRun, MaxAttempts: cfg.IncidentAutomation.MaxAnalysisAttempts, BaseBackoff: cfg.IncidentAutomation.RetryBackoff.Duration,
				MaxEpisodeItems: cfg.IncidentAutomation.MaxEpisodeItems,
			},
		},
	}
	return automation, incidentStore
}

func (a App) incident(ctx context.Context, l layout.Layout, args []string) error {
	if len(args) == 0 {
		return exitError{2, fmt.Errorf("incident requires analyze-once, status, decisions, seed-canary, or retry")}
	}
	command := args[0]
	fs := flag.NewFlagSet("incident "+command, flag.ContinueOnError)
	fs.SetOutput(a.Err)
	repo := fs.String("repo", "", "repository path")
	jsonOut := fs.Bool("json", false, "emit JSON")
	fingerprint := fs.String("fingerprint", "", "incident fingerprint (retry only)")
	canaryID := fs.String("id", "", "stable canary identifier (seed-canary only)")
	confirmSyntheticEvidence := fs.Bool("confirm-synthetic-evidence", false, "confirm writing synthetic invariant signals (seed-canary only)")
	if err := fs.Parse(args[1:]); err != nil {
		return exitError{2, err}
	}
	if fs.NArg() != 0 {
		return exitError{2, fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))}
	}
	entry, err := a.resolvePath(l, *repo)
	if err != nil {
		return err
	}
	cfg, err := config.Load(entry.RepoPath)
	if err != nil {
		return err
	}
	secrets := cfg.RedactionValues()
	policy := retention.Policy{MaxBytes: cfg.Logs.RotateBytes, MaxAge: cfg.Logs.RotateInterval.Duration, Keep: cfg.Logs.Generations}
	source := state.Store{Dir: l.RepoDir(entry.RepoID), RepoID: entry.RepoID, RepoPath: entry.RepoPath, Secrets: secrets, EventRetention: policy}
	automation, incidentStore := newIncidentAutomation(l, entry.RepoID, cfg, source, entry.Commands[cfg.Worker.Backend], entry.Commands["gh"], secrets, policy)
	switch command {
	case "analyze-once":
		report, runErr := automation.RunOnce(ctx)
		if outputErr := a.output(*jsonOut, report); outputErr != nil {
			return outputErr
		}
		return runErr
	case "status":
		stateValue, stateErr := incidentStore.LoadState()
		if stateErr != nil {
			return stateErr
		}
		metrics, metricsErr := incidentStore.LoadMetrics()
		if metricsErr != nil {
			return metricsErr
		}
		decisions, decisionsErr := incidentStore.ReadDecisions()
		if decisionsErr != nil {
			return decisionsErr
		}
		var oldestAt, newestAt *time.Time
		if len(decisions) > 0 {
			oldest, newest := decisions[0].DecidedAt, decisions[0].DecidedAt
			for _, decision := range decisions[1:] {
				if decision.DecidedAt.Before(oldest) {
					oldest = decision.DecidedAt
				}
				if decision.DecidedAt.After(newest) {
					newest = decision.DecidedAt
				}
			}
			oldestAt, newestAt = &oldest, &newest
		}
		status := struct {
			Version     int                       `json:"version"`
			Enabled     bool                      `json:"enabled"`
			DryRun      bool                      `json:"dry_run"`
			State       incidentloop.DurableState `json:"state"`
			Metrics     incidentloop.Metrics      `json:"metrics"`
			DecisionLog struct {
				RetentionDays int        `json:"retention_days"`
				RecordCount   int        `json:"record_count"`
				OldestAt      *time.Time `json:"oldest_at,omitempty"`
				NewestAt      *time.Time `json:"newest_at,omitempty"`
			} `json:"decision_log"`
		}{Version: incidentloop.SchemaVersion, Enabled: cfg.IncidentAutomation.Enabled, DryRun: cfg.IncidentAutomation.DryRun, State: stateValue, Metrics: metrics}
		status.DecisionLog.RetentionDays = int(incidentloop.DecisionRetention / (24 * time.Hour))
		status.DecisionLog.RecordCount = len(decisions)
		status.DecisionLog.OldestAt = oldestAt
		status.DecisionLog.NewestAt = newestAt
		return a.output(*jsonOut, status)
	case "decisions":
		decisions, decisionsErr := incidentStore.ReadDecisions()
		if decisionsErr != nil {
			return decisionsErr
		}
		return a.output(*jsonOut, struct {
			Version       int                          `json:"version"`
			RetentionDays int                          `json:"retention_days"`
			RecordCount   int                          `json:"record_count"`
			Records       []incidentloop.IssueDecision `json:"records"`
		}{incidentloop.SchemaVersion, int(incidentloop.DecisionRetention / (24 * time.Hour)), len(decisions), decisions})
	case "seed-canary":
		if !*confirmSyntheticEvidence {
			return exitError{2, fmt.Errorf("incident seed-canary requires --confirm-synthetic-evidence")}
		}
		report, seedErr := incidentloop.SeedCanary(incidentStore, cfg.GitHub.Repo, *canaryID, time.Now().UTC())
		if seedErr != nil {
			return seedErr
		}
		return a.output(*jsonOut, report)
	case "retry":
		episode, retryErr := incidentStore.ResetCircuit(*fingerprint, time.Now().UTC())
		if retryErr != nil {
			return retryErr
		}
		return a.output(*jsonOut, struct {
			Version     int    `json:"version"`
			Fingerprint string `json:"fingerprint"`
			Status      string `json:"status"`
		}{incidentloop.SchemaVersion, episode.Fingerprint, "retry_enabled"})
	default:
		return exitError{2, fmt.Errorf("unknown incident command %q", command)}
	}
}
