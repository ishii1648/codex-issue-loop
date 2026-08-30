package app

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	assets "github.com/ishii1648/codex-issue-loop"
	"github.com/ishii1648/codex-issue-loop/internal/adapter/state"
	schema "github.com/ishii1648/codex-issue-loop/internal/application/migration"
	"github.com/ishii1648/codex-issue-loop/internal/application/supervisor"
	"github.com/ishii1648/codex-issue-loop/internal/domain/statecontract"
	"github.com/ishii1648/codex-issue-loop/internal/platform/config"
	"github.com/ishii1648/codex-issue-loop/internal/platform/fsutil"
	"github.com/ishii1648/codex-issue-loop/internal/platform/launchd"
	"github.com/ishii1648/codex-issue-loop/internal/platform/layout"
	"github.com/ishii1648/codex-issue-loop/internal/platform/registry"
	schemaversion "github.com/ishii1648/codex-issue-loop/internal/platform/schema"
)

func (a App) install(ctx context.Context, l layout.Layout, args []string) error {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	fs.SetOutput(a.Err)
	jsonOut := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return exitError{2, err}
	}
	loaded, err := loadedEntries(ctx, l)
	if err != nil {
		return err
	}
	if len(loaded) > 0 {
		return fmt.Errorf("cannot install over running supervisors; use agent-loop update")
	}
	if _, brokerLoaded, brokerErr := loadedWebhookBroker(ctx, l); brokerErr != nil {
		return brokerErr
	} else if brokerLoaded {
		return fmt.Errorf("cannot install over a running webhook broker; use agent-loop update")
	}
	source, err := os.Executable()
	if err != nil {
		return err
	}
	source, err = filepath.EvalSymlinks(source)
	if err != nil {
		return err
	}
	manifest, changed, err := installArtifacts(l, source, Version, Commit)
	if err != nil {
		return err
	}
	return a.output(*jsonOut, map[string]any{"binary": filepath.Join(l.BinDir, "agent-loop"), "skill": filepath.Join(l.SkillsDir, "agent-loop", "SKILL.md"), "manifest": manifest, "changed": changed})
}

func (a App) update(ctx context.Context, l layout.Layout, args []string) error {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	fs.SetOutput(a.Err)
	jsonOut := fs.Bool("json", false, "emit JSON")
	deliveryBackup := fs.String("delivery-backup", "", "managed backup path reserved by the delivery controller")
	if err := fs.Parse(args); err != nil {
		return exitError{2, err}
	}
	schemaReport, err := schema.Inspect(l)
	if err != nil {
		return fmt.Errorf("inspect schema compatibility: %w", err)
	}
	if len(schemaReport.Unsupported) > 0 {
		return fmt.Errorf("update refused: unsupported schema versions detected; run agent-loop migrate --json")
	}
	migrationNeeded := schemaReport.NeedsMigration
	source, err := os.Executable()
	if err != nil {
		return err
	}
	source, err = filepath.EvalSymlinks(source)
	if err != nil {
		return err
	}
	match, err := installationMatches(l, source, Version, Commit)
	if err != nil {
		return err
	}
	if match {
		manifest, _ := readInstallManifest(filepath.Join(l.Root, "install.json"))
		return a.output(*jsonOut, map[string]any{"changed": false, "manifest": manifest, "schema_migration_required": migrationNeeded})
	}
	var loaded []registry.Entry
	if migrationNeeded {
		loaded, err = loadedMigrationEntries(ctx, l, schemaReport.Repositories)
		if err != nil {
			return err
		}
		if len(loaded) > 0 {
			return fmt.Errorf("update to a schema-changing binary requires every registered LaunchAgent to be stopped; loaded: %v", repoIDs(loaded))
		}
	} else {
		loaded, err = loadedEntries(ctx, l)
		if err != nil {
			return err
		}
	}
	brokerManager := launchd.Manager{Layout: l}
	brokerLoaded := false
	if !migrationNeeded {
		brokerManager, brokerLoaded, err = loadedWebhookBroker(ctx, l)
		if err != nil {
			return err
		}
	}
	var backup string
	if *deliveryBackup != "" {
		backup, err = validateDeliveryBackupPath(l, *deliveryBackup)
		if err == nil {
			backup, err = backupInstallationAt(l, backup)
		}
	} else {
		backup, err = backupInstallation(l)
	}
	if err != nil {
		return fmt.Errorf("backup current installation: %w", err)
	}
	if err := stopEntries(ctx, l, loaded, a.ProcessController); err != nil {
		_ = startEntries(ctx, l, loaded)
		return err
	}
	if brokerLoaded {
		if err := brokerManager.StopBroker(ctx); err != nil {
			_ = startEntries(ctx, l, loaded)
			return err
		}
	}
	manifest, _, updateErr := installArtifacts(l, source, Version, Commit)
	if updateErr == nil && !migrationNeeded {
		updateErr = rewritePlists(l)
	}
	if updateErr == nil {
		if brokerLoaded {
			updateErr = brokerManager.StartBroker(ctx)
		}
	}
	if updateErr == nil {
		updateErr = startEntries(ctx, l, loaded)
	}
	if updateErr != nil {
		rollbackErr := restoreInstallation(l, backup)
		_ = rewritePlists(l)
		if brokerLoaded {
			_ = brokerManager.StartBroker(ctx)
		}
		_ = startEntries(ctx, l, loaded)
		if rollbackErr != nil {
			return fmt.Errorf("update failed: %v; automatic rollback failed: %w", updateErr, rollbackErr)
		}
		return fmt.Errorf("update failed and was rolled back: %w", updateErr)
	}
	return a.output(*jsonOut, map[string]any{"changed": true, "backup": backup, "manifest": manifest, "restarted": repoIDs(loaded), "schema_migration_required": migrationNeeded})
}

