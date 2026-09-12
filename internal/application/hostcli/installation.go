package hostcli

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	assets "github.com/ishii1648/codex-issue-loop"
	meta "github.com/ishii1648/codex-issue-loop/internal/platform/deliverymeta"
	"github.com/ishii1648/codex-issue-loop/internal/platform/fsutil"
	"github.com/ishii1648/codex-issue-loop/internal/platform/layout"
	"github.com/ishii1648/codex-issue-loop/internal/platform/registry"
)

const retiredCLI = "#!/bin/sh\nprintf '%s\\n' 'The host CLI is now agent-loopctl. Run agent-loopctl with the same repository selection.' >&2\nexit 2\n"

func (a App) installation(ctx context.Context, l layout.Layout, args []string) error {
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	fs.SetOutput(a.Err)
	fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("unexpected installation arguments")
	}
	lock, err := meta.AcquireLock(meta.RuntimePaths(l.Root).Lock)
	if err != nil {
		return err
	}
	defer lock.Close()
	current, loadErr := meta.ReadHostInstallation(l)
	if loadErr != nil && !errors.Is(loadErr, os.ErrNotExist) {
		return loadErr
	}
	switch args[0] {
	case "rollback":
		if loadErr != nil {
			return loadErr
		}
		if current.Previous == "" {
			return errors.New("no host installation backup")
		}
		return a.restoreHost(l, current)
	case "uninstall":
		if loadErr != nil {
			return loadErr
		}
		registered, err := (registry.Store{Path: l.RegistryPath}).Load()
		if err != nil {
			return err
		}
		if len(registered.Repos) != 0 {
			return errors.New("unregister repositories before removing the host CLI")
		}
		manager := hostManager(l)
		for _, kind := range []string{"broker", "delivery"} {
			path := l.BrokerPlistPath()
			if kind == "delivery" {
				path = l.DeliveryPlistPath()
			}
			if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
				continue
			} else if err != nil {
				return err
			}
			var err error
			if kind == "broker" {
				err = manager.StopBroker(ctx)
			} else {
				err = manager.StopDelivery(ctx)
			}
			if err != nil {
				return err
			}
			if err := os.Remove(path); err != nil {
				return err
			}
		}
		for _, path := range []string{filepath.Join(l.SkillsDir, "agent-loop", "VERSION"), filepath.Join(l.BinDir, "agent-loopctl"), filepath.Join(l.BinDir, "agent-loop"), filepath.Join(l.Root, "host-install.json"), filepath.Join(l.SkillsDir, "agent-loop", "SKILL.md")} {
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		return json.NewEncoder(a.Out).Encode(map[string]any{"uninstalled": true})
	case "update":
		if loadErr != nil {
			return fmt.Errorf("install the host CLI before update: %w", loadErr)
		}
	}
	source, err := os.Executable()
	if err != nil {
		return err
	}
	data, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(data))
	if loadErr == nil && current.Digest == digest {
		if err := activateHostServices(ctx, l); err != nil {
			return err
		}
		return json.NewEncoder(a.Out).Encode(map[string]any{"changed": false})
	}
	bootstrap := current.Bootstrap
	if loadErr != nil {
		path, err := meta.DefaultConfigPath()
		if err != nil {
			return err
		}
		cfg, err := meta.LoadConfig(path)
		if errors.Is(err, os.ErrNotExist) {
			cfg = meta.DefaultConfig("ishii1648/codex-issue-loop")
		} else if err != nil {
			return err
		}
		candidate, err := (meta.Verifier{CacheDir: meta.RuntimePaths(l.Root).Cache, ExpectedVersion: Version}).Check(ctx, cfg)
		if err != nil {
			return err
		}
		if candidate.Manifest.Version != Version || candidate.Manifest.Commit != Commit {
			return errors.New("bootstrap release differs from the host CLI version and commit")
		}
		bootstrap = meta.SlotRef(l, candidate.Manifest.Version, candidate.Manifest.Commit, candidate.Digest)
		if err := meta.StageSlot(l, bootstrap, filepath.Join(candidate.Dir, meta.BinaryAsset)); err != nil {
			return err
		}
		if err := a.probe(ctx, bootstrap); err != nil {
			return err
		}
		registered, err := (registry.Store{Path: l.RegistryPath}).Load()
		if err != nil {
			return err
		}
		for _, entry := range registered.Repos {
			_, ref, err := meta.ResolveAssignment(l, entry.RepoPath)
			if err != nil {
				return err
			}
			if err := a.probe(ctx, ref.AssignmentRef); err != nil {
				return fmt.Errorf("repository %s must support host dispatch before switching the common CLI: %w", entry.RepoID, err)
			}
			program, err := hostManager(l).Program(entry)
			if err != nil {
				return err
			}
			if program != ref.Slot {
				return errors.New("migrate repository LaunchAgents to immutable assignments before installing agent-loopctl")
			}
		}
		if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
			if err := meta.WriteConfig(path, cfg); err != nil {
				return err
			}
		}
	}
	next := meta.HostInstallation{Format: 1, Version: Version, Commit: Commit, Digest: digest, SkillDigest: fmt.Sprintf("%x", sha256.Sum256(assets.AgentLoopSkill)), Bootstrap: bootstrap}
	if loadErr == nil {
		backup := filepath.Join(l.Root, "host-backups", current.Digest)
		if err := meta.EnsurePrivateDirectory(filepath.Dir(backup)); err != nil {
			return err
		}
		if err := meta.EnsurePrivateDirectory(backup); err != nil {
			return err
		}
		for src, name := range map[string]string{filepath.Join(l.BinDir, "agent-loopctl"): "agent-loopctl", filepath.Join(l.Root, "host-install.json"): "host-install.json", filepath.Join(l.SkillsDir, "agent-loop", "SKILL.md"): "SKILL.md"} {
			content, err := fsutil.ReadPrivateRegular(src, "installed host artifact")
			if err != nil {
				return err
			}
			if name == "agent-loopctl" && fmt.Sprintf("%x", sha256.Sum256(content)) != current.Digest || name == "SKILL.md" && fmt.Sprintf("%x", sha256.Sum256(content)) != current.SkillDigest {
				return errors.New("installed host artifact digest mismatch")
			}
			if err := fsutil.WriteFile(filepath.Join(backup, name), content, 0o600); err != nil {
				return err
			}
		}
		next.Previous = backup
	}
	if err := writeHost(l, data, assets.AgentLoopSkill, next); err != nil {
		return err
	}
	if err := activateHostServices(ctx, l); err != nil {
		return err
	}
	return json.NewEncoder(a.Out).Encode(map[string]any{"changed": true, "binary": filepath.Join(l.BinDir, "agent-loopctl"), "manifest": next})
}

