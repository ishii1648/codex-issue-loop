package delivery

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/ishii1648/codex-issue-loop/internal/platform/fsutil"
	"gopkg.in/yaml.v3"
)

const (
	ConfigVersion             = 2
	LegacyConfigVersion       = 1
	ProtocolVersion           = 1
	AssignmentProtocolVersion = 1
)

type AssignmentRef struct {
	Version        string `yaml:"version" json:"version"`
	Commit         string `yaml:"commit" json:"commit"`
	ArtifactSHA256 string `yaml:"artifact_sha256" json:"artifact_sha256"`
	Slot           string `yaml:"slot" json:"slot"`
}

type RepositoryAssignment struct {
	RepositoryID  string `yaml:"repository_id" json:"repository_id"`
	AssignmentRef `yaml:",inline"`
	Generation    uint64         `yaml:"generation" json:"generation"`
	Previous      *AssignmentRef `yaml:"previous,omitempty" json:"previous,omitempty"`
	UpdatedAt     time.Time      `yaml:"updated_at" json:"updated_at"`
}

type Config struct {
	Version           int                             `yaml:"version" json:"version"`
	Enabled           bool                            `yaml:"enabled" json:"enabled"`
	ReleaseRepository string                          `yaml:"release_repository" json:"release_repository"`
	Channel           string                          `yaml:"channel" json:"channel"`
	PollInterval      string                          `yaml:"poll_interval" json:"poll_interval"`
	DrainTimeout      string                          `yaml:"drain_timeout" json:"drain_timeout"`
	AutoApply         string                          `yaml:"auto_apply" json:"auto_apply"`
	TrustedWorkflow   string                          `yaml:"trusted_workflow,omitempty" json:"trusted_workflow"`
	Assignments       map[string]RepositoryAssignment `yaml:"assignments" json:"assignments"`
}

type LegacyConfig struct {
	Version           int    `yaml:"version" json:"version"`
	Enabled           bool   `yaml:"enabled" json:"enabled"`
	ReleaseRepository string `yaml:"release_repository" json:"release_repository"`
	Channel           string `yaml:"channel" json:"channel"`
	PollInterval      string `yaml:"poll_interval" json:"poll_interval"`
	DrainTimeout      string `yaml:"drain_timeout" json:"drain_timeout"`
	AutoApply         string `yaml:"auto_apply" json:"auto_apply"`
	TrustedWorkflow   string `yaml:"trusted_workflow,omitempty" json:"trusted_workflow"`
}

func DefaultConfig(repository string) Config {
	return Config{Version: ConfigVersion, Enabled: true, ReleaseRepository: repository, Channel: "stable", PollInterval: "15m", DrainTimeout: "2h30m", AutoApply: "never", Assignments: map[string]RepositoryAssignment{}}
}

func DefaultConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve user home: %w", err)
	}
	return filepath.Join(home, ".agent-loop-delivery.yaml"), nil
}

func ResolveConfigPath(explicit string) (string, error) {
	if explicit == "" {
		return DefaultConfigPath()
	}
	if !filepath.IsAbs(explicit) {
		return "", errors.New("--config must be an absolute path")
	}
	return filepath.Clean(explicit), nil
}

