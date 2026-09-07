package app

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	gh "github.com/ishii1648/codex-issue-loop/internal/adapter/github"
	"github.com/ishii1648/codex-issue-loop/internal/adapter/state"
	"github.com/ishii1648/codex-issue-loop/internal/adapter/worker"
	"github.com/ishii1648/codex-issue-loop/internal/platform/config"
	"github.com/ishii1648/codex-issue-loop/internal/platform/layout"
	"github.com/ishii1648/codex-issue-loop/internal/platform/redact"
)

func (a App) issueAsk(ctx context.Context, l layout.Layout, args []string) error {
	fs := flag.NewFlagSet("issue ask", flag.ContinueOnError)
	fs.SetOutput(a.Err)
	repo := fs.String("repo", "", "repository path")
	number := fs.Int("issue", 0, "existing Issue number")
	jsonOut := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return exitError{2, err}
	}
	if *number < 1 || fs.NArg() != 0 {
		return exitError{2, fmt.Errorf("--issue is required; provide question JSON on stdin")}
	}
	entry, err := a.resolvePath(l, *repo)
	if err != nil {
		return err
	}
	cfg, err := config.Load(entry.RepoPath)
	if err != nil {
		return err
	}
	data, err := io.ReadAll(io.LimitReader(a.In, 16*1024+1))
	if err != nil {
		return err
	}
	if len(data) > 16*1024 || redact.StringWithSecrets(string(data), cfg.RedactionValues()) != string(data) {
		return fmt.Errorf("question is too large or contains a configured secret")
	}
	var question worker.Question
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&question); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return fmt.Errorf("provide exactly one question JSON object")
	}
	if strings.TrimSpace(question.Text) == "" || strings.TrimSpace(question.Reason) == "" || len(question.Options) > 3 {
		return fmt.Errorf("question and reason are required; at most three options are supported")
	}
	client := gh.CLI{Path: entry.Commands["gh"], Secrets: cfg.RedactionValues()}
	remote, err := client.Get(ctx, cfg, *number)
	if err != nil {
		return err
	}
	if !strings.EqualFold(remote.State, "open") {
		return fmt.Errorf("questions require an open Issue")
	}
	store := state.Store{Dir: l.RepoDir(entry.RepoID), RepoID: entry.RepoID, RepoPath: entry.RepoPath, Secrets: cfg.RedactionValues()}
	updated, request, err := store.AskIntake(*number, remote.Title, question.Text, question.Reason, question.RecommendedOption, question.Options, question.AllowFreeText, time.Now().UTC())
	if err != nil {
		return err
	}
	if err := client.ReconcileIssue(ctx, cfg, *number, updated.Issues[fmt.Sprint(*number)].Status, true); err != nil {
		return fmt.Errorf("question %s saved; label synchronization pending: %w", request.ID, err)
	}
	return a.output(*jsonOut, map[string]any{"recorded": true, "request": request})
}
