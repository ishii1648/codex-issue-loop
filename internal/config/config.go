package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/ishii1648/codex-issue-loop/internal/admission"
	schemaversion "github.com/ishii1648/codex-issue-loop/internal/schema"
	"gopkg.in/yaml.v3"
)

const (
	FileName       = ".agent-loop.yaml"
	CurrentVersion = schemaversion.Current
)

type Duration struct{ time.Duration }

func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	v, err := time.ParseDuration(node.Value)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", node.Value, err)
	}
	d.Duration = v
	return nil
}

type Config struct {
	Version          int              `yaml:"version" json:"version"`
	GitHub           GitHub           `yaml:"github" json:"github"`
	Queue            Queue            `yaml:"queue" json:"queue"`
	Resources        Resources        `yaml:"resources" json:"resources"`
	Worker           Worker           `yaml:"worker" json:"worker"`
	Watch            Watch            `yaml:"watch" json:"watch"`
	Git              Git              `yaml:"git" json:"git"`
	Formatters       Formatters       `yaml:"formatters" json:"formatters"`
	Completion       Completion       `yaml:"completion" json:"completion"`
	ConflictRecovery ConflictRecovery `yaml:"conflict_recovery" json:"conflict_recovery"`
	Worktrees        Worktrees        `yaml:"worktrees" json:"worktrees"`
	Logs             Logs             `yaml:"logs" json:"logs"`
	Security         Security         `yaml:"security" json:"security"`
	RepoPath         string           `yaml:"-" json:"repo_path"`
}

type GitHub struct {
	Repo            string   `yaml:"repo" json:"repo"`
	ReadyLabels     []string `yaml:"ready_labels" json:"ready_labels"`
	ExcludeLabels   []string `yaml:"exclude_labels" json:"exclude_labels"`
	RunningLabel    string   `yaml:"running_label" json:"running_label"`
	NeedsInputLabel string   `yaml:"needs_input_label" json:"needs_input_label"`
	FailedLabel     string   `yaml:"failed_label" json:"failed_label"`
	DoneLabel       string   `yaml:"done_label" json:"done_label"`
	Assignee        string   `yaml:"assignee" json:"assignee,omitempty"`
	Milestone       string   `yaml:"milestone" json:"milestone,omitempty"`
}

type Queue struct {
	PollInterval            Duration `yaml:"poll_interval" json:"poll_interval"`
	Concurrency             int      `yaml:"concurrency" json:"concurrency"`
	Order                   string   `yaml:"order" json:"order"`
	PriorityLabels          []string `yaml:"priority_labels" json:"priority_labels,omitempty"`
	MaxAttempts             int      `yaml:"max_attempts" json:"max_attempts"`
	ContinueAfterNeedsInput bool     `yaml:"continue_after_needs_input" json:"continue_after_needs_input"`
}

type Resources struct {
	MetadataVersion int                  `yaml:"metadata_version" json:"metadata_version"`
	Definitions     []ResourceDefinition `yaml:"definitions" json:"definitions,omitempty"`
}

type ResourceDefinition struct {
	Name  string   `yaml:"name" json:"name"`
	Paths []string `yaml:"paths" json:"paths"`
}

type Worker struct {
	Backend          string             `yaml:"backend" json:"backend"`
	Command          string             `yaml:"command" json:"command"`
	Model            string             `yaml:"model" json:"model,omitempty"`
	Variant          string             `yaml:"variant" json:"variant,omitempty"`
	AppServer        AppServer          `yaml:"app_server" json:"app_server"`
	CommandNetwork   CommandNetwork     `yaml:"command_network" json:"command_network"`
	Sandbox          string             `yaml:"sandbox" json:"sandbox"`
	SessionMode      string             `yaml:"session_mode" json:"session_mode"`
	Timeout          Duration           `yaml:"timeout" json:"timeout"`
	TimeoutGrace     Duration           `yaml:"timeout_grace" json:"timeout_grace"`
	AmbiguousProfile string             `yaml:"ambiguous_profile" json:"ambiguous_profile"`
	Profiles         map[string]Profile `yaml:"profiles" json:"profiles"`
}

