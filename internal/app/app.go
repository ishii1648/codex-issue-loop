package app

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	assets "github.com/ishii1648/codex-issue-loop"
	"github.com/ishii1648/codex-issue-loop/internal/compat"
	"github.com/ishii1648/codex-issue-loop/internal/config"
	"github.com/ishii1648/codex-issue-loop/internal/conflict"
	"github.com/ishii1648/codex-issue-loop/internal/fsutil"
	gh "github.com/ishii1648/codex-issue-loop/internal/github"
	"github.com/ishii1648/codex-issue-loop/internal/launchd"
	"github.com/ishii1648/codex-issue-loop/internal/layout"
	schema "github.com/ishii1648/codex-issue-loop/internal/migration"
	"github.com/ishii1648/codex-issue-loop/internal/notify"
	"github.com/ishii1648/codex-issue-loop/internal/observe"
	"github.com/ishii1648/codex-issue-loop/internal/publish"
	"github.com/ishii1648/codex-issue-loop/internal/redact"
	"github.com/ishii1648/codex-issue-loop/internal/registry"
	"github.com/ishii1648/codex-issue-loop/internal/retention"
	"github.com/ishii1648/codex-issue-loop/internal/state"
	"github.com/ishii1648/codex-issue-loop/internal/supervisor"
	"github.com/ishii1648/codex-issue-loop/internal/userrules"
	"github.com/ishii1648/codex-issue-loop/internal/worker"
	"github.com/ishii1648/codex-issue-loop/internal/worktree"
)

var (
	Version = "0.1.0-dev"
	Commit  = "unknown"
)

type versionInfo struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
}

type installManifest struct {
	Version       string    `json:"version"`
	Commit        string    `json:"commit"`
	SchemaVersion int       `json:"schema_version"`
	BinarySHA256  string    `json:"binary_sha256"`
	SkillSHA256   string    `json:"skill_sha256"`
	InstalledAt   time.Time `json:"installed_at"`
}

type App struct {
	In                io.Reader
	Out               io.Writer
	Err               io.Writer
	ProcessController supervisor.ProcessGroupController
}

type exitError struct {
	Code int
	Err  error
}

func (e exitError) Error() string { return e.Err.Error() }

func (a App) Run(ctx context.Context, args []string) int {
	if a.In == nil {
		a.In = os.Stdin
	}
	if a.Out == nil {
		a.Out = os.Stdout
	}
	if a.Err == nil {
		a.Err = os.Stderr
	}
	if len(args) == 0 {
		a.usage()
		return 2
	}
	if args[0] == "--version" || args[0] == "version" {
		if len(args) > 1 && args[1] == "--json" {
			_ = json.NewEncoder(a.Out).Encode(versionInfo{Version: Version, Commit: Commit})
		} else {
			fmt.Fprintf(a.Out, "agent-loop %s (%s)\n", Version, Commit)
		}
		return 0
	}
	// init is deliberately handled before Layout.Ensure so a preview does not
	// create agent-loop directories or change user-owned files.
	if args[0] == "init" {
		err := a.initUserRules(args[1:])
		if err == nil {
			return 0
		}
		var ee exitError
		if errors.As(err, &ee) {
			fmt.Fprintln(a.Err, ee.Err)
			return ee.Code
		}
		fmt.Fprintln(a.Err, err)
		return 1
	}
	l, err := layout.New()
	if err != nil {
		fmt.Fprintln(a.Err, err)
		return 1
	}
	if err := l.Ensure(); err != nil {
		fmt.Fprintln(a.Err, err)
		return 1
	}
	err = a.run(ctx, l, args[0], args[1:])
	if err == nil {
		return 0
	}
	var ee exitError
	if errors.As(err, &ee) {
		fmt.Fprintln(a.Err, ee.Err)
		return ee.Code
	}
	fmt.Fprintln(a.Err, err)
	return 1
}

func (a App) run(ctx context.Context, l layout.Layout, command string, args []string) error {
	switch command {
	case "install":
		return a.install(ctx, l, args)
	case "update":
		return a.update(ctx, l, args)
	case "rollback":
		return a.rollback(ctx, l, args)
	case "migrate":
		return a.migrate(ctx, l, args)
	case "uninstall":
		return a.uninstall(ctx, l, args)
	case "register":
		return a.register(l, args)
	case "unregister":
		return a.unregister(ctx, l, args)
	case "start", "stop", "restart":
		return a.control(ctx, l, command, args)
	case "status":
		return a.status(ctx, l, args)
	case "watch":
		return a.watch(ctx, l, args)
	case "answer":
		return a.answer(l, args)
	case "retry":
		return a.retryConflict(ctx, l, args)
	case "notification-token":
		return a.notificationToken(ctx, l, args)
	case "logs":
		return a.logs(l, args)
	case "cleanup":
		return a.cleanup(ctx, l, args)
	case "purge":
		return a.purge(ctx, l, args)
	case "doctor":
		return a.doctor(ctx, l, args)
	case "bootstrap-labels":
		return a.bootstrapLabels(ctx, args)
	case "run":
		return a.supervise(ctx, l, args)
	case "help", "--help", "-h":
		a.usage()
		return nil
	default:
		return exitError{2, fmt.Errorf("unknown command %q", command)}
	}
}

