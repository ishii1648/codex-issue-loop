package app

import (
	"context"
	"flag"
	"fmt"
	"sort"
	"strings"

	gh "github.com/ishii1648/codex-issue-loop/internal/adapter/github"
	"github.com/ishii1648/codex-issue-loop/internal/adapter/state"
	"github.com/ishii1648/codex-issue-loop/internal/platform/config"
	"github.com/ishii1648/codex-issue-loop/internal/platform/launchd"
	"github.com/ishii1648/codex-issue-loop/internal/platform/layout"
)

type quarantinedSnapshotRecoveryReport struct {
	state.QuarantinedSnapshotRecoveryPlan
	Repository           string                             `json:"repository"`
	RepositoryPath       string                             `json:"repository_path"`
	Launchd              launchd.Status                     `json:"launchd"`
	GitHubVerified       bool                               `json:"github_verified"`
	Repairs              []state.LegacyMergedIdentityRepair `json:"repairs"`
	Applied              bool                               `json:"applied"`
	RecoveryMarkerBackup string                             `json:"recovery_marker_backup,omitempty"`
	ResultRevision       uint64                             `json:"result_revision,omitempty"`
}

type semanticMismatchRecoveryReport struct {
	state.SemanticMismatchRecoveryPlan
	Repository           string         `json:"repository"`
	RepositoryPath       string         `json:"repository_path"`
	Launchd              launchd.Status `json:"launchd"`
	Applied              bool           `json:"applied"`
	RecoveryMarkerBackup string         `json:"recovery_marker_backup,omitempty"`
}

func (a App) recoverSemanticQuarantine(ctx context.Context, l layout.Layout, args []string) error {
	fs := flag.NewFlagSet("recover-semantic-quarantine", flag.ContinueOnError)
	fs.SetOutput(a.Err)
	repo := fs.String("repo", "", "repository path")
	backup := fs.String("backup", "", "exact recovery backup path recorded by the current marker")
	dryRun := fs.Bool("dry-run", false, "preview exact recovery predicates without mutation")
	confirmed := fs.Bool("confirm-exact-backup", false, "confirm one exact semantic mismatch recovery step")
	jsonOut := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return exitError{2, err}
	}
	if fs.NArg() != 0 {
		return exitError{2, fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))}
	}
	if *backup == "" {
		return exitError{2, fmt.Errorf("--backup is required")}
	}
	if *dryRun == *confirmed {
		return exitError{2, fmt.Errorf("exactly one of --dry-run or --confirm-exact-backup is required")}
	}
	entry, err := a.resolvePath(l, *repo)
	if err != nil {
		return err
	}
	cfg, err := config.Load(entry.RepoPath)
	if err != nil {
		return err
	}
	manager := launchd.Manager{Layout: l, Launchctl: entry.Commands["launchctl"]}
	launchStatus, err := manager.Status(ctx, entry)
	if err != nil {
		return err
	}
	if launchStatus.Loaded || launchStatus.Running {
		return exitError{4, fmt.Errorf("repository LaunchAgent must be unloaded during semantic quarantine recovery")}
	}
	store := state.Store{Dir: l.RepoDir(entry.RepoID), RepoID: entry.RepoID, RepoPath: entry.RepoPath, Secrets: cfg.RedactionValues()}
	plan, err := store.PreviewSemanticMismatchRecovery(*backup)
	if err != nil {
		return exitError{4, err}
	}
	report := semanticMismatchRecoveryReport{SemanticMismatchRecoveryPlan: plan, Repository: cfg.GitHub.Repo,
		RepositoryPath: entry.RepoPath, Launchd: launchStatus}
	if *dryRun {
		return a.output(*jsonOut, report)
	}
	launchStatus, err = manager.Status(ctx, entry)
	if err != nil {
		return err
	}
	if launchStatus.Loaded || launchStatus.Running {
		return exitError{4, fmt.Errorf("repository LaunchAgent changed while semantic recovery was being verified")}
	}
	plan, markerBackup, err := store.ApplySemanticMismatchRecovery(*backup)
	if err != nil {
		return exitError{4, err}
	}
	report.SemanticMismatchRecoveryPlan = plan
	report.Launchd = launchStatus
	report.Applied = true
	report.RecoveryMarkerBackup = markerBackup
	return a.output(*jsonOut, report)
}

