package supervisor

import (
	"context"
	"fmt"
	"time"

	"github.com/ishii1648/codex-issue-loop/internal/config"
	gh "github.com/ishii1648/codex-issue-loop/internal/github"
	"github.com/ishii1648/codex-issue-loop/internal/ratelimit"
)

const unknownRateLimitCooldown = 1 * time.Hour
const recoveredRateLimitCooldown = 5 * time.Second

type primaryRateLimitStatusObserver interface {
	PrimaryRateLimitStatus(context.Context, string) (gh.RateLimitStatus, bool)
}

type rateLimitedGitHub struct {
	loop     *Loop
	delegate gh.Client
}

func (c *rateLimitedGitHub) before() error {
	cooldown, active, err := c.loop.RateLimits.Suppress(c.loop.now())
	if err != nil {
		return fmt.Errorf("read shared GitHub rate-limit cooldown: %w", err)
	}
	if !active {
		return nil
	}
	return &gh.RateLimitError{
		Resource: cooldown.Resource, ResetAt: cooldown.ResetAt, Source: cooldown.Source,
		Err: fmt.Errorf("GitHub %s primary rate limit is in shared cooldown until %s", cooldown.Resource, cooldown.ResetAt.Format(time.RFC3339)),
	}
}

func (c *rateLimitedGitHub) ListReady(ctx context.Context, cfg config.Config) ([]gh.Issue, error) {
	if err := c.before(); err != nil {
		return nil, err
	}
	return c.delegate.ListReady(ctx, cfg)
}

func (c *rateLimitedGitHub) Get(ctx context.Context, cfg config.Config, number int) (gh.Issue, error) {
	if err := c.before(); err != nil {
		return gh.Issue{}, err
	}
	return c.delegate.Get(ctx, cfg, number)
}

func (c *rateLimitedGitHub) Inspect(ctx context.Context, cfg config.Config, number int, branch string) (gh.RemoteState, error) {
	if err := c.before(); err != nil {
		return gh.RemoteState{}, err
	}
	return c.delegate.Inspect(ctx, cfg, number, branch)
}

func (c *rateLimitedGitHub) Claim(ctx context.Context, cfg config.Config, issue gh.Issue, runID string) error {
	if err := c.before(); err != nil {
		return err
	}
	return c.delegate.Claim(ctx, cfg, issue, runID)
}

func (c *rateLimitedGitHub) MarkNeedsInput(ctx context.Context, cfg config.Config, number int, requestID, question string) error {
	if err := c.before(); err != nil {
		return err
	}
	return c.delegate.MarkNeedsInput(ctx, cfg, number, requestID, question)
}

func (c *rateLimitedGitHub) MarkDone(ctx context.Context, cfg config.Config, number int, prURL string) error {
	if err := c.before(); err != nil {
		return err
	}
	return c.delegate.MarkDone(ctx, cfg, number, prURL)
}

func (c *rateLimitedGitHub) MarkFailed(ctx context.Context, cfg config.Config, number int, reason string, blocked bool) error {
	if err := c.before(); err != nil {
		return err
	}
	return c.delegate.MarkFailed(ctx, cfg, number, reason, blocked)
}

func (c *rateLimitedGitHub) MarkRunning(ctx context.Context, cfg config.Config, number int) error {
	if err := c.before(); err != nil {
		return err
	}
	return c.delegate.MarkRunning(ctx, cfg, number)
}

func (c *rateLimitedGitHub) MarkConflictRetry(ctx context.Context, cfg config.Config, number int, recoveryID string) error {
	if err := c.before(); err != nil {
		return err
	}
	return c.delegate.MarkConflictRetry(ctx, cfg, number, recoveryID)
}

func (c *rateLimitedGitHub) ReadyPullRequest(ctx context.Context, cfg config.Config, url string) error {
	if err := c.before(); err != nil {
		return err
	}
	return c.delegate.ReadyPullRequest(ctx, cfg, url)
}

func (c *rateLimitedGitHub) UpdatePullRequest(ctx context.Context, cfg config.Config, url string) error {
	if err := c.before(); err != nil {
		return err
	}
	return c.delegate.UpdatePullRequest(ctx, cfg, url)
}

func (c *rateLimitedGitHub) MergePullRequest(ctx context.Context, cfg config.Config, url string) error {
	if err := c.before(); err != nil {
		return err
	}
	return c.delegate.MergePullRequest(ctx, cfg, url)
}

func (c *rateLimitedGitHub) MarkEnvironmentResume(ctx context.Context, cfg config.Config, number int, resumeID string) error {
	if err := c.before(); err != nil {
		return err
	}
	resumer, ok := c.delegate.(interface {
		MarkEnvironmentResume(context.Context, config.Config, int, string) error
	})
	if !ok {
		return fmt.Errorf("GitHub client does not support environment resume")
	}
	return resumer.MarkEnvironmentResume(ctx, cfg, number, resumeID)
}

func (l *Loop) enableRateLimitGate() {
	if l.RateLimits.Path == "" {
		return
	}
	if _, wrapped := l.GitHub.(*rateLimitedGitHub); !wrapped {
		l.GitHub = &rateLimitedGitHub{loop: l, delegate: l.GitHub}
	}
}

func cooldownFromError(err error, now time.Time) (ratelimit.Cooldown, bool) {
	limited, ok := gh.AsRateLimit(err)
	if !ok {
		return ratelimit.Cooldown{}, false
	}
	resetAt := limited.ResetAt
	source := limited.Source
	if !resetAt.After(now) {
		resetAt = now.Add(unknownRateLimitCooldown)
		source = "fallback"
	}
	resource := limited.Resource
	if resource == "" {
		resource = "graphql"
	}
	return ratelimit.Cooldown{Resource: resource, ResetAt: resetAt, Source: source}, true
}

func (l *Loop) revalidateStartupCooldown(ctx context.Context, cooldown ratelimit.Cooldown, now time.Time) (ratelimit.Cooldown, error) {
	if cooldown.Source == "rest-rate-limit-recovered" {
		return cooldown, nil
	}
	delegate := l.GitHub
	if guarded, ok := delegate.(*rateLimitedGitHub); ok {
		delegate = guarded.delegate
	}
	observer, ok := delegate.(primaryRateLimitStatusObserver)
	if !ok {
		return cooldown, nil
	}
	status, ok := observer.PrimaryRateLimitStatus(ctx, cooldown.Resource)
	if !ok {
		return cooldown, nil
	}
	if status.Remaining <= 0 {
		if status.ResetAt.After(cooldown.ResetAt) {
			cooldown.ResetAt = status.ResetAt
			cooldown.Source = "rest-rate-limit"
			return l.RateLimits.Observe(cooldown, now)
		}
		return cooldown, nil
	}
	cooldown.ResetAt = now.Add(recoveredRateLimitCooldown)
	cooldown.Source = "rest-rate-limit-recovered"
	return l.RateLimits.Replace(cooldown, now)
}
