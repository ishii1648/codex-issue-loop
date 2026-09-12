package hostcli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	meta "github.com/ishii1648/codex-issue-loop/internal/platform/deliverymeta"
	"github.com/ishii1648/codex-issue-loop/internal/platform/launchd"
	"github.com/ishii1648/codex-issue-loop/internal/platform/layout"
)

func hostManager(l layout.Layout) launchd.Manager { return launchd.Manager{Layout: l} }

func (a App) delivery(ctx context.Context, l layout.Layout, args []string) int {
	fail := func(err error) int { fmt.Fprintln(a.Err, err); return 1 }
	if len(args) < 2 {
		return fail(errors.New("delivery subcommand is required"))
	}
	if args[1] == "assignment" {
		if len(args) < 3 {
			return fail(errors.New("assignment subcommand is required"))
		}
		for _, arg := range args {
			if arg == "--config" || len(arg) > 9 && arg[:9] == "--config=" {
				return fail(errors.New("repository assignment commands require the authoritative default config"))
			}
		}
		return a.repository(ctx, l, args)
	}
	fs := flag.NewFlagSet("delivery "+args[1], flag.ContinueOnError)
	fs.SetOutput(a.Err)
	apply := fs.Bool("apply", false, "apply host delivery configuration")
	fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args[2:]); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		return 2
	}
	path, err := meta.DefaultConfigPath()
	if err != nil {
		return fail(err)
	}
	if *apply && args[1] != "configure" {
		return fail(errors.New("--apply is only valid with delivery configure"))
	}
	if *apply || args[1] == "pause" || args[1] == "resume" {
		lock, err := meta.AcquireLock(meta.RuntimePaths(l.Root).Lock)
		if err != nil {
			return fail(err)
		}
		defer lock.Close()
	}
	cfg, err := meta.LoadConfig(path)
	if err != nil {
		if args[1] == "configure" && errors.Is(err, os.ErrNotExist) {
			cfg = meta.DefaultConfig("ishii1648/codex-issue-loop")
		} else {
			return fail(err)
		}
	}
	switch args[1] {
	case "configure":
		if *apply {
			installed, err := meta.ReadHostInstallation(l)
			if err != nil {
				return fail(err)
			}
			digest, err := meta.FileDigest(filepath.Join(l.BinDir, "agent-loopctl"))
			if err != nil {
				return fail(err)
			}
			if digest != installed.Digest {
				return fail(errors.New("installed host binary digest mismatch"))
			}
			if err := meta.WriteConfig(path, cfg); err != nil {
				return fail(err)
			}
			manager := hostManager(l)
			if err := manager.WriteDeliveryPlist(filepath.Join(l.BinDir, "agent-loopctl"), os.Getenv("PATH"), cfg.PollDuration()); err != nil {
				return fail(err)
			}
			status, err := manager.DeliveryStatus(ctx)
			if err != nil {
				return fail(err)
			}
			if status.Loaded {
				if err := manager.StopDelivery(ctx); err != nil {
					return fail(err)
				}
			}
			if err := manager.StartDelivery(ctx); err != nil {
				return fail(err)
			}
		}
	case "pause", "resume":
		if _, err := os.Lstat(meta.RuntimePaths(l.Root).Maintenance); err == nil {
			return fail(errors.New("cannot change delivery while a maintenance transaction is active"))
		} else if !errors.Is(err, os.ErrNotExist) {
			return fail(err)
		}
		cfg.Enabled = args[1] == "resume"
		if err := meta.WriteConfig(path, cfg); err != nil {
			return fail(err)
		}
	case "check", "reconcile":
		if cfg.Enabled {
			ref, err := (meta.Verifier{CacheDir: meta.RuntimePaths(l.Root).Cache}).Discover(ctx, cfg)
			if err != nil {
				return fail(err)
			}
			if err := json.NewEncoder(a.Out).Encode(map[string]any{"available": ref, "applied": false}); err != nil {
				return fail(err)
			}
			return 0
		}
	case "status":
	default:
		return fail(errors.New("host-wide runtime replacement is disabled; use delivery assignment preview/apply --repo"))
	}
	result := map[string]any{"config": cfg, "applied": *apply}
	switch args[1] {
	case "configure":
		result["config_path"] = path
		result["runtime_root"] = l.DeliveryDir()
		result["plist"] = l.DeliveryPlistPath()
		result["label"] = l.DeliveryLabel()
	case "pause", "resume":
		result = map[string]any{"enabled": cfg.Enabled, "config_path": path}
	}
	if err := json.NewEncoder(a.Out).Encode(result); err != nil {
		return fail(err)
	}
	return 0
}

func activateHostServices(ctx context.Context, l layout.Layout) error {
	manager := hostManager(l)
	binary := filepath.Join(l.BinDir, "agent-loopctl")
	for _, kind := range []string{"broker", "delivery"} {
		path := l.BrokerPlistPath()
		if kind == "delivery" {
			path = l.DeliveryPlistPath()
		}
		_, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		var program string
		if kind == "broker" {
			program, err = manager.BrokerProgram()
		} else {
			program, err = manager.DeliveryProgram()
		}
		if err != nil {
			return err
		}
		if program == binary {
			continue
		}
		var status launchd.Status
		if kind == "broker" {
			status, err = manager.BrokerStatus(ctx)
		} else {
			status, err = manager.DeliveryStatus(ctx)
		}
		if err != nil {
			return err
		}
		if status.Loaded {
			if kind == "broker" {
				err = manager.StopBroker(ctx)
			} else {
				err = manager.StopDelivery(ctx)
			}
			if err != nil {
				return err
			}
		}
		if kind == "broker" {
			err = manager.WriteBrokerPlist(binary, os.Getenv("PATH"))
		} else {
			configPath, pathErr := meta.DefaultConfigPath()
			if pathErr != nil {
				return pathErr
			}
			cfg, loadErr := meta.LoadConfig(configPath)
			if loadErr != nil {
				return loadErr
			}
			err = manager.WriteDeliveryPlist(binary, os.Getenv("PATH"), cfg.PollDuration())
		}
		if err != nil {
			return err
		}
		if status.Loaded {
			if kind == "broker" {
				err = manager.StartBroker(ctx)
			} else {
				err = manager.StartDelivery(ctx)
			}
			if err != nil {
				return err
			}
		}
	}
	return nil
}