func (a App) usage() {
	fmt.Fprintln(a.Out, `Usage: agent-loop <command> [options]

Commands:
  init          Preview or apply user-scoped Codex / Claude Code rules
  install       Install the binary and Codex Skill
  update        Safely replace the binary and Skill, preserving state
  rollback      Restore a backup created by update
  migrate       Inspect, apply, or roll back durable schema migrations
  uninstall     Remove installed binary and Skill when no loop is running
  register      Register a repository and write its LaunchAgent
  unregister    Stop and unregister a repository
  start         Start the repository LaunchAgent
  stop          Gracefully drain and stop the repository LaunchAgent
  restart       Gracefully drain, stop, and restart the LaunchAgent
  status        Show durable and launchd state
  watch         Wait for needs_input, blocked, stopped, or optional idle
  answer        Record an answer for a pending request
  retry         Explicitly resume a blocked Pull Request conflict recovery
  notification-token  Configure or clear a managed notification credential
  logs          Print supervisor logs
  cleanup       Preview or remove expired safe worktrees
  purge         Force-remove one explicitly confirmed worktree
  doctor        Validate dependencies, auth, config, and registration
  bootstrap-labels  Preview or create required GitHub labels
  run           Run the supervisor (used by launchd)`)
}

func (a App) initUserRules(args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(a.Err)
	apply := fs.Bool("apply", false, "apply the planned user rule changes")
	agentNames := fs.String("agents", "codex,claude", "comma-separated agents: codex,claude")
	jsonOut := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return exitError{2, err}
	}
	if fs.NArg() != 0 {
		return exitError{2, fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))}
	}
	agents, err := userrules.ParseAgents(*agentNames)
	if err != nil {
		return exitError{2, err}
	}
	config, err := userrules.ConfigFromEnvironment()
	if err != nil {
		return err
	}
	report, err := userrules.Plan(config, agents)
	if err != nil {
		return err
	}
	if *apply {
		report, err = userrules.Apply(report)
	}
	if outputErr := a.output(*jsonOut, report); outputErr != nil {
		return outputErr
	}
	return err
}