// CommandNetwork is deliberately narrower than Codex's native proxy
// configuration. Repository configuration can select no command networking or
// the one reviewed localhost-only policy; it cannot pass arbitrary proxy
// options through to Codex.
type CommandNetwork struct {
	Policy       string   `yaml:"policy" json:"policy"`
	Proxy        bool     `yaml:"proxy" json:"proxy"`
	AllowedHosts []string `yaml:"allowed_hosts" json:"allowed_hosts,omitempty"`
}

// AppServer controls the opt-in Codex App Server Goal adapter. It is only
// eligible for continuations already classified as extended; initial and
// standard executions keep using the codex exec adapter.
type AppServer struct {
	Enabled         bool     `yaml:"enabled" json:"enabled"`
	GoalTokenBudget int64    `yaml:"goal_token_budget" json:"goal_token_budget"`
	GoalTimeBudget  Duration `yaml:"goal_time_budget" json:"goal_time_budget"`
}

type Profile struct {
	MaxContinuations int `yaml:"max_continuations" json:"max_continuations"`
}

type Watch struct {
	ReconcileInterval Duration `yaml:"reconcile_interval" json:"reconcile_interval"`
	ReconcileJitter   float64  `yaml:"reconcile_jitter" json:"reconcile_jitter"`
}

func (w *Watch) UnmarshalYAML(node *yaml.Node) error {
	type rawWatch struct {
		ReconcileInterval Duration `yaml:"reconcile_interval"`
		ReconcileJitter   any      `yaml:"reconcile_jitter"`
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		switch node.Content[i].Value {
		case "reconcile_interval", "reconcile_jitter":
		default:
			return fmt.Errorf("field %s not found in type config.Watch", node.Content[i].Value)
		}
	}
	raw := rawWatch{ReconcileInterval: w.ReconcileInterval, ReconcileJitter: w.ReconcileJitter}
	if err := node.Decode(&raw); err != nil {
		return err
	}
	w.ReconcileInterval = raw.ReconcileInterval
	switch value := raw.ReconcileJitter.(type) {
	case nil:
	case float64:
		w.ReconcileJitter = value
	case int:
		w.ReconcileJitter = float64(value)
	case string:
		if !strings.HasSuffix(value, "%") {
			return fmt.Errorf("reconcile_jitter must be a percentage")
		}
		var percent float64
		if _, err := fmt.Sscanf(strings.TrimSuffix(value, "%"), "%f", &percent); err != nil {
			return fmt.Errorf("invalid reconcile_jitter %q", value)
		}
		w.ReconcileJitter = percent / 100
	default:
		return fmt.Errorf("invalid reconcile_jitter type")
	}
	return nil
}

type Git struct {
	BranchPrefix string `yaml:"branch_prefix" json:"branch_prefix"`
	WorktreeRoot string `yaml:"worktree_root" json:"worktree_root,omitempty"`
	BaseBranch   string `yaml:"base_branch" json:"base_branch"`
}

// Formatters contains the fixed, built-in publication adapters. It is not a
// command hook: repositories can only opt in to adapters implemented by the
// supervisor, with no repository-supplied executable or arguments.
type Formatters struct {
	Go GoFormatter `yaml:"go" json:"go"`
}

type GoFormatter struct {
	Enabled bool     `yaml:"enabled" json:"enabled"`
	Timeout Duration `yaml:"timeout" json:"timeout"`
}

type Completion struct {
	CreateDraftPR bool `yaml:"create_draft_pr" json:"create_draft_pr"`
	AutoMerge     bool `yaml:"auto_merge" json:"auto_merge"`
	CloseIssue    bool `yaml:"close_issue" json:"close_issue"`
}

