package github

import (
	"context"
	"github.com/ishii1648/codex-issue-loop/internal/adapter/githubqueue"
	"github.com/ishii1648/codex-issue-loop/internal/platform/config"
)

type ConditionalQueueResult = githubqueue.ConditionalQueueResult
type ConditionalQueueClient = githubqueue.ConditionalQueueClient
type PagedConditionalQueueClient = githubqueue.PagedConditionalQueueClient

var parseIncludedResponse = githubqueue.ParseIncludedResponse

func (c CLI) ListReadyConditional(ctx context.Context, cfg config.Config, etag, lastModified string) (ConditionalQueueResult, error) {
	return (githubqueue.CLI{Path: c.Path, Secrets: c.Secrets}).ListReadyConditional(ctx, cfg, etag, lastModified)
}
func (c CLI) ListReadyConditionalPage(ctx context.Context, cfg config.Config, page int, etag, lastModified string) (ConditionalQueueResult, error) {
	return (githubqueue.CLI{Path: c.Path, Secrets: c.Secrets}).ListReadyConditionalPage(ctx, cfg, page, etag, lastModified)
}
