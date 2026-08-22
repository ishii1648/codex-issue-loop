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
	ConfigVersion   = 1
	ProtocolVersion = 1
)

type Config struct {
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
	return Config{Version: ConfigVersion, Enabled: true, ReleaseRepository: repository, Channel: "stable", PollInterval: "15m", DrainTimeout: "2h30m", AutoApply: "schema_compatible"}
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
	if c.AutoApply != "schema_compatible" && c.AutoApply != "never" {
		return errors.New("auto_apply must be schema_compatible or never")
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
