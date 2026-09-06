package app

import (
	"flag"
	"fmt"

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

func parseIssueResolveArgs(args []string) (*issueResolveOptions, error) {
	fs := flag.NewFlagSet("issue resolve", flag.ContinueOnError)
	repo := fs.String("repo", "", "repository path")
	number := fs.Int("issue", 0, "Issue number")
	actionText := fs.String("action", "", "resume, retry-stage, adopt-head, adopt-worktree, adopt-pr, or cancel")
	expectedHead := fs.String("expected-head", "", "exact current HEAD approved for adopt-head")
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
	if (action == issuedomain.ResolutionAdoptHead) != (*expectedHead != "") {
		return nil, exitError{2, fmt.Errorf("--expected-head is required only for adopt-head")}
	}
	normalizedAllowPaths, err := normalizeAdoptionAllowPaths(allowPaths)
	if err != nil {
		return nil, exitError{2, err}
	}
	if len(normalizedAllowPaths) > 0 && action != issuedomain.ResolutionAdoptWorktree {
		return nil, exitError{2, fmt.Errorf("--allow-path is valid only with --action adopt-worktree")}
	}
	return &issueResolveOptions{*repo, *number, action, *expectedHead, normalizedAllowPaths, *jsonOut}, nil
}