func (a App) rollback(ctx context.Context, l layout.Layout, args []string) error {
	fs := flag.NewFlagSet("rollback", flag.ContinueOnError)
	fs.SetOutput(a.Err)
	backup := fs.String("backup", "", "backup directory returned by update")
	jsonOut := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return exitError{2, err}
	}
	resolved, err := validateBackupPath(l, *backup)
	if err != nil {
		return exitError{2, err}
	}
	backupManifest, err := readInstallManifest(filepath.Join(resolved, "install.json"))
	if err != nil {
		return err
	}
	backupSchemaVersion := backupManifest.SchemaVersion
	if backupSchemaVersion == 0 {
		backupSchemaVersion = 1
	}
	schemaReport, err := schema.Inspect(l)
	if err != nil {
		return err
	}
	for _, artifact := range schemaReport.Artifacts {
		if artifact.Version != 0 && artifact.Version != backupSchemaVersion {
			return fmt.Errorf("installation rollback requires schema version %d but %s is version %d; restore the matching migration backup first", backupSchemaVersion, artifact.Path, artifact.Version)
		}
	}
	legacySchemaRollback := backupSchemaVersion != schema.CurrentVersion
	var loaded []registry.Entry
	if legacySchemaRollback {
		loaded, err = loadedMigrationEntries(ctx, l, schemaReport.Repositories)
		if err != nil {
			return err
		}
		if len(loaded) > 0 {
			return fmt.Errorf("installation rollback across schema versions requires every registered LaunchAgent to be stopped; loaded: %v", repoIDs(loaded))
		}
	} else {
		loaded, err = loadedEntries(ctx, l)
		if err != nil {
			return err
		}
	}
	brokerManager := launchd.Manager{Layout: l}
	brokerLoaded := false
	if !legacySchemaRollback {
		brokerManager, brokerLoaded, err = loadedWebhookBroker(ctx, l)
		if err != nil {
			return err
		}
	}
	if err := stopEntries(ctx, l, loaded, a.ProcessController); err != nil {
		_ = startEntries(ctx, l, loaded)
		return err
	}
	if brokerLoaded {
		if err := brokerManager.StopBroker(ctx); err != nil {
			_ = startEntries(ctx, l, loaded)
			return err
		}
	}
	if err := restoreInstallation(l, resolved); err != nil {
		if brokerLoaded {
			_ = brokerManager.StartBroker(ctx)
		}
		_ = startEntries(ctx, l, loaded)
		return err
	}
	if !legacySchemaRollback {
		if err := rewritePlists(l); err != nil {
			_ = startEntries(ctx, l, loaded)
			return err
		}
	}
	if brokerLoaded {
		if err := brokerManager.StartBroker(ctx); err != nil {
			return err
		}
	}
	if err := startEntries(ctx, l, loaded); err != nil {
		return err
	}
	manifest, err := readInstallManifest(filepath.Join(l.Root, "install.json"))
	if err != nil {
		return err
	}
	return a.output(*jsonOut, map[string]any{"restored": resolved, "manifest": manifest, "restarted": repoIDs(loaded), "state_preserved": true})
}

