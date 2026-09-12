package hostcli

import (
	"context"
	"flag"
	"fmt"
	"github.com/ishii1648/codex-issue-loop/internal/adapter/webhook"
	"github.com/ishii1648/codex-issue-loop/internal/platform/config"
	"github.com/ishii1648/codex-issue-loop/internal/platform/layout"
	"github.com/ishii1648/codex-issue-loop/internal/platform/registry"
	"log"
	"sort"
	"strings"
)

func (a App) Broker(ctx context.Context, l layout.Layout, args []string) error {
	fs := flag.NewFlagSet("broker", flag.ContinueOnError)
	fs.SetOutput(a.Err)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	registered, err := (registry.Store{Path: l.RegistryPath}).Load()
	if err != nil {
		return err
	}
	ids := make([]string, 0, len(registered.Repos))
	for id := range registered.Repos {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	registrations := make([]webhook.Registration, 0, len(ids))
	for _, id := range ids {
		entry := registered.Repos[id]
		cfg, loadErr := config.Load(entry.RepoPath)
		if loadErr != nil {
			return fmt.Errorf("load webhook repository %s: %w", entry.RepoID, loadErr)
		}
		if cfg.Webhook.Enabled() {
			registrations = append(registrations, webhook.Registration{Entry: entry, Config: cfg})
		}
	}
	broker := &webhook.Broker{Root: l.Root, Registrations: registrations, Logger: log.New(a.Err, "agent-loop broker: ", log.LstdFlags|log.LUTC)}
	return broker.Run(ctx)
}