type ConflictRecovery struct {
	MaxAttemptsPerBase int `yaml:"max_attempts_per_base" json:"max_attempts_per_base"`
	MaxBaseUpdates     int `yaml:"max_base_updates" json:"max_base_updates"`
}

type Worktrees struct {
	CompletedMaxAge  Duration `yaml:"completed_max_age" json:"completed_max_age"`
	FailedMaxAge     Duration `yaml:"failed_max_age" json:"failed_max_age"`
	BlockedMaxAge    Duration `yaml:"blocked_max_age" json:"blocked_max_age"`
	NeedsInputMaxAge Duration `yaml:"needs_input_max_age" json:"needs_input_max_age"`
}

type Logs struct {
	RotateBytes       int64    `yaml:"rotate_bytes" json:"rotate_bytes"`
	RotateInterval    Duration `yaml:"rotate_interval" json:"rotate_interval"`
	Generations       int      `yaml:"generations" json:"generations"`
	WorkerRunMaxAge   Duration `yaml:"worker_run_max_age" json:"worker_run_max_age"`
	WorkerRunMaxCount int      `yaml:"worker_run_max_count" json:"worker_run_max_count"`
}

type Security struct {
	// RedactEnv contains environment-variable names, never secret values.
	RedactEnv []string `yaml:"redact_env" json:"redact_env,omitempty"`
}

