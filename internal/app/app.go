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
	"reflect"
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
	"github.com/ishii1648/codex-issue-loop/internal/observe"
	"github.com/ishii1648/codex-issue-loop/internal/publish"
	"github.com/ishii1648/codex-issue-loop/internal/ratelimit"
	"github.com/ishii1648/codex-issue-loop/internal/redact"
	"github.com/ishii1648/codex-issue-loop/internal/registry"
	"github.com/ishii1648/codex-issue-loop/internal/retention"
	"github.com/ishii1648/codex-issue-loop/internal/state"
	"github.com/ishii1648/codex-issue-loop/internal/supervisor"
	"github.com/ishii1648/codex-issue-loop/internal/userrules"
	"github.com/ishii1648/codex-issue-loop/internal/webhook"
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
	In                      io.Reader
	Out                     io.Writer
	Err                     io.Writer
	ProcessController       supervisor.ProcessGroupController
	validateResumeWorkspace func(context.Context, worktree.Manager, config.Config, string, string) (worktree.LaunchValidation, error)
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
		return a.answer(ctx, l, args)
	case "retry":
		return a.retryConflict(ctx, l, args)
	case "resume-blocked":
		return a.resumeBlocked(ctx, l, args)
	case "recover-publication":
		return a.recoverPublication(ctx, l, args)
	case "recover-checks":
		return a.recoverPullRequestChecks(ctx, l, args)
	case "adopt-merged-pr":
		return a.adoptMergedPullRequest(ctx, l, args)
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
	case "broker":
		return a.runBroker(ctx, l, args)
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
  stop          Stop the repository LaunchAgent without deleting state
  restart       Restart the repository LaunchAgent
  status        Show durable and launchd state
  watch         Wait for needs_input, blocked, stopped, or optional idle
  answer        Record an answer for a pending request
  retry         Explicitly resume a blocked Pull Request conflict recovery
  resume-blocked  Explicitly resume a worker environment-blocked Issue
  recover-publication  Recover an eligible failed Issue at the publication boundary
  recover-checks  Return an externally repaired Pull Request to its saved lifecycle
  adopt-merged-pr  Adopt one externally merged saved branch into terminal state
  logs          Print supervisor logs
  cleanup       Preview or remove expired safe worktrees
  purge         Force-remove one explicitly confirmed worktree
  doctor        Validate dependencies, auth, config, and registration
  bootstrap-labels  Preview or create required GitHub labels
  run           Run the supervisor (used by launchd)`)
	// broker is intentionally omitted from the primary operator workflow; it is
	// the shared LaunchAgent entrypoint managed by register/start/unregister.
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
	backup, err := backupInstallation(l)
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

func recordSupervisorControl(store state.Store, supervisorState, message string) error {
	_, err := store.Update("supervisor_"+supervisorState, 0, "", map[string]string{"reason": message}, func(s *state.Snapshot) error {
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
	if cooldown, active, cooldownErr := (ratelimit.Store{Path: l.RateLimitPath()}).Current(time.Now().UTC()); cooldownErr != nil {
		return cooldownErr
	} else if active {
		snapshot.Supervisor.RateLimit = &state.RateLimit{
			Resource: cooldown.Resource, ObservedResetAt: cooldown.ResetAt,
			CooldownSource: cooldown.Source, SuppressedRetryCount: cooldown.SuppressedRetryCount,
		}
		if snapshot.Supervisor.RetryAfter == nil || cooldown.ResetAt.After(*snapshot.Supervisor.RetryAfter) {
			snapshot.Supervisor.RetryAfter = &cooldown.ResetAt
		}
	}
	launchStatus, err := (launchd.Manager{Layout: l, Launchctl: entry.Commands["launchctl"]}).Status(ctx, entry)
	if err != nil {
		return err
	}
	result := buildStatus(launchStatus, snapshot, cfg.Queue.Concurrency)
	if cfg.Webhook.Enabled() {
		manager := launchd.Manager{Layout: l, Launchctl: entry.Commands["launchctl"]}
		brokerLaunchd, statusErr := manager.BrokerStatus(ctx)
		if statusErr != nil {
			return statusErr
		}
		var brokerState webhook.Status
		data, readErr := os.ReadFile(filepath.Join(l.BrokerDir(), "status.json"))
		if readErr == nil {
			if err := json.Unmarshal(data, &brokerState); err != nil {
				return fmt.Errorf("decode broker status: %w", err)
			}
		} else if !errors.Is(readErr, os.ErrNotExist) {
			return readErr
		}
		sweep, sweepErr := webhook.LoadSweepState(l.RepoDir(entry.RepoID))
		if sweepErr != nil {
			return sweepErr
		}
		brokerState.LastSuccessfulSafetySweep = sweep.LastSuccessful
		brokerState.NotModified304 = sweep.NotModified304
		if mailboxEntries, mailboxErr := os.ReadDir(webhook.MailboxDir(l.RepoDir(entry.RepoID))); mailboxErr == nil {
			for _, mailboxEntry := range mailboxEntries {
				if !mailboxEntry.IsDir() && strings.HasSuffix(mailboxEntry.Name(), ".json") {
					brokerState.QueueDepth++
				}
			}
		} else if !errors.Is(mailboxErr, os.ErrNotExist) {
			return mailboxErr
		}
		result.Broker = &brokerStatus{Launchd: brokerLaunchd, State: brokerState, Sweep: sweep}
	}
	return a.output(jsonOut, result)
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

func (a App) answer(ctx context.Context, l layout.Layout, args []string) error {
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
		return a.answerOutput(*jsonOut, currentSnapshot, cfg.Queue.Concurrency, currentRequest)
	}
	if currentRequest.Status != "pending" {
		return exitError{4, fmt.Errorf("request %s is not pending", *requestID)}
	}
	currentIssue := currentSnapshot.Issues[fmt.Sprint(currentRequest.IssueNumber)]
	if currentIssue == nil {
		return exitError{4, fmt.Errorf("Issue #%d is missing from state", currentRequest.IssueNumber)}
	}
	parkedNeedsInput := currentRequest.ResumeStatus == ""
	if parkedNeedsInput {
		pendingForIssue := 0
		for _, request := range currentSnapshot.PendingRequests {
			if request != nil && request.IssueNumber == currentIssue.Number && request.Status == "pending" {
				pendingForIssue++
			}
		}
		if pendingForIssue != 1 {
			return exitError{4, fmt.Errorf("Issue #%d has ambiguous pending requests", currentIssue.Number)}
		}
		if currentIssue.Status != "needs_input" || currentIssue.WorkerPID != 0 || currentIssue.WorkerPGID != 0 || currentIssue.Lease != nil {
			return exitError{4, fmt.Errorf("Issue #%d is not a stopped parked needs-input continuation", currentIssue.Number)}
		}
		if err := state.ValidateNeedsInputPark(currentIssue, currentRequest); err != nil {
			return exitError{4, err}
		}
	}
	payload := map[string]any{"request_id": *requestID}
	updated, err := store.Update("answer_recorded", currentRequest.IssueNumber, currentIssue.RunID, payload, func(s *state.Snapshot) error {
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
		if request.Status != "pending" {
			return exitError{4, fmt.Errorf("request %s is not pending", *requestID)}
		}
		now := time.Now().UTC()
		request.Status, request.Answer, request.AnsweredAt = "answered", answer, &now
		issue := s.Issues[fmt.Sprint(request.IssueNumber)]
		if issue == nil {
			return fmt.Errorf("Issue #%d is missing from state", request.IssueNumber)
		}
		resumeStatus := request.ResumeStatus
		if resumeStatus == "" {
			pendingForIssue := 0
			for _, candidate := range s.PendingRequests {
				if candidate != nil && candidate.IssueNumber == issue.Number && candidate.Status == "pending" {
					pendingForIssue++
				}
			}
			if pendingForIssue != 0 {
				return exitError{4, fmt.Errorf("Issue #%d has ambiguous pending requests", issue.Number)}
			}
			if issue.Status != "needs_input" || issue.WorkerPID != 0 || issue.WorkerPGID != 0 || issue.Lease != nil {
				return exitError{4, fmt.Errorf("Issue #%d changed before its answer was recorded", issue.Number)}
			}
			if err := state.ValidateNeedsInputPark(issue, request); err != nil {
				return exitError{4, err}
			}
			resumeStatus = "answer_claim_waiting"
			if slot, ok := availableLeaseSlot(s, cfg.Queue.Concurrency, issue.ResourcePark.OriginalLease.Slot, issue.Number); ok {
				owner, resumeErr := state.ResumeParkedLease(s, issue.Number, issue.ResourcePark.ID, slot, now)
				if resumeErr == nil {
					resumeStatus = "resume_pending"
					payload["lease_owner"] = owner
					payload["lease_slot"] = slot
				} else {
					var conflict state.LeaseConflictError
					if !errors.As(resumeErr, &conflict) {
						return resumeErr
					}
					payload["claim_waiting"] = true
					payload["blocked_by_issue"] = conflict.IssueNumber
				}
			} else {
				payload["claim_waiting"] = true
				payload["blocked_by_worker_slot"] = true
			}
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
	_ = ctx // answer is a durable local transaction; the supervisor owns remote reconciliation.
	return a.answerOutput(*jsonOut, updated, cfg.Queue.Concurrency, updated.PendingRequests[*requestID])
}

func (a App) answerOutput(jsonOut bool, snapshot state.Snapshot, concurrency int, request *state.Request) error {
	output := map[string]any{"request_id": request.ID, "recorded": true}
	issue := snapshot.Issues[strconv.Itoa(request.IssueNumber)]
	if issue != nil {
		output["status"] = issue.Status
		if issue.ResourcePark != nil && issue.ResourcePark.Kind == state.ResourceParkKindNeedsInput {
			output["resource_park_id"] = issue.ResourcePark.ID
			output["claim_waiting"] = issue.Status == "answer_claim_waiting"
			if issue.Lease != nil {
				output["lease_owner"] = issue.Lease.Owner
			}
			status := buildStatus(launchd.Status{}, snapshot, concurrency)
			for _, candidate := range status.ResourceAdmission.ClaimWaitingCandidates {
				if candidate.IssueNumber == issue.Number {
					output["blocked_by"] = candidate.BlockedBy
					break
				}
			}
		}
	}
	return a.output(jsonOut, output)
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

func (a App) resumeBlocked(ctx context.Context, l layout.Layout, args []string) error {
	fs := flag.NewFlagSet("resume-blocked", flag.ContinueOnError)
	fs.SetOutput(a.Err)
	repo := fs.String("repo", "", "repository path")
	issueNumber := fs.Int("issue", 0, "environment-blocked Issue number")
	confirmed := fs.Bool("confirm-prerequisite-resolved", false, "confirm the external environment prerequisite is resolved")
	jsonOut := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return exitError{2, err}
	}
	if *issueNumber <= 0 {
		return exitError{2, fmt.Errorf("--issue must be a positive Issue number")}
	}
	if !*confirmed {
		return exitError{2, fmt.Errorf("--confirm-prerequisite-resolved is required")}
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
	pendingResume := false
	idempotentResume := false
	if current.EnvironmentResume != nil && current.EnvironmentResume.ID != "" && current.Status != "blocked" {
		if current.Status != "environment_resume_pending" {
			return exitError{4, fmt.Errorf("Issue #%d is not waiting for an environment resume (status=%s)", *issueNumber, current.Status)}
		}
		if current.Lease == nil || current.RunID == "" || current.Worktree == "" || current.Branch == "" {
			return exitError{4, fmt.Errorf("Issue #%d pending environment resume does not retain a consistent run, worktree, branch, and resource lease", *issueNumber)}
		}
		if current.GitHubSync == "environment_resume" {
			if current.EnvironmentResume.Status != "requested" {
				return exitError{4, fmt.Errorf("Issue #%d pending environment resume has inconsistent durable synchronization state", *issueNumber)}
			}
			pendingResume = true
		} else if current.GitHubSync != "" || current.EnvironmentResume.Status != "github_synced" {
			return exitError{4, fmt.Errorf("Issue #%d pending environment resume has inconsistent durable synchronization state", *issueNumber)}
		} else {
			idempotentResume = true
		}
	}
	if !pendingResume && !idempotentResume && (current.Status != "blocked" || current.GitHubSync != "") {
		return exitError{4, fmt.Errorf("Issue #%d must be fully synchronized and blocked before environment resume (status=%s github_sync=%s)", *issueNumber, current.Status, current.GitHubSync)}
	}
	interruptedResume := current.Status == "blocked" && current.EnvironmentResume != nil && current.EnvironmentResume.ID != "" &&
		(current.EnvironmentResume.Status == "requested" || current.EnvironmentResume.Status == "github_synced")
	interruptedWorkspaceRecovery := false
	var interruptedWorkspaceEvidence *state.InterruptedWorkspaceResumeEvidence
	if state.MayHaveInterruptedWorkspaceResumeEvidence(current) {
		interruptedWorkspaceEvidence, err = store.InterruptedWorkspaceResumeEvidence(*current)
		if err != nil {
			return exitError{4, fmt.Errorf("verify interrupted missing-workspace environment resume: %w; state was not changed", err)}
		}
		interruptedWorkspaceRecovery = true
		// A running EnvironmentResume is only an interrupted intent after the
		// exact v0.6.14 durable chain above has established that authority.
		interruptedResume = true
	}
	resumeIntent := interruptedResume || pendingResume || idempotentResume
	var legacyCause *state.BlockedCause
	var legacyRecovery *state.LegacyWorkerBlockRecovery
	if state.MayHaveLegacyWorkerBlockRecoveryProvenance(current) {
		legacyCause, _ = store.LegacyWorkerBlockProvenance(*current)
		if current.Lease == nil {
			legacyRecovery, _ = store.LegacyWorkerBlockRecoveryEvidence(*current)
		}
	}
	legacyWorkerBlock := legacyCause != nil
	if !legacyWorkerBlock && !interruptedWorkspaceRecovery && (current.BlockedCause == nil || current.BlockedCause.Origin != "worker" || current.BlockedCause.Kind != "environment" || !current.BlockedCause.Resumable) {
		return exitError{4, fmt.Errorf("Issue #%d is not a resumable worker environment block", *issueNumber)}
	}
	if current.ConflictRecovery != nil {
		return exitError{4, fmt.Errorf("Issue #%d is a Pull Request conflict block; use agent-loop retry", *issueNumber)}
	}
	parkedClaim := current.ResourcePark != nil && current.ResourcePark.Status == "parked" && current.ResourcePark.ID != ""
	if current.RunID == "" || current.Worktree == "" || current.Branch == "" || (current.Lease == nil && !parkedClaim && legacyRecovery == nil && !interruptedResume) {
		return exitError{4, fmt.Errorf("Issue #%d does not retain a consistent run, worktree, branch, and resource lease", *issueNumber)}
	}
	controller := a.ProcessController
	if controller == nil {
		controller = supervisor.OSProcessGroupController{}
	}
	pgid := current.WorkerPGID
	if pgid <= 1 {
		pgid = current.WorkerPID
	}
	if controller.Alive(current.WorkerPID) || controller.GroupAlive(pgid) {
		return exitError{4, fmt.Errorf("Issue #%d still has an active worker process", *issueNumber)}
	}
	for _, request := range snapshot.PendingRequests {
		if request != nil && request.IssueNumber == *issueNumber && request.Status == "pending" {
			return exitError{4, fmt.Errorf("Issue #%d has a pending manual answer request and is not an environment-blocked resume candidate", *issueNumber)}
		}
	}
	manager := worktree.Manager{StateRoot: l.Root, GitPath: entry.Commands["git"]}
	inspection, err := manager.Inspect(ctx, cfg, current.Worktree, current.Branch)
	if err != nil {
		return fmt.Errorf("inspect environment resume worktree: %w", err)
	}
	if !inspection.Exists || !inspection.Valid || inspection.Branch != current.Branch || !inspection.LocalBranchExists {
		return exitError{4, fmt.Errorf("saved worktree/branch is not consistent enough to resume: %+v", inspection)}
	}
	if interruptedWorkspaceRecovery && interruptedWorkspaceEvidence.WorktreeHead != "" && inspection.Head != interruptedWorkspaceEvidence.WorktreeHead {
		return exitError{4, fmt.Errorf("Issue #%d interrupted environment resume worktree HEAD changed; state was not changed", *issueNumber)}
	}
	launchValidator := a.validateResumeWorkspace
	if launchValidator == nil {
		launchValidator = func(ctx context.Context, manager worktree.Manager, cfg config.Config, path, branch string) (worktree.LaunchValidation, error) {
			return manager.ValidateLaunch(ctx, cfg, path, branch)
		}
	}
	launch, err := launchValidator(ctx, manager, cfg, current.Worktree, current.Branch)
	if err != nil || !launch.Valid {
		if err == nil {
			err = fmt.Errorf("worktree validator did not establish a valid launch boundary")
		}
		return exitError{4, fmt.Errorf("saved workspace provenance recovery validation failed: %w", err)}
	}
	workspaceBackfill := current.Workspace == nil
	if workspaceBackfill && (pendingResume || idempotentResume) {
		return exitError{4, fmt.Errorf("Issue #%d has an ambiguous in-progress environment resume with missing workspace provenance", *issueNumber)}
	}
	if current.Workspace != nil && !current.Workspace.Matches(launch.CanonicalCWD, launch.Branch, entry.RepoID, cfg.GitHub.Repo,
		cfg.GitHub.RepositoryID, launch.CommonDir, launch.MainCheckout) {
		return exitError{4, fmt.Errorf("saved workspace provenance does not match the validated launch target")}
	}
	if idempotentResume {
		baseSHA := current.Lease.BaseSHA
		output := map[string]any{
			"issue": *issueNumber, "status": current.Status, "resume_id": current.EnvironmentResume.ID,
			"branch": current.Branch, "worktree": current.Worktree, "base_sha": baseSHA,
			"current_base_sha": current.EnvironmentResume.CurrentBaseSHA, "idempotent": true,
			"workspace_provenance": current.Workspace,
		}
		if current.ResourcePark != nil {
			output["resource_park_id"] = current.ResourcePark.ID
			output["lease_owner"] = current.Lease.Owner
		}
		return a.output(*jsonOut, output)
	}
	if interruptedResume && current.Lease == nil && current.EnvironmentResume.BaseSHA == "" {
		baseSHA, recoverErr := store.EnvironmentResumeBaseSHA(*issueNumber, current.RunID, current.EnvironmentResume.ID)
		if recoverErr != nil {
			return exitError{4, fmt.Errorf("recover publication base SHA for interrupted environment resume: %w; state was not changed", recoverErr)}
		}
		current.EnvironmentResume.BaseSHA = baseSHA
	}
	publicationState := current
	if current.Lease == nil && parkedClaim {
		copy := *current
		lease := current.ResourcePark.OriginalLease
		copy.Lease = &lease
		publicationState = &copy
	} else if current.Lease == nil && legacyRecovery != nil {
		copy := *current
		copy.Lease = &state.ResourceLease{BaseSHA: legacyRecovery.BaseSHA}
		publicationState = &copy
	}
	baseSHA, err := environmentResumeBaseSHA(ctx, entry.Commands["git"], cfg, publicationState, inspection)
	if err != nil {
		return exitError{4, err}
	}
	currentBaseSHA, err := currentRemoteBaseSHA(ctx, entry.Commands["git"], current.Worktree, cfg.Git.BaseBranch)
	if err != nil {
		return exitError{4, err}
	}
	if interruptedWorkspaceRecovery && (baseSHA != interruptedWorkspaceEvidence.BaseSHA || currentBaseSHA != interruptedWorkspaceEvidence.CurrentBaseSHA) {
		return exitError{4, fmt.Errorf("Issue #%d interrupted environment resume base SHA provenance changed; state was not changed", *issueNumber)}
	}
	client := gh.CLI{Path: entry.Commands["gh"], Secrets: cfg.RedactionValues()}
	remote, err := client.Inspect(ctx, cfg, *issueNumber, current.Branch)
	if err != nil {
		return fmt.Errorf("inspect environment-blocked Issue: %w", err)
	}
	if !strings.EqualFold(remote.Issue.State, "open") {
		return exitError{4, fmt.Errorf("Issue #%d is not open", *issueNumber)}
	}
	labels := map[string]bool{}
	for _, label := range remote.Issue.Labels {
		labels[strings.ToLower(label)] = true
	}
	blockedLabel := ""
	for _, label := range cfg.GitHub.ExcludeLabels {
		if strings.EqualFold(label, "blocked") {
			blockedLabel = strings.ToLower(label)
			continue
		}
		if labels[strings.ToLower(label)] {
			return exitError{4, fmt.Errorf("Issue #%d has manual exclusion label %q", *issueNumber, label)}
		}
	}
	if blockedLabel == "" {
		return exitError{4, fmt.Errorf("GitHub exclude labels do not define the supervisor-owned blocked label")}
	}
	runningLabel := labels[strings.ToLower(cfg.GitHub.RunningLabel)]
	blocked := labels[blockedLabel]
	if interruptedWorkspaceRecovery {
		resumeMarker := "<!-- codex-issue-loop:environment-resume:" + current.EnvironmentResume.ID + " -->"
		failureMarker := fmt.Sprintf("<!-- codex-issue-loop:failed:%d -->", *issueNumber)
		resumeMarkers, expectedResumeMarkers, failureMarkers, expectedFailureMarkers := 0, 0, 0, 0
		failureIDMarkers, originalFailureIDMarkers, workspaceFailureIDMarkers := 0, 0, 0
		originalFailureComments, workspaceFailureComments := 0, 0
		originalFailureReason := "worker blocked: " + interruptedWorkspaceEvidence.PreviousReason
		workspaceFailureReason := current.BlockedCause.Reason
		originalFailureIDMarker := failureIDMarker(originalFailureReason)
		workspaceFailureIDMarker := failureIDMarker(workspaceFailureReason)
		for _, comment := range remote.Issue.Comments {
			resumeMarkers += strings.Count(comment, "<!-- codex-issue-loop:environment-resume:")
			expectedResumeMarkers += strings.Count(comment, resumeMarker)
			failureMarkers += strings.Count(comment, "<!-- codex-issue-loop:failed:")
			expectedFailureMarkers += strings.Count(comment, failureMarker)
			failureIDMarkers += strings.Count(comment, "<!-- codex-issue-loop:failure:")
			originalFailureIDMarkers += strings.Count(comment, originalFailureIDMarker)
			workspaceFailureIDMarkers += strings.Count(comment, workspaceFailureIDMarker)
			if exactFailureComment(comment, *issueNumber, originalFailureReason) {
				originalFailureComments++
			}
			if exactFailureComment(comment, *issueNumber, workspaceFailureReason) {
				workspaceFailureComments++
			}
		}
		if !blocked || runningLabel || resumeMarkers != 2 || expectedResumeMarkers != 2 || failureMarkers != 2 || expectedFailureMarkers != 2 ||
			failureIDMarkers != 2 || originalFailureIDMarkers != 1 || workspaceFailureIDMarkers != 1 ||
			originalFailureComments != 1 || workspaceFailureComments != 1 {
			return exitError{4, fmt.Errorf("Issue #%d GitHub state does not prove the interrupted missing-workspace resume", *issueNumber)}
		}
	} else if resumeIntent {
		switch current.EnvironmentResume.Status {
		case "requested":
			if blocked == runningLabel {
				return exitError{4, fmt.Errorf("Issue #%d interrupted environment resume has ambiguous GitHub blocked/running labels", *issueNumber)}
			}
		case "github_synced":
			marker := "<!-- codex-issue-loop:environment-resume:" + current.EnvironmentResume.ID + " -->"
			commented := false
			for _, comment := range remote.Issue.Comments {
				commented = commented || strings.Contains(comment, marker)
			}
			if blocked || !runningLabel || !commented {
				return exitError{4, fmt.Errorf("Issue #%d GitHub state no longer matches the synchronized environment resume", *issueNumber)}
			}
		}
	} else if !blocked || runningLabel {
		return exitError{4, fmt.Errorf("Issue #%d does not have only the supervisor-owned blocked label", *issueNumber)}
	}
	if labels[strings.ToLower(cfg.GitHub.DoneLabel)] || labels[strings.ToLower(cfg.GitHub.FailedLabel)] || labels[strings.ToLower(cfg.GitHub.NeedsInputLabel)] {
		return exitError{4, fmt.Errorf("Issue #%d has an incompatible terminal or needs-input label", *issueNumber)}
	}
	if current.PullRequestURL == "" {
		if len(remote.PullRequests) != 0 {
			return exitError{4, fmt.Errorf("Issue #%d has a Pull Request not recorded in durable state", *issueNumber)}
		}
	} else {
		matched := len(remote.PullRequests) == 1 && remote.PullRequests[0].URL == current.PullRequestURL &&
			strings.EqualFold(remote.PullRequests[0].State, "open") && remote.PullRequests[0].HeadRefName == current.Branch
		if !matched {
			return exitError{4, fmt.Errorf("Issue #%d saved Pull Request is inconsistent with GitHub", *issueNumber)}
		}
	}

	resumeID := state.NewID("resume")
	if resumeIntent {
		resumeID = current.EnvironmentResume.ID
	}
	now := time.Now().UTC()
	validatedWorkspace := state.WorkerWorkspace{
		Path: launch.CanonicalCWD, Branch: launch.Branch,
		RepoID: entry.RepoID, Repository: cfg.GitHub.Repo, RepositoryID: cfg.GitHub.RepositoryID,
		GitCommonDir: launch.CommonDir, MainCheckout: launch.MainCheckout, CapturedAt: now,
	}
	previousReason := current.LastError
	if interruptedWorkspaceRecovery {
		previousReason = interruptedWorkspaceEvidence.PreviousReason
	} else if current.BlockedCause != nil && current.BlockedCause.Reason != "" {
		previousReason = current.BlockedCause.Reason
	} else if legacyCause != nil {
		previousReason = legacyCause.Reason
	}
	if !pendingResume {
		eventType := "environment_resume_requested"
		if interruptedResume {
			eventType = "environment_resume_recovered"
		}
		resumePayload := map[string]any{
			"resume_id": resumeID, "previous_reason": previousReason, "resource_park_id": resourceParkID(current), "parked_lease_reacquired": parkedClaim,
			"legacy_worker_block":    legacyWorkerBlock || (interruptedWorkspaceEvidence != nil && interruptedWorkspaceEvidence.LegacyLeaseRecovered),
			"legacy_lease_recovered": legacyRecovery != nil || (interruptedWorkspaceEvidence != nil && interruptedWorkspaceEvidence.LegacyLeaseRecovered),
			"interrupted_resume":     interruptedResume,
			"base_sha":               baseSHA, "current_base_sha": currentBaseSHA,
			"interrupted_workspace_recovery": interruptedWorkspaceRecovery,
			"interrupted_blocked_cause":      current.BlockedCause,
			"workspace_recovery": map[string]any{
				"old_provenance_missing": workspaceBackfill,
				"operator_confirmation":  map[string]any{"confirm_prerequisite_resolved": true},
				"expected": map[string]any{
					"path": current.Worktree, "branch": current.Branch, "repo_id": entry.RepoID,
					"repository": cfg.GitHub.Repo, "repository_id": cfg.GitHub.RepositoryID,
					"git_common_dir": launch.CommonDir, "main_checkout": launch.MainCheckout,
				},
				"actual": map[string]any{
					"path": launch.CanonicalCWD, "branch": launch.Branch, "repo_id": entry.RepoID,
					"repository": cfg.GitHub.Repo, "repository_id": cfg.GitHub.RepositoryID,
					"git_common_dir": launch.CommonDir, "main_checkout": launch.MainCheckout,
					"validation": launch,
				},
			},
		}
		_, err = store.Update(eventType, *issueNumber, current.RunID, resumePayload, func(s *state.Snapshot) error {
			if s.StateRevision != snapshot.StateRevision {
				return fmt.Errorf("Issue #%d durable state changed while environment resume was being prepared", *issueNumber)
			}
			item := s.Issues[strconv.Itoa(*issueNumber)]
			if item == nil || item.RunID != current.RunID || item.Status != "blocked" || item.GitHubSync != "" ||
				(interruptedResume && (item.EnvironmentResume == nil || item.EnvironmentResume.ID != resumeID || item.EnvironmentResume.Status != current.EnvironmentResume.Status)) {
				return fmt.Errorf("Issue #%d changed while environment resume was being prepared", *issueNumber)
			}
			if item.Worktree != current.Worktree || item.Branch != current.Branch || item.PullRequestURL != current.PullRequestURL ||
				!reflect.DeepEqual(item.Lease, current.Lease) || !reflect.DeepEqual(item.Workspace, current.Workspace) {
				return fmt.Errorf("Issue #%d worktree, branch, Pull Request, resource lease, or workspace provenance changed while environment resume was being prepared", *issueNumber)
			}
			transactionPGID := item.WorkerPGID
			if transactionPGID <= 1 {
				transactionPGID = item.WorkerPID
			}
			if !reflect.DeepEqual(item.BlockedCause, current.BlockedCause) || item.WorkerPID != current.WorkerPID || item.WorkerPGID != current.WorkerPGID ||
				controller.Alive(item.WorkerPID) || controller.GroupAlive(transactionPGID) {
				return fmt.Errorf("Issue #%d block provenance or worker process changed while environment resume was being prepared", *issueNumber)
			}
			for _, request := range s.PendingRequests {
				if request != nil && request.IssueNumber == *issueNumber && request.Status == "pending" {
					return fmt.Errorf("Issue #%d gained a pending manual answer request while environment resume was being prepared", *issueNumber)
				}
			}
			if item.Lease == nil && legacyRecovery != nil {
				if legacyRecovery.BaseSHA != baseSHA || !reflect.DeepEqual(&legacyRecovery.Cause, legacyCause) ||
					(item.BlockedCause != nil && !reflect.DeepEqual(item.BlockedCause, &legacyRecovery.Cause)) {
					return fmt.Errorf("Issue #%d legacy recovery evidence changed while environment resume was being prepared", *issueNumber)
				}
			}
			if item.Lease == nil && (legacyRecovery != nil || interruptedResume) {
				for _, other := range s.Issues {
					if other != nil && other.Number != item.Number && other.Lease != nil {
						return fmt.Errorf("Issue #%d cannot recover repo:* while Issue #%d retains a resource lease", *issueNumber, other.Number)
					}
				}
			}
			if interruptedWorkspaceEvidence != nil && interruptedWorkspaceEvidence.LegacyLeaseRecovered {
				if item.Lease == nil || item.Lease.Owner != interruptedWorkspaceEvidence.LeaseOwner ||
					item.Lease.Slot != interruptedWorkspaceEvidence.LeaseSlot || item.LeaseGeneration != interruptedWorkspaceEvidence.LeaseOwner.Generation {
					return fmt.Errorf("Issue #%d v0.6.14 recovered lease changed while workspace recovery was being prepared", *issueNumber)
				}
				for _, other := range s.Issues {
					if other == nil || other.Number == item.Number || other.Lease == nil {
						continue
					}
					if occupiesWorkerSlot(other.Status) && other.Lease.Slot == item.Lease.Slot {
						return fmt.Errorf("Issue #%d cannot fence recovered slot %d while Issue #%d occupies it", *issueNumber, item.Lease.Slot, other.Number)
					}
					if state.ResourcesConflict(item.Lease.ResolvedResources, other.Lease.ResolvedResources) {
						return fmt.Errorf("Issue #%d cannot fence recovered resources while Issue #%d retains a conflicting lease", *issueNumber, other.Number)
					}
				}
				owner, transferErr := state.TransferIssueLease(item, interruptedWorkspaceEvidence.LeaseOwner, item.RunID)
				if transferErr != nil {
					return fmt.Errorf("Issue #%d cannot fence v0.6.14 recovered lease: %w", *issueNumber, transferErr)
				}
				resumePayload["lease_owner"] = owner
				resumePayload["lease_slot"] = item.Lease.Slot
				resumePayload["previous_lease_owner"] = interruptedWorkspaceEvidence.LeaseOwner
			}
			if parkedClaim {
				if item.ResourcePark == nil || current.ResourcePark == nil || !reflect.DeepEqual(item.ResourcePark, current.ResourcePark) {
					return fmt.Errorf("Issue #%d parked resource claim changed while environment resume was being prepared", *issueNumber)
				}
				slot, ok := availableLeaseSlot(s, cfg.Queue.Concurrency, item.ResourcePark.OriginalLease.Slot, item.Number)
				if !ok {
					return fmt.Errorf("Issue #%d parked resource claim is waiting for an available worker slot", *issueNumber)
				}
				owner, reserveErr := state.ResumeParkedLease(s, item.Number, item.ResourcePark.ID, slot, now)
				if reserveErr != nil {
					return fmt.Errorf("Issue #%d cannot reacquire parked resource claim: %w", *issueNumber, reserveErr)
				}
				if item.Lease.BaseSHA == "" {
					item.Lease.BaseSHA = baseSHA
				}
				if item.ResourcePark.ResumeOwner == nil || *item.ResourcePark.ResumeOwner != owner {
					return fmt.Errorf("Issue #%d parked resource owner was not fenced", *issueNumber)
				}
				resumePayload["lease_owner"] = owner
				resumePayload["lease_slot"] = item.Lease.Slot
			}
			if workspaceBackfill {
				if item.Workspace != nil {
					return fmt.Errorf("Issue #%d workspace provenance changed while environment resume was being prepared", *issueNumber)
				}
				workspace := validatedWorkspace
				item.Workspace = &workspace
			}
			item.Status = "environment_resume_pending"
			item.GitHubSync = "environment_resume"
			if interruptedWorkspaceRecovery {
				blockedAt := item.EnvironmentResume.ConfirmedAt
				if item.ResourcePark != nil && !item.ResourcePark.ParkedAt.IsZero() {
					blockedAt = item.ResourcePark.ParkedAt
				}
				item.BlockedCause = &state.BlockedCause{
					Origin: "worker", Kind: "environment", Resumable: true,
					Reason: interruptedWorkspaceEvidence.PreviousReason, BlockedAt: blockedAt,
				}
				item.LastError = interruptedWorkspaceEvidence.PreviousReason
			}
			if item.BlockedCause == nil && legacyWorkerBlock {
				cause := *legacyCause
				item.BlockedCause = &cause
			}
			if item.Lease == nil && (legacyRecovery != nil || interruptedResume) {
				item.LeaseGeneration++
				owner := state.LeaseOwner{RunID: item.RunID, Generation: item.LeaseGeneration}
				item.Lease = &state.ResourceLease{
					Owner: owner, Slot: 0, DeclaredResources: []string{},
					ResolvedResources: []string{state.RepositoryResource}, BaseSHA: baseSHA, ReservedAt: now,
				}
			} else if item.Lease != nil && item.Lease.BaseSHA == "" {
				item.Lease.BaseSHA = baseSHA
			}
			if interruptedResume {
				item.EnvironmentResume.Status = "requested"
				item.EnvironmentResume.BaseSHA = baseSHA
				item.EnvironmentResume.CurrentBaseSHA = currentBaseSHA
			} else {
				item.EnvironmentResume = &state.EnvironmentResume{ID: resumeID, Status: "requested", ConfirmedAt: now, PreviousReason: previousReason, BaseSHA: baseSHA, CurrentBaseSHA: currentBaseSHA}
			}
			item.RetryAfter = nil
			item.UpdatedAt = now
			return nil
		})
		if err != nil {
			return err
		}
	}
	if err := client.MarkEnvironmentResume(ctx, cfg, *issueNumber, resumeID); err != nil {
		return fmt.Errorf("sync environment resume to GitHub (durable resume remains pending): %w", err)
	}
	updated, err := store.Update("github_state_synced", *issueNumber, current.RunID, map[string]string{"state": "environment_resume", "resume_id": resumeID}, func(s *state.Snapshot) error {
		item := s.Issues[strconv.Itoa(*issueNumber)]
		if item != nil && item.GitHubSync == "environment_resume" && item.EnvironmentResume != nil && item.EnvironmentResume.ID == resumeID {
			item.GitHubSync = ""
			item.EnvironmentResume.Status = "github_synced"
			item.UpdatedAt = time.Now().UTC()
		}
		return nil
	})
	if err != nil {
		return err
	}
	status := "environment_resume_pending"
	var leaseOwner *state.LeaseOwner
	parkID := resourceParkID(current)
	if item := updated.Issues[strconv.Itoa(*issueNumber)]; item != nil {
		status = item.Status
		if item.Lease != nil {
			owner := item.Lease.Owner
			leaseOwner = &owner
		}
		if item.ResourcePark != nil {
			parkID = item.ResourcePark.ID
		}
	}
	output := map[string]any{
		"issue": *issueNumber, "status": status, "resume_id": resumeID,
		"branch": current.Branch, "worktree": current.Worktree, "session_id": current.SessionID,
		"base_sha": baseSHA, "current_base_sha": currentBaseSHA,
		"dirty": inspection.Dirty, "unpushed_commits": inspection.UnpushedCommits,
		"workspace_provenance_backfilled": workspaceBackfill,
	}
	if leaseOwner != nil {
		output["lease_owner"] = leaseOwner
	}
	if parkID != "" {
		output["resource_park_id"] = parkID
	}
	return a.output(*jsonOut, output)
}

func exactFailureComment(comment string, issueNumber int, reason string) bool {
	return comment == failureComment(issueNumber, reason)
}

func failureComment(issueNumber int, reason string) string {
	return fmt.Sprintf("<!-- codex-issue-loop:failed:%d -->\n%s\nAutomation stopped: %s", issueNumber, failureIDMarker(reason), reason)
}

func failureIDMarker(reason string) string {
	digest := sha256.Sum256([]byte(reason))
	return fmt.Sprintf("<!-- codex-issue-loop:failure:%x -->", digest[:8])
}

func resourceParkID(issue *state.Issue) string {
	if issue == nil || issue.ResourcePark == nil {
		return ""
	}
	return issue.ResourcePark.ID
}

func availableLeaseSlot(snapshot *state.Snapshot, limit, preferred, issueNumber int) (int, bool) {
	if limit < 1 {
		limit = 1
	}
	used := map[int]bool{}
	for _, other := range snapshot.Issues {
		if other == nil || other.Number == issueNumber || other.Lease == nil || !occupiesWorkerSlot(other.Status) {
			continue
		}
		used[other.Lease.Slot] = true
	}
	if preferred >= 0 && preferred < limit && !used[preferred] {
		return preferred, true
	}
	for slot := 0; slot < limit; slot++ {
		if !used[slot] {
			return slot, true
		}
	}
	return -1, false
}

func environmentResumeBaseSHA(ctx context.Context, git string, cfg config.Config, current *state.Issue, inspection worktree.Inspection) (string, error) {
	return verifiedPublicationBaseSHA(ctx, git, cfg, current, inspection, "environment resume")
}

func currentRemoteBaseSHA(ctx context.Context, git, worktreePath, baseBranch string) (string, error) {
	if git == "" {
		git = "git"
	}
	ref := "refs/heads/" + baseBranch
	out, err := exec.CommandContext(ctx, git, "-C", worktreePath, "ls-remote", "--exit-code", "origin", ref).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("inspect current configured base branch %q for environment resume: %w: %s; state was not changed", baseBranch, err, strings.TrimSpace(string(out)))
	}
	fields := strings.Fields(string(out))
	if len(fields) != 2 || fields[1] != ref || !canonicalGitObjectID(fields[0]) {
		return "", fmt.Errorf("inspect current configured base branch %q for environment resume: git returned an invalid ref; state was not changed", baseBranch)
	}
	return fields[0], nil
}

func canonicalGitObjectID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

func verifiedPublicationBaseSHA(ctx context.Context, git string, cfg config.Config, current *state.Issue, inspection worktree.Inspection, operation string) (string, error) {
	if git == "" {
		git = "git"
	}
	baseSHA := ""
	if current.Lease != nil {
		baseSHA = current.Lease.BaseSHA
		if baseSHA != strings.TrimSpace(baseSHA) {
			return "", fmt.Errorf("saved publication base SHA is not canonical; state was not changed")
		}
	}
	if baseSHA == "" && current.EnvironmentResume != nil {
		baseSHA = current.EnvironmentResume.BaseSHA
		if baseSHA != strings.TrimSpace(baseSHA) {
			return "", fmt.Errorf("saved environment resume base SHA is not canonical; state was not changed")
		}
	}
	if baseSHA == "" {
		ref := "refs/remotes/origin/" + cfg.Git.BaseBranch
		out, err := exec.CommandContext(ctx, git, "-C", current.Worktree, "rev-parse", "--verify", ref+"^{commit}").Output()
		if err != nil {
			return "", fmt.Errorf("resolve configured base branch %q for %s: %w; fetch origin/%s and retry without editing durable state", cfg.Git.BaseBranch, operation, err, cfg.Git.BaseBranch)
		}
		baseSHA = strings.TrimSpace(string(out))
		if baseSHA == "" {
			return "", fmt.Errorf("resolve configured base branch %q for %s: git returned an empty commit SHA; state was not changed", cfg.Git.BaseBranch, operation)
		}
	}
	verified, err := exec.CommandContext(ctx, git, "-C", current.Worktree, "rev-parse", "--verify", baseSHA+"^{commit}").Output()
	if err != nil {
		return "", fmt.Errorf("verify publication base SHA %q for %s: %w; state was not changed", baseSHA, operation, err)
	}
	if strings.TrimSpace(string(verified)) != baseSHA {
		return "", fmt.Errorf("verify publication base SHA %q for %s: value is not a full canonical commit SHA; state was not changed", baseSHA, operation)
	}
	if inspection.Head == "" {
		return "", fmt.Errorf("verify publication base SHA for %s: saved worktree HEAD is empty; state was not changed", operation)
	}
	if err := exec.CommandContext(ctx, git, "-C", current.Worktree, "merge-base", "--is-ancestor", baseSHA, inspection.Head).Run(); err != nil {
		return "", fmt.Errorf("verify publication base SHA %s for %s: it is not an ancestor of worktree HEAD %s; state was not changed", baseSHA, operation, inspection.Head)
	}
	return baseSHA, nil
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
	if cfg.Worker.CommandNetwork.LocalhostOnly() && !workerCompatibility.Has("localhost_network_proxy") {
		return fmt.Errorf("Codex runtime does not provide the required localhost network proxy capability; worker was not started")
	}
	if entry.WorkerVersion != "" && entry.WorkerVersion != workerCompatibility.Version {
		return fmt.Errorf("worker runtime version drift detected for %s (registered=%s current=%s); run doctor and re-register after compatibility review", backendID, entry.WorkerVersion, workerCompatibility.Version)
	}
	ghCompatibility := compat.ProbeGH(ctx, entry.Commands["gh"])
	if !ghCompatibility.OK() {
		return fmt.Errorf("unsupported gh CLI: %s", compatibilityDetail(ghCompatibility))
	}
	secrets := cfg.RedactionValues()
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
		Secrets: secrets, EventRetention: logPolicy,
	}
	cfg.Worker.Command = workerPath
	backend, err := worker.NewBackend(cfg, worker.FactoryOptions{StateDir: l.RepoDir(entry.RepoID), Secrets: secrets,
		RuntimeVersion: workerCompatibility.Version, ResumeSupported: boolPointer(workerCompatibility.Has("session_resume")),
		AppServerGoalSupported: boolPointer(workerCompatibility.Has("app_server_goal"))})
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
		RateLimits:     ratelimit.Store{Path: l.RateLimitPath()},
		Worktrees:      worktree.Manager{StateRoot: l.Root, GitPath: entry.Commands["git"]},
		Worker:         backend,
		WorkerIdentity: identity,
		Publisher:      publish.Manager{GitPath: entry.Commands["git"], GHPath: entry.Commands["gh"], GofmtPath: entry.Commands["gofmt"], Secrets: secrets},
		Conflicts:      conflict.Manager{GitPath: entry.Commands["git"]},
		Logger:         log.New(safeLog, "agent-loop: ", log.LstdFlags|log.LUTC),
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
