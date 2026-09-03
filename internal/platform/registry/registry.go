package registry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ishii1648/codex-issue-loop/internal/platform/compat"
	"github.com/ishii1648/codex-issue-loop/internal/platform/config"
	"github.com/ishii1648/codex-issue-loop/internal/platform/fsutil"
	schemaversion "github.com/ishii1648/codex-issue-loop/internal/platform/schema"
)

type Entry struct {
	RepoID          string            `json:"repo_id"`
	RepoPath        string            `json:"repo_path"`
	GitHubRepo      string            `json:"github_repo"`
	RegisteredAt    time.Time         `json:"registered_at"`
	Commands        map[string]string `json:"commands,omitempty"`
	EnvironmentPath string            `json:"environment_path,omitempty"`
	WorkerBackend   string            `json:"worker_backend,omitempty"`
	WorkerVersion   string            `json:"worker_version,omitempty"`
}

type Registry struct {
	Version int              `json:"version"`
	Repos   map[string]Entry `json:"repos"`
}

const CurrentVersion = schemaversion.Current

type Store struct{ Path string }

func (s Store) Load() (Registry, error) {
	r := Registry{Version: CurrentVersion, Repos: map[string]Entry{}}
	info, err := os.Lstat(s.Path)
	if errors.Is(err, os.ErrNotExist) {
		return r, nil
	}
	if err != nil {
		return Registry{}, fmt.Errorf("inspect registry: %w", err)
	}
	if !info.Mode().IsRegular() {
		return Registry{}, fmt.Errorf("registry is not a regular file: %s", s.Path)
	}
	if err := os.Chmod(s.Path, 0o600); err != nil {
		return Registry{}, fmt.Errorf("secure registry: %w", err)
	}
	data, err := os.ReadFile(s.Path)
	if err != nil {
		return Registry{}, fmt.Errorf("read registry: %w", err)
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return Registry{}, fmt.Errorf("decode registry: %w", err)
	}
	if r.Version != CurrentVersion {
		if r.Version == schemaversion.Previous {
			return Registry{}, fmt.Errorf("registry schema migration required from version %d to %d; stop loops and run agent-loop migrate --apply", schemaversion.Previous, CurrentVersion)
		}
		return Registry{}, fmt.Errorf("unsupported registry version %d; this binary supports version %d", r.Version, CurrentVersion)
	}
	if r.Repos == nil {
		r.Repos = map[string]Entry{}
	}
	return r, nil
}

func (s Store) Add(cfg config.Config) (Entry, error) {
	r, err := s.Load()
	if err != nil {
		return Entry{}, err
	}
	entry := Entry{
		RepoID:          RepoID(cfg.GitHub.Repo, cfg.RepoPath),
		RepoPath:        cfg.RepoPath,
		GitHubRepo:      cfg.GitHub.Repo,
		RegisteredAt:    time.Now().UTC(),
		Commands:        map[string]string{},
		EnvironmentPath: os.Getenv("PATH"),
		WorkerBackend:   cfg.Worker.Backend,
	}
	if entry.WorkerBackend == "" {
		entry.WorkerBackend = "codex"
	}
	commands := map[string]string{"git": "git", "gh": "gh", entry.WorkerBackend: cfg.Worker.EffectiveCommand(), "launchctl": "launchctl"}
	if cfg.Formatters.Go.Enabled {
		commands["gofmt"] = "gofmt"
	}
	for name, command := range commands {
		path, resolveErr := exec.LookPath(command)
		if resolveErr != nil {
			return Entry{}, fmt.Errorf("resolve %s command %q: %w", name, command, resolveErr)
		}
		absolute, resolveErr := filepath.Abs(path)
		if resolveErr != nil {
			return Entry{}, resolveErr
		}
		if name == "gofmt" {
			formatterCtx, cancelFormatter := context.WithTimeout(context.Background(), 5*time.Second)
			absolute, resolveErr = resolveGofmt(formatterCtx, absolute)
			cancelFormatter()
			if resolveErr != nil {
				return Entry{}, fmt.Errorf("verify gofmt capability: %w", resolveErr)
			}
		}
		entry.Commands[name] = absolute
	}
	probeCtx, cancelProbe := context.WithTimeout(context.Background(), 10*time.Second)
	entry.WorkerVersion = compat.ProbeBackend(probeCtx, entry.WorkerBackend, entry.Commands[entry.WorkerBackend]).Version
	cancelProbe()
	if old, ok := r.Repos[entry.RepoID]; ok {
		entry.RegisteredAt = old.RegisteredAt
	}
	for id, existing := range r.Repos {
		if existing.RepoPath == entry.RepoPath && id != entry.RepoID {
			delete(r.Repos, id)
		}
	}
	r.Repos[entry.RepoID] = entry
	if err := fsutil.WriteJSON(s.Path, r, 0o600); err != nil {
		return Entry{}, err
	}
	return entry, nil
}

