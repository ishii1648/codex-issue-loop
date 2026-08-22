package worker

import (
	"fmt"

	"github.com/ishii1648/codex-issue-loop/internal/config"
)

type FactoryOptions struct {
	StateDir        string
	Secrets         []string
	RuntimeVersion  string
	ResumeSupported *bool
}

// NewBackend resolves only built-in adapters. It intentionally does not
// support command templates or shell expansion.
func NewBackend(cfg config.Config, options FactoryOptions) (Backend, error) {
	switch cfg.Worker.Backend {
	case "", "codex":
		return Codex{StateDir: options.StateDir, Secrets: options.Secrets, ResumeSupported: options.ResumeSupported, RuntimeVersion: options.RuntimeVersion}, nil
	case "claude-code":
		return ClaudeCode{StateDir: options.StateDir, Secrets: options.Secrets, ResumeSupported: options.ResumeSupported, RuntimeVersion: options.RuntimeVersion}, nil
	case "opencode":
		return OpenCode{StateDir: options.StateDir, Secrets: options.Secrets, ResumeSupported: options.ResumeSupported, RuntimeVersion: options.RuntimeVersion}, nil
	default:
		return nil, fmt.Errorf("unknown worker backend %q", cfg.Worker.Backend)
	}
}
