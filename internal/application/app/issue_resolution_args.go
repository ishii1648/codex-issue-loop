package app

import (
	"context"
	"flag"
	"fmt"
	"github.com/ishii1648/codex-issue-loop/internal/platform/layout"

	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
)

type issueResolveOptions struct {
	repo         string
	number       int
	action       issuedomain.ResolutionAction
	expectedHead string
	allowPaths   []string
	jsonOut      bool
}

func (a App) parseIssueResolveArgs(args []string) (*issueResolveOptions, error) {
	fs := flag.NewFlagSet("issue resolve", flag.ContinueOnError)
	fs.SetOutput(a.Err)
	repo := fs.String("repo", "", "repository path")
	number := fs.Int("issue", 0, "Issue number")
	actionText := fs.String("action", "", "resume, retry-stage, adopt-head, adopt-input, adopt-worktree, approve-conflict-paths, adopt-pr, or cancel")
	expectedHead := fs.String("expected-head", "", "exact current HEAD approved for adopt-head or adopt-input")
	var allowPaths pathListFlag
	fs.Var(&allowPaths, "allow-path", "explicit changed path to add to conflict recovery scope; repeatable")
	jsonOut := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return nil, exitError{2, err}
	}
	action := issuedomain.ResolutionAction(*actionText)
	if *number <= 0 || action.Validate() != nil || fs.NArg() != 0 {
		return nil, exitError{2, fmt.Errorf("--issue and a valid --action are required")}
	}
	if (action == issuedomain.ResolutionAdoptHead || action == issuedomain.ResolutionAdoptInput) != (*expectedHead != "") {
		return nil, exitError{2, fmt.Errorf("--expected-head is required only for adopt-head or adopt-input")}
	}
	normalizedAllowPaths, err := normalizeAdoptionAllowPaths(allowPaths)
	if err != nil {
		return nil, exitError{2, err}
	}
	if len(normalizedAllowPaths) > 0 && action != issuedomain.ResolutionAdoptWorktree && action != issuedomain.ResolutionApproveConflictPaths {
		return nil, exitError{2, fmt.Errorf("--allow-path is valid only with --action adopt-worktree or approve-conflict-paths")}
	}
	return &issueResolveOptions{*repo, *number, action, *expectedHead, normalizedAllowPaths, *jsonOut}, nil
}

func (a App) issueCommand(ctx context.Context, l layout.Layout, args []string) error {
	if len(args) == 0 {
		return exitError{2, fmt.Errorf("issue requires ask, plan or resolve")}
	}
	switch args[0] {
	case "ask":
		return a.issueAsk(ctx, l, args[1:])
	case "plan":
		return a.issuePlan(ctx, l, args[1:])
	case "resolve":
		return a.issueResolve(ctx, l, args[1:])
	case "help", "--help", "-h":
		fmt.Fprintln(a.Out, "Usage: agent-loop issue ask --repo PATH --issue N --json < question.json\n       agent-loop issue plan --repo PATH --issue N [--allow-path PATH] --json\n       agent-loop issue resolve --repo PATH --issue N --action resume|retry-stage|adopt-head|adopt-input|adopt-worktree|approve-conflict-paths|adopt-pr|cancel [--expected-head SHA] [--allow-path PATH] --json")
		return nil
	default:
		return exitError{2, fmt.Errorf("unknown issue command %q", args[0])}
	}
}
