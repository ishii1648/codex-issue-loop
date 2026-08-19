package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ishii1648/codex-issue-loop/internal/config"
	"github.com/ishii1648/codex-issue-loop/internal/fsutil"
	gh "github.com/ishii1648/codex-issue-loop/internal/github"
	"github.com/ishii1648/codex-issue-loop/internal/layout"
	"github.com/ishii1648/codex-issue-loop/internal/recoveryfixture"
	"github.com/ishii1648/codex-issue-loop/internal/state"
	"github.com/ishii1648/codex-issue-loop/internal/worktree"
)

type rawFixtureSnapshot struct {
	Version         int                        `json:"version"`
	RepoID          string                     `json:"repo_id"`
	RepoPath        string                     `json:"repo_path"`
	StateRevision   uint64                     `json:"state_revision"`
	Issues          map[string]json.RawMessage `json:"issues"`
	PendingRequests map[string]json.RawMessage `json:"pending_requests"`
}

func (a App) exportRecoveryFixture(ctx context.Context, l layout.Layout, args []string) error {
	fs := flag.NewFlagSet("export-recovery-fixture", flag.ContinueOnError)
	fs.SetOutput(a.Err)
	repo := fs.String("repo", "", "registered repository path")
	issueNumber := fs.Int("issue", 0, "Issue number to capture")
	output := fs.String("output", "", "new fixture JSON path")
	jsonOut := fs.Bool("json", false, "emit JSON result")
	if err := fs.Parse(args); err != nil {
		return exitError{2, err}
	}
	if *issueNumber <= 0 || strings.TrimSpace(*output) == "" {
		return exitError{2, errors.New("--issue and --output are required")}
	}
	if _, err := os.Lstat(*output); err == nil {
		return exitError{2, fmt.Errorf("refuse to overwrite existing fixture %s", *output)}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	entry, err := a.resolvePath(l, *repo)
	if err != nil {
		return err
	}
	cfg, err := config.Load(entry.RepoPath)
	if err != nil {
		return err
	}
	statePath := (state.Store{Dir: l.RepoDir(entry.RepoID)}).StatePath()
	eventsPath := (state.Store{Dir: l.RepoDir(entry.RepoID)}).EventsPath()
	stateBytes, err := os.ReadFile(statePath)
	if err != nil {
		return fmt.Errorf("read durable snapshot without recovery: %w", err)
	}
	eventBytes, err := os.ReadFile(eventsPath)
	if err != nil {
		return fmt.Errorf("read durable events without recovery: %w", err)
	}

	var snapshot rawFixtureSnapshot
	if err := json.Unmarshal(stateBytes, &snapshot); err != nil {
		return fmt.Errorf("decode durable snapshot without recovery: %w", err)
	}
	issueRaw := snapshot.Issues[strconv.Itoa(*issueNumber)]
	if len(issueRaw) == 0 {
		return exitError{4, fmt.Errorf("Issue #%d is missing from durable state", *issueNumber)}
	}
	var issue state.Issue
	if err := json.Unmarshal(issueRaw, &issue); err != nil {
		return fmt.Errorf("decode target Issue: %w", err)
	}
	requests := make([]json.RawMessage, 0)
	requestKeys := make([]string, 0, len(snapshot.PendingRequests))
	for key := range snapshot.PendingRequests {
		requestKeys = append(requestKeys, key)
	}
	sort.Strings(requestKeys)
	for _, key := range requestKeys {
		var request struct {
			IssueNumber int `json:"issue_number"`
		}
		if err := json.Unmarshal(snapshot.PendingRequests[key], &request); err != nil {
			return fmt.Errorf("decode pending request %s: %w", key, err)
		}
		if request.IssueNumber == *issueNumber {
			requests = append(requests, append(json.RawMessage(nil), snapshot.PendingRequests[key]...))
		}
	}
	events, err := selectFixtureEvents(eventBytes, *issueNumber)
	if err != nil {
		return err
	}
	if len(events) == 0 {
		return exitError{4, fmt.Errorf("Issue #%d has no durable events", *issueNumber)}
	}

	manager := worktree.Manager{StateRoot: l.Root, GitPath: entry.Commands["git"]}
	inspection, err := manager.Inspect(ctx, cfg, issue.Worktree, issue.Branch)
	if err != nil {
		return fmt.Errorf("inspect recovery fixture worktree read-only: %w", err)
	}
	remote, err := (gh.CLI{Path: entry.Commands["gh"], Secrets: cfg.RedactionValues()}).Inspect(ctx, cfg, *issueNumber, issue.Branch)
	if err != nil {
		return fmt.Errorf("inspect recovery fixture GitHub state read-only: %w", err)
	}
	stateAfter, err := os.ReadFile(statePath)
	if err != nil {
		return err
	}
	eventsAfter, err := os.ReadFile(eventsPath)
	if err != nil {
		return err
	}
	if !bytes.Equal(stateBytes, stateAfter) || !bytes.Equal(eventBytes, eventsAfter) {
		return exitError{4, errors.New("durable state changed during recovery fixture acquisition; no fixture was written")}
	}

	bundle, err := recoveryfixture.Build(recoveryfixture.Input{
		SourceSchemaVersion: snapshot.Version, SourceVersion: Version, CapturedAt: time.Now().UTC(),
		Repository: cfg.GitHub.Repo, IssueNumber: *issueNumber, RepoID: snapshot.RepoID,
		RepoPath: snapshot.RepoPath, StateRevision: snapshot.StateRevision, Issue: issueRaw,
		PendingRequests: requests, Events: events, Worktree: inspection, Remote: remote,
		Secrets: cfg.RedactionValues(),
	})
	if err != nil {
		return err
	}
	if err := fsutil.WriteJSON(*output, bundle, 0o600); err != nil {
		return err
	}
	return a.output(*jsonOut, map[string]any{
		"path": *output, "issue": *issueNumber, "event_count": len(events),
		"content_sha256": bundle.Manifest.ContentSHA256, "read_only_source": true,
	})
}

func selectFixtureEvents(data []byte, issueNumber int) ([]json.RawMessage, error) {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	result := make([]json.RawMessage, 0)
	line := 0
	for scanner.Scan() {
		line++
		raw := bytes.TrimSpace(scanner.Bytes())
		if len(raw) == 0 {
			continue
		}
		var envelope struct {
			IssueNumber int `json:"issue_number"`
		}
		if err := json.Unmarshal(raw, &envelope); err != nil {
			return nil, fmt.Errorf("decode event line %d: %w", line, err)
		}
		if envelope.IssueNumber == issueNumber {
			result = append(result, append(json.RawMessage(nil), raw...))
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan durable events: %w", err)
	}
	return result, nil
}

func (a App) verifyRecoveryFixture(args []string) error {
	fs := flag.NewFlagSet("verify-recovery-fixture", flag.ContinueOnError)
	fs.SetOutput(a.Err)
	fixturePath := fs.String("fixture", "", "fixture JSON path")
	jsonOut := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return exitError{2, err}
	}
	if strings.TrimSpace(*fixturePath) == "" {
		return exitError{2, errors.New("--fixture is required")}
	}
	bundle, err := recoveryfixture.Load(*fixturePath)
	if err != nil {
		return exitError{4, err}
	}
	return a.output(*jsonOut, map[string]any{
		"valid": true, "fixture": *fixturePath, "issue": bundle.Manifest.IssueNumber,
		"event_count": bundle.Completeness.EventCount, "content_sha256": bundle.Manifest.ContentSHA256,
	})
}