func installArtifacts(l layout.Layout, source, version, commit string) (installManifest, bool, error) {
	binary, err := os.ReadFile(source)
	if err != nil {
		return installManifest{}, false, err
	}
	binaryHash := fmt.Sprintf("%x", sha256.Sum256(binary))
	skillHash := fmt.Sprintf("%x", sha256.Sum256(assets.AgentLoopSkill))
	manifestPath := filepath.Join(l.Root, "install.json")
	if current, err := readInstallManifest(manifestPath); err == nil && current.Version == version && current.Commit == commit && current.BinarySHA256 == binaryHash && current.SkillSHA256 == skillHash {
		if match, _ := installationMatches(l, source, version, commit); match {
			return current, false, nil
		}
	}
	destination := filepath.Join(l.BinDir, "agent-loop")
	skillDir := filepath.Join(l.SkillsDir, "agent-loop")
	if err := os.MkdirAll(skillDir, 0o700); err != nil {
		return installManifest{}, false, err
	}
	if err := os.Chmod(skillDir, 0o700); err != nil {
		return installManifest{}, false, err
	}
	if err := fsutil.WriteFile(destination, binary, 0o755); err != nil {
		return installManifest{}, false, fmt.Errorf("install binary: %w", err)
	}
	if err := fsutil.WriteFile(filepath.Join(skillDir, "SKILL.md"), assets.AgentLoopSkill, 0o600); err != nil {
		return installManifest{}, false, fmt.Errorf("install Skill: %w", err)
	}
	if err := fsutil.WriteFile(filepath.Join(skillDir, "VERSION"), []byte(version+"\n"), 0o600); err != nil {
		return installManifest{}, false, fmt.Errorf("install Skill version: %w", err)
	}
	manifest := installManifest{Version: version, Commit: commit, SchemaVersion: schema.CurrentVersion,
		SchemaMigrationFrom: schemaversion.Previous, SemanticContractVersion: statecontract.CurrentVersion,
		BinarySHA256: binaryHash, SkillSHA256: skillHash, InstalledAt: time.Now().UTC()}
	if err := fsutil.WriteJSON(manifestPath, manifest, 0o600); err != nil {
		return installManifest{}, false, fmt.Errorf("write install manifest: %w", err)
	}
	return manifest, true, nil
}

func installationMatches(l layout.Layout, source, version, commit string) (bool, error) {
	manifest, err := readInstallManifest(filepath.Join(l.Root, "install.json"))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if manifest.Version != version || manifest.Commit != commit || manifest.SchemaVersion != schema.CurrentVersion ||
		manifest.SchemaMigrationFrom != schemaversion.Previous || manifest.SemanticContractVersion != statecontract.CurrentVersion {
		return false, nil
	}
	sourceHash, err := fileSHA256(source)
	if err != nil {
		return false, err
	}
	installedHash, err := fileSHA256(filepath.Join(l.BinDir, "agent-loop"))
	if err != nil {
		return false, nil
	}
	skillHash, err := fileSHA256(filepath.Join(l.SkillsDir, "agent-loop", "SKILL.md"))
	if err != nil {
		return false, nil
	}
	versionData, err := os.ReadFile(filepath.Join(l.SkillsDir, "agent-loop", "VERSION"))
	if err != nil {
		return false, nil
	}
	return sourceHash == installedHash && installedHash == manifest.BinarySHA256 && skillHash == manifest.SkillSHA256 && strings.TrimSpace(string(versionData)) == version, nil
}

