package app

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/ishii1648/codex-issue-loop/internal/config"
	"github.com/ishii1648/codex-issue-loop/internal/fsutil"
	"github.com/ishii1648/codex-issue-loop/internal/launchd"
	"github.com/ishii1648/codex-issue-loop/internal/layout"
	"github.com/ishii1648/codex-issue-loop/internal/registry"
)

const maxNotificationTokenBytes = 16 * 1024

func (a App) notificationToken(ctx context.Context, l layout.Layout, args []string) error {
	fs := flag.NewFlagSet("notification-token", flag.ContinueOnError)
	fs.SetOutput(a.Err)
	repo := fs.String("repo", "", "repository path")
	tokenFile := fs.String("token-file", "", "token file or - for stdin")
	clear := fs.Bool("clear", false, "remove the managed token")
	jsonOut := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return exitError{2, err}
	}
	if (*tokenFile == "" && !*clear) || (*tokenFile != "" && *clear) {
		return exitError{2, fmt.Errorf("provide exactly one of --token-file or --clear")}
	}
	entry, err := a.resolvePath(l, *repo)
	if err != nil {
		return err
	}
	cfg, err := config.Load(entry.RepoPath)
	if err != nil {
		return err
	}
	if cfg.Notifications.Enabled {
		status, statusErr := (launchd.Manager{Layout: l, Launchctl: entry.Commands["launchctl"]}).Status(ctx, entry)
		if statusErr != nil {
			return fmt.Errorf("check loop status before changing notification credential: %w", statusErr)
		}
		if status.Loaded {
			return fmt.Errorf("stop the repository loop before changing its notification credential")
		}
	}
	path := l.NotificationTokenPath(entry.RepoID)
	if *clear {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove managed notification token: %w", err)
		}
		return a.output(*jsonOut, map[string]any{"repo_id": entry.RepoID, "configured": false})
	}
	token, err := readTokenInput(a.In, *tokenFile)
	if err != nil {
		return exitError{2, err}
	}
	if err := fsutil.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		return fmt.Errorf("store managed notification token: %w", err)
	}
	return a.output(*jsonOut, map[string]any{"repo_id": entry.RepoID, "configured": true})
}

func readTokenInput(stdin io.Reader, path string) (string, error) {
	var reader io.Reader
	if path == "-" {
		reader = stdin
	} else {
		file, err := os.Open(path)
		if err != nil {
			return "", err
		}
		defer file.Close()
		reader = file
	}
	data, err := io.ReadAll(io.LimitReader(reader, maxNotificationTokenBytes+1))
	if err != nil {
		return "", err
	}
	if len(data) > maxNotificationTokenBytes {
		return "", fmt.Errorf("notification token must not exceed %d bytes", maxNotificationTokenBytes)
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return "", fmt.Errorf("notification token must not be empty")
	}
	if strings.IndexFunc(token, func(r rune) bool { return r == '\n' || r == '\r' }) >= 0 {
		return "", fmt.Errorf("notification token must be one line")
	}
	return token, nil
}

func loadNotificationToken(l layout.Layout, entry registry.Entry) (string, error) {
	path := l.NotificationTokenPath(entry.RepoID)
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("managed notification token is not a regular file")
	}
	if info.Mode().Perm() != 0o600 {
		return "", fmt.Errorf("managed notification token must have mode 0600, got %04o", info.Mode().Perm())
	}
	return readTokenInput(nil, path)
}
