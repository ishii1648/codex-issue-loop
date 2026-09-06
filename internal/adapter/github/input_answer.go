package github

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strings"

	"github.com/ishii1648/codex-issue-loop/internal/adapter/state"
	"github.com/ishii1648/codex-issue-loop/internal/platform/config"
)

var inputRepoPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

type InputAnswerResult struct {
	RequestID  string `json:"request_id"`
	Status     string `json:"status"`
	CommentID  int64  `json:"comment_id,omitempty"`
	CommentURL string `json:"comment_url,omitempty"`
	Recorded   string `json:"recorded"`
	Execution  string `json:"execution"`
}

func decodeInputMarker(body, prefix string, destination any) error {
	line, _, _ := strings.Cut(body, "\n")
	if !strings.HasPrefix(line, prefix) || !strings.HasSuffix(line, " -->") {
		return fmt.Errorf("invalid input marker")
	}
	fields := strings.Fields(strings.TrimSuffix(strings.TrimPrefix(line, prefix), " -->"))
	if len(fields) != 2 || !strings.HasPrefix(fields[0], "payload=") || !strings.HasPrefix(fields[1], "digest=") {
		return fmt.Errorf("invalid input marker fields")
	}
	data, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(fields[0], "payload="))
	if err != nil || state.BodyDigest(string(data)) != strings.TrimPrefix(fields[1], "digest=") {
		return fmt.Errorf("invalid input marker digest")
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return fmt.Errorf("invalid input marker payload")
	}
	if !json.Valid(data) {
		return fmt.Errorf("invalid input marker JSON")
	}
	return nil
}

func (c CLI) inputAnswerContext(ctx context.Context, repo string, issue int, requestID string, requireCurrent bool) (inputRequestMarkerPayload, []InputComment, error) {
	var target inputRequestMarkerPayload
	if !inputRepoPattern.MatchString(repo) || issue <= 0 || !state.ValidID(requestID, "req_") {
		return target, nil, fmt.Errorf("GitHub answer requires --repo owner/repo, positive --issue and valid --request-id")
	}
	cfg := config.Config{GitHub: config.GitHub{Repo: repo}}
	comments, err := c.ListInputComments(ctx, cfg, issue)
	if err != nil {
		return target, nil, err
	}
	trusted := make([]InputComment, 0)
	var latest inputRequestMarkerPayload
	count := 0
	for _, comment := range comments {
		verification, err := c.VerifyInputActor(ctx, cfg, comment)
		if err != nil {
			return target, nil, err
		}
		if !verification.Trusted {
			continue
		}
		trusted = append(trusted, comment)
		if !strings.HasPrefix(comment.Body, "<!-- codex-issue-loop:input-request:") {
			continue
		}
		line, _, _ := strings.Cut(comment.Body, "\n")
		parts := strings.Fields(line)
		if len(parts) < 3 {
			return target, nil, fmt.Errorf("invalid question marker")
		}
		id := strings.TrimPrefix(parts[2], "request=")
		var payload inputRequestMarkerPayload
		prefix := "<!-- codex-issue-loop:input-request:v1 request=" + id + " "
		if err := decodeInputMarker(comment.Body, prefix, &payload); err != nil {
			return target, nil, err
		}
		if payload.Version != InputControlVersion || payload.RequestID != id || !state.ValidID(id, "req_") ||
			payload.IssueNumber != issue || (payload.RunID != "" && !state.ValidID(payload.RunID, "run_")) || payload.Question == "" || payload.CreatedAt.IsZero() ||
			comment.Body != renderInputRequest(line, "<!-- codex-issue-loop:request:"+id+" -->", payload) {
			return target, nil, fmt.Errorf("question does not match its Issue/request/run contract")
		}
		if latest.CreatedAt.IsZero() || !payload.CreatedAt.Before(latest.CreatedAt) {
			latest = payload
		}
		if id == requestID {
			target = payload
			count++
		}
	}
	if count != 1 {
		return target, nil, fmt.Errorf("expected one authenticated question for the specified request")
	}
	// GitHub is a projection; RecordAnswer still checks the canonical run and request under lock.
	if requireCurrent && latest.RequestID != target.RequestID {
		return target, nil, fmt.Errorf("request has been superseded by a newer question")
	}
	return target, trusted, nil
}

func inputAnswerResult(repo string, issue int, requestID string, commentID int64) InputAnswerResult {
	result := InputAnswerResult{RequestID: requestID, Status: "unknown", Recorded: "unknown", Execution: "unknown", CommentID: commentID}
	if commentID > 0 {
		result.CommentURL = fmt.Sprintf("https://github.com/%s/issues/%d#issuecomment-%d", repo, issue, commentID)
	}
	return result
}

func inputAnswerReceipt(result InputAnswerResult, comments []InputComment) (InputAnswerResult, error) {
	count := 0
	for _, comment := range comments {
		prefix := fmt.Sprintf("<!-- codex-issue-loop:answer-ack:v1 comment=%d ", result.CommentID)
		if !strings.HasPrefix(comment.Body, prefix) {
			continue
		}
		var payload struct {
			Version   int    `json:"version"`
			CommentID int64  `json:"comment_id"`
			RequestID string `json:"request_id"`
			Outcome   string `json:"outcome"`
		}
		if err := decodeInputMarker(comment.Body, prefix, &payload); err != nil {
			return result, err
		}
		if payload.Version != InputControlVersion || payload.CommentID != result.CommentID || payload.RequestID != result.RequestID {
			return result, fmt.Errorf("acknowledgement does not match request and submitted comment")
		}
		count++
		if count > 1 {
			return result, fmt.Errorf("ambiguous answer acknowledgements")
		}
		switch payload.Outcome {
		case "accepted":
			result.Status = "accepted"
			result.Recorded = "true"
		case "conflict", "stale", "unauthorized", "malformed":
			result.Status = payload.Outcome
		default:
			return result, fmt.Errorf("unknown answer acknowledgement outcome")
		}
	}
	return result, nil
}

