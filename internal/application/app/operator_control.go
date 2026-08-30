package app

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ishii1648/codex-issue-loop/internal/adapter/state"
	"github.com/ishii1648/codex-issue-loop/internal/adapter/webhook"
	"github.com/ishii1648/codex-issue-loop/internal/application/supervisor"
	"github.com/ishii1648/codex-issue-loop/internal/platform/config"
	"github.com/ishii1648/codex-issue-loop/internal/platform/launchd"
	"github.com/ishii1648/codex-issue-loop/internal/platform/layout"
	"github.com/ishii1648/codex-issue-loop/internal/platform/registry"
)

func (a App) register(l layout.Layout, args []string) error {
	fs := flag.NewFlagSet("register", flag.ContinueOnError)
	fs.SetOutput(a.Err)
	repo := fs.String("repo", "", "repository path")
	jsonOut := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return exitError{2, err}
	}
	if *repo == "" {
		return exitError{2, fmt.Errorf("--repo is required")}
	}
	cfg, err := config.Load(*repo)
	if err != nil {
		return exitError{2, err}
	}
	gitCheck := exec.Command("git", "-C", cfg.RepoPath, "rev-parse", "--show-toplevel")
	rootOutput, err := gitCheck.CombinedOutput()
	if err != nil {
		return fmt.Errorf("repository validation failed: %w: %s", err, strings.TrimSpace(string(rootOutput)))
	}
	gitRoot, err := config.CanonicalRepoPath(strings.TrimSpace(string(rootOutput)))
	if err != nil {
		return err
	}
	if gitRoot != cfg.RepoPath {
		return fmt.Errorf("--repo must point to the Git repository root: %s", gitRoot)
	}
	installed := filepath.Join(l.BinDir, "agent-loop")
	if _, err := os.Stat(installed); err != nil {
		return fmt.Errorf("agent-loop is not installed at %s; run agent-loop install first", installed)
	}
	entry, err := (registry.Store{Path: l.RegistryPath}).Add(cfg)
	if err != nil {
		return err
	}
	store := state.Store{Dir: l.RepoDir(entry.RepoID), RepoID: entry.RepoID, RepoPath: entry.RepoPath, Secrets: cfg.RedactionValues()}
	if err := store.Initialize(); err != nil {
		return err
	}
	if err := (launchd.Manager{Layout: l}).WritePlist(entry, installed); err != nil {
		return err
	}
	if cfg.Webhook.Enabled() {
		if err := (launchd.Manager{Layout: l}).WriteBrokerPlist(installed, entry.EnvironmentPath); err != nil {
			return err
		}
	}
	return a.output(*jsonOut, map[string]any{"entry": entry, "plist": l.PlistPath(entry.RepoID)})
}