func backupInstallation(l layout.Layout) (string, error) {
	manifest, err := readInstallManifest(filepath.Join(l.Root, "install.json"))
	if err != nil {
		return "", fmt.Errorf("existing install manifest is required: %w", err)
	}
	backup := filepath.Join(l.Root, "backups", time.Now().UTC().Format("20060102T150405.000000000Z")+"-"+safeVersion(manifest.Version))
	return backupInstallationAt(l, backup)
}

func backupInstallationAt(l layout.Layout, backup string) (string, error) {
	if info, err := os.Lstat(backup); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return "", fmt.Errorf("installation backup path is not a regular directory")
		}
		if err := validateInstallationBackup(backup); err == nil {
			// A delivery retry must retain the pre-update image captured by the
			// first attempt, even if the live installation is now half-updated.
			return backup, nil
		}
		current, currentErr := readInstallManifest(filepath.Join(l.Root, "install.json"))
		if currentErr != nil {
			return "", fmt.Errorf("incomplete backup exists and current installation cannot be verified: %w", currentErr)
		}
		matches, matchErr := installationMatches(l, filepath.Join(l.BinDir, "agent-loop"), current.Version, current.Commit)
		if matchErr != nil || !matches {
			return "", fmt.Errorf("incomplete backup exists and current installation is not internally consistent")
		}
		// Only an incomplete, controller-owned backup at this exact path is
		// discarded. A complete previous image is never overwritten.
		if err := os.RemoveAll(backup); err != nil {
			return "", err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if err := os.MkdirAll(backup, 0o700); err != nil {
		return "", err
	}
	if err := os.Chmod(backup, 0o700); err != nil {
		return "", err
	}
	files := map[string]string{
		filepath.Join(l.BinDir, "agent-loop"):                filepath.Join(backup, "agent-loop"),
		filepath.Join(l.SkillsDir, "agent-loop", "SKILL.md"): filepath.Join(backup, "SKILL.md"),
		filepath.Join(l.SkillsDir, "agent-loop", "VERSION"):  filepath.Join(backup, "VERSION"),
		filepath.Join(l.Root, "install.json"):                filepath.Join(backup, "install.json"),
	}
	for source, destination := range files {
		data, err := os.ReadFile(source)
		if err != nil {
			return "", err
		}
		mode := os.FileMode(0o600)
		if filepath.Base(destination) == "agent-loop" {
			mode = 0o700
		}
		if err := fsutil.WriteFile(destination, data, mode); err != nil {
			return "", err
		}
	}
	return backup, nil
}

func validateDeliveryBackupPath(l layout.Layout, path string) (string, error) {
	if path == "" || !filepath.IsAbs(path) {
		return "", fmt.Errorf("--delivery-backup must be an absolute managed path")
	}
	clean := filepath.Clean(path)
	root := filepath.Join(l.Root, "backups")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", err
	}
	if err := os.Chmod(root, 0o700); err != nil {
		return "", err
	}
	relative, err := filepath.Rel(root, clean)
	if err != nil || relative == "." || strings.Contains(relative, string(os.PathSeparator)) || strings.HasPrefix(relative, "..") || !strings.HasPrefix(relative, "delivery-") {
		return "", fmt.Errorf("--delivery-backup must name a direct delivery-* child of %s", root)
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	clean = filepath.Join(resolvedRoot, relative)
	if info, err := os.Lstat(clean); err == nil && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0) {
		return "", fmt.Errorf("delivery backup path is not a regular directory")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	return clean, nil
}