func LoadConfig(path string) (Config, error) {
	if !filepath.IsAbs(path) {
		return Config{}, errors.New("delivery config path must be absolute")
	}
	if err := validatePrivateRegularFile(path); err != nil {
		return Config{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read delivery config: %w", err)
	}
	var cfg Config
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("decode delivery config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func LoadLegacyConfig(path string) (LegacyConfig, error) {
	if !filepath.IsAbs(path) {
		return LegacyConfig{}, errors.New("delivery config path must be absolute")
	}
	if err := validatePrivateRegularFile(path); err != nil {
		return LegacyConfig{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return LegacyConfig{}, fmt.Errorf("read delivery config: %w", err)
	}
	var cfg LegacyConfig
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&cfg); err != nil {
		return LegacyConfig{}, fmt.Errorf("decode legacy delivery config: %w", err)
	}
	if cfg.Version != LegacyConfigVersion {
		return LegacyConfig{}, fmt.Errorf("delivery config version is %d, not the migratable version %d", cfg.Version, LegacyConfigVersion)
	}
	if cfg.ReleaseRepository == "" || !validRepository(cfg.ReleaseRepository) || cfg.Channel != "stable" {
		return LegacyConfig{}, errors.New("legacy delivery config repository or channel is invalid")
	}
	if poll, err := time.ParseDuration(cfg.PollInterval); err != nil || poll < time.Minute {
		return LegacyConfig{}, errors.New("legacy poll_interval must be at least 1m")
	}
	if drain, err := time.ParseDuration(cfg.DrainTimeout); err != nil || drain <= 0 {
		return LegacyConfig{}, errors.New("legacy drain_timeout must be positive")
	}
	if cfg.AutoApply != "schema_compatible" && cfg.AutoApply != "never" {
		return LegacyConfig{}, errors.New("legacy auto_apply is invalid")
	}
	return cfg, nil
}

func (c LegacyConfig) Migrated(assignments map[string]RepositoryAssignment) Config {
	return Config{
		Version: ConfigVersion, Enabled: c.Enabled, ReleaseRepository: c.ReleaseRepository,
		Channel: c.Channel, PollInterval: c.PollInterval, DrainTimeout: c.DrainTimeout,
		AutoApply: "never", TrustedWorkflow: c.TrustedWorkflow, Assignments: assignments,
	}
}

func (c Config) Validate() error {
	if c.Version != ConfigVersion {
		return fmt.Errorf("unsupported delivery config version %d; this controller supports %d", c.Version, ConfigVersion)
	}
	if c.ReleaseRepository == "" || !validRepository(c.ReleaseRepository) {
		return errors.New("release_repository must be owner/repository")
	}
	if c.Channel != "stable" {
		return fmt.Errorf("unsupported delivery channel %q", c.Channel)
	}
	poll, err := time.ParseDuration(c.PollInterval)
	if err != nil || poll < time.Minute {
		return errors.New("poll_interval must be a duration of at least 1m")
	}
	drain, err := time.ParseDuration(c.DrainTimeout)
	if err != nil || drain <= 0 {
		return errors.New("drain_timeout must be a positive duration")
	}
	if c.AutoApply != "never" {
		return errors.New("per-repository delivery requires auto_apply: never")
	}
	if c.Assignments == nil {
		return errors.New("assignments must be present")
	}
	for id, assignment := range c.Assignments {
		if id == "" || assignment.RepositoryID != id {
			return fmt.Errorf("assignment key and repository_id must match: %q", id)
		}
		if assignment.Generation == 0 {
			return fmt.Errorf("assignment %s generation must be positive", id)
		}
		if assignment.UpdatedAt.IsZero() {
			return fmt.Errorf("assignment %s updated_at must be present", id)
		}
		if err := validateAssignmentRef(assignment.AssignmentRef); err != nil {
			return fmt.Errorf("assignment %s: %w", id, err)
		}
		if assignment.Previous != nil {
			if err := validateAssignmentRef(*assignment.Previous); err != nil {
				return fmt.Errorf("assignment %s previous: %w", id, err)
			}
			if *assignment.Previous == assignment.AssignmentRef {
				return fmt.Errorf("assignment %s previous must differ from current", id)
			}
		}
	}
	return nil
}

func validateAssignmentRef(ref AssignmentRef) error {
	if _, err := ParseSemVer(ref.Version); err != nil {
		return err
	}
	if !validSHA(ref.Commit) {
		return errors.New("commit must be a lowercase 40-character SHA")
	}
	if !validDigest(ref.ArtifactSHA256) {
		return errors.New("artifact_sha256 must be a lowercase SHA-256")
	}
	if !filepath.IsAbs(ref.Slot) {
		return errors.New("slot must be an absolute path")
	}
	return nil
}

func (c Config) PollDuration() time.Duration {
	value, _ := time.ParseDuration(c.PollInterval)
	return value
}
func (c Config) DrainDuration() time.Duration {
	value, _ := time.ParseDuration(c.DrainTimeout)
	return value
}

func (c Config) WorkflowIdentity() string {
	if c.TrustedWorkflow != "" {
		return c.TrustedWorkflow
	}
	return c.ReleaseRepository + "/.github/workflows/release.yml"
}

func WriteConfig(path string, cfg Config) error {
	if !filepath.IsAbs(path) {
		return errors.New("delivery config path must be absolute")
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	if _, err := os.Lstat(path); err == nil {
		if err := validatePrivateRegularFile(path); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect delivery config: %w", err)
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	if err := fsutil.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write delivery config: %w", err)
	}
	return validatePrivateRegularFile(path)
}

func validatePrivateRegularFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect delivery config: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return errors.New("delivery config must be a regular file and not a symbolic link")
	}
	if info.Mode().Perm() != 0o600 {
		return fmt.Errorf("delivery config mode must be 0600, got %04o", info.Mode().Perm())
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != os.Getuid() {
		return errors.New("delivery config must be owned by the current user")
	}
	return nil
}

func validRepository(value string) bool {
	parts := 0
	segment := 0
	for _, r := range value {
		switch {
		case r == '/':
			if segment == 0 || parts != 0 {
				return false
			}
			parts++
			segment = 0
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			segment++
		default:
			return false
		}
	}
	return parts == 1 && segment > 0
}