func (a App) bootstrapLabels(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("bootstrap-labels", flag.ContinueOnError)
	fs.SetOutput(a.Err)
	repo := fs.String("repo", "", "repository path")
	apply := fs.Bool("apply", false, "create missing labels")
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
	result, bootstrapErr := (gh.CLI{Secrets: cfg.RedactionValues()}).BootstrapLabels(ctx, cfg, *apply)
	if outputErr := a.output(*jsonOut, result); outputErr != nil {
		return outputErr
	}
	return bootstrapErr
}

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
	backup, err := backupInstallation(l)
	if err != nil {
		return fmt.Errorf("backup current installation: %w", err)
	}
	if err := stopEntries(ctx, l, loaded, a.ProcessController); err != nil {
		_ = startEntries(ctx, l, loaded)
		return err
	}
	manifest, _, updateErr := installArtifacts(l, source, Version, Commit)
	if updateErr == nil && !migrationNeeded {
		updateErr = rewritePlists(l)
	}
	if updateErr == nil {
		updateErr = startEntries(ctx, l, loaded)
	}
	if updateErr != nil {
		rollbackErr := restoreInstallation(l, backup)
		_ = rewritePlists(l)
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
	if err := stopEntries(ctx, l, loaded, a.ProcessController); err != nil {
		_ = startEntries(ctx, l, loaded)
		return err
	}
	if err := restoreInstallation(l, resolved); err != nil {
		_ = startEntries(ctx, l, loaded)
		return err
	}
	if !legacySchemaRollback {
		if err := rewritePlists(l); err != nil {
			_ = startEntries(ctx, l, loaded)
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
	manifest := installManifest{Version: version, Commit: commit, SchemaVersion: schema.CurrentVersion, BinarySHA256: binaryHash, SkillSHA256: skillHash, InstalledAt: time.Now().UTC()}
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
	if manifest.Version != version || manifest.Commit != commit || manifest.SchemaVersion != schema.CurrentVersion {
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
	if err := os.MkdirAll(backup, 0o700); err != nil {
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

func restoreInstallation(l layout.Layout, backup string) error {
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
	for _, entry := range registered.Repos {
		if err := (launchd.Manager{Layout: l}).WritePlist(entry, binary); err != nil {
			return err
		}
	}
	return nil
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
	return a.output(jsonOut, map[string]any{"unregistered": entry.RepoID, "state_preserved": true})
}

func (a App) control(ctx context.Context, l layout.Layout, command string, args []string) error {
	entry, jsonOut, timeout, force, err := a.resolveControl(l, command, args)
	if err != nil {
		return err
	}
	cfg, err := config.Load(entry.RepoPath)
	if err != nil {
		return err
	}
	lm := launchd.Manager{Layout: l, Launchctl: entry.Commands["launchctl"]}
	store := state.Store{Dir: l.RepoDir(entry.RepoID), RepoID: entry.RepoID, RepoPath: entry.RepoPath, Secrets: cfg.RedactionValues()}
	stopReport := supervisor.WorkerStopReport{Workers: []supervisor.WorkerStop{}}
	var drain *state.Drain
	restartStartAttempted := false
	switch command {
	case "start":
		var launchStatus launchd.Status
		launchStatus, err = lm.Status(ctx, entry)
		if err == nil && !launchStatus.Loaded {
			err = recordSupervisorControl(store, "starting", "start requested")
		}
		if err == nil {
			err = lm.Start(ctx, entry)
		}
	case "stop":
		drain, stopReport, err = a.stopGracefully(ctx, lm, entry, store, cfg, "stop", timeout, force)
	case "restart":
		var previousGeneration uint64
		if snapshot, loadErr := store.Load(); loadErr == nil {
			previousGeneration = snapshot.Supervisor.Generation
		}
		drain, stopReport, err = a.stopGracefully(ctx, lm, entry, store, cfg, "restart", timeout, force)
		if err == nil {
			err = recordSupervisorControl(store, "starting", "restart requested")
		}
		if err == nil {
			restartStartAttempted = true
			err = lm.Start(ctx, entry)
		}
		if err == nil {
			drain, err = waitForRestartGeneration(ctx, store, previousGeneration, timeout)
		}
	}
	if err != nil {
		var controlExit exitError
		if drain != nil && errors.As(err, &controlExit) && controlExit.Code == 5 {
			status, _ := lm.Status(ctx, entry)
			result := map[string]any{
				"repo_id": entry.RepoID, "command": command, "launchd": status,
				"drain": drain, "result": "timed_out", "workers_signaled": false,
				"remediation": []string{"retry the graceful operation later", "inspect active workers and use --force only if interruption is acceptable"},
			}
			if outputErr := a.output(jsonOut, result); outputErr != nil {
				return outputErr
			}
		}
		if command == "start" || command == "restart" && restartStartAttempted {
			_ = recordSupervisorControl(store, "stopped", "start failed: "+err.Error())
		}
		return err
	}
	status, _ := lm.Status(ctx, entry)
	result := map[string]any{"repo_id": entry.RepoID, "command": command, "launchd": status}
	if command == "stop" || command == "restart" {
		result["worker_stop"] = stopReport
		result["drain"] = drain
	}
	return a.output(jsonOut, result)
}

const defaultDrainTimeout = 5 * time.Minute

func (a App) resolveControl(l layout.Layout, command string, args []string) (registry.Entry, bool, time.Duration, bool, error) {
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	fs.SetOutput(a.Err)
	repo := fs.String("repo", "", "repository path")
	jsonOut := fs.Bool("json", false, "emit JSON")
	timeout := defaultDrainTimeout
	force := false
	if command == "stop" || command == "restart" {
		fs.DurationVar(&timeout, "timeout", defaultDrainTimeout, "maximum graceful drain duration")
		fs.BoolVar(&force, "force", false, "skip graceful drain and terminate worker process groups")
	}
	if err := fs.Parse(args); err != nil {
		return registry.Entry{}, false, 0, false, exitError{2, err}
	}
	if fs.NArg() != 0 {
		return registry.Entry{}, false, 0, false, exitError{2, fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))}
	}
	if timeout <= 0 {
		return registry.Entry{}, false, 0, false, exitError{2, fmt.Errorf("--timeout must be positive")}
	}
	entry, err := a.resolvePath(l, *repo)
	return entry, *jsonOut, timeout, force, err
}

func (a App) stopGracefully(ctx context.Context, lm launchd.Manager, entry registry.Entry, store state.Store, cfg config.Config, operation string, timeout time.Duration, force bool) (*state.Drain, supervisor.WorkerStopReport, error) {
	empty := supervisor.WorkerStopReport{Workers: []supervisor.WorkerStop{}}
	if force {
		if err := lm.Stop(ctx, entry); err != nil {
			return nil, empty, err
		}
		report, err := supervisor.StopWorkers(ctx, store, cfg.Worker.TimeoutGrace.Duration, "worker canceled by forced "+operation, a.ProcessController)
		if err != nil {
			return nil, report, err
		}
		now := time.Now().UTC()
			snapshot, err := store.Update("forced_stop", 0, "", map[string]any{"operation": operation, "workers": report.Workers}, func(s *state.Snapshot) error {
			if s.Supervisor.Drain == nil {
				s.Supervisor.Drain = &state.Drain{ID: state.NewID("drain"), Operation: operation, RequestedAt: now, Deadline: now, PreviousGeneration: s.Supervisor.Generation, Targets: []state.DrainTarget{}}
			}
			s.Supervisor.Drain.Operation = operation
			s.Supervisor.Drain.Status = "forced"
			s.Supervisor.Drain.Forced = true
			s.Supervisor.Drain.CompletedAt = &now
			s.Supervisor.Drain.RemainingActive = 0
			s.Supervisor.Drain.Message = "forced stop completed"
			s.Supervisor.State, s.Supervisor.PID, s.Supervisor.Message = "stopped", 0, "forced "+operation
			return nil
		})
		return snapshot.Supervisor.Drain, report, err
	}

	launchStatus, err := lm.Status(ctx, entry)
	if err != nil {
		return nil, empty, err
	}
	snapshot, err := store.Load()
	if err != nil {
		return nil, empty, err
	}
	if snapshot.Supervisor.PID == 0 && snapshot.Supervisor.Drain != nil && snapshot.Supervisor.Drain.Status == "completed" {
		if err := lm.Stop(ctx, entry); err != nil {
			return snapshot.Supervisor.Drain, empty, err
		}
		if err := verifyWorkerCheckpoint(store, a.ProcessController); err != nil {
			return snapshot.Supervisor.Drain, empty, err
		}
		return snapshot.Supervisor.Drain, empty, nil
	}
	if !launchStatus.Loaded && snapshot.Supervisor.PID == 0 {
		if activeDrainTargets(snapshot) > 0 {
			return snapshot.Supervisor.Drain, empty, exitError{5, fmt.Errorf("supervisor is not running and saved workers cannot be drained; inspect status, then retry with --force to recover process groups")}
		}
		now := time.Now().UTC()
		completed, updateErr := store.Update("drain_completed", 0, "", map[string]string{"operation": operation, "reason": "already stopped"}, func(s *state.Snapshot) error {
			s.Supervisor.Drain = &state.Drain{ID: state.NewID("drain"), Operation: operation, Status: "completed", RequestedAt: now, Deadline: now, CompletedAt: &now, PreviousGeneration: s.Supervisor.Generation, Targets: []state.DrainTarget{}}
			s.Supervisor.State, s.Supervisor.PID, s.Supervisor.Message = "stopped", 0, "already stopped"
			return nil
		})
		return completed.Supervisor.Drain, empty, updateErr
	}

	drain, err := requestDrain(store, operation, timeout)
	if err != nil {
		return nil, empty, err
	}
	if drain.Operation != operation {
		return drain, empty, exitError{4, fmt.Errorf("graceful %s cannot replace active %s drain %s; wait for the current operation to finish", operation, drain.Operation, drain.ID)}
	}
	drain, err = waitForDrain(ctx, store, drain.ID)
	if err != nil {
		return drain, empty, err
	}
	if drain.Status == "timed_out" {
		return drain, empty, exitError{5, fmt.Errorf("graceful %s timed out with %d active worker(s); workers were not signaled; retry later or use --force after assessing interruption risk", operation, drain.RemainingActive)}
	}
	if err := lm.Stop(ctx, entry); err != nil {
		return drain, empty, err
	}
	if err := verifyWorkerCheckpoint(store, a.ProcessController); err != nil {
		return drain, empty, err
	}
	return drain, empty, nil
}

func requestDrain(store state.Store, operation string, timeout time.Duration) (*state.Drain, error) {
	now := time.Now().UTC()
	deadline := now.Add(timeout)
	payload := map[string]any{"operation": operation, "deadline": deadline}
	snapshot, err := store.Update("drain_requested", 0, "", payload, func(s *state.Snapshot) error {
		if s.Supervisor.Drain.Active() {
			return errDrainAlreadyActive
		}
		targets := make([]state.DrainTarget, 0)
		for _, issue := range s.Issues {
			if issue == nil || !occupiesWorkerSlot(issue.Status) && issue.WorkerPID == 0 && issue.WorkerPGID == 0 {
				continue
			}
			targets = append(targets, state.DrainTarget{IssueNumber: issue.Number, RunID: issue.RunID, Phase: issue.Status, PID: issue.WorkerPID, PGID: issue.WorkerPGID})
		}
		sort.Slice(targets, func(i, j int) bool { return targets[i].IssueNumber < targets[j].IssueNumber })
		s.Supervisor.Drain = &state.Drain{ID: state.NewID("drain"), Operation: operation, Status: "requested", RequestedAt: now, Deadline: deadline, PreviousGeneration: s.Supervisor.Generation, RemainingActive: len(targets), Targets: targets}
		payload["drain_id"] = s.Supervisor.Drain.ID
		payload["targets"] = targets
		s.Supervisor.State = "draining"
		s.Supervisor.Message = "graceful " + operation + " requested"
		return nil
	})
	if errors.Is(err, errDrainAlreadyActive) {
		current, loadErr := store.Load()
		return current.Supervisor.Drain, loadErr
	}
	return snapshot.Supervisor.Drain, err
}

var (
	errDrainAlreadyActive = errors.New("graceful drain is already active")
	errDrainStateChanged  = errors.New("durable drain state changed")
)

func waitForDrain(ctx context.Context, store state.Store, id string) (*state.Drain, error) {
	initial, err := store.Load()
	if err != nil {
		return nil, err
	}
	if initial.Supervisor.Drain == nil || initial.Supervisor.Drain.ID != id {
		return initial.Supervisor.Drain, fmt.Errorf("graceful drain %s is no longer current", id)
	}
	wait := time.Until(initial.Supervisor.Drain.Deadline) + 250*time.Millisecond
	if wait <= 0 {
		wait = 250 * time.Millisecond
	}
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		snapshot, err := store.Load()
		if err != nil {
			return nil, err
		}
		drain := snapshot.Supervisor.Drain
		if drain == nil || drain.ID != id {
			return drain, fmt.Errorf("graceful drain %s was replaced before completion", id)
		}
		if !drain.Active() {
			return drain, nil
		}
		select {
		case <-ctx.Done():
			return drain, ctx.Err()
		case <-deadline.C:
			return markDrainTimedOut(store, id)
		case <-ticker.C:
		}
	}
}

func markDrainTimedOut(store state.Store, id string) (*state.Drain, error) {
	now := time.Now().UTC()
	snapshot, err := store.Update("drain_timed_out", 0, "", map[string]string{"drain_id": id, "reason": "operator timeout"}, func(s *state.Snapshot) error {
		if s.Supervisor.Drain == nil || s.Supervisor.Drain.ID != id || !s.Supervisor.Drain.Active() {
			return errDrainStateChanged
		}
		s.Supervisor.Drain.Status = "timed_out"
		s.Supervisor.Drain.CompletedAt = &now
		s.Supervisor.Drain.Message = "graceful drain timed out; workers were not signaled"
		s.Supervisor.State = "running"
		s.Supervisor.Message = s.Supervisor.Drain.Message
		return nil
	})
	if errors.Is(err, errDrainStateChanged) {
		current, loadErr := store.Load()
		return current.Supervisor.Drain, loadErr
	}
	return snapshot.Supervisor.Drain, err
}

func waitForRestartGeneration(ctx context.Context, store state.Store, previous uint64, timeout time.Duration) (*state.Drain, error) {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		snapshot, err := store.Load()
		if err != nil {
			return nil, err
		}
		if snapshot.Supervisor.Generation > previous && snapshot.Supervisor.PID > 0 {
			if snapshot.Supervisor.Drain != nil && snapshot.Supervisor.Drain.Status == "restart_completed" && snapshot.Supervisor.Drain.NewGeneration == snapshot.Supervisor.Generation {
				return snapshot.Supervisor.Drain, nil
			}
			now := time.Now().UTC()
			updated, updateErr := store.Update("restart_completed", 0, "", map[string]any{"previous_generation": previous, "new_generation": snapshot.Supervisor.Generation}, func(s *state.Snapshot) error {
				if s.Supervisor.Drain == nil || s.Supervisor.Drain.Status == "restart_completed" {
					return errDrainStateChanged
				}
				s.Supervisor.Drain.Status = "restart_completed"
				s.Supervisor.Drain.NewGeneration = s.Supervisor.Generation
				s.Supervisor.Drain.CompletedAt = &now
				return nil
			})
			if errors.Is(updateErr, errDrainStateChanged) {
				current, loadErr := store.Load()
				return current.Supervisor.Drain, loadErr
			}
			return updated.Supervisor.Drain, updateErr
		}
		select {
		case <-ctx.Done():
			return snapshot.Supervisor.Drain, ctx.Err()
		case <-deadline.C:
			return snapshot.Supervisor.Drain, fmt.Errorf("new supervisor generation did not start within %s", timeout)
		case <-ticker.C:
		}
	}
}

func activeDrainTargets(snapshot state.Snapshot) int {
	count := 0
	for _, issue := range snapshot.Issues {
		if issue != nil && (occupiesWorkerSlot(issue.Status) || issue.WorkerPID != 0 || issue.WorkerPGID != 0) {
			count++
		}
	}
	return count
}

func verifyWorkerCheckpoint(store state.Store, controller supervisor.ProcessGroupController) error {
	snapshot, err := store.Load()
	if err != nil {
		return err
	}
	for _, issue := range snapshot.Issues {
		if issue == nil || issue.WorkerPID == 0 && issue.WorkerPGID == 0 {
			continue
		}
		if controller != nil && (controller.Alive(issue.WorkerPID) || controller.GroupAlive(issue.WorkerPGID)) {
			return fmt.Errorf("Issue #%d worker process group %d is still alive after graceful drain", issue.Number, issue.WorkerPGID)
		}
		return fmt.Errorf("Issue #%d retained worker PID/PGID after graceful drain", issue.Number)
	}
	return nil
}

func recordSupervisorControl(store state.Store, supervisorState, message string) error {
	_, err := store.Update("supervisor_"+supervisorState, 0, "", map[string]string{"reason": message}, func(s *state.Snapshot) error {
		s.Supervisor.State = supervisorState
		s.Supervisor.PID = 0
		s.Supervisor.Message = message
		s.Supervisor.FailureKind = ""
		s.Supervisor.ConsecutiveFailures = 0
		s.Supervisor.RetryAfter = nil
		return nil
	})
	return err
}

func (a App) status(ctx context.Context, l layout.Layout, args []string) error {
	entry, jsonOut, err := a.resolve(l, "status", args)
	if err != nil {
		return err
	}
	cfg, err := config.Load(entry.RepoPath)
	if err != nil {
		return err
	}
	store := state.Store{Dir: l.RepoDir(entry.RepoID), RepoID: entry.RepoID, RepoPath: entry.RepoPath, Secrets: cfg.RedactionValues()}
	snapshot, err := store.Load()
	if err != nil {
		return err
	}
	launchStatus, err := (launchd.Manager{Layout: l, Launchctl: entry.Commands["launchctl"]}).Status(ctx, entry)
	if err != nil {
		return err
	}
	return a.output(jsonOut, buildStatus(launchStatus, snapshot, cfg.Queue.Concurrency))
}

func (a App) watch(ctx context.Context, l layout.Layout, args []string) error {
	fs := flag.NewFlagSet("watch", flag.ContinueOnError)
	fs.SetOutput(a.Err)
	repo := fs.String("repo", "", "repository path")
	jsonOut := fs.Bool("json", false, "emit JSON")
	untilAttention := fs.Bool("until-attention", false, "wait for attention")
	untilIdle := fs.Bool("until-idle", false, "also return when idle")
	if err := fs.Parse(args); err != nil {
		return exitError{2, err}
	}
	if !*untilAttention && !*untilIdle {
		return exitError{2, fmt.Errorf("watch requires --until-attention or --until-idle")}
	}
	entry, err := a.resolvePath(l, *repo)
	if err != nil {
		return err
	}
	cfg, err := config.Load(entry.RepoPath)
	if err != nil {
		return err
	}
	store := state.Store{Dir: l.RepoDir(entry.RepoID), RepoID: entry.RepoID, RepoPath: entry.RepoPath, Secrets: cfg.RedactionValues()}
	result, err := observe.Wait(ctx, store, cfg.Watch.ReconcileInterval.Duration, cfg.Watch.ReconcileJitter, *untilIdle)
	if err != nil {
		return err
	}
	return a.output(*jsonOut, result)
}

func (a App) answer(l layout.Layout, args []string) error {
	const maxAnswerBytes = 16 * 1024
	fs := flag.NewFlagSet("answer", flag.ContinueOnError)
	fs.SetOutput(a.Err)
	repo := fs.String("repo", "", "repository path")
	requestID := fs.String("request-id", "", "request ID")
	message := fs.String("message", "", "answer text")
	messageFile := fs.String("message-file", "", "answer file or - for stdin")
	jsonOut := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return exitError{2, err}
	}
	if *requestID == "" {
		return exitError{2, fmt.Errorf("--request-id is required")}
	}
	if (*message == "") == (*messageFile == "") {
		return exitError{2, fmt.Errorf("provide exactly one of --message or --message-file")}
	}
	answer := *message
	if *messageFile != "" {
		var data []byte
		var err error
		if *messageFile == "-" {
			data, err = io.ReadAll(io.LimitReader(a.In, maxAnswerBytes+1))
		} else {
			file, openErr := os.Open(*messageFile)
			if openErr != nil {
				return openErr
			}
			data, err = io.ReadAll(io.LimitReader(file, maxAnswerBytes+1))
			_ = file.Close()
		}
		if err != nil {
			return err
		}
		answer = strings.TrimSpace(string(data))
	}
	if answer == "" {
		return exitError{2, fmt.Errorf("answer must not be empty")}
	}
	if len(answer) > maxAnswerBytes {
		return exitError{2, fmt.Errorf("answer must not exceed %d bytes", maxAnswerBytes)}
	}
	entry, err := a.resolvePath(l, *repo)
	if err != nil {
		return err
	}
	cfg, err := config.Load(entry.RepoPath)
	if err != nil {
		return err
	}
	secrets := cfg.RedactionValues()
	if redact.StringWithSecrets(answer, secrets) != answer {
		return exitError{2, fmt.Errorf("answer must not contain a credential or configured secret")}
	}
	store := state.Store{Dir: l.RepoDir(entry.RepoID), RepoID: entry.RepoID, RepoPath: entry.RepoPath, Secrets: secrets}
	currentSnapshot, err := store.Load()
	if err != nil {
		return err
	}
	currentRequest := currentSnapshot.PendingRequests[*requestID]
	if currentRequest == nil {
		return exitError{4, fmt.Errorf("unknown request ID %s", *requestID)}
	}
	if currentRequest.Status == "answered" {
		if currentRequest.Answer != answer {
			return exitError{4, fmt.Errorf("request %s already has a different answer", *requestID)}
		}
		return a.output(*jsonOut, map[string]any{"request_id": *requestID, "recorded": true})
	}
	_, err = store.Update("answer_recorded", currentRequest.IssueNumber, "", map[string]string{"request_id": *requestID}, func(s *state.Snapshot) error {
		request := s.PendingRequests[*requestID]
		if request == nil {
			return exitError{4, fmt.Errorf("unknown request ID %s", *requestID)}
		}
		if request.Status == "answered" {
			if request.Answer == answer {
				return nil
			}
			return exitError{4, fmt.Errorf("request %s already has a different answer", *requestID)}
		}
		now := time.Now().UTC()
		request.Status, request.Answer, request.AnsweredAt = "answered", answer, &now
		issue := s.Issues[fmt.Sprint(request.IssueNumber)]
		if issue == nil {
			return fmt.Errorf("Issue #%d is missing from state", request.IssueNumber)
		}
		resumeStatus := request.ResumeStatus
		if resumeStatus == "" {
			resumeStatus = "resume_pending"
		}
		issue.Status, issue.RetryAfter, issue.UpdatedAt = resumeStatus, nil, now
		if resumeStatus == "resolving_conflict" {
			issue.GitHubSync = "conflict_retry"
		}
		issue.Answers = append(issue.Answers, state.AnswerRecord{RequestID: request.ID, Question: request.Question, Answer: answer, AnsweredAt: now})
		return nil
	})
	if err != nil {
		return err
	}
	return a.output(*jsonOut, map[string]any{"request_id": *requestID, "recorded": true})
}

