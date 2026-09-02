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
		return exitError{2, fmt.Errorf("incident requires analyze-once, status, or retry")}
	}
	command := args[0]
	fs := flag.NewFlagSet("incident "+command, flag.ContinueOnError)
	fs.SetOutput(a.Err)
	repo := fs.String("repo", "", "repository path")
	jsonOut := fs.Bool("json", false, "emit JSON")
	fingerprint := fs.String("fingerprint", "", "incident fingerprint (retry only)")
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
		status := struct {
			Version int                       `json:"version"`
			Enabled bool                      `json:"enabled"`
			DryRun  bool                      `json:"dry_run"`
			State   incidentloop.DurableState `json:"state"`
			Metrics incidentloop.Metrics      `json:"metrics"`
		}{incidentloop.SchemaVersion, cfg.IncidentAutomation.Enabled, cfg.IncidentAutomation.DryRun, stateValue, metrics}
		return a.output(*jsonOut, status)
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
