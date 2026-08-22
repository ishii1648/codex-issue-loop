package app

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/ishii1648/codex-issue-loop/internal/delivery"
	"github.com/ishii1648/codex-issue-loop/internal/launchd"
	"github.com/ishii1648/codex-issue-loop/internal/layout"
)

func (a App) delivery(ctx context.Context, l layout.Layout, args []string) error {
	if len(args) == 0 {
		return exitError{2, errors.New("delivery subcommand is required: configure, check, status, reconcile, apply, recover-rollback, pause, resume")}
	}
	switch args[0] {
	case "configure":
		return a.deliveryConfigure(ctx, l, args[1:])
	case "check", "status", "reconcile", "apply":
		return a.deliveryOperation(ctx, l, args[0], args[1:])
	case "recover-rollback":
		return a.deliveryRecoverRollback(ctx, l, args[1:])
	case "pause", "resume":
		return a.deliveryEnabled(l, args[0], args[1:])
	default:
		return exitError{2, fmt.Errorf("unknown delivery subcommand %q", args[0])}
	}
}

func (a App) deliveryRecoverRollback(ctx context.Context, l layout.Layout, args []string) error {
	fs := flag.NewFlagSet("delivery recover-rollback", flag.ContinueOnError)
	fs.SetOutput(a.Err)
	configFlag := fs.String("config", "", "absolute config path (tests and explicit operations only)")
	confirm := fs.Bool("confirm-restored-baseline", false, "confirm verified previous installation recovery")
	jsonOut := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return exitError{2, err}
	}
	if fs.NArg() != 0 {
		return exitError{2, fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))}
	}
	path, err := delivery.ResolveConfigPath(*configFlag)
	if err != nil {
		return exitError{2, err}
	}
	report, err := (delivery.Controller{Layout: l, ConfigPath: path}).RecoverRollback(ctx, *confirm)
	if outputErr := a.output(*jsonOut, report); outputErr != nil {
		return outputErr
	}
	return err
}

func (a App) deliveryConfigure(ctx context.Context, l layout.Layout, args []string) error {
	fs := flag.NewFlagSet("delivery configure", flag.ContinueOnError)
	fs.SetOutput(a.Err)
	configFlag := fs.String("config", "", "absolute config path (tests and explicit operations only)")
	repository := fs.String("repository", "ishii1648/codex-issue-loop", "production Release repository")
	apply := fs.Bool("apply", false, "write config and LaunchAgent")
	jsonOut := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return exitError{2, err}
	}
	if fs.NArg() != 0 {
		return exitError{2, fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))}
	}
	path, err := delivery.ResolveConfigPath(*configFlag)
	if err != nil {
		return exitError{2, err}
	}
	cfg := delivery.DefaultConfig(*repository)
	if *repository == "ishii1648/codex-issue-loop" {
		if existing, loadErr := delivery.LoadConfig(path); loadErr == nil {
			cfg = existing
		} else if !errors.Is(unwrapPathError(loadErr), os.ErrNotExist) {
			if _, statErr := os.Lstat(path); statErr == nil {
				return loadErr
			}
		}
	}
	result := map[string]any{"applied": false, "config_path": path, "runtime_root": l.DeliveryDir(), "plist": l.DeliveryPlistPath(), "label": l.DeliveryLabel(), "config": cfg}
	if !*apply {
		return a.output(*jsonOut, result)
	}
	defaultPath, defaultErr := delivery.DefaultConfigPath()
	if defaultErr != nil {
		return defaultErr
	}
	if filepath.Clean(path) != filepath.Clean(defaultPath) {
		return exitError{2, errors.New("delivery configure --apply must use the default $HOME/.agent-loop-delivery.yaml path; --config is only for tests and explicit one-shot operations")}
	}
	binary := filepath.Join(l.BinDir, "agent-loop")
	if info, err := os.Stat(binary); err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("managed agent-loop installation is required before delivery configure --apply")
	}
	if err := delivery.WriteConfig(path, cfg); err != nil {
		return err
	}
	manager := launchd.Manager{Layout: l}
	if err := manager.WriteDeliveryPlist(binary, os.Getenv("PATH"), cfg.PollDuration()); err != nil {
		return err
	}
	status, err := manager.DeliveryStatus(ctx)
	if err != nil {
		return err
	}
	if status.Loaded {
		if err := manager.StopDelivery(ctx); err != nil {
			return err
		}
	}
	if err := manager.StartDelivery(ctx); err != nil {
		return err
	}
	result["applied"] = true
	return a.output(*jsonOut, result)
}

func (a App) deliveryOperation(ctx context.Context, l layout.Layout, operation string, args []string) error {
	fs := flag.NewFlagSet("delivery "+operation, flag.ContinueOnError)
	fs.SetOutput(a.Err)
	configFlag := fs.String("config", "", "absolute config path (tests and explicit operations only)")
	jsonOut := fs.Bool("json", false, "emit JSON")
	expectedVersion := fs.String("version", "", "exact production version to apply")
	if err := fs.Parse(args); err != nil {
		return exitError{2, err}
	}
	if fs.NArg() != 0 {
		return exitError{2, fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))}
	}
	path, err := delivery.ResolveConfigPath(*configFlag)
	if err != nil {
		return exitError{2, err}
	}
	ghPath := "gh"
	if resolved, lookErr := exec.LookPath("gh"); lookErr == nil {
		ghPath = resolved
	}
	if operation != "apply" && *expectedVersion != "" {
		return exitError{2, errors.New("--version is only valid with delivery apply")}
	}
	if *expectedVersion != "" {
		if _, parseErr := delivery.ParseSemVer(*expectedVersion); parseErr != nil {
			return exitError{2, parseErr}
		}
	}
	controller := delivery.Controller{Layout: l, ConfigPath: path, GH: ghPath, ExpectedVersion: *expectedVersion}
	var report delivery.Report
	switch operation {
	case "check":
		report, err = controller.Check(ctx)
	case "status":
		report, err = controller.Status()
	case "reconcile":
		report, err = controller.Reconcile(ctx, false)
	case "apply":
		report, err = controller.Reconcile(ctx, true)
	}
	if outputErr := a.output(*jsonOut, report); outputErr != nil {
		return outputErr
	}
	return err
}

func (a App) deliveryEnabled(l layout.Layout, operation string, args []string) error {
	fs := flag.NewFlagSet("delivery "+operation, flag.ContinueOnError)
	fs.SetOutput(a.Err)
	configFlag := fs.String("config", "", "absolute config path")
	jsonOut := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return exitError{2, err}
	}
	if fs.NArg() != 0 {
		return exitError{2, fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))}
	}
	path, err := delivery.ResolveConfigPath(*configFlag)
	if err != nil {
		return exitError{2, err}
	}
	cfg, err := delivery.LoadConfig(path)
	if err != nil {
		return err
	}
	paths := delivery.RuntimePaths(l.Root)
	if _, statErr := os.Stat(paths.Maintenance); statErr == nil {
		return errors.New("cannot pause or resume while a delivery maintenance transaction is active")
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return statErr
	}
	cfg.Enabled = operation == "resume"
	if err := delivery.WriteConfig(path, cfg); err != nil {
		return err
	}
	return a.output(*jsonOut, map[string]any{"enabled": cfg.Enabled, "config_path": path})
}

func unwrapPathError(err error) error {
	var pathErr *os.PathError
	if errors.As(err, &pathErr) {
		return pathErr.Err
	}
	return err
}
