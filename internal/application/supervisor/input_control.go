package supervisor

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	gh "github.com/ishii1648/codex-issue-loop/internal/adapter/github"
	"github.com/ishii1648/codex-issue-loop/internal/adapter/state"
	"github.com/ishii1648/codex-issue-loop/internal/application/inputanswer"
	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
	"github.com/ishii1648/codex-issue-loop/internal/platform/config"
)

var answerCommandPattern = regexp.MustCompile(`(?s)^/agent-loop answer ([A-Za-z0-9._-]{1,200}) (.+)$`)

type guardedInputControl struct {
	guard   *rateLimitedGitHub
	control gh.InputControlClient
}

func (c guardedInputControl) SyncInputRequest(ctx context.Context, cfg config.Config, request state.Request) error {
	if err := c.guard.before(); err != nil {
		return err
	}
	return c.control.SyncInputRequest(ctx, cfg, request)
}

func (c guardedInputControl) ListInputComments(ctx context.Context, cfg config.Config, number int) ([]gh.InputComment, error) {
	if err := c.guard.before(); err != nil {
		return nil, err
	}
	return c.control.ListInputComments(ctx, cfg, number)
}

func (c guardedInputControl) VerifyInputActor(ctx context.Context, cfg config.Config, comment gh.InputComment) (gh.AuthorVerification, error) {
	if err := c.guard.before(); err != nil {
		return gh.AuthorVerification{}, err
	}
	return c.control.VerifyInputActor(ctx, cfg, comment)
}

func (c guardedInputControl) SyncInputAcknowledgement(ctx context.Context, cfg config.Config, number int, acknowledgement gh.InputAcknowledgement) error {
	if err := c.guard.before(); err != nil {
		return err
	}
	return c.control.SyncInputAcknowledgement(ctx, cfg, number, acknowledgement)
}

func (l *Loop) inputControlClient() (gh.InputControlClient, bool) {
	if guarded, ok := l.GitHub.(*rateLimitedGitHub); ok {
		control, supported := guarded.delegate.(gh.InputControlClient)
		if !supported {
			return nil, false
		}
		return guardedInputControl{guard: guarded, control: control}, true
	}
	control, ok := l.GitHub.(gh.InputControlClient)
	return control, ok
}

func needsInputIssues(snapshot state.Snapshot) []int {
	numbers := make([]int, 0)
	for _, issue := range snapshot.Issues {
		if issue != nil && issue.Status == issuedomain.StatusNeedsInput {
			numbers = append(numbers, issue.Number)
		}
	}
	sort.Ints(numbers)
	return numbers
}

