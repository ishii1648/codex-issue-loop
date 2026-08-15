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
	"strings"
	"time"

	assets "github.com/ishii1648/codex-issue-loop"
	"github.com/ishii1648/codex-issue-loop/internal/compat"
	"github.com/ishii1648/codex-issue-loop/internal/config"
	"github.com/ishii1648/codex-issue-loop/internal/fsutil"
	gh "github.com/ishii1648/codex-issue-loop/internal/github"
	"github.com/ishii1648/codex-issue-loop/internal/launchd"
	"github.com/ishii1648/codex-issue-loop/internal/layout"
	"github.com/ishii1648/codex-issue-loop/internal/observe"
	"github.com/ishii1648/codex-issue-loop/internal/redact"
	"github.com/ishii1648/codex-issue-loop/internal/registry"
	"github.com/ishii1648/codex-issue-loop/internal/state"
	"github.com/ishii1648/codex-issue-loop/internal/supervisor"
	"github.com/ishii1648/codex-issue-loop/internal/worker"
	"github.com/ishii1648/codex-issue-loop/internal/worktree"
)

const Version = "0.1.0-dev"

type App struct {
	In  io.Reader
	Out io.Writer
	Err io.Writer
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
		fmt.Fprintf(a.Out, "agent-loop %s\n", Version)
		return 0
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
		return a.install(l, args)
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
	case "logs":
		return a.logs(l, args)
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
  install       Install the binary and Codex Skill
  uninstall     Remove installed binary and Skill when no loop is running
  register      Register a repository and write its LaunchAgent
  unregister    Stop and unregister a repository
  start         Start the repository LaunchAgent
  stop          Stop the repository LaunchAgent without deleting state
  restart       Restart the repository LaunchAgent
  status        Show durable and launchd state
  watch         Wait for needs_input, blocked, stopped, or optional idle
  answer        Record an answer for a pending request
  logs          Print supervisor logs
  doctor        Validate dependencies, auth, config, and registration
  bootstrap-labels  Preview or create required GitHub labels
  run           Run the supervisor (used by launchd)`)
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

func (a App) install(l layout.Layout, args []string) error {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	fs.SetOutput(a.Err)
	jsonOut := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return exitError{2, err}
	}
	source, err := os.Executable()
	if err != nil {
		return err
	}
	source, err = filepath.EvalSymlinks(source)
	if err != nil {
		return err
	}
	destination := filepath.Join(l.BinDir, "agent-loop")
	if source != destination {
		data, err := os.ReadFile(source)
		if err != nil {
			return err
		}
		if err := fsutil.WriteFile(destination, data, 0o755); err != nil {
			return fmt.Errorf("install binary: %w", err)
		}
	}
	skillDir := filepath.Join(l.SkillsDir, "agent-loop")
	if err := os.MkdirAll(skillDir, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), assets.AgentLoopSkill, 0o600); err != nil {
		return err
	}
	return a.output(*jsonOut, map[string]any{"binary": destination, "skill": filepath.Join(skillDir, "SKILL.md")})
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
	for _, path := range []string{filepath.Join(l.BinDir, "agent-loop"), filepath.Join(l.SkillsDir, "agent-loop", "SKILL.md")} {
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
	lm := launchd.Manager{Layout: l}
	if err := lm.Stop(ctx, entry); err != nil {
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
	entry, jsonOut, err := a.resolve(l, command, args)
	if err != nil {
		return err
	}
	cfg, err := config.Load(entry.RepoPath)
	if err != nil {
		return err
	}
	lm := launchd.Manager{Layout: l, Launchctl: entry.Commands["launchctl"]}
	switch command {
	case "start":
		err = lm.Start(ctx, entry)
	case "stop":
		err = lm.Stop(ctx, entry)
	case "restart":
		err = lm.Restart(ctx, entry)
	}
	if err != nil {
		return err
	}
	if command == "stop" {
		store := state.Store{Dir: l.RepoDir(entry.RepoID), RepoID: entry.RepoID, RepoPath: entry.RepoPath, Secrets: cfg.RedactionValues()}
		_, _ = store.Update("supervisor_stopped", 0, "", map[string]string{"reason": "explicit stop"}, func(s *state.Snapshot) error {
			s.Supervisor.State, s.Supervisor.PID, s.Supervisor.Message = "stopped", 0, "explicit stop"
			return nil
		})
	}
	status, _ := lm.Status(ctx, entry)
	return a.output(jsonOut, map[string]any{"repo_id": entry.RepoID, "command": command, "launchd": status})
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
	return a.output(jsonOut, map[string]any{"launchd": launchStatus, "state": snapshot})
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
		issue.Status, issue.RetryAfter, issue.UpdatedAt = "resume_pending", nil, now
		issue.Answers = append(issue.Answers, state.AnswerRecord{RequestID: request.ID, Question: request.Question, Answer: answer, AnsweredAt: now})
		return nil
	})
	if err != nil {
		return err
	}
	return a.output(*jsonOut, map[string]any{"request_id": *requestID, "recorded": true})
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
		name = "supervisor.err.log"
	}
	f, err := os.Open(filepath.Join(l.RepoDir(entry.RepoID), name))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(a.Out, f)
	return err
}

type check struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

func (a App) doctor(ctx context.Context, l layout.Layout, args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(a.Err)
	repo := fs.String("repo", "", "repository path")
	jsonOut := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return exitError{2, err}
	}
	checks := []check{}
	failed := false
	for _, command := range []string{"git", "gh", "codex", "launchctl"} {
		path, err := exec.LookPath(command)
		ok := err == nil
		checks = append(checks, check{Name: command, OK: ok, Detail: path})
		failed = failed || !ok
	}
	ghAuthOut, ghAuthErr := exec.CommandContext(ctx, "gh", "auth", "status").CombinedOutput()
	checks = append(checks, check{Name: "gh_auth", OK: ghAuthErr == nil, Detail: truncate(strings.TrimSpace(string(ghAuthOut)), 300)})
	failed = failed || ghAuthErr != nil
	codexCompatibility := compat.ProbeCodex(ctx, "codex")
	checks = append(checks, check{Name: "codex_compat", OK: codexCompatibility.OK(), Detail: compatibilityDetail(codexCompatibility)})
	failed = failed || !codexCompatibility.OK()
	ghCompatibility := compat.ProbeGH(ctx, "gh")
	checks = append(checks, check{Name: "gh_compat", OK: ghCompatibility.OK(), Detail: compatibilityDetail(ghCompatibility)})
	failed = failed || !ghCompatibility.OK()
	pmsetOut, pmsetErr := exec.CommandContext(ctx, "pmset", "-g", "custom").CombinedOutput()
	sleepOK, sleepDetail := evaluateSleepSettings(string(pmsetOut), pmsetErr)
	checks = append(checks, check{Name: "macos_sleep", OK: sleepOK, Detail: sleepDetail})
	failed = failed || !sleepOK
	if *repo != "" {
		cfg, err := config.Load(*repo)
		checks = append(checks, check{Name: "config", OK: err == nil, Detail: errorText(err)})
		failed = failed || err != nil
		if err == nil {
			_, resolveErr := (registry.Store{Path: l.RegistryPath}).Resolve(cfg.RepoPath, "")
			checks = append(checks, check{Name: "registered", OK: resolveErr == nil, Detail: errorText(resolveErr)})
			failed = failed || resolveErr != nil
			repoOut, repoErr := exec.CommandContext(ctx, "gh", "repo", "view", cfg.GitHub.Repo, "--json", "nameWithOwner").CombinedOutput()
			checks = append(checks, check{Name: "github_repo_access", OK: repoErr == nil, Detail: truncate(strings.TrimSpace(string(repoOut)), 300)})
			failed = failed || repoErr != nil
			labelOut, labelErr := exec.CommandContext(ctx, "gh", "label", "list", "--repo", cfg.GitHub.Repo, "--limit", "1000", "--json", "name").CombinedOutput()
			missing := []string{}
			if labelErr == nil {
				var labels []struct {
					Name string `json:"name"`
				}
				if decodeErr := json.Unmarshal(labelOut, &labels); decodeErr != nil {
					labelErr = decodeErr
				} else {
					available := map[string]bool{}
					for _, label := range labels {
						available[strings.ToLower(label.Name)] = true
					}
					for _, label := range requiredLabels(cfg) {
						if label != "" && !available[strings.ToLower(label)] {
							missing = append(missing, label)
						}
					}
					if len(missing) > 0 {
						labelErr = fmt.Errorf("missing labels: %s; repair: agent-loop bootstrap-labels --repo %q --apply", strings.Join(missing, ", "), cfg.RepoPath)
					}
				}
			}
			checks = append(checks, check{Name: "github_labels", OK: labelErr == nil, Detail: errorText(labelErr)})
			failed = failed || labelErr != nil
		}
	}
	result := map[string]any{"ok": !failed, "checks": checks, "macos_sleep_recommendation": "Enable prevent automatic sleeping when the display is off."}
	if err := a.output(*jsonOut, result); err != nil {
		return err
	}
	if failed {
		return exitError{1, fmt.Errorf("doctor found failing checks")}
	}
	return nil
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
	codexCompatibility := compat.ProbeCodex(ctx, entry.Commands["codex"])
	if !codexCompatibility.OK() {
		return fmt.Errorf("unsupported Codex CLI: %s", compatibilityDetail(codexCompatibility))
	}
	ghCompatibility := compat.ProbeGH(ctx, entry.Commands["gh"])
	if !ghCompatibility.OK() {
		return fmt.Errorf("unsupported gh CLI: %s", compatibilityDetail(ghCompatibility))
	}
	secrets := cfg.RedactionValues()
	store := state.Store{Dir: l.RepoDir(entry.RepoID), RepoID: entry.RepoID, RepoPath: entry.RepoPath, Secrets: secrets}
	cfg.Worker.Command = entry.Commands["codex"]
	safeLog := redact.NewLineWriterWithSecrets(a.Err, secrets)
	loop := &supervisor.Loop{
		Config: cfg, Store: store, GitHub: gh.CLI{Path: entry.Commands["gh"], Secrets: secrets},
		Worktrees: worktree.Manager{StateRoot: l.Root, GitPath: entry.Commands["git"]},
		Worker:    worker.Codex{StateDir: l.RepoDir(entry.RepoID), Secrets: secrets, ResumeSupported: boolPointer(codexCompatibility.Has("session_resume"))},
		Logger:    log.New(safeLog, "agent-loop: ", log.LstdFlags|log.LUTC),
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
		for _, request := range v.Snapshot.PendingRequests {
			if request.Status == "pending" {
				fmt.Fprintf(a.Out, "%s Issue #%d: %s\n", request.ID, request.IssueNumber, request.Question)
			}
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

func requiredLabels(cfg config.Config) []string {
	labels := []string{}
	for _, spec := range gh.RequiredLabelSpecs(cfg) {
		labels = append(labels, spec.Name)
	}
	return labels
}

func evaluateSleepSettings(output string, commandErr error) (bool, string) {
	if commandErr != nil {
		return false, commandErr.Error()
	}
	inAC := false
	for _, line := range strings.Split(output, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasSuffix(trimmed, "Power:") {
			inAC = strings.HasPrefix(trimmed, "AC ")
			continue
		}
		if !inAC {
			continue
		}
		fields := strings.Fields(trimmed)
		if len(fields) == 2 && fields[0] == "sleep" {
			if fields[1] == "0" {
				return true, "AC sleep is disabled"
			}
			return false, "AC sleep is enabled; set Prevent automatic sleeping when the display is off"
		}
	}
	return false, "could not determine AC sleep setting from pmset"
}
