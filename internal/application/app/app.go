package app

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	gh "github.com/ishii1648/codex-issue-loop/internal/adapter/github"
	"github.com/ishii1648/codex-issue-loop/internal/adapter/publish"
	"github.com/ishii1648/codex-issue-loop/internal/adapter/state"
	"github.com/ishii1648/codex-issue-loop/internal/adapter/worker"
	"github.com/ishii1648/codex-issue-loop/internal/adapter/worktree"
	"github.com/ishii1648/codex-issue-loop/internal/application/conflict"
	schema "github.com/ishii1648/codex-issue-loop/internal/application/migration"
	"github.com/ishii1648/codex-issue-loop/internal/application/observe"
	"github.com/ishii1648/codex-issue-loop/internal/application/supervisor"
	"github.com/ishii1648/codex-issue-loop/internal/domain/statecontract"
	"github.com/ishii1648/codex-issue-loop/internal/platform/compat"
	"github.com/ishii1648/codex-issue-loop/internal/platform/config"
	"github.com/ishii1648/codex-issue-loop/internal/platform/layout"
	"github.com/ishii1648/codex-issue-loop/internal/platform/ratelimit"
	"github.com/ishii1648/codex-issue-loop/internal/platform/redact"
	"github.com/ishii1648/codex-issue-loop/internal/platform/registry"
	"github.com/ishii1648/codex-issue-loop/internal/platform/retention"
	schemaversion "github.com/ishii1648/codex-issue-loop/internal/platform/schema"
	"github.com/ishii1648/codex-issue-loop/internal/platform/userrules"
)

var (
	Version = "0.1.0-dev"
	Commit  = "unknown"
)

type versionInfo struct {
	Version                  string `json:"version"`
	Commit                   string `json:"commit"`
	Target                   string `json:"target"`
	DeliveryProtocol         int    `json:"delivery_protocol"`
	StateSchemaCurrent       int    `json:"state_schema_current"`
	StateSchemaMigrationFrom int    `json:"state_schema_migration_from"`
	SemanticContractCurrent  int    `json:"semantic_contract_current"`
	SemanticContractMinimum  int    `json:"semantic_contract_minimum"`
}

type installManifest struct {
	Version                 string    `json:"version"`
	Commit                  string    `json:"commit"`
	SchemaVersion           int       `json:"schema_version"`
	SchemaMigrationFrom     int       `json:"schema_migration_from"`
	SemanticContractVersion int       `json:"semantic_contract_version"`
	BinarySHA256            string    `json:"binary_sha256"`
	SkillSHA256             string    `json:"skill_sha256"`
	InstalledAt             time.Time `json:"installed_at"`
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
			_ = json.NewEncoder(a.Out).Encode(versionInfo{Version: Version, Commit: Commit, Target: runtime.GOOS + "/" + runtime.GOARCH, DeliveryProtocol: 1,
				StateSchemaCurrent: schema.CurrentVersion, StateSchemaMigrationFrom: schemaversion.Previous,
				SemanticContractCurrent: statecontract.CurrentVersion, SemanticContractMinimum: statecontract.MinimumVersion})
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
	// Fixture export must not call Layout.Ensure: chmod or directory creation is
	// observable mutation outside the requested output file. Verification also
	// needs no agent-loop home at all.
	if args[0] == "export-recovery-fixture" || args[0] == "verify-recovery-fixture" {
		if args[0] == "export-recovery-fixture" {
			err = a.exportRecoveryFixture(ctx, l, args[1:])
		} else {
			err = a.verifyRecoveryFixture(args[1:])
		}
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
	case "explain-recovery":
		return a.explainRecovery(ctx, l, args)
	case "recover-publication":
		return a.recoverPublication(ctx, l, args)
	case "recover-checks":
		return a.recoverPullRequestChecks(ctx, l, args)
	case "recover-answered-workspace":
		return a.recoverAnsweredWorkspace(ctx, l, args)
	case "recover-workspace":
		return a.recoverWorkspace(ctx, l, args)
	case "recover-quarantined-snapshot":
		return a.recoverQuarantinedSnapshot(ctx, l, args)
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
	case "delivery":
		return a.delivery(ctx, l, args)
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
  explain-recovery  Explain recovery predicates without changing state
  recover-publication  Recover an eligible failed Issue at the publication boundary
  recover-checks  Return an externally repaired Pull Request to its saved lifecycle
  recover-answered-workspace  Recover the exact answered legacy missing-Workspace chain
  recover-workspace  Verify and backfill missing Workspace provenance without resuming execution
  recover-quarantined-snapshot  Restore an exact quarantined snapshot after verifying legacy merged PR identities
  export-recovery-fixture  Export sanitized read-only recovery evidence
  verify-recovery-fixture  Fail closed unless a fixture is complete and untampered
  adopt-merged-pr  Adopt one externally merged saved branch into terminal state
  logs          Print supervisor logs
  cleanup       Preview or remove expired safe worktrees
  purge         Force-remove one explicitly confirmed worktree
  doctor        Validate dependencies, auth, config, and registration
  delivery      Configure and operate the host-level Release delivery controller
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
		RateLimits:           ratelimit.Store{Path: l.RateLimitPath()},
		Worktrees:            worktree.Manager{StateRoot: l.Root, GitPath: entry.Commands["git"]},
		Worker:               backend,
		WorkerIdentity:       identity,
		Publisher:            publish.Manager{GitPath: entry.Commands["git"], GHPath: entry.Commands["gh"], GofmtPath: entry.Commands["gofmt"], Secrets: secrets},
		Conflicts:            conflict.Manager{GitPath: entry.Commands["git"]},
		Logger:               log.New(safeLog, "agent-loop: ", log.LstdFlags|log.LUTC),
		MaintenanceFencePath: filepath.Join(l.DeliveryDir(), "maintenance.json"),
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
