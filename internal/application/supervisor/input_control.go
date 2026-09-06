package supervisor

import (
	"context"
	"errors"
	"regexp"
	"sort"
	"strconv"
	"strings"

	gh "github.com/ishii1648/codex-issue-loop/internal/adapter/github"
	"github.com/ishii1648/codex-issue-loop/internal/adapter/state"
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
	seen := map[int]bool{}
	for _, request := range snapshot.Requests() {
		seen[request.IssueNumber] = true
	}
	numbers := make([]int, 0, len(seen))
	for number := range seen {
		numbers = append(numbers, number)
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
	requests := []*state.Request{}
	for _, request := range snapshot.Requests() {
		if request.IssueNumber != issueNumber {
			continue
		}
		requests = append(requests, request)
		if request.Status == issuedomain.RequestStatusPending {
			if err := control.SyncInputRequest(ctx, l.Config, *request); err != nil {
				return err
			}
		}
	}
	if len(requests) == 0 {
		return nil
	}
	comments, err := control.ListInputComments(ctx, l.Config, issueNumber)
	if err != nil {
		return err
	}
	sort.SliceStable(comments, func(i, j int) bool {
		if comments[i].CreatedAt.Equal(comments[j].CreatedAt) {
			return comments[i].ID < comments[j].ID
		}
		return comments[i].CreatedAt.Before(comments[j].CreatedAt)
	})
	seen := map[int64]bool{}
	for _, comment := range comments {
		seen[comment.ID] = true
		edited := false
		for _, request := range requests {
			previous := request.AnswerProvenance
			if previous != nil && previous.CommentID == comment.ID &&
				(previous.BodySHA256 != state.BodyDigest(comment.Body) || !previous.CommentEdited.Equal(comment.UpdatedAt)) {
				if err := control.SyncInputAcknowledgement(ctx, l.Config, issueNumber, gh.InputAcknowledgement{
					RequestID: request.ID, CommentID: comment.ID, Outcome: "conflict", Detail: "The accepted comment was edited; its recorded answer is unchanged.",
				}); err != nil {
					return err
				}
				edited = true
				break
			}
		}
		if edited {
			continue
		}
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
			ack.Outcome, ack.Detail = "unauthorized", "The comment author is not the authenticated supervisor user."
			if err := control.SyncInputAcknowledgement(ctx, l.Config, issueNumber, ack); err != nil {
				return err
			}
			continue
		}
		current, loadErr := l.Store.Load()
		if loadErr != nil {
			return loadErr
		}
		candidate, candidateErr := current.Request(requestID)
		if candidateErr != nil || candidate.IssueNumber != issueNumber || candidate.Status == issuedomain.RequestStatusCanceled ||
			comment.CreatedAt.Before(candidate.CreatedAt) {
			ack.Outcome, ack.Detail = "stale", "The request is not current for this Issue."
			if err := control.SyncInputAcknowledgement(ctx, l.Config, issueNumber, ack); err != nil {
				return err
			}
			continue
		}
		if candidate.Status == issuedomain.RequestStatusPending {
			key := strconv.Itoa(issueNumber)
			runID := candidate.RunID
			if item := current.Issues[key]; item != nil {
				runID = item.RunID
			} else if item := current.QuarantinedIssues[key]; item != nil {
				runID = item.RunID
			}
			if runID != candidate.RunID {
				ack.Outcome, ack.Detail = "stale", "The request belongs to a previous run."
				if err := control.SyncInputAcknowledgement(ctx, l.Config, issueNumber, ack); err != nil {
					return err
				}
				continue
			}
		}
		digest := state.BodyDigest(comment.Body)
		if candidate.Status == issuedomain.RequestStatusAnswered {
			if candidate.AnswerProvenance != nil && candidate.AnswerProvenance.CommentID == comment.ID && candidate.AnswerProvenance.BodySHA256 == digest && candidate.Answer == answer && strings.EqualFold(candidate.AnswerProvenance.Actor, verification.Login) && candidate.AnswerProvenance.CommentEdited.Equal(comment.UpdatedAt) {
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
		if err := state.ValidateAnswer(candidate, answer, l.Config.RedactionValues()); err != nil {
			ack.Outcome, ack.Detail = "malformed", err.Error()
			if err := control.SyncInputAcknowledgement(ctx, l.Config, issueNumber, ack); err != nil {
				return err
			}
			continue
		}
		_, _, recordErr := l.Store.RecordAnswer(requestID, answer, l.now(), provenance)
		if recordErr == nil {
			ack.Outcome = "accepted"
		} else {
			var conflict state.ConflictError
			if errors.As(recordErr, &conflict) {
				ack.Outcome, ack.Detail = "conflict", conflict.Error()
			} else {
				return recordErr
			}
		}
		if err := control.SyncInputAcknowledgement(ctx, l.Config, issueNumber, ack); err != nil {
			return err
		}
	}
	for _, request := range requests {
		provenance := request.AnswerProvenance
		if provenance != nil && !seen[provenance.CommentID] {
			if err := control.SyncInputAcknowledgement(ctx, l.Config, issueNumber, gh.InputAcknowledgement{
				RequestID: request.ID, CommentID: provenance.CommentID, Outcome: "accepted",
				Detail: "The accepted answer remains recorded after comment deletion.",
			}); err != nil {
				return err
			}
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
	answer = strings.TrimSpace(match[2])
	if answer == "" || answer != match[2] {
		return match[1], answer, true, false
	}
	return match[1], answer, true, true
}