func writeHost(l layout.Layout, binary, skill []byte, manifest meta.HostInstallation) error {
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	type artifact struct {
		path               string
		data, previous     []byte
		mode, previousMode os.FileMode
		existed            bool
	}
	files := []artifact{
		{path: filepath.Join(l.BinDir, "agent-loopctl"), data: binary, mode: 0o700},
		{path: filepath.Join(l.SkillsDir, "agent-loop", "SKILL.md"), data: skill, mode: 0o600},
		{path: filepath.Join(l.SkillsDir, "agent-loop", "VERSION"), data: []byte(manifest.Version + "\n"), mode: 0o600},
		{path: filepath.Join(l.BinDir, "agent-loop"), data: []byte(retiredCLI), mode: 0o700},
		{path: filepath.Join(l.Root, "host-install.json"), data: data, mode: 0o600},
	}
	for i := range files {
		info, err := os.Lstat(files[i].path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("host artifact is not a regular file: %s", files[i].path)
		}
		files[i].previous, err = os.ReadFile(files[i].path)
		if err != nil {
			return err
		}
		files[i].existed = true
		files[i].previousMode = info.Mode().Perm()
	}
	for i, file := range files {
		if err := fsutil.WriteFile(file.path, file.data, file.mode); err != nil {
			for j := i - 1; j >= 0; j-- {
				var restoreErr error
				if files[j].existed {
					restoreErr = fsutil.WriteFile(files[j].path, files[j].previous, files[j].previousMode)
				} else {
					restoreErr = os.Remove(files[j].path)
				}
				err = errors.Join(err, restoreErr)
			}
			return err
		}
	}
	return nil
}

func (a App) restoreHost(l layout.Layout, current meta.HostInstallation) error {
	parent := filepath.Join(l.Root, "host-backups")
	if filepath.Dir(current.Previous) != parent {
		return errors.New("backup is outside host-backups")
	}
	info, err := os.Lstat(current.Previous)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
		return errors.New("host backup must be an owner-only regular directory")
	}
	data, err := fsutil.ReadPrivateRegular(filepath.Join(current.Previous, "host-install.json"), "host backup manifest")
	if err != nil {
		return err
	}
	var previous meta.HostInstallation
	if err := fsutil.DecodeStrictJSON(data, &previous); err != nil {
		return err
	}
	binary, err := fsutil.ReadPrivateRegular(filepath.Join(current.Previous, "agent-loopctl"), "host backup binary")
	if err != nil {
		return err
	}
	skill, err := fsutil.ReadPrivateRegular(filepath.Join(current.Previous, "SKILL.md"), "host backup skill")
	if err != nil {
		return err
	}
	if previous.Format != 1 || fmt.Sprintf("%x", sha256.Sum256(binary)) != previous.Digest || fmt.Sprintf("%x", sha256.Sum256(skill)) != previous.SkillDigest || filepath.Base(current.Previous) != previous.Digest {
		return errors.New("host backup digest mismatch")
	}
	previous.Bootstrap = current.Bootstrap
	previous.Previous = ""
	if err := writeHost(l, binary, skill, previous); err != nil {
		return err
	}
	return json.NewEncoder(a.Out).Encode(map[string]any{"rolled_back": true, "version": previous.Version})
}

func (a App) bootstrap(ctx context.Context, l layout.Layout, args []string) int {
	fail := func(err error) int { fmt.Fprintln(a.Err, err); return 1 }
	if args[0] == "register" {
		repo, err := meta.RepositoryArgument(args)
		if err != nil {
			return fail(err)
		}
		if repo == "" {
			return fail(errors.New("register requires --repo"))
		}
		if _, err := (registry.Store{Path: l.RegistryPath}).Resolve(repo, ""); err == nil {
			return a.repository(ctx, l, args)
		}
		args = append([]string{"bootstrap"}, args...)
	}
	installed, err := meta.ReadHostInstallation(l)
	if err != nil {
		return fail(err)
	}
	if err := a.probe(ctx, installed.Bootstrap); err != nil {
		return fail(err)
	}
	return a.execute(ctx, installed.Bootstrap.Slot, args)
}
