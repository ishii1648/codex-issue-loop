package github

import (
	"context"
	"fmt"
	"strings"
	"time"

	queuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/queue"
	"github.com/ishii1648/codex-issue-loop/internal/platform/config"
)

type AuthorVerification = queuedomain.AuthorVerification

type AuthorVerifier interface {
	VerifyIssueAuthor(context.Context, config.Config, Issue) (AuthorVerification, error)
}

func (c CLI) VerifyIssueAuthor(ctx context.Context, cfg config.Config, issue Issue) (AuthorVerification, error) {
	login := strings.ToLower(strings.TrimSpace(issue.AuthorLogin))
	owner, _, _ := strings.Cut(strings.ToLower(cfg.GitHub.Repo), "/")
	policy := queuedomain.AuthorPolicy{AllowLogins: cfg.GitHub.TrustedIssueAuthors.AllowLogins}
	baseFacts := queuedomain.AuthorFacts{Login: login, AccountType: issue.AuthorType, RepositoryOwner: login != "" && login == owner}
	preflight := queuedomain.VerifyAuthor(policy, baseFacts, time.Now().UTC())
	if preflight.Reason != "permission_below_write" {
		return preflight, nil
	}
	var response struct {
		Permission string `json:"permission"`
		User       struct {
			Login string `json:"login"`
		} `json:"user"`
	}
	path := fmt.Sprintf("/repos/%s/collaborators/%s/permission", cfg.GitHub.Repo, login)
	if err := c.apiJSON(ctx, path, &response); err != nil {
		preflight.Reason = "permission_unverifiable"
		return preflight, err
	}
	if response.User.Login == "" || !strings.EqualFold(response.User.Login, login) {
		preflight.Reason = "identity_mismatch"
		return preflight, nil
	}
	baseFacts.Permission = response.Permission
	return queuedomain.VerifyAuthor(policy, baseFacts, time.Now().UTC()), nil
}
