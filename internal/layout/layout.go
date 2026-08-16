package layout

import (
	"fmt"
	"os"
	"path/filepath"
)

type Layout struct {
	Root         string
	RegistryPath string
	ReposRoot    string
	BinDir       string
	SkillsDir    string
	LaunchAgents string
}

func New() (Layout, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Layout{}, fmt.Errorf("resolve user home: %w", err)
	}
	root := os.Getenv("AGENT_LOOP_HOME")
	if root == "" {
		root = filepath.Join(home, "Library", "Application Support", "codex-issue-loop")
	}
	skills := os.Getenv("AGENT_LOOP_SKILLS_DIR")
	if skills == "" {
		skills = filepath.Join(home, ".codex", "skills")
	}
	launchAgents := os.Getenv("AGENT_LOOP_LAUNCH_AGENTS_DIR")
	if launchAgents == "" {
		launchAgents = filepath.Join(home, "Library", "LaunchAgents")
	}
	return Layout{
		Root:         root,
		RegistryPath: filepath.Join(root, "registry.json"),
		ReposRoot:    filepath.Join(root, "repos"),
		BinDir:       filepath.Join(root, "bin"),
		SkillsDir:    skills,
		LaunchAgents: launchAgents,
	}, nil
}

func (l Layout) Ensure() error {
	for _, dir := range []string{l.Root, l.ReposRoot, l.BinDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			return fmt.Errorf("secure %s: %w", dir, err)
		}
	}
	return nil
}

func (l Layout) RepoDir(repoID string) string {
	return filepath.Join(l.ReposRoot, repoID)
}

func (l Layout) PlistPath(repoID string) string {
	return filepath.Join(l.LaunchAgents, "com.codex-issue-loop."+repoID+".plist")
}

func (l Layout) Label(repoID string) string {
	return "com.codex-issue-loop." + repoID
}