func restoreInstallation(l layout.Layout, backup string) error {
	if err := validateInstallationBackup(backup); err != nil {
		return err
	}
	files := map[string]struct {
		destination string
		mode        os.FileMode
	}{
		"agent-loop":   {filepath.Join(l.BinDir, "agent-loop"), 0o755},
		"SKILL.md":     {filepath.Join(l.SkillsDir, "agent-loop", "SKILL.md"), 0o600},
		"VERSION":      {filepath.Join(l.SkillsDir, "agent-loop", "VERSION"), 0o600},
		"install.json": {filepath.Join(l.Root, "install.json"), 0o600},
	}
	for name, target := range files {
		data, err := os.ReadFile(filepath.Join(backup, name))
		if err != nil {
			return err
		}
		if err := fsutil.WriteFile(target.destination, data, target.mode); err != nil {
			return err
		}
	}
	return nil
}

func validateInstallationBackup(backup string) error {
	for _, name := range []string{"agent-loop", "SKILL.md", "VERSION", "install.json"} {
		info, err := os.Lstat(filepath.Join(backup, name))
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("backup %s is missing or not a regular file", name)
		}
	}
	manifest, err := readInstallManifest(filepath.Join(backup, "install.json"))
	if err != nil {
		return err
	}
	binaryHash, err := fileSHA256(filepath.Join(backup, "agent-loop"))
	if err != nil || binaryHash != manifest.BinarySHA256 {
		return fmt.Errorf("backup binary checksum mismatch")
	}
	skillHash, err := fileSHA256(filepath.Join(backup, "SKILL.md"))
	if err != nil || skillHash != manifest.SkillSHA256 {
		return fmt.Errorf("backup Skill checksum mismatch")
	}
	version, err := os.ReadFile(filepath.Join(backup, "VERSION"))
	if err != nil || strings.TrimSpace(string(version)) != manifest.Version {
		return fmt.Errorf("backup Skill version mismatch")
	}
	return nil
}

func validateBackupPath(l layout.Layout, path string) (string, error) {
	if path == "" || !filepath.IsAbs(path) {
		return "", fmt.Errorf("--backup must be an absolute path returned by update")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	root, err := filepath.EvalSymlinks(filepath.Join(l.Root, "backups"))
	if err != nil {
		return "", err
	}
	relative, err := filepath.Rel(root, resolved)
	if err != nil || relative == "." || strings.HasPrefix(relative, ".."+string(os.PathSeparator)) || relative == ".." {
		return "", fmt.Errorf("backup must be a child of %s", root)
	}
	return resolved, nil
}

func readInstallManifest(path string) (installManifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return installManifest{}, err
	}
	var manifest installManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return installManifest{}, err
	}
	return manifest, nil
}

func fileSHA256(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256(data)), nil
}

func safeVersion(version string) string {
	return strings.Map(func(value rune) rune {
		if value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' || value >= '0' && value <= '9' || value == '.' || value == '-' {
			return value
		}
		return '-'
	}, version)
}

func loadedEntries(ctx context.Context, l layout.Layout) ([]registry.Entry, error) {
	registered, err := (registry.Store{Path: l.RegistryPath}).Load()
	if err != nil {
		return nil, err
	}
	var loaded []registry.Entry
	for _, entry := range registered.Repos {
		status, err := (launchd.Manager{Layout: l, Launchctl: entry.Commands["launchctl"]}).Status(ctx, entry)
		if err != nil {
			return nil, err
		}
		if status.Loaded {
			loaded = append(loaded, entry)
		}
	}
	sort.Slice(loaded, func(i, j int) bool { return loaded[i].RepoID < loaded[j].RepoID })
	return loaded, nil
}

func stopEntries(ctx context.Context, l layout.Layout, entries []registry.Entry, controller supervisor.ProcessGroupController) error {
	for _, entry := range entries {
		if err := (launchd.Manager{Layout: l, Launchctl: entry.Commands["launchctl"]}).Stop(ctx, entry); err != nil {
			return err
		}
		if _, err := stopEntryWorkers(ctx, l, entry, "worker canceled for installation lifecycle", controller); err != nil {
			return err
		}
	}
	return nil
}

