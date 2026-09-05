package app

import (
	"context"
	"flag"
	"fmt"

	gh "github.com/ishii1648/codex-issue-loop/internal/adapter/github"
	"github.com/ishii1648/codex-issue-loop/internal/adapter/state"
	"github.com/ishii1648/codex-issue-loop/internal/adapter/worktree"
	"github.com/ishii1648/codex-issue-loop/internal/application/lifecycle"
	"github.com/ishii1648/codex-issue-loop/internal/platform/config"
	"github.com/ishii1648/codex-issue-loop/internal/platform/launchd"
	"github.com/ishii1648/codex-issue-loop/internal/platform/layout"
	"github.com/ishii1648/codex-issue-loop/internal/platform/registry"
)

func (a App) cleanup(ctx context.Context, l layout.Layout, args []string) error {
	fs := flag.NewFlagSet("cleanup", flag.ContinueOnError)
	fs.SetOutput(a.Err)
	repo := fs.String("repo", "", "repository path")
	apply := fs.Bool("apply", false, "remove eligible worktrees")
	jsonOut := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return exitError{2, err}
	}
	entry, err := a.resolvePath(l, *repo)
	if err != nil {
		return err
	}
	cfg, store, snapshot, manager, err := worktreeLifecycle(l, entry)
	if err != nil {
		return err
	}
	if !*apply {
		result, planErr := manager.Plan(ctx, cfg, entry.RepoID, snapshot)
		if outputErr := a.output(*jsonOut, result); outputErr != nil {
			return outputErr
		}
		return planErr
	}
	if err := requireStopped(ctx, l, entry); err != nil {
		return err
	}
	result, cleanupErr := manager.Cleanup(ctx, cfg, entry.RepoID, store, snapshot)
	if outputErr := a.output(*jsonOut, result); outputErr != nil {
		return outputErr
	}
	return cleanupErr
}

func (a App) purge(ctx context.Context, l layout.Layout, args []string) error {
	fs := flag.NewFlagSet("purge", flag.ContinueOnError)
	fs.SetOutput(a.Err)
	repo := fs.String("repo", "", "repository path")
	issueNumber := fs.Int("issue", 0, "Issue number")
	confirm := fs.String("confirm", "", "exact confirmation token from cleanup")
	jsonOut := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return exitError{2, err}
	}
	if *issueNumber <= 0 {
		return exitError{2, fmt.Errorf("--issue must be positive")}
	}
	entry, err := a.resolvePath(l, *repo)
	if err != nil {
		return err
	}
	expected := lifecycle.ConfirmationToken(entry.RepoID, *issueNumber)
	if *confirm != expected {
		return exitError{2, fmt.Errorf("purge requires --confirm %q", expected)}
	}
	if err := requireStopped(ctx, l, entry); err != nil {
		return err
	}
	cfg, store, snapshot, manager, err := worktreeLifecycle(l, entry)
	if err != nil {
		return err
	}
	result, purgeErr := manager.Purge(ctx, cfg, entry.RepoID, store, snapshot, *issueNumber, *confirm)
	if outputErr := a.output(*jsonOut, result); outputErr != nil {
		return outputErr
	}
	return purgeErr
}

func worktreeLifecycle(l layout.Layout, entry registry.Entry) (config.Config, state.Store, state.Snapshot, lifecycle.Manager, error) {
	cfg, err := config.Load(entry.RepoPath)
	if err != nil {
		return config.Config{}, state.Store{}, state.Snapshot{}, lifecycle.Manager{}, err
	}
	store := state.Store{
		Dir: l.RepoDir(entry.RepoID), RepoID: entry.RepoID, RepoPath: entry.RepoPath,
		Secrets: cfg.RedactionValues(),
	}
	snapshot, err := store.Load()
	if err != nil {
		return config.Config{}, state.Store{}, state.Snapshot{}, lifecycle.Manager{}, err
	}
	manager := lifecycle.Manager{
		Worktrees: worktree.Manager{StateRoot: l.Root, GitPath: entry.Commands["git"]},
		Remote:    gh.CLI{Path: entry.Commands["gh"], Secrets: cfg.RedactionValues()},
	}
	return cfg, store, snapshot, manager, nil
}

func requireStopped(ctx context.Context, l layout.Layout, entry registry.Entry) error {
	status, err := (launchd.Manager{Layout: l, Launchctl: entry.Commands["launchctl"]}).Status(ctx, entry)
	if err != nil {
		return err
	}
	if status.Loaded {
		return fmt.Errorf("worktree removal requires the repository LaunchAgent to be stopped; run agent-loop stop --repo %q", entry.RepoPath)
	}
	return nil
}