func (a App) recoverQuarantinedSnapshot(ctx context.Context, l layout.Layout, args []string) error {
	fs := flag.NewFlagSet("recover-quarantined-snapshot", flag.ContinueOnError)
	fs.SetOutput(a.Err)
	repo := fs.String("repo", "", "repository path")
	backup := fs.String("backup", "", "exact recovery backup path from status")
	dryRun := fs.Bool("dry-run", false, "preview exact recovery predicates without mutation")
	confirmed := fs.Bool("confirm-legacy-merged-identities", false, "confirm verified legacy merged identity recovery")
	jsonOut := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return exitError{2, err}
	}
	if fs.NArg() != 0 {
		return exitError{2, fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))}
	}
	if *backup == "" {
		return exitError{2, fmt.Errorf("--backup is required")}
	}
	if !*dryRun && !*confirmed {
		return exitError{2, fmt.Errorf("--confirm-legacy-merged-identities is required")}
	}
	entry, err := a.resolvePath(l, *repo)
	if err != nil {
		return err
	}
	cfg, err := config.Load(entry.RepoPath)
	if err != nil {
		return err
	}
	manager := launchd.Manager{Layout: l, Launchctl: entry.Commands["launchctl"]}
	launchStatus, err := manager.Status(ctx, entry)
	if err != nil {
		return err
	}
	if launchStatus.Running {
		return exitError{4, fmt.Errorf("repository LaunchAgent must not be running during quarantined snapshot recovery")}
	}
	store := state.Store{Dir: l.RepoDir(entry.RepoID), RepoID: entry.RepoID, RepoPath: entry.RepoPath, Secrets: cfg.RedactionValues()}
	plan, err := store.PreviewLegacyMergedIdentityRecovery(*backup)
	if err != nil {
		return exitError{4, err}
	}
	client := gh.CLI{Path: entry.Commands["gh"], Secrets: cfg.RedactionValues()}
	merged, err := client.ListMergedPullRequests(ctx, cfg)
	if err != nil {
		return fmt.Errorf("verify quarantined Pull Request identities: %w", err)
	}
	byURL := make(map[string]gh.PullRequest, len(merged))
	for _, pr := range merged {
		if _, duplicate := byURL[pr.URL]; duplicate {
			return exitError{4, fmt.Errorf("GitHub returned duplicate Pull Request URL %s", pr.URL)}
		}
		byURL[pr.URL] = pr
	}
	repairs := make([]state.LegacyMergedIdentityRepair, 0, len(plan.Targets))
	for _, target := range plan.Targets {
		pr, ok := byURL[target.PullRequestURL]
		if !ok || pr.MergedAt == nil || !strings.EqualFold(pr.State, "merged") || pr.Number <= 0 || pr.HeadSHA == "" ||
			pr.HeadRefName != target.Branch || pr.BaseRefName != cfg.Git.BaseBranch || !strings.EqualFold(pr.HeadRepository, cfg.GitHub.Repo) ||
			(target.PullRequestNumber > 0 && target.PullRequestNumber != pr.Number) {
			return exitError{4, fmt.Errorf("Issue #%d Pull Request %s is not the authoritative merged repository-local publication for branch %s", target.IssueNumber, target.PullRequestURL, target.Branch)}
		}
		repairs = append(repairs, state.LegacyMergedIdentityRepair{IssueNumber: target.IssueNumber, Branch: target.Branch,
			PullRequestURL: target.PullRequestURL, PullRequestNumber: pr.Number, HeadSHA: pr.HeadSHA})
	}
	sort.Slice(repairs, func(i, j int) bool { return repairs[i].IssueNumber < repairs[j].IssueNumber })
	report := quarantinedSnapshotRecoveryReport{QuarantinedSnapshotRecoveryPlan: plan, Repository: cfg.GitHub.Repo,
		RepositoryPath: entry.RepoPath, Launchd: launchStatus, GitHubVerified: true, Repairs: repairs}
	if *dryRun {
		return a.output(*jsonOut, report)
	}
	launchStatus, err = manager.Status(ctx, entry)
	if err != nil {
		return err
	}
	if launchStatus.Running {
		return exitError{4, fmt.Errorf("repository LaunchAgent started while recovery was being verified")}
	}
	report.Launchd = launchStatus
	result, markerBackup, err := store.ApplyLegacyMergedIdentityRecovery(*backup, repairs)
	if err != nil {
		return exitError{4, err}
	}
	report.Applied = true
	report.RecoveryMarkerBackup = markerBackup
	report.ResultRevision = result.StateRevision
	return a.output(*jsonOut, report)
}