func stopEntryWorkers(ctx context.Context, l layout.Layout, entry registry.Entry, reason string, controller supervisor.ProcessGroupController) (supervisor.WorkerStopReport, error) {
	cfg, err := config.Load(entry.RepoPath)
	if err != nil {
		return supervisor.WorkerStopReport{}, err
	}
	store := state.Store{Dir: l.RepoDir(entry.RepoID), RepoID: entry.RepoID, RepoPath: entry.RepoPath, Secrets: cfg.RedactionValues()}
	return supervisor.StopWorkers(ctx, store, cfg.Worker.TimeoutGrace.Duration, reason, controller)
}

func startEntries(ctx context.Context, l layout.Layout, entries []registry.Entry) error {
	for _, entry := range entries {
		if err := (launchd.Manager{Layout: l, Launchctl: entry.Commands["launchctl"]}).Start(ctx, entry); err != nil {
			return err
		}
	}
	return nil
}

func rewritePlists(l layout.Layout) error {
	registered, err := (registry.Store{Path: l.RegistryPath}).Load()
	if err != nil {
		return err
	}
	binary := filepath.Join(l.BinDir, "agent-loop")
	brokerWritten := false
	for _, entry := range registered.Repos {
		if err := (launchd.Manager{Layout: l}).WritePlist(entry, binary); err != nil {
			return err
		}
		cfg, err := config.Load(entry.RepoPath)
		if err != nil {
			return err
		}
		if cfg.Webhook.Enabled() && !brokerWritten {
			if err := (launchd.Manager{Layout: l}).WriteBrokerPlist(binary, entry.EnvironmentPath); err != nil {
				return err
			}
			brokerWritten = true
		}
	}
	return nil
}

func loadedWebhookBroker(ctx context.Context, l layout.Layout) (launchd.Manager, bool, error) {
	registered, err := (registry.Store{Path: l.RegistryPath}).Load()
	if err != nil {
		return launchd.Manager{}, false, err
	}
	for _, entry := range registered.Repos {
		cfg, loadErr := config.Load(entry.RepoPath)
		if loadErr != nil {
			return launchd.Manager{}, false, loadErr
		}
		if !cfg.Webhook.Enabled() {
			continue
		}
		manager := launchd.Manager{Layout: l, Launchctl: entry.Commands["launchctl"]}
		status, statusErr := manager.BrokerStatus(ctx)
		if statusErr != nil {
			return manager, false, statusErr
		}
		return manager, status.Loaded, nil
	}
	return launchd.Manager{Layout: l}, false, nil
}

func repoIDs(entries []registry.Entry) []string {
	ids := make([]string, 0, len(entries))
	for _, entry := range entries {
		ids = append(ids, entry.RepoID)
	}
	return ids
}

func (a App) uninstall(ctx context.Context, l layout.Layout, args []string) error {
	fs := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	fs.SetOutput(a.Err)
	jsonOut := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return exitError{2, err}
	}
	r, err := (registry.Store{Path: l.RegistryPath}).Load()
	if err != nil {
		return err
	}
	lm := launchd.Manager{Layout: l}
	for _, entry := range r.Repos {
		status, err := lm.Status(ctx, entry)
		if err != nil {
			return err
		}
		if status.Loaded {
			return fmt.Errorf("cannot uninstall while %s is loaded; stop it first", entry.RepoID)
		}
	}
	if _, brokerLoaded, brokerErr := loadedWebhookBroker(ctx, l); brokerErr != nil {
		return brokerErr
	} else if brokerLoaded {
		return fmt.Errorf("cannot uninstall while the shared webhook broker is loaded; stop and unregister webhook repositories first")
	}
	removed := []string{}
	for _, path := range []string{
		filepath.Join(l.BinDir, "agent-loop"),
		filepath.Join(l.SkillsDir, "agent-loop", "SKILL.md"),
		filepath.Join(l.SkillsDir, "agent-loop", "VERSION"),
		filepath.Join(l.Root, "install.json"),
	} {
		if err := os.Remove(path); err == nil {
			removed = append(removed, path)
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return a.output(*jsonOut, map[string]any{"removed": removed, "state_preserved": true})
}