func (c CLI) CheckInputAnswer(ctx context.Context, repo string, issue int, requestID string, commentID int64) (InputAnswerResult, error) {
	result := inputAnswerResult(repo, issue, requestID, commentID)
	if commentID < 0 {
		return result, fmt.Errorf("--comment-id must not be negative")
	}
	_, comments, err := c.inputAnswerContext(ctx, repo, issue, requestID, false)
	if err != nil {
		return result, err
	}
	if commentID == 0 {
		for _, comment := range comments {
			if strings.HasPrefix(comment.Body, "/agent-loop answer "+requestID+" ") {
				if result.CommentID != 0 {
					return result, fmt.Errorf("multiple submitted answers; specify --comment-id")
				}
				result = inputAnswerResult(repo, issue, requestID, comment.ID)
			}
		}
		if result.CommentID == 0 {
			return result, nil
		}
		commentID = result.CommentID
	}
	for _, comment := range comments {
		if comment.ID != commentID {
			continue
		}
		if !strings.HasPrefix(comment.Body, "/agent-loop answer "+requestID+" ") {
			// A receipt can still confirm a recorded answer after its source was edited.
			return inputAnswerReceipt(result, comments)
		}
		result.Status = "submitted"
	}
	return inputAnswerReceipt(result, comments)
}

func (c CLI) SubmitInputAnswer(ctx context.Context, repo string, issue int, requestID, answer string) (InputAnswerResult, error) {
	result := inputAnswerResult(repo, issue, requestID, 0)
	if strings.TrimSpace(answer) != answer || IsManagedInputComment(answer) {
		return result, fmt.Errorf("invalid answer text")
	}
	if err := state.ValidateAnswer(nil, answer, c.Secrets); err != nil {
		return result, err
	}
	request, comments, err := c.inputAnswerContext(ctx, repo, issue, requestID, true)
	if err != nil {
		return result, err
	}
	if err := state.ValidateAnswer(&state.Request{Options: request.Options, AllowFreeText: request.AllowFreeText}, answer, c.Secrets); err != nil {
		return result, err
	}
	body := "/agent-loop answer " + requestID + " " + answer
	for _, comment := range comments {
		if !strings.HasPrefix(comment.Body, "/agent-loop answer "+requestID+" ") {
			continue
		}
		if comment.Body != body {
			return result, fmt.Errorf("a different answer has already been submitted for this request")
		}
		if comment.CreatedAt.Before(request.CreatedAt) {
			return result, fmt.Errorf("existing answer predates request")
		}
		result = inputAnswerResult(repo, issue, requestID, comment.ID)
		result.Status = "submitted"
		return inputAnswerReceipt(result, comments)
	}
	for _, comment := range comments {
		if strings.HasPrefix(comment.Body, "<!-- codex-issue-loop:answer-ack:v1 ") {
			var ack struct {
				Version   int    `json:"version"`
				CommentID int64  `json:"comment_id"`
				RequestID string `json:"request_id"`
				Outcome   string `json:"outcome"`
			}
			fields := strings.Fields(comment.Body)
			if len(fields) < 3 {
				return result, fmt.Errorf("invalid acknowledgement")
			}
			prefix := "<!-- codex-issue-loop:answer-ack:v1 " + fields[2] + " "
			if err := decodeInputMarker(comment.Body, prefix, &ack); err != nil {
				return result, err
			}
			if ack.RequestID == requestID && ack.Outcome == "accepted" {
				return result, fmt.Errorf("request already has an accepted answer")
			}
		}
	}
	var remoteIssue struct {
		Number      int             `json:"number"`
		State       string          `json:"state"`
		PullRequest json.RawMessage `json:"pull_request"`
	}
	if err := c.apiJSON(ctx, fmt.Sprintf("/repos/%s/issues/%d", repo, issue), &remoteIssue); err != nil {
		return result, err
	}
	if remoteIssue.Number != issue || remoteIssue.State != "open" || len(remoteIssue.PullRequest) != 0 {
		return result, fmt.Errorf("answer target must be an open Issue")
	}
	path := c.Path
	if path == "" {
		path = "gh"
	}
	payload, err := json.Marshal(map[string]string{"body": body})
	if err != nil {
		return result, err
	}
	command := exec.CommandContext(ctx, path, "api", "--method", "POST", "-H", "Accept: application/vnd.github+json", "--input", "-", fmt.Sprintf("/repos/%s/issues/%d/comments", repo, issue))
	command.Stdin = strings.NewReader(string(payload))
	out, postErr := command.Output()
	var posted struct {
		ID int64 `json:"id"`
	}
	if postErr == nil && json.Unmarshal(out, &posted) == nil && posted.ID > 0 {
		result = inputAnswerResult(repo, issue, requestID, posted.ID)
		result.Status = "submitted"
		return result, nil
	}

	// A failed POST may have committed. Reconcile once and never automatically resend.
	observed, err := c.ListInputComments(ctx, config.Config{GitHub: config.GitHub{Repo: repo}}, issue)
	if err != nil {
		return result, nil
	}
	for _, comment := range observed {
		if comment.Body != body || comment.CreatedAt.Before(request.CreatedAt) {
			continue
		}
		verification, err := c.VerifyInputActor(ctx, config.Config{}, comment)
		if err != nil {
			return result, nil
		}
		if !verification.Trusted {
			continue
		}
		result = inputAnswerResult(repo, issue, requestID, comment.ID)
		result.Status = "submitted"
		return result, nil
	}
	return result, nil
}