func (a App) retryConflict(ctx context.Context, l layout.Layout, args []string) error {
	fs := flag.NewFlagSet("retry", flag.ContinueOnError)
	fs.SetOutput(a.Err)
	repo := fs.String("repo", "", "repository path")
	issueNumber := fs.Int("issue", 0, "blocked Issue number")
	jsonOut := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return exitError{2, err}
	}
	if *issueNumber <= 0 {
		return exitError{2, fmt.Errorf("--issue must be a positive Issue number")}
	}
	entry, err := a.resolvePath(l, *repo)
	if err != nil {
		return err
	}
	cfg, err := config.Load(entry.RepoPath)
	if err != nil {
		return err
	}
	store := state.Store{Dir: l.RepoDir(entry.RepoID), RepoID: entry.RepoID, RepoPath: entry.RepoPath, Secrets: cfg.RedactionValues()}
	snapshot, err := store.Load()
	if err != nil {
		return err
	}
	current := snapshot.Issues[strconv.Itoa(*issueNumber)]
	if current == nil {
		return exitError{4, fmt.Errorf("Issue #%d is missing from durable state", *issueNumber)}
	}
	if current.Status != "blocked" || current.GitHubSync != "" {
		return exitError{4, fmt.Errorf("Issue #%d must be fully synchronized and blocked before retry (status=%s github_sync=%s)", *issueNumber, current.Status, current.GitHubSync)}
	}
	reason := strings.ToLower(current.LastError)
	if current.ConflictRecovery == nil && !strings.Contains(reason, "merge conflict") && !strings.Contains(reason, "conflict recovery") {
		return exitError{4, fmt.Errorf("Issue #%d was not blocked by a Pull Request conflict", *issueNumber)}
	}
	if current.Worktree == "" || current.Branch == "" || current.PullRequestURL == "" {
		return exitError{4, fmt.Errorf("Issue #%d does not retain the worktree, branch, and Pull Request required for retry", *issueNumber)}
	}
	manager := worktree.Manager{StateRoot: l.Root, GitPath: entry.Commands["git"]}
	inspection, err := manager.Inspect(ctx, cfg, current.Worktree, current.Branch)
	if err != nil {
		return fmt.Errorf("inspect retry worktree: %w", err)
	}
	if !inspection.Exists || !inspection.Valid || inspection.Branch != current.Branch || !inspection.LocalBranchExists || !inspection.RemoteBranchExists {
		return exitError{4, fmt.Errorf("saved worktree/branch is not consistent enough to retry: %+v", inspection)}
	}
	client := gh.CLI{Path: entry.Commands["gh"], Secrets: cfg.RedactionValues()}
	remote, err := client.Inspect(ctx, cfg, *issueNumber, current.Branch)
	if err != nil {
		return fmt.Errorf("inspect retry Pull Request: %w", err)
	}
	matched := false
	for _, pr := range remote.PullRequests {
		if pr.URL == current.PullRequestURL && strings.EqualFold(pr.State, "open") && pr.HeadRefName == current.Branch {
			matched = true
			break
		}
	}
	if !matched {
		return exitError{4, fmt.Errorf("saved Pull Request is not open on branch %s: %s", current.Branch, current.PullRequestURL)}
	}
	retryID := state.NewID("retry")
	_, err = store.Update("conflict_recovery_retry_requested", *issueNumber, current.RunID, map[string]any{
		"retry_id": retryID, "pull_request_url": current.PullRequestURL, "previous_reason": current.LastError,
	}, func(s *state.Snapshot) error {
		item := s.Issues[strconv.Itoa(*issueNumber)]
		if item.Status != "blocked" || item.GitHubSync != "" {
			return fmt.Errorf("Issue #%d changed while retry was being prepared", *issueNumber)
		}
		if item.ConflictRecovery == nil {
			item.ConflictRecovery = &state.ConflictRecovery{
				PullRequestURL: item.PullRequestURL, StartedAt: time.Now().UTC(),
			}
		}
		item.Status = "resolving_conflict"
		item.GitHubSync = "conflict_retry"
		item.FailureKind = ""
		item.LastError = "explicit conflict recovery retry requested"
		item.RetryAfter = nil
		item.ConflictRecovery.Attempts = 0
		item.ConflictRecovery.BaseUpdates = 0
		item.ConflictRecovery.RetryID = retryID
		item.ConflictRecovery.LastReason = "explicit retry " + retryID
		item.ConflictRecovery.UpdatedAt = time.Now().UTC()
		item.UpdatedAt = time.Now().UTC()
		return nil
	})
	if err != nil {
		return err
	}
	if err := client.MarkConflictRetry(ctx, cfg, *issueNumber, retryID); err != nil {
		return fmt.Errorf("sync explicit conflict retry to GitHub (durable retry remains pending): %w", err)
	}
	_, err = store.Update("github_state_synced", *issueNumber, current.RunID, map[string]string{"state": "conflict_retry", "retry_id": retryID}, func(s *state.Snapshot) error {
		item := s.Issues[strconv.Itoa(*issueNumber)]
		if item.GitHubSync == "conflict_retry" {
			item.GitHubSync = ""
		}
		item.UpdatedAt = time.Now().UTC()
		return nil
	})
	if err != nil {
		return err
	}
	return a.output(*jsonOut, map[string]any{
		"issue": *issueNumber, "status": "resolving_conflict", "retry_id": retryID,
		"branch": current.Branch, "pull_request_url": current.PullRequestURL,
	})
}