func (a App) unregister(ctx context.Context, l layout.Layout, args []string) error {
	entry, jsonOut, err := a.resolve(l, "unregister", args)
	if err != nil {
		return err
	}
	lm := launchd.Manager{Layout: l, Launchctl: entry.Commands["launchctl"]}
	if err := lm.Stop(ctx, entry); err != nil {
		return err
	}
	if _, err := stopEntryWorkers(ctx, l, entry, "worker canceled by unregister", a.ProcessController); err != nil {
		return err
	}
	if err := (registry.Store{Path: l.RegistryPath}).Remove(entry.RepoID); err != nil {
		return err
	}
	if err := os.Remove(l.PlistPath(entry.RepoID)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	registered, loadErr := (registry.Store{Path: l.RegistryPath}).Load()
	if loadErr != nil {
		return loadErr
	}
	webhookRemaining := false
	for _, remaining := range registered.Repos {
		remainingConfig, configErr := config.Load(remaining.RepoPath)
		if configErr != nil {
			return fmt.Errorf("cannot determine shared broker ownership for %s: %w", remaining.RepoID, configErr)
		}
		if remainingConfig.Webhook.Enabled() {
			webhookRemaining = true
			break
		}
	}
	if !webhookRemaining {
		if err := lm.StopBroker(ctx); err != nil {
			return err
		}
		if err := os.Remove(l.BrokerPlistPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	} else if brokerStatus, statusErr := lm.BrokerStatus(ctx); statusErr != nil {
		return statusErr
	} else if brokerStatus.Loaded {
		if err := lm.RestartBroker(ctx); err != nil {
			return err
		}
	}
	return a.output(jsonOut, map[string]any{"unregistered": entry.RepoID, "state_preserved": true})
}

func (a App) control(ctx context.Context, l layout.Layout, command string, args []string) error {
	entry, jsonOut, err := a.resolve(l, command, args)
	if err != nil {
		return err
	}
	cfg, err := config.Load(entry.RepoPath)
	if err != nil {
		return err
	}
	lm := launchd.Manager{Layout: l, Launchctl: entry.Commands["launchctl"]}
	store := state.Store{Dir: l.RepoDir(entry.RepoID), RepoID: entry.RepoID, RepoPath: entry.RepoPath, Secrets: cfg.RedactionValues()}
	if command == "start" || command == "restart" {
		snapshot, loadErr := store.Load()
		if loadErr != nil {
			return fmt.Errorf("load durable state before %s: %w", command, loadErr)
		}
		if semanticErr := state.ValidateSemanticContract(snapshot); semanticErr != nil {
			return exitError{4, fmt.Errorf("refuse %s before launchd mutation: %w; run agent-loop migrate --json", command, semanticErr)}
		}
	}
	stopReport := supervisor.WorkerStopReport{Workers: []supervisor.WorkerStop{}}
	switch command {
	case "start":
		var launchStatus launchd.Status
		launchStatus, err = lm.Status(ctx, entry)
		if err == nil && !launchStatus.Loaded {
			err = recordSupervisorControl(store, "starting", "start requested")
		}
		if err == nil && cfg.Webhook.Enabled() {
			if _, statErr := os.Stat(l.BrokerPlistPath()); statErr != nil {
				err = fmt.Errorf("webhook broker LaunchAgent is not registered: %w", statErr)
			} else {
				// StartBroker is a no-op for a healthy loaded broker. In particular,
				// starting one already-loaded repository never restarts sibling
				// supervisors or a healthy shared broker.
				err = lm.StartBroker(ctx)
			}
		}
		if err == nil {
			err = lm.Start(ctx, entry)
		}
	case "stop":
		err = lm.Stop(ctx, entry)
		if err == nil {
			stopReport, err = supervisor.StopWorkers(ctx, store, cfg.Worker.TimeoutGrace.Duration, "worker canceled by explicit stop", a.ProcessController)
		}
	case "restart":
		err = lm.Stop(ctx, entry)
		if err == nil {
			stopReport, err = supervisor.StopWorkers(ctx, store, cfg.Worker.TimeoutGrace.Duration, "worker canceled by supervisor restart", a.ProcessController)
		}
		if err == nil {
			err = recordSupervisorControl(store, "starting", "restart requested")
		}
		if err == nil && cfg.Webhook.Enabled() {
			err = lm.RestartBroker(ctx)
		}
		if err == nil {
			err = lm.Start(ctx, entry)
		}
	}
	if err != nil {
		if command == "start" || command == "restart" {
			_ = recordSupervisorControl(store, "stopped", "start failed: "+err.Error())
		}
		return err
	}
	if command == "stop" {
		_, err = store.Update("supervisor_stopped", 0, "", map[string]string{"reason": "explicit stop"}, func(s *state.Snapshot) error {
			s.Supervisor.State, s.Supervisor.PID, s.Supervisor.Message = "stopped", 0, "explicit stop"
			return nil
		})
		if err != nil {
			return err
		}
	}
	status, _ := lm.Status(ctx, entry)
	result := map[string]any{"repo_id": entry.RepoID, "command": command, "launchd": status}
	if command == "stop" || command == "restart" {
		result["worker_stop"] = stopReport
	}
	return a.output(jsonOut, result)
}

func (a App) runBroker(ctx context.Context, l layout.Layout, args []string) error {
	fs := flag.NewFlagSet("broker", flag.ContinueOnError)
	fs.SetOutput(a.Err)
	if err := fs.Parse(args); err != nil {
		return exitError{2, err}
	}
	if fs.NArg() != 0 {
		return exitError{2, fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))}
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

func recordSupervisorControl(store state.Store, supervisorState state.SupervisorState, message string) error {
	_, err := store.Update("supervisor_"+string(supervisorState), 0, "", map[string]string{"reason": message}, func(s *state.Snapshot) error {
		s.Supervisor.State = supervisorState
		s.Supervisor.PID = 0
		s.Supervisor.Message = message
		s.Supervisor.FailureKind = ""
		s.Supervisor.ConsecutiveFailures = 0
		s.Supervisor.RetryAfter = nil
		s.Supervisor.RateLimit = nil
		return nil
	})
	return err
}