func (l *Loop) reconcileInputIssue(ctx context.Context, issueNumber int) error {
	control, ok := l.inputControlClient()
	if !ok {
		return nil
	}
	snapshot, err := l.Store.Load()
	if err != nil {
		return err
	}
	issue := snapshot.Issues[strconv.Itoa(issueNumber)]
	if issue == nil || (issue.Status != issuedomain.StatusNeedsInput && issue.Status != issuedomain.StatusResumePending) {
		return nil
	}
	request := requestForInputIssue(snapshot, *issue)
	if request == nil {
		return fmt.Errorf("Issue #%d has no canonical needs-input request", issueNumber)
	}
	if err := control.SyncInputRequest(ctx, l.Config, *request); err != nil {
		return err
	}
	comments, err := control.ListInputComments(ctx, l.Config, issueNumber)
	if err != nil {
		return err
	}
	for _, comment := range comments {
		if gh.IsManagedInputComment(comment.Body) {
			continue
		}
		requestID, answer, recognized, valid := parseAnswerCommand(comment.Body)
		if !recognized {
			continue
		}
		ack := gh.InputAcknowledgement{RequestID: requestID, CommentID: comment.ID}
		if !valid {
			ack.Outcome, ack.Detail = "malformed", "Expected `/agent-loop answer <request-id> <answer>`."
			if err := control.SyncInputAcknowledgement(ctx, l.Config, issueNumber, ack); err != nil {
				return err
			}
			continue
		}
		if strings.EqualFold(comment.ActorType, "Bot") || strings.EqualFold(comment.ActorType, "App") {
			ack.Outcome, ack.Detail = "unauthorized", "Automation accounts cannot answer needs-input requests."
			if err := control.SyncInputAcknowledgement(ctx, l.Config, issueNumber, ack); err != nil {
				return err
			}
			continue
		}
		verification, verifyErr := control.VerifyInputActor(ctx, l.Config, comment)
		if verifyErr != nil {
			return verifyErr
		}
		if !verification.Trusted {
			ack.Outcome, ack.Detail = "unauthorized", "The comment author does not currently have write permission."
			if err := control.SyncInputAcknowledgement(ctx, l.Config, issueNumber, ack); err != nil {
				return err
			}
			continue
		}
		current, loadErr := l.Store.Load()
		if loadErr != nil {
			return loadErr
		}
		candidate := current.PendingRequests[requestID]
		if candidate == nil || candidate.IssueNumber != issueNumber || candidate.RunID != issue.RunID {
			ack.Outcome, ack.Detail = "stale", "The request is not the active request for this Issue and run."
			if err := control.SyncInputAcknowledgement(ctx, l.Config, issueNumber, ack); err != nil {
				return err
			}
			continue
		}
		digest := inputanswer.BodyDigest(comment.Body)
		if candidate.Status == issuedomain.RequestStatusAnswered {
			if candidate.AnswerProvenance != nil && candidate.AnswerProvenance.CommentID == comment.ID && candidate.AnswerProvenance.BodySHA256 == digest && candidate.Answer == answer {
				ack.Outcome = "accepted"
			} else if candidate.Answer == answer && (candidate.AnswerProvenance == nil || candidate.AnswerProvenance.CommentID != comment.ID) {
				ack.Outcome, ack.Detail = "stale", "This request was already answered."
			} else {
				ack.Outcome, ack.Detail = "conflict", "This request already has a different accepted answer or the accepted comment was edited."
			}
			if err := control.SyncInputAcknowledgement(ctx, l.Config, issueNumber, ack); err != nil {
				return err
			}
			continue
		}
		provenance := &state.AnswerProvenance{
			Source: "github_issue_comment", CommentID: comment.ID, Actor: verification.Login, Permission: verification.Permission,
			RequestID: requestID, IssueNumber: issueNumber, RunID: candidate.RunID, BodySHA256: digest,
			CommentedAt: comment.CreatedAt.UTC(), CommentEdited: comment.UpdatedAt.UTC(),
		}
		_, _, recordErr := inputanswer.Record(l.Store, requestID, answer, l.Config.RedactionValues(), provenance, l.now())
		if recordErr == nil {
			ack.Outcome = "accepted"
		} else {
			var conflict inputanswer.ConflictError
			if errors.As(recordErr, &conflict) {
				ack.Outcome, ack.Detail = "conflict", conflict.Error()
			} else {
				ack.Outcome, ack.Detail = "malformed", recordErr.Error()
			}
		}
		if err := control.SyncInputAcknowledgement(ctx, l.Config, issueNumber, ack); err != nil {
			return err
		}
	}
	return nil
}

func requestForInputIssue(snapshot state.Snapshot, issue state.Issue) *state.Request {
	if issue.Continuation != nil && issue.Continuation.Kind == state.ContinuationKindNeedsInput {
		if request := snapshot.PendingRequests[issue.Continuation.RequestID]; request != nil {
			return request
		}
	}
	for _, request := range snapshot.PendingRequests {
		if request != nil && request.IssueNumber == issue.Number && request.Status == issuedomain.RequestStatusPending {
			return request
		}
	}
	return nil
}

func parseAnswerCommand(body string) (requestID, answer string, recognized, valid bool) {
	if !strings.HasPrefix(body, "/agent-loop") {
		return "", "", false, false
	}
	match := answerCommandPattern.FindStringSubmatch(body)
	if len(match) != 3 {
		return "", "", true, false
	}
	answer = inputanswer.NormalizeCommandAnswer(match[2])
	if answer == "" || answer != match[2] {
		return match[1], answer, true, false
	}
	return match[1], answer, true, true
}