var environmentName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
var githubRepository = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9_.-]{0,99})/[A-Za-z0-9_.-]{1,100}$`)

func Defaults() Config {
	return Config{
		Version: CurrentVersion,
		GitHub: GitHub{
			ReadyLabels:     []string{"codex-loop:ready"},
			ExcludeLabels:   []string{"blocked", "do-not-automate"},
			RunningLabel:    "codex-loop:running",
			NeedsInputLabel: "codex-loop:needs-input",
			FailedLabel:     "codex-loop:failed",
			DoneLabel:       "codex-loop:done",
		},
		Queue: Queue{
			PollInterval:            Duration{60 * time.Second},
			Concurrency:             1,
			Order:                   "issue_number_asc",
			MaxAttempts:             3,
			ContinueAfterNeedsInput: true,
		},
		Resources: Resources{MetadataVersion: 1},
		Worker: Worker{
			Backend:        "codex",
			CommandNetwork: CommandNetwork{Policy: "disabled"},
			AppServer: AppServer{
				GoalTokenBudget: 200000,
				GoalTimeBudget:  Duration{2 * time.Hour},
			},
			Sandbox:          "workspace-write",
			SessionMode:      "resumable",
			Timeout:          Duration{2 * time.Hour},
			TimeoutGrace:     Duration{30 * time.Second},
			AmbiguousProfile: "extended",
			Profiles: map[string]Profile{
				"standard": {MaxContinuations: 0},
				"extended": {MaxContinuations: 3},
			},
		},
		Watch: Watch{
			ReconcileInterval: Duration{60 * time.Second},
			ReconcileJitter:   0.10,
		},
		Git: Git{
			BranchPrefix: "codex/issue-",
			BaseBranch:   "main",
		},
		Formatters:       Formatters{Go: GoFormatter{Timeout: Duration{30 * time.Second}}},
		Completion:       Completion{CreateDraftPR: true, AutoMerge: false, CloseIssue: true},
		ConflictRecovery: ConflictRecovery{MaxAttemptsPerBase: 3, MaxBaseUpdates: 3},
		Worktrees: Worktrees{
			CompletedMaxAge:  Duration{7 * 24 * time.Hour},
			FailedMaxAge:     Duration{30 * 24 * time.Hour},
			BlockedMaxAge:    Duration{0},
			NeedsInputMaxAge: Duration{0},
		},
		Logs: Logs{
			RotateBytes:       16 * 1024 * 1024,
			RotateInterval:    Duration{24 * time.Hour},
			Generations:       7,
			WorkerRunMaxAge:   Duration{30 * 24 * time.Hour},
			WorkerRunMaxCount: 100,
		},
	}
}

func Load(repoPath string) (Config, error) {
	canonical, err := CanonicalRepoPath(repoPath)
	if err != nil {
		return Config{}, err
	}
	f, err := os.Open(filepath.Join(canonical, FileName))
	if err != nil {
		return Config{}, fmt.Errorf("open %s: %w", FileName, err)
	}
	defer f.Close()
	cfg := Defaults()
	decoder := yaml.NewDecoder(f)
	decoder.KnownFields(true)
	if err := decoder.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("decode %s: %w", FileName, err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != nil && !errors.Is(err, io.EOF) {
		return Config{}, fmt.Errorf("decode trailing YAML: %w", err)
	} else if err == nil {
		return Config{}, fmt.Errorf("%s must contain one YAML document", FileName)
	}
	cfg.RepoPath = canonical
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func CanonicalRepoPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve repository path: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("resolve repository symlinks: %w", err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("stat repository: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("repository path is not a directory: %s", resolved)
	}
	return filepath.Clean(resolved), nil
}

func (c Config) Validate() error {
	if c.Version != CurrentVersion {
		if c.Version == schemaversion.Previous {
			return fmt.Errorf("config schema migration required from version %d to %d; stop loops and run agent-loop migrate --apply", schemaversion.Previous, CurrentVersion)
		}
		return fmt.Errorf("unsupported config version %d; this binary supports version %d", c.Version, CurrentVersion)
	}
	if !githubRepository.MatchString(c.GitHub.Repo) {
		return fmt.Errorf("github.repo must use owner/name format")
	}
	if len(c.GitHub.ReadyLabels) == 0 {
		return fmt.Errorf("github.ready_labels must not be empty")
	}
	if c.Queue.Concurrency < 1 {
		return fmt.Errorf("queue.concurrency must be at least 1")
	}
	settings := c.AdmissionSettings()
	if err := settings.Validate(); err != nil {
		return fmt.Errorf("resources: %w", err)
	}
	if len(c.Resources.Definitions) == 0 {
		if c.Queue.Concurrency != 1 {
			return fmt.Errorf("queue.concurrency greater than 1 requires resources.definitions")
		}
	}
	switch c.Queue.Order {
	case "issue_number_asc", "created_at_asc":
	case "priority_then_created_at":
		if len(c.Queue.PriorityLabels) == 0 {
			return fmt.Errorf("queue.priority_labels must not be empty when queue.order is priority_then_created_at")
		}
	default:
		return fmt.Errorf("queue.order must be issue_number_asc, created_at_asc, or priority_then_created_at")
	}
	seenPriorities := map[string]bool{}
	for _, label := range c.Queue.PriorityLabels {
		trimmed := strings.TrimSpace(label)
		canonical := strings.ToLower(trimmed)
		if canonical == "" {
			return fmt.Errorf("queue.priority_labels must not contain empty labels")
		}
		if trimmed != label {
			return fmt.Errorf("queue.priority_labels must not contain leading or trailing whitespace in %q", label)
		}
		if seenPriorities[canonical] {
			return fmt.Errorf("queue.priority_labels must not contain duplicate label %q", label)
		}
		seenPriorities[canonical] = true
	}
	if c.Queue.PollInterval.Duration <= 0 || c.Watch.ReconcileInterval.Duration <= 0 || c.Worker.Timeout.Duration <= 0 {
		return fmt.Errorf("durations must be positive")
	}
	if c.Worker.TimeoutGrace.Duration <= 0 {
		return fmt.Errorf("worker.timeout_grace must be positive")
	}
	if c.Worker.TimeoutGrace.Duration > c.Worker.Timeout.Duration {
		return fmt.Errorf("worker.timeout_grace must not exceed worker.timeout")
	}
	switch c.Worker.Backend {
	case "codex", "claude-code", "opencode":
	default:
		return fmt.Errorf("worker.backend must be codex, claude-code, or opencode")
	}
	if strings.TrimSpace(c.Worker.Command) != c.Worker.Command || strings.ContainsRune(c.Worker.Command, '\x00') {
		return fmt.Errorf("worker.command must be a command name or path without surrounding whitespace")
	}
	if c.Worker.Backend == "opencode" {
		provider, model, ok := strings.Cut(c.Worker.Model, "/")
		if !ok || provider == "" || model == "" || strings.Contains(provider, "#") || strings.Contains(model, "#") {
			return fmt.Errorf("worker.model for opencode must use provider/model format")
		}
	}
	if c.Worker.Backend != "opencode" && c.Worker.Variant != "" && c.Worker.Backend != "claude-code" {
		return fmt.Errorf("worker.variant is supported by claude-code and opencode only")
	}
	if c.Worker.AppServer.Enabled && c.Worker.Backend != "codex" {
		return fmt.Errorf("worker.app_server is supported by the codex backend only")
	}
	if c.Worker.AppServer.Enabled && (c.Worker.AppServer.GoalTokenBudget <= 0 || c.Worker.AppServer.GoalTimeBudget.Duration <= 0) {
		return fmt.Errorf("worker.app_server requires positive goal_token_budget and goal_time_budget")
	}
	if err := c.Worker.CommandNetwork.Validate(c.Worker); err != nil {
		return err
	}
	if c.Logs.RotateBytes < 1024 || c.Logs.RotateInterval.Duration <= 0 || c.Logs.Generations < 1 {
		return fmt.Errorf("logs rotation requires rotate_bytes >= 1024, positive rotate_interval, and generations >= 1")
	}
	if c.Logs.WorkerRunMaxAge.Duration <= 0 || c.Logs.WorkerRunMaxCount < 1 {
		return fmt.Errorf("logs worker run retention requires positive worker_run_max_age and worker_run_max_count >= 1")
	}
	if c.Worktrees.CompletedMaxAge.Duration < 0 || c.Worktrees.FailedMaxAge.Duration < 0 ||
		c.Worktrees.BlockedMaxAge.Duration < 0 || c.Worktrees.NeedsInputMaxAge.Duration < 0 {
		return fmt.Errorf("worktree retention durations must not be negative; 0 keeps a status indefinitely")
	}
	if c.Queue.MaxAttempts < 1 {
		return fmt.Errorf("queue.max_attempts must be at least 1")
	}
	if c.ConflictRecovery.MaxAttemptsPerBase < 1 || c.ConflictRecovery.MaxBaseUpdates < 1 {
		return fmt.Errorf("conflict_recovery.max_attempts_per_base and max_base_updates must be at least 1")
	}
	if c.Worker.AmbiguousProfile != "extended" {
		return fmt.Errorf("worker.ambiguous_profile must be extended")
	}
	if c.Worker.SessionMode != "resumable" {
		return fmt.Errorf("worker.session_mode must be resumable")
	}
	if _, ok := c.Worker.Profiles["standard"]; !ok {
		return fmt.Errorf("worker.profiles.standard is required")
	}
	if _, ok := c.Worker.Profiles["extended"]; !ok {
		return fmt.Errorf("worker.profiles.extended is required")
	}
	if c.Watch.ReconcileJitter < 0 || c.Watch.ReconcileJitter > 1 {
		return fmt.Errorf("watch.reconcile_jitter must be between 0%% and 100%%")
	}
	if c.Worker.Sandbox != "read-only" && c.Worker.Sandbox != "workspace-write" {
		return fmt.Errorf("worker.sandbox must be read-only or workspace-write")
	}
	if c.Git.WorktreeRoot != "" && !filepath.IsAbs(c.Git.WorktreeRoot) {
		return fmt.Errorf("git.worktree_root must be an absolute path")
	}
	if c.Formatters.Go.Timeout.Duration <= 0 {
		return fmt.Errorf("formatters.go.timeout must be positive")
	}
	if c.Completion.AutoMerge && !c.Completion.CreateDraftPR {
		return fmt.Errorf("completion.auto_merge requires completion.create_draft_pr")
	}
	if !safeRefFragment(c.Git.BranchPrefix) || !safeRefFragment(c.Git.BaseBranch) {
		return fmt.Errorf("git.branch_prefix and git.base_branch must be safe Git ref fragments")
	}
	for _, name := range c.Security.RedactEnv {
		if !environmentName.MatchString(name) {
			return fmt.Errorf("security.redact_env contains invalid environment variable name %q", name)
		}
	}
	return nil
}

func (n CommandNetwork) Validate(worker Worker) error {
	switch n.Policy {
	case "disabled":
		if n.Proxy || len(n.AllowedHosts) != 0 {
			return fmt.Errorf("worker.command_network disabled policy requires proxy=false and an empty allowed_hosts list")
		}
		return nil
	case "localhost-only":
		if worker.Backend != "codex" {
			return fmt.Errorf("worker.command_network localhost-only is supported by the codex backend only")
		}
		if worker.Sandbox != "workspace-write" {
			return fmt.Errorf("worker.command_network localhost-only requires worker.sandbox=workspace-write")
		}
		if worker.AppServer.Enabled {
			return fmt.Errorf("worker.command_network localhost-only cannot be combined with worker.app_server; codex exec isolation is required")
		}
		if !n.Proxy {
			return fmt.Errorf("worker.command_network localhost-only requires proxy=true")
		}
		if len(n.AllowedHosts) != 2 || n.AllowedHosts[0] != "localhost" || n.AllowedHosts[1] != "127.0.0.1" {
			return fmt.Errorf("worker.command_network localhost-only allowed_hosts must be exactly [localhost, 127.0.0.1] in that order")
		}
		return nil
	default:
		return fmt.Errorf("worker.command_network.policy must be disabled or localhost-only")
	}
}

func (n CommandNetwork) LocalhostOnly() bool { return n.Policy == "localhost-only" }

// AdmissionSettings returns an immutable copy of the resource taxonomy used
// by both queue admission and publication auditing. A config without resource
// definitions remains a conservative repository-wide legacy queue.
func (c Config) AdmissionSettings() admission.Settings {
	definitions := make([]admission.ResourceDefinition, len(c.Resources.Definitions))
	for index, definition := range c.Resources.Definitions {
		definitions[index] = admission.ResourceDefinition{
			Name: definition.Name, Paths: append([]string(nil), definition.Paths...),
		}
	}
	return admission.Settings{
		Concurrency: c.Queue.Concurrency, MetadataVersion: c.Resources.MetadataVersion,
		Definitions: definitions, Legacy: len(definitions) == 0,
	}
}

// EffectiveCommand returns the fixed executable selected by the built-in
// backend. The manifest never expands this value through a shell.
func (w Worker) EffectiveCommand() string {
	if w.Command != "" {
		return w.Command
	}
	switch w.Backend {
	case "claude-code":
		return "claude"
	case "opencode":
		return "opencode"
	default:
		return "codex"
	}
}

func safeRefFragment(value string) bool {
	return value != "" && !strings.HasPrefix(value, "-") && !strings.HasPrefix(value, "/") &&
		!strings.HasSuffix(value, "/") && !strings.Contains(value, "..") &&
		!strings.Contains(value, "//") && !strings.Contains(value, "@{") &&
		!strings.ContainsAny(value, " ~^:?*[\\") && !strings.HasSuffix(value, ".") && !strings.HasSuffix(value, ".lock") &&
		strings.IndexFunc(value, unicode.IsControl) < 0
}

func (c Config) RedactionValues() []string {
	values := make([]string, 0, len(c.Security.RedactEnv))
	for _, name := range c.Security.RedactEnv {
		if value := os.Getenv(name); len(value) >= 4 {
			values = append(values, value)
		}
	}
	return values
}
