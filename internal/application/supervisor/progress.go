package supervisor

import (
	"context"
	"fmt"
	"time"

	"github.com/ishii1648/codex-issue-loop/internal/adapter/state"
	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
	"github.com/ishii1648/codex-issue-loop/internal/platform/config"
)

type progressCommenter interface {
	CommentProgress(context.Context, config.Config, int, string, string) error
}

// Progress is best effort; notification failures must not retry execution or publication.
func (l *Loop) commentProgress(ctx context.Context, current state.Issue, milestone, body string) {
	client := l.GitHub
	if guarded, ok := client.(*rateLimitedGitHub); ok {
		if err := guarded.before(ctx); err != nil {
			return
		}
		client = guarded.delegate
	}
	commenter, ok := client.(progressCommenter)
	if !ok {
		return
	}
	key := fmt.Sprintf("%s:%d:%d:%d:%s:%s", current.RunID, current.Generation, current.Attempts, current.Continuations, current.HeadSHA, milestone)
	_ = commenter.CommentProgress(ctx, l.Config, current.Number, key, body)
}

func (l *Loop) commentPublication(ctx context.Context, current state.Issue, prURL, head string) {
	if prURL == "" {
		return
	}
	message := "実装結果をPRに公開しました："
	if current.PullRequestURL != "" {
		message = "PRに修正を反映しました："
	}
	message += prURL + "\nCIの結果を確認しています。"
	if l.Config.Completion.AutoMerge {
		message += "必要なチェックとレビューが通り、マージ可能になれば自動マージします。"
	} else {
		message += "自動マージは無効のため、チェック確認後は手動でのマージを待ちます。"
	}
	l.commentProgress(ctx, current, "published:"+prURL+":"+head, message)
}

func (l *Loop) commentRetry(ctx context.Context, current state.Issue, stage, reason string, retryAt time.Time) {
	message := stage + "で失敗しました。"
	if stage == "CI確認" {
		message = "PRのCIが失敗しました：" + current.PullRequestURL
	}
	message += "\n理由：" + reason + "\n" + retryAt.UTC().Format("2006-01-02 15:04:05 MST") + "以降にworkerを再実行し、修正・検証を行います。"
	l.commentProgress(ctx, current, "retry:"+retryAt.Format(time.RFC3339Nano), message)
}

func (l *Loop) commentResume(ctx context.Context, current state.Issue, stage string) {
	id := ""
	if current.Continuation != nil {
		id = current.Continuation.ID
	}
	if current.Suspension != nil {
		id += ":" + current.Suspension.ID
	}
	l.commentProgress(ctx, current, "resumed:"+id+":"+stage, "回答に基づき、"+stage+"を再開しました。")
}

func (l *Loop) commentWorkerStart(ctx context.Context, current state.Issue) {
	switch current.LaunchSource {
	case issuedomain.StatusResumePending:
		l.commentResume(ctx, current, "実装")
	case issuedomain.StatusRetryWait:
		l.commentProgress(ctx, current, "retry-started", "再試行予定に基づき、実装・検証を開始しました。")
	case issuedomain.StatusResolvingConflict:
		l.commentConflictStart(ctx, current)
	}
}

func (l *Loop) commentConflictStart(ctx context.Context, current state.Issue) {
	answered := current.Suspension != nil && current.Suspension.Status == issuedomain.SuspensionResolved &&
		current.Continuation != nil && current.Continuation.Stage == issuedomain.ContinuationStageConflict
	if recovery := current.ConflictRecovery; recovery != nil {
		index := len(recovery.History) - 1
		if index >= 0 && recovery.History[index].Status == issuedomain.ConflictAttemptStatusRunning {
			index--
		}
		answered = answered || index >= 0 && recovery.History[index].Status == issuedomain.ConflictAttemptStatusNeedsInput
	}
	if answered {
		l.commentResume(ctx, current, "競合解消")
	} else {
		l.commentProgress(ctx, current, "conflict-started", fmt.Sprintf("PRに `%s` との競合があるため、自動解消を開始しました：%s\n解消後に検証し、PRを更新してCIを再確認します。", l.Config.Git.BaseBranch, current.PullRequestURL))
	}
}
