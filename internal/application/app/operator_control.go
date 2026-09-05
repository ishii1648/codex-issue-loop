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
	"time"

	"github.com/ishii1648/codex-issue-loop/internal/adapter/state"
	"github.com/ishii1648/codex-issue-loop/internal/adapter/webhook"
	"github.com/ishii1648/codex-issue-loop/internal/application/delivery"
	"github.com/ishii1648/codex-issue-loop/internal/application/drain"
	"github.com/ishii1648/codex-issue-loop/internal/application/operatorcontrol"
	"github.com/ishii1648/codex-issue-loop/internal/application/supervisor"
	"github.com/ishii1648/codex-issue-loop/internal/platform/config"
	"github.com/ishii1648/codex-issue-loop/internal/platform/fsutil"
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
	registryStore := registry.Store{Path: l.RegistryPath}
	registryBefore, err := registryStore.Load()
	if err != nil {
		return err
	}
	entry, err := registryStore.Add(cfg)
	if err != nil {
		return err
	}
	assignmentPath, err := delivery.DefaultConfigPath()
	if err != nil {
		_ = fsutil.WriteJSON(l.RegistryPath, registryBefore, 0o600)
		return err
	}
	assignment, assignmentManaged, err := (delivery.AssignmentController{Layout: l, ConfigPath: assignmentPath}).EnsureRepositoryAssignment(entry)
	if err != nil {
		_ = fsutil.WriteJSON(l.RegistryPath, registryBefore, 0o600)
		return err
	}
	store := state.Store{Dir: l.RepoDir(entry.RepoID), RepoID: entry.RepoID, RepoPath: entry.RepoPath, Secrets: cfg.RedactionValues()}
	if err := store.Initialize(); err != nil {
		return err
	}
	runtimeBinary := installed
	if assignmentManaged {
		runtimeBinary = assignment.Slot
	}
	if err := (launchd.Manager{Layout: l}).WritePlist(entry, runtimeBinary); err != nil {
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
	registryStore := registry.Store{Path: l.RegistryPath}
	registryBefore, err := registryStore.Load()
	if err != nil {
		return err
	}
	if err := lm.Stop(ctx, entry); err != nil {
		return err
	}
	if _, err := stopEntryWorkers(ctx, l, entry, "worker canceled by unregister", a.ProcessController); err != nil {
		return err
	}
	if err := registryStore.Remove(entry.RepoID); err != nil {
		return err
	}
	assignmentPath, err := delivery.DefaultConfigPath()
	if err != nil {
		_ = fsutil.WriteJSON(l.RegistryPath, registryBefore, 0o600)
		return err
	}
	if _, err := (delivery.AssignmentController{Layout: l, ConfigPath: assignmentPath}).RemoveRepositoryAssignment(entry.RepoID); err != nil {
		_ = fsutil.WriteJSON(l.RegistryPath, registryBefore, 0o600)
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
	entry, options, err := a.resolveControl(l, command, args)
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
	switch command {
	case "start":
		tx, loadErr := operatorcontrol.Load(l.OperatorControlPath(entry.RepoID))
		if loadErr != nil {
			return loadErr
		}
		if tx.Active() || drain.Requested(l.OperatorMaintenanceFencePath(entry.RepoID)) {
			return exitError{4, errors.New("an interrupted stop/restart drain is active; resume the recorded operation or use its explicit --force form")}
		}
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
	case "stop", "restart":
		return a.drainControl(ctx, l, entry, cfg, lm, store, command, options)
	}
	if err != nil {
		if command == "start" {
			_ = recordSupervisorControl(store, "stopped", "start failed: "+err.Error())
		}
		return err
	}
	status, _ := lm.Status(ctx, entry)
	result := map[string]any{"repo_id": entry.RepoID, "command": command, "launchd": status}
	return a.output(options.json, result)
}

type controlOptions struct {
	json    bool
	force   bool
	timeout time.Duration
}

func (a App) resolveControl(l layout.Layout, command string, args []string) (registry.Entry, controlOptions, error) {
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	fs.SetOutput(a.Err)
	repo := fs.String("repo", "", "repository path")
	jsonOut := fs.Bool("json", false, "emit JSON")
	force := fs.Bool("force", false, "terminate active workers before stopping")
	timeout := fs.Duration("timeout", 0, "graceful drain timeout")
	if err := fs.Parse(args); err != nil {
		return registry.Entry{}, controlOptions{}, exitError{2, err}
	}
	if command == "start" && (*force || *timeout != 0) {
		return registry.Entry{}, controlOptions{}, exitError{2, errors.New("--force and --timeout are only valid for stop/restart")}
	}
	if *timeout < 0 {
		return registry.Entry{}, controlOptions{}, exitError{2, errors.New("--timeout must be positive")}
	}
	entry, err := a.resolvePath(l, *repo)
	return entry, controlOptions{json: *jsonOut, force: *force, timeout: *timeout}, err
}

func (a App) drainControl(ctx context.Context, l layout.Layout, entry registry.Entry, cfg config.Config, lm launchd.Manager, store state.Store, command string, options controlOptions) error {
	hostLock, err := delivery.AcquireLock(delivery.RuntimePaths(l.Root).Lock)
	if err != nil {
		return err
	}
	defer hostLock.Close()
	lock, err := delivery.AcquireLock(l.OperatorControlLockPath(entry.RepoID))
	if err != nil {
		return err
	}
	defer lock.Close()
	if drain.Requested(filepath.Join(l.DeliveryDir(), "maintenance.json"), l.DeliveryAssignmentFencePath(entry.RepoID)) {
		return errors.New("cannot stop or restart while a delivery maintenance transaction is active")
	}

	operation := operatorcontrol.Operation(command)
	txPath := l.OperatorControlPath(entry.RepoID)
	fencePath := l.OperatorMaintenanceFencePath(entry.RepoID)
	tx, err := operatorcontrol.Load(txPath)
	if err != nil {
		return err
	}
	if tx.Active() && tx.Operation != operation {
		return fmt.Errorf("operator %s generation %s is active; finish it before requesting %s", tx.Operation, tx.Generation, command)
	}
	initialSnapshot, err := store.Load()
	if err != nil {
		return err
	}
	if options.force {
		return a.forceControl(ctx, l, entry, cfg, lm, store, operation, options, tx)
	}
	if !tx.Active() {
		drainTimeout := options.timeout
		if drainTimeout == 0 {
			drainTimeout = cfg.Worker.Timeout.Duration + cfg.Worker.TimeoutGrace.Duration
		}
		now := time.Now().UTC()
		tx = operatorcontrol.Transaction{
			Version: 1, Generation: state.NewID("operator"), Operation: operation,
			Phase: operatorcontrol.PhaseDraining, RequestedAt: now, DrainDeadline: now.Add(drainTimeout), UpdatedAt: now,
			SupervisorPID: initialSnapshot.Supervisor.PID,
			RestartBroker: operation == operatorcontrol.OperationRestart && cfg.Webhook.Enabled(),
		}
		if err := operatorcontrol.Save(txPath, tx); err != nil {
			return err
		}
	}

	status, err := lm.Status(ctx, entry)
	if err != nil {
		return err
	}
	snapshot, err := store.Load()
	if err != nil {
		return err
	}
	needsDrain := status.Loaded || snapshot.ActiveExecution != nil || drain.HasWorker(snapshot)
	if tx.Phase == operatorcontrol.PhaseDraining && needsDrain {
		if err := ensureOperatorFence(fencePath, tx); err != nil {
			return err
		}
		if err := wakeMaintenance(l, entry.RepoID, tx.Generation); err != nil {
			return err
		}
		if err := waitForOperatorDrain(ctx, store, tx.DrainDeadline); err != nil {
			if errors.Is(err, errOperatorDrainTimeout) {
				timeoutSnapshot, loadErr := store.Load()
				if loadErr != nil {
					return loadErr
				}
				if tx.SupervisorPID > 0 && timeoutSnapshot.Supervisor.PID != tx.SupervisorPID && (timeoutSnapshot.ActiveExecution != nil || drain.HasWorker(timeoutSnapshot)) {
					tx.Reason, tx.UpdatedAt = "supervisor identity changed during drain; retained work requires explicit force recovery", time.Now().UTC()
					if saveErr := operatorcontrol.Save(txPath, tx); saveErr != nil {
						return saveErr
					}
					return exitError{4, errors.New("graceful drain cannot resume an orphaned lifecycle after supervisor restart; maintenance remains active and explicit --force is required")}
				}
				if clearErr := operatorcontrol.ClearFence(fencePath); clearErr != nil {
					return fmt.Errorf("restore scheduling after drain timeout: %w", clearErr)
				}
				if wakeErr := wakeMaintenance(l, entry.RepoID, "cleared-"+tx.Generation); wakeErr != nil {
					return fmt.Errorf("wake scheduling after drain timeout: %w", wakeErr)
				}
				tx.Phase, tx.Reason, tx.CompletedAt, tx.UpdatedAt = operatorcontrol.PhaseTimedOut, err.Error(), time.Now().UTC(), time.Now().UTC()
				if saveErr := operatorcontrol.Save(txPath, tx); saveErr != nil {
					return saveErr
				}
				return exitError{4, fmt.Errorf("%w; normal scheduling was restored; retry later or use %s --force", err, command)}
			}
			tx.Reason, tx.UpdatedAt = err.Error(), time.Now().UTC()
			_ = operatorcontrol.Save(txPath, tx)
			return err
		}
	}
	if tx.Phase == operatorcontrol.PhaseDraining {
		tx.Phase, tx.UpdatedAt = operatorcontrol.PhaseStopping, time.Now().UTC()
		if err := operatorcontrol.Save(txPath, tx); err != nil {
			return err
		}
	}
	if tx.Phase == operatorcontrol.PhaseStopping {
		if err := lm.Stop(ctx, entry); err != nil {
			return err
		}
		if operation == operatorcontrol.OperationStop {
			if err := recordStopped(store, "explicit graceful stop"); err != nil {
				return err
			}
			return a.finishControl(l, entry, lm, txPath, fencePath, tx, options, supervisor.WorkerStopReport{Workers: []supervisor.WorkerStop{}})
		}
		tx.Phase, tx.UpdatedAt = operatorcontrol.PhaseStarting, time.Now().UTC()
		if err := operatorcontrol.Save(txPath, tx); err != nil {
			return err
		}
	}
	if tx.Phase == operatorcontrol.PhaseStarting {
		if err := recordSupervisorControl(store, state.SupervisorStateStarting, "graceful restart requested"); err != nil {
			return err
		}
		if tx.RestartBroker {
			tx.Phase = operatorcontrol.PhaseStoppingBroker
		} else {
			tx.Phase = operatorcontrol.PhaseStartingRepository
		}
		tx.UpdatedAt = time.Now().UTC()
		if err := operatorcontrol.Save(txPath, tx); err != nil {
			return err
		}
	}
	if tx.Phase == operatorcontrol.PhaseStoppingBroker {
		if err := lm.StopBroker(ctx); err != nil {
			return err
		}
		tx.Phase, tx.UpdatedAt = operatorcontrol.PhaseStartingBroker, time.Now().UTC()
		if err := operatorcontrol.Save(txPath, tx); err != nil {
			return err
		}
	}
	if tx.Phase == operatorcontrol.PhaseStartingBroker {
		if err := lm.StartBroker(ctx); err != nil {
			return err
		}
		tx.Phase, tx.UpdatedAt = operatorcontrol.PhaseStartingRepository, time.Now().UTC()
		if err := operatorcontrol.Save(txPath, tx); err != nil {
			return err
		}
	}
	if tx.Phase == operatorcontrol.PhaseStartingRepository {
		if err := lm.Start(ctx, entry); err != nil {
			return err
		}
		tx.Phase, tx.UpdatedAt = operatorcontrol.PhaseHealthCheck, time.Now().UTC()
		if err := operatorcontrol.Save(txPath, tx); err != nil {
			return err
		}
	}
	if tx.Phase == operatorcontrol.PhaseHealthCheck {
		if err := a.checkControlHealth(ctx, lm, entry, store, tx.RestartBroker, 10*time.Second); err != nil {
			return err
		}
		return a.finishControl(l, entry, lm, txPath, fencePath, tx, options, supervisor.WorkerStopReport{Workers: []supervisor.WorkerStop{}})
	}
	return fmt.Errorf("operator control generation %s has unsupported active phase %s", tx.Generation, tx.Phase)
}

var errOperatorDrainTimeout = errors.New("graceful drain timeout reached without signaling or killing active workers")

func waitForOperatorDrain(ctx context.Context, store state.Store, deadline time.Time) error {
	for {
		snapshot, err := store.Load()
		if err != nil {
			return err
		}
		if drain.Ready(snapshot) {
			return nil
		}
		if !time.Now().UTC().Before(deadline) {
			return errOperatorDrainTimeout
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func ensureOperatorFence(path string, tx operatorcontrol.Transaction) error {
	fence, err := operatorcontrol.LoadFence(path)
	if err == nil {
		if fence.Generation != tx.Generation || fence.Operation != tx.Operation {
			return errors.New("operator maintenance fence does not match the durable control transaction")
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return operatorcontrol.WriteFence(path, operatorcontrol.Fence{Generation: tx.Generation, Operation: tx.Operation, RequestedAt: tx.RequestedAt})
}

func wakeMaintenance(l layout.Layout, repoID, generation string) error {
	return fsutil.WriteFile(l.MaintenanceWakePath(repoID), []byte(generation+"\n"), 0o600)
}

func recordStopped(store state.Store, reason string) error {
	_, err := store.Update("supervisor_stopped", 0, "", map[string]string{"reason": reason}, func(snapshot *state.Snapshot) error {
		snapshot.Supervisor.State, snapshot.Supervisor.PID, snapshot.Supervisor.Message = state.SupervisorStateStopped, 0, reason
		return nil
	})
	return err
}

func (a App) checkControlHealth(ctx context.Context, lm launchd.Manager, entry registry.Entry, store state.Store, broker bool, timeout time.Duration) error {
	if a.controlHealth != nil {
		return a.controlHealth(ctx, lm, entry, store, broker, timeout)
	}
	return waitForControlHealth(ctx, lm, entry, store, broker, timeout)
}

func waitForControlHealth(ctx context.Context, lm launchd.Manager, entry registry.Entry, store state.Store, broker bool, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		status, err := lm.Status(ctx, entry)
		if err != nil {
			return err
		}
		healthy := status.Loaded && status.Running
		if healthy && broker {
			brokerStatus, brokerErr := lm.BrokerStatus(ctx)
			if brokerErr != nil {
				return brokerErr
			}
			healthy = brokerStatus.Loaded && brokerStatus.Running
		}
		if healthy {
			snapshot, stateErr := store.Load()
			if stateErr != nil {
				return stateErr
			}
			healthy = snapshot.Supervisor.State == state.SupervisorStateMaintenance && snapshot.Supervisor.PID > 0
		}
		if healthy {
			return nil
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return errors.New("restarted services did not become healthy; operator maintenance fence remains active")
}

func (a App) finishControl(l layout.Layout, entry registry.Entry, lm launchd.Manager, txPath, fencePath string, tx operatorcontrol.Transaction, options controlOptions, report supervisor.WorkerStopReport) error {
	if err := operatorcontrol.ClearFence(fencePath); err != nil {
		return err
	}
	if tx.Operation == operatorcontrol.OperationRestart {
		if err := wakeMaintenance(l, entry.RepoID, "cleared-"+tx.Generation); err != nil {
			return err
		}
	}
	now := time.Now().UTC()
	tx.Phase, tx.Reason, tx.CompletedAt, tx.UpdatedAt = operatorcontrol.PhaseSucceeded, "", now, now
	if err := operatorcontrol.Save(txPath, tx); err != nil {
		return err
	}
	status, _ := lm.Status(context.Background(), entry)
	return a.output(options.json, map[string]any{"repo_id": entry.RepoID, "command": string(tx.Operation), "generation": tx.Generation, "phase": tx.Phase, "launchd": status, "worker_stop": report})
}

func (a App) forceControl(ctx context.Context, l layout.Layout, entry registry.Entry, cfg config.Config, lm launchd.Manager, store state.Store, operation operatorcontrol.Operation, options controlOptions, previous operatorcontrol.Transaction) error {
	now := time.Now().UTC()
	tx := operatorcontrol.Transaction{Version: 1, Generation: state.NewID("operator"), Operation: operation, Phase: operatorcontrol.PhaseStopping, RequestedAt: now, DrainDeadline: now, UpdatedAt: now, Reason: "explicit force requested", RestartBroker: operation == operatorcontrol.OperationRestart && cfg.Webhook.Enabled()}
	if previous.Active() {
		tx.Generation, tx.RequestedAt, tx.DrainDeadline, tx.RestartBroker = previous.Generation, previous.RequestedAt, previous.DrainDeadline, previous.RestartBroker
	}
	if err := operatorcontrol.Save(l.OperatorControlPath(entry.RepoID), tx); err != nil {
		return err
	}
	if err := ensureOperatorFence(l.OperatorMaintenanceFencePath(entry.RepoID), tx); err != nil {
		return err
	}
	if err := lm.Stop(ctx, entry); err != nil {
		return err
	}
	reason := "worker canceled by explicit forced stop"
	if operation == operatorcontrol.OperationRestart {
		reason = "worker canceled by explicit forced restart"
	}
	report, err := supervisor.StopWorkers(ctx, store, cfg.Worker.TimeoutGrace.Duration, reason, a.ProcessController)
	if err != nil {
		return err
	}
	if operation == operatorcontrol.OperationStop {
		if err := recordStopped(store, "explicit forced stop"); err != nil {
			return err
		}
		return a.finishControl(l, entry, lm, l.OperatorControlPath(entry.RepoID), l.OperatorMaintenanceFencePath(entry.RepoID), tx, options, report)
	}
	tx.Phase, tx.UpdatedAt = operatorcontrol.PhaseStarting, time.Now().UTC()
	if err := operatorcontrol.Save(l.OperatorControlPath(entry.RepoID), tx); err != nil {
		return err
	}
	if err := recordSupervisorControl(store, state.SupervisorStateStarting, "forced restart requested"); err != nil {
		return err
	}
	if tx.RestartBroker {
		tx.Phase = operatorcontrol.PhaseStoppingBroker
	} else {
		tx.Phase = operatorcontrol.PhaseStartingRepository
	}
	tx.UpdatedAt = time.Now().UTC()
	if err := operatorcontrol.Save(l.OperatorControlPath(entry.RepoID), tx); err != nil {
		return err
	}
	if tx.Phase == operatorcontrol.PhaseStoppingBroker {
		if err := lm.StopBroker(ctx); err != nil {
			return err
		}
		tx.Phase, tx.UpdatedAt = operatorcontrol.PhaseStartingBroker, time.Now().UTC()
		if err := operatorcontrol.Save(l.OperatorControlPath(entry.RepoID), tx); err != nil {
			return err
		}
	}
	if tx.Phase == operatorcontrol.PhaseStartingBroker {
		if err := lm.StartBroker(ctx); err != nil {
			return err
		}
		tx.Phase, tx.UpdatedAt = operatorcontrol.PhaseStartingRepository, time.Now().UTC()
		if err := operatorcontrol.Save(l.OperatorControlPath(entry.RepoID), tx); err != nil {
			return err
		}
	}
	if tx.Phase == operatorcontrol.PhaseStartingRepository {
		if err := lm.Start(ctx, entry); err != nil {
			return err
		}
		tx.Phase, tx.UpdatedAt = operatorcontrol.PhaseHealthCheck, time.Now().UTC()
		if err := operatorcontrol.Save(l.OperatorControlPath(entry.RepoID), tx); err != nil {
			return err
		}
	}
	if err := a.checkControlHealth(ctx, lm, entry, store, tx.RestartBroker, 10*time.Second); err != nil {
		return err
	}
	return a.finishControl(l, entry, lm, l.OperatorControlPath(entry.RepoID), l.OperatorMaintenanceFencePath(entry.RepoID), tx, options, report)
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
