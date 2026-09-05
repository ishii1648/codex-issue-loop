package config

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const SchemaVersion = 1

var repositoryName = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

type Duration struct{ time.Duration }

func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	value, err := time.ParseDuration(node.Value)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", node.Value, err)
	}
	d.Duration = value
	return nil
}

type Config struct {
	Version            int          `yaml:"version" json:"version"`
	PollInterval       Duration     `yaml:"poll_interval" json:"poll_interval"`
	ObservationTimeout Duration     `yaml:"observation_timeout" json:"observation_timeout"`
	StateDir           string       `yaml:"state_dir" json:"state_dir"`
	GitHubCLI          string       `yaml:"github_cli" json:"github_cli"`
	Repositories       []Repository `yaml:"repositories" json:"repositories"`
	Path               string       `yaml:"-" json:"-"`
}

type Repository struct {
	Name              string   `yaml:"name" json:"name"`
	ReadyLabels       []string `yaml:"ready_labels" json:"ready_labels"`
	ExcludeLabels     []string `yaml:"exclude_labels" json:"exclude_labels,omitempty"`
	RunningLabel      string   `yaml:"running_label" json:"running_label"`
	TerminalLabels    []string `yaml:"terminal_labels" json:"terminal_labels"`
	AcceptanceTimeout Duration `yaml:"acceptance_timeout" json:"acceptance_timeout"`
	ProcessingTimeout Duration `yaml:"processing_timeout" json:"processing_timeout"`
}

func DefaultPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".agent-loop-monitor.yaml"), nil
}

func Load(path string) (Config, error) {
	if path == "" {
		var err error
		path, err = DefaultPath()
		if err != nil {
			return Config{}, err
		}
	}
	file, err := os.Open(path)
	if err != nil {
		return Config{}, err
	}
	defer file.Close()
	decoder := yaml.NewDecoder(file)
	decoder.KnownFields(true)
	var cfg Config
	if err := decoder.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("decode monitor config: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return Config{}, fmt.Errorf("monitor config contains multiple YAML documents")
		}
		return Config{}, err
	}
	cfg.Path, err = filepath.Abs(path)
	if err != nil {
		return Config{}, err
	}
	if cfg.PollInterval.Duration == 0 {
		cfg.PollInterval.Duration = time.Minute
	}
	if cfg.ObservationTimeout.Duration == 0 {
		cfg.ObservationTimeout.Duration = 3 * cfg.PollInterval.Duration
	}
	if cfg.GitHubCLI == "" {
		cfg.GitHubCLI = "gh"
	}
	if cfg.StateDir == "" {
		home, homeErr := os.UserHomeDir()
		if homeErr != nil {
			return Config{}, homeErr
		}
		cfg.StateDir = filepath.Join(home, "Library", "Application Support", "codex-issue-loop-monitor")
	}
	if err := validate(&cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func validate(cfg *Config) error {
	if cfg.Version != SchemaVersion {
		return fmt.Errorf("monitor config version must be %d", SchemaVersion)
	}
	if cfg.PollInterval.Duration < 5*time.Second {
		return fmt.Errorf("poll_interval must be at least 5s")
	}
	if cfg.ObservationTimeout.Duration < cfg.PollInterval.Duration {
		return fmt.Errorf("observation_timeout must not be shorter than poll_interval")
	}
	if !filepath.IsAbs(cfg.StateDir) {
		return fmt.Errorf("state_dir must be absolute")
	}
	if len(cfg.Repositories) == 0 {
		return fmt.Errorf("at least one repository is required")
	}
	seen := map[string]bool{}
	for index := range cfg.Repositories {
		repo := &cfg.Repositories[index]
		if !repositoryName.MatchString(repo.Name) || strings.Contains(repo.Name, "..") {
			return fmt.Errorf("repositories[%d].name must be owner/name", index)
		}
		key := strings.ToLower(repo.Name)
		if seen[key] {
			return fmt.Errorf("duplicate repository %q", repo.Name)
		}
		seen[key] = true
		if len(repo.ReadyLabels) == 0 {
			repo.ReadyLabels = []string{"codex-loop:ready"}
		}
		if repo.RunningLabel == "" {
			repo.RunningLabel = "codex-loop:running"
		}
		if len(repo.TerminalLabels) == 0 {
			repo.TerminalLabels = []string{"codex-loop:done", "codex-loop:needs-input", "codex-loop:failed", "blocked"}
		}
		if repo.AcceptanceTimeout.Duration == 0 {
			repo.AcceptanceTimeout.Duration = 10 * time.Minute
		}
		if repo.ProcessingTimeout.Duration == 0 {
			repo.ProcessingTimeout.Duration = 2 * time.Hour
		}
		if repo.AcceptanceTimeout.Duration <= 0 || repo.ProcessingTimeout.Duration <= 0 {
			return fmt.Errorf("repository %q timeouts must be positive", repo.Name)
		}
		if hasBlank(repo.ReadyLabels) || hasBlank(repo.TerminalLabels) || strings.TrimSpace(repo.RunningLabel) == "" {
			return fmt.Errorf("repository %q labels must not be blank", repo.Name)
		}
	}
	return nil
}

func hasBlank(values []string) bool {
	for _, value := range values {
		if strings.TrimSpace(value) == "" {
			return true
		}
	}
	return false
}

func RepoID(name string) string {
	return strings.NewReplacer("/", "--", "\\", "--").Replace(strings.ToLower(name))
}