func (a App) logs(l layout.Layout, args []string) error {
	fs := flag.NewFlagSet("logs", flag.ContinueOnError)
	fs.SetOutput(a.Err)
	repo := fs.String("repo", "", "repository path")
	stderrLog := fs.Bool("stderr", false, "show stderr log")
	if err := fs.Parse(args); err != nil {
		return exitError{2, err}
	}
	entry, err := a.resolvePath(l, *repo)
	if err != nil {
		return err
	}
	name := "supervisor.log"
	if *stderrLog {
		name = "launchd.stderr.log"
	}
	return retention.WriteHistory(a.Out, filepath.Join(l.RepoDir(entry.RepoID), name))
}

func (a App) supervise(ctx context.Context, l layout.Layout, args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(a.Err)
	repo := fs.String("repo", "", "repository path")
	if err := fs.Parse(args); err != nil {
		return exitError{2, err}
	}
	entry, err := a.resolvePath(l, *repo)
	if err != nil {
		return err
	}
	cfg, err := config.Load(entry.RepoPath)
	if err != nil {
		return err
	}
	backendID := cfg.Worker.Backend
	if backendID == "" {
		backendID = "codex"
	}
	registeredBackend := entry.WorkerBackend
	if registeredBackend == "" {
		registeredBackend = "codex"
	}
	if registeredBackend != backendID {
		return fmt.Errorf("worker backend changed from %s to %s; re-run agent-loop register --repo %q", registeredBackend, backendID, entry.RepoPath)
	}
	workerPath := entry.Commands[backendID]
	if workerPath == "" {
		return fmt.Errorf("registered worker command for backend %s is missing; re-run agent-loop register --repo %q", backendID, entry.RepoPath)
	}
	if currentPath, resolveErr := exec.LookPath(cfg.Worker.EffectiveCommand()); resolveErr == nil {
		absolute, _ := filepath.Abs(currentPath)
		if absolute != workerPath {
			return fmt.Errorf("worker command path drift detected for %s (registered=%s current=%s); re-run agent-loop register --repo %q", backendID, workerPath, absolute, entry.RepoPath)
		}
	}
	workerCompatibility := compat.ProbeBackend(ctx, backendID, workerPath)
	if !workerCompatibility.OK() {
		return fmt.Errorf("unsupported %s worker runtime: %s", backendID, compatibilityDetail(workerCompatibility))
	}
	if entry.WorkerVersion != "" && entry.WorkerVersion != workerCompatibility.Version {
		return fmt.Errorf("worker runtime version drift detected for %s (registered=%s current=%s); run doctor and re-register after compatibility review", backendID, entry.WorkerVersion, workerCompatibility.Version)
	}
	ghCompatibility := compat.ProbeGH(ctx, entry.Commands["gh"])
	if !ghCompatibility.OK() {
		return fmt.Errorf("unsupported gh CLI: %s", compatibilityDetail(ghCompatibility))
	}
	secrets := cfg.RedactionValues()
	notificationToken := ""
	if cfg.Notifications.Enabled {
		notificationToken, err = loadNotificationToken(l, entry)
		if err != nil {
			return fmt.Errorf("load notification credential: %w", err)
		}
		secrets = append(secrets, notificationToken)
	}
	logPolicy := retention.Policy{MaxBytes: cfg.Logs.RotateBytes, MaxAge: cfg.Logs.RotateInterval.Duration, Keep: cfg.Logs.Generations}
	for _, name := range []string{"launchd.stdout.log", "launchd.stderr.log"} {
		if err := retention.RotateExisting(filepath.Join(l.RepoDir(entry.RepoID), name), logPolicy); err != nil {
			return fmt.Errorf("rotate %s: %w", name, err)
		}
	}
	supervisorLog, err := retention.OpenWriter(filepath.Join(l.RepoDir(entry.RepoID), "supervisor.log"), logPolicy)
	if err != nil {
		return fmt.Errorf("open supervisor log: %w", err)
	}
	defer supervisorLog.Close()
	store := state.Store{
		Dir: l.RepoDir(entry.RepoID), RepoID: entry.RepoID, RepoPath: entry.RepoPath,
		Secrets: secrets, EventRetention: logPolicy, NotificationsEnabled: cfg.Notifications.Enabled,
	}
	cfg.Worker.Command = workerPath
	backend, err := worker.NewBackend(cfg, worker.FactoryOptions{StateDir: l.RepoDir(entry.RepoID), Secrets: secrets,
		RuntimeVersion: workerCompatibility.Version, ResumeSupported: boolPointer(workerCompatibility.Has("session_resume"))})
	if err != nil {
		return err
	}
	provider := ""
	if backendID == "opencode" {
		provider, _, _ = strings.Cut(cfg.Worker.Model, "/")
	}
	identity := worker.Identity{Backend: backendID, RuntimeVersion: workerCompatibility.Version, Provider: provider,
		RequestedModel: cfg.Worker.Model, ResolvedModel: cfg.Worker.Model, Variant: cfg.Worker.Variant}
	safeLog := redact.NewLineWriterWithSecrets(supervisorLog, secrets)
	defer safeLog.Flush()
	loop := &supervisor.Loop{
		Config: cfg, Store: store, GitHub: gh.CLI{Path: entry.Commands["gh"], Secrets: secrets},
		Worktrees:      worktree.Manager{StateRoot: l.Root, GitPath: entry.Commands["git"]},
		Worker:         backend,
		WorkerIdentity: identity,
		Publisher:      publish.Manager{GitPath: entry.Commands["git"], GHPath: entry.Commands["gh"], Secrets: secrets},
		Conflicts:      conflict.Manager{GitPath: entry.Commands["git"]},
		Logger:         log.New(safeLog, "agent-loop: ", log.LstdFlags|log.LUTC),
		Notifications:  notify.NewDispatcher(cfg, store, notificationToken),
	}
	err = loop.Run(ctx)
	var blocked supervisor.BlockedError
	if errors.As(err, &blocked) {
		fmt.Fprintln(a.Err, blocked.Error())
		return nil // Preserve blocked state; do not ask launchd to restart-loop.
	}
	return err
}