func resolveGofmt(ctx context.Context, discovered string) (string, error) {
	candidate, candidateErr := canonicalExecutable(discovered)
	if candidateErr == nil {
		if probeErr := compat.ProbeGofmt(ctx, candidate); probeErr == nil {
			return candidate, nil
		} else {
			candidateErr = probeErr
		}
	}
	goPath, err := exec.LookPath("go")
	if err != nil {
		return "", fmt.Errorf("registered path is not self-contained (%v) and Go toolchain discovery failed: %w", candidateErr, err)
	}
	output, err := exec.CommandContext(ctx, goPath, "env", "GOROOT").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("registered path is not self-contained (%v) and resolve GOROOT: %w: %s", candidateErr, err, strings.TrimSpace(string(output)))
	}
	root := strings.TrimSpace(string(output))
	if !filepath.IsAbs(root) {
		return "", fmt.Errorf("registered path is not self-contained (%v) and GOROOT is not absolute", candidateErr)
	}
	toolchainFormatter, err := canonicalExecutable(filepath.Join(root, "bin", "gofmt"))
	if err != nil {
		return "", fmt.Errorf("registered path is not self-contained (%v) and resolve toolchain gofmt: %w", candidateErr, err)
	}
	if err := compat.ProbeGofmt(ctx, toolchainFormatter); err != nil {
		return "", fmt.Errorf("registered path is not self-contained (%v) and toolchain gofmt probe failed: %w", candidateErr, err)
	}
	return toolchainFormatter, nil
}

func canonicalExecutable(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
		return "", fmt.Errorf("command is not an executable regular file: %s", resolved)
	}
	return resolved, nil
}

func (s Store) Remove(repoID string) error {
	r, err := s.Load()
	if err != nil {
		return err
	}
	if _, ok := r.Repos[repoID]; !ok {
		return nil
	}
	delete(r.Repos, repoID)
	return fsutil.WriteJSON(s.Path, r, 0o600)
}

func (s Store) Resolve(explicitPath, cwd string) (Entry, error) {
	r, err := s.Load()
	if err != nil {
		return Entry{}, err
	}
	if explicitPath != "" {
		canonical, err := config.CanonicalRepoPath(explicitPath)
		if err != nil {
			return Entry{}, err
		}
		for _, entry := range r.Repos {
			if entry.RepoPath == canonical {
				return entry, nil
			}
		}
		return Entry{}, fmt.Errorf("repository is not registered: %s", canonical)
	}
	if cwd != "" {
		canonical, err := config.CanonicalRepoPath(cwd)
		if err == nil {
			for _, entry := range r.Repos {
				if canonical == entry.RepoPath || strings.HasPrefix(canonical, entry.RepoPath+string(filepath.Separator)) {
					return entry, nil
				}
			}
		}
	}
	if len(r.Repos) == 1 {
		for _, entry := range r.Repos {
			return entry, nil
		}
	}
	ids := make([]string, 0, len(r.Repos))
	for id := range r.Repos {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	if len(ids) == 0 {
		return Entry{}, errors.New("no repositories are registered")
	}
	return Entry{}, fmt.Errorf("repository is ambiguous; specify --repo (registered: %s)", strings.Join(ids, ", "))
}

func RepoID(githubRepo, repoPath string) string {
	sum := sha256.Sum256([]byte(githubRepo + "\x00" + repoPath))
	name := strings.ToLower(strings.ReplaceAll(filepath.Base(repoPath), "_", "-"))
	return fmt.Sprintf("%s-%s", name, hex.EncodeToString(sum[:4]))
}
