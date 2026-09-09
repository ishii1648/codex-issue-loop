package app

import (
	"context"
	"flag"
	"fmt"

	schema "github.com/ishii1648/codex-issue-loop/internal/application/migration"
	"github.com/ishii1648/codex-issue-loop/internal/platform/launchd"
	"github.com/ishii1648/codex-issue-loop/internal/platform/layout"
	"github.com/ishii1648/codex-issue-loop/internal/platform/registry"
)

func (a App) migrate(ctx context.Context, l layout.Layout, args []string) error {
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	fs.SetOutput(a.Err)
	repo := fs.String("repo", "", "registered repository path")
	apply := fs.Bool("apply", false, "apply the supported forward migration")
	rollback := fs.Bool("rollback", false, "restore a migration backup")
	backup := fs.String("backup", "", "absolute migration backup path")
	jsonOut := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return exitError{2, err}
	}
	if *apply && *rollback {
		return exitError{2, fmt.Errorf("--apply and --rollback are mutually exclusive")}
	}
	if *rollback != (*backup != "") {
		return exitError{2, fmt.Errorf("--rollback requires --backup, and --backup requires --rollback")}
	}

	var repositoryID string
	if *repo != "" {
		entry, err := (registry.Store{Path: l.RegistryPath}).Resolve(*repo, "")
		if err != nil {
			return err
		}
		repositoryID = entry.RepoID
	}
	_, brokerLoaded, err := loadedWebhookBroker(ctx, l)
	if err != nil {
		return err
	}
	if (*apply || *rollback) && brokerLoaded {
		return fmt.Errorf("schema migration requires the shared webhook broker to be stopped")
	}

	if *rollback {
		repositories, err := schema.RegisteredRepositories(l)
		if repositoryID != "" {
			selected := []registry.Entry{}
			for _, entry := range repositories {
				if entry.RepoID == repositoryID {
					selected = append(selected, entry)
				}
			}
			repositories = selected
		}
		if err != nil {
			return err
		}
		loaded, err := loadedMigrationEntries(ctx, l, repositories)
		if err != nil {
			return err
		}
		if len(loaded) > 0 {
			return fmt.Errorf("schema migration requires every registered LaunchAgent to be stopped; loaded: %v", repoIDs(loaded))
		}
		result, err := (schema.Migrator{Layout: l, RepositoryID: repositoryID}).Restore(*backup)
		if err != nil {
			return err
		}
		return a.output(*jsonOut, result)
	}

	report, err := schema.InspectRepository(l, repositoryID)
	if err != nil {
		return err
	}
	loaded, err := loadedMigrationEntries(ctx, l, report.Repositories)
	if err != nil {
		return err
	}
	if !*apply && !*rollback {
		return a.output(*jsonOut, map[string]any{"report": report, "loaded_repositories": repoIDs(loaded), "loaded_webhook_broker": brokerLoaded, "apply_allowed": !brokerLoaded && len(loaded) == 0 && len(report.Unsupported) == 0 && len(report.NonMigratable) == 0})
	}
	if len(loaded) > 0 {
		return fmt.Errorf("schema migration requires every registered LaunchAgent to be stopped; loaded: %v", repoIDs(loaded))
	}

	migrator := schema.Migrator{Layout: l, RepositoryID: repositoryID}
	var result schema.Result
	result, err = migrator.Apply()
	if err != nil {
		return err
	}
	if *apply && repositoryID == "" {
		if err := rewritePlists(l); err != nil {
			return fmt.Errorf("schema migrated but LaunchAgent plist rewrite failed: %w", err)
		}
	}
	return a.output(*jsonOut, result)
}

func loadedMigrationEntries(ctx context.Context, l layout.Layout, entries []registry.Entry) ([]registry.Entry, error) {
	loaded := make([]registry.Entry, 0, len(entries))
	for _, entry := range entries {
		launchctl := entry.Commands["launchctl"]
		status, err := (launchd.Manager{Layout: l, Launchctl: launchctl}).Status(ctx, entry)
		if err != nil {
			return nil, err
		}
		if status.Loaded {
			loaded = append(loaded, entry)
		}
	}
	return loaded, nil
}