func compatibilityDetail(report compat.Report) string {
	value := fmt.Sprintf("version=%s minimum=%s", report.Version, report.Minimum)
	if len(report.Missing) > 0 {
		value += " missing=" + strings.Join(report.Missing, ",")
	}
	if !report.Has("session_resume") && report.Tool == "codex" && report.Has("exec_structured") {
		value += " session_resume=fresh-session-fallback"
	}
	if report.Detail != "" {
		value += " detail=" + report.Detail
	}
	return value
}

func boolPointer(value bool) *bool { return &value }

func (a App) resolve(l layout.Layout, name string, args []string) (registry.Entry, bool, error) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(a.Err)
	repo := fs.String("repo", "", "repository path")
	jsonOut := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return registry.Entry{}, false, exitError{2, err}
	}
	entry, err := a.resolvePath(l, *repo)
	return entry, *jsonOut, err
}

func (a App) resolvePath(l layout.Layout, path string) (registry.Entry, error) {
	cwd, _ := os.Getwd()
	entry, err := (registry.Store{Path: l.RegistryPath}).Resolve(path, cwd)
	if err != nil {
		return registry.Entry{}, exitError{3, err}
	}
	return entry, nil
}

func (a App) output(asJSON bool, value any) error {
	if asJSON {
		encoder := json.NewEncoder(a.Out)
		encoder.SetIndent("", "  ")
		return encoder.Encode(value)
	}
	switch v := value.(type) {
	case observe.Result:
		fmt.Fprintf(a.Out, "%s (revision %d)\n", v.Reason, v.Snapshot.StateRevision)
		for _, request := range v.PendingRequests {
			fmt.Fprintf(a.Out, "%s Issue #%d: %s\n", request.ID, request.IssueNumber, request.Question)
		}
	default:
		data, err := json.MarshalIndent(value, "", "  ")
		if err != nil {
			return err
		}
		fmt.Fprintln(a.Out, string(data))
	}
	return nil
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func truncate(value string, max int) string {
	if len(value) <= max {
		return value
	}
	return value[:max]
}
