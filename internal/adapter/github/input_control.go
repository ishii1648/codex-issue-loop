package github

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/ishii1648/codex-issue-loop/internal/adapter/state"
	"github.com/ishii1648/codex-issue-loop/internal/platform/config"
	"github.com/ishii1648/codex-issue-loop/internal/platform/redact"
)

const InputControlVersion = 1

type InputComment struct {
	ID        int64
	Body      string
	Actor     string
	ActorType string
	CreatedAt time.Time
	UpdatedAt time.Time
}

type InputAcknowledgement struct {
	RequestID string
	CommentID int64
	Outcome   string
	Detail    string
}

type InputControlClient interface {
	SyncInputRequest(context.Context, config.Config, state.Request) error
	ListInputComments(context.Context, config.Config, int) ([]InputComment, error)
	VerifyInputActor(context.Context, config.Config, InputComment) (AuthorVerification, error)
	SyncInputAcknowledgement(context.Context, config.Config, int, InputAcknowledgement) error
}

type inputRequestMarkerPayload struct {
	Version           int            `json:"version"`
	RequestID         string         `json:"request_id"`
	IssueNumber       int            `json:"issue_number"`
	RunID             string         `json:"run_id,omitempty"`
	Question          string         `json:"question"`
	Reason            string         `json:"reason,omitempty"`
	RecommendedOption string         `json:"recommended_option,omitempty"`
	Options           []state.Option `json:"options"`
	AllowFreeText     bool           `json:"allow_free_text"`
	CreatedAt         time.Time      `json:"created_at"`
}

func (c CLI) SyncInputRequest(ctx context.Context, cfg config.Config, request state.Request) error {
	if err := c.editLabels(ctx, cfg.GitHub.Repo, request.IssueNumber, []string{cfg.GitHub.NeedsInputLabel}, []string{cfg.GitHub.RunningLabel}); err != nil {
		return err
	}
	payload := inputRequestMarkerPayload{
		Version: InputControlVersion, RequestID: request.ID, IssueNumber: request.IssueNumber, RunID: request.RunID,
		Question:          redact.StringWithSecrets(request.Question, c.Secrets),
		Reason:            redact.StringWithSecrets(request.Reason, c.Secrets),
		RecommendedOption: redact.StringWithSecrets(request.Recommended, c.Secrets),
		Options:           append([]state.Option(nil), request.Options...), AllowFreeText: request.AllowFreeText, CreatedAt: request.CreatedAt.UTC(),
	}
	for index := range payload.Options {
		payload.Options[index].ID = redact.StringWithSecrets(payload.Options[index].ID, c.Secrets)
		payload.Options[index].Label = redact.StringWithSecrets(payload.Options[index].Label, c.Secrets)
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(encoded)
	markerKey := fmt.Sprintf("<!-- codex-issue-loop:input-request:v1 request=%s ", request.ID)
	marker := fmt.Sprintf("%spayload=%s digest=%s -->", markerKey,
		base64.RawURLEncoding.EncodeToString(encoded), hex.EncodeToString(digest[:]))
	legacyMarker := fmt.Sprintf("<!-- codex-issue-loop:request:%s -->", request.ID)
	body := renderInputRequest(marker, legacyMarker, payload)
	return c.syncManagedComment(ctx, cfg.GitHub.Repo, request.IssueNumber, markerKey, body)
}

func renderInputRequest(marker, legacyMarker string, payload inputRequestMarkerPayload) string {
	var body strings.Builder
	body.WriteString(marker)
	body.WriteByte('\n')
	body.WriteString(legacyMarker)
	body.WriteString("\n## Input required\n\n")
	body.WriteString(payload.Question)
	if payload.Reason != "" {
		body.WriteString("\n\nReason: ")
		body.WriteString(payload.Reason)
	}
	if payload.RecommendedOption != "" {
		body.WriteString("\n\nRecommended option: `")
		body.WriteString(payload.RecommendedOption)
		body.WriteByte('`')
	}
	if len(payload.Options) > 0 {
		body.WriteString("\n\nOptions:")
		for _, option := range payload.Options {
			body.WriteString("\n- `")
			body.WriteString(option.ID)
			body.WriteString("`: ")
			body.WriteString(option.Label)
		}
	}
	body.WriteString(fmt.Sprintf("\n\nFree-text allowed: `%t`\nCreated: `%s`", payload.AllowFreeText, payload.CreatedAt.Format(time.RFC3339Nano)))
	body.WriteString("\n\nReply with exactly:\n```text\n/agent-loop answer ")
	body.WriteString(payload.RequestID)
	body.WriteString(" <answer>\n```")
	return body.String()
}

func (c CLI) ListInputComments(ctx context.Context, cfg config.Config, number int) ([]InputComment, error) {
	path := c.Path
	if path == "" {
		path = "gh"
	}
	endpoint := fmt.Sprintf("/repos/%s/issues/%d/comments?per_page=100", cfg.GitHub.Repo, number)
	out, err := exec.CommandContext(ctx, path, "api", "--paginate", "--slurp", "-H", "Accept: application/vnd.github+json", endpoint).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("list comments for Issue #%d: %w: %s", number, err, c.safe(out))
	}
	var pages [][]struct {
		ID        int64     `json:"id"`
		Body      string    `json:"body"`
		CreatedAt time.Time `json:"created_at"`
		UpdatedAt time.Time `json:"updated_at"`
		User      struct {
			Login string `json:"login"`
			Type  string `json:"type"`
		} `json:"user"`
	}
	if err := json.Unmarshal(out, &pages); err != nil {
		return nil, fmt.Errorf("decode comments for Issue #%d: %w", number, err)
	}
	comments := make([]InputComment, 0)
	for _, page := range pages {
		for _, raw := range page {
			comments = append(comments, InputComment{ID: raw.ID, Body: raw.Body, Actor: raw.User.Login, ActorType: raw.User.Type, CreatedAt: raw.CreatedAt, UpdatedAt: raw.UpdatedAt})
		}
	}
	sort.SliceStable(comments, func(i, j int) bool {
		if comments[i].CreatedAt.Equal(comments[j].CreatedAt) {
			return comments[i].ID < comments[j].ID
		}
		return comments[i].CreatedAt.Before(comments[j].CreatedAt)
	})
	return comments, nil
}

func (c CLI) VerifyInputActor(ctx context.Context, cfg config.Config, comment InputComment) (AuthorVerification, error) {
	return c.VerifyIssueAuthor(ctx, cfg, Issue{AuthorLogin: comment.Actor, AuthorType: comment.ActorType})
}

func (c CLI) SyncInputAcknowledgement(ctx context.Context, cfg config.Config, number int, acknowledgement InputAcknowledgement) error {
	payload, err := json.Marshal(struct {
		Version   int    `json:"version"`
		CommentID int64  `json:"comment_id"`
		RequestID string `json:"request_id"`
		Outcome   string `json:"outcome"`
	}{
		Version: InputControlVersion, CommentID: acknowledgement.CommentID,
		RequestID: acknowledgement.RequestID, Outcome: acknowledgement.Outcome,
	})
	if err != nil {
		return err
	}
	digest := sha256.Sum256(payload)
	markerKey := fmt.Sprintf("<!-- codex-issue-loop:answer-ack:v1 comment=%d ", acknowledgement.CommentID)
	marker := fmt.Sprintf("%spayload=%s digest=%s -->", markerKey, base64.RawURLEncoding.EncodeToString(payload), hex.EncodeToString(digest[:]))
	body := fmt.Sprintf("%s\n`agent-loop` answer `%s`: **%s**.", marker, acknowledgement.RequestID, acknowledgement.Outcome)
	if acknowledgement.Detail != "" {
		body += " " + redact.StringWithSecrets(acknowledgement.Detail, c.Secrets)
	}
	return c.syncManagedComment(ctx, cfg.GitHub.Repo, number, markerKey, body)
}

func (c CLI) syncManagedComment(ctx context.Context, repo string, number int, marker, body string) error {
	comments, err := c.ListInputComments(ctx, config.Config{GitHub: config.GitHub{Repo: repo}}, number)
	if err != nil {
		return err
	}
	viewer, err := c.viewerLogin(ctx)
	if err != nil {
		return err
	}
	managed := make([]InputComment, 0)
	for _, comment := range comments {
		if strings.EqualFold(comment.Actor, viewer) && strings.Contains(comment.Body, marker) {
			managed = append(managed, comment)
		}
	}
	if len(managed) == 0 {
		return c.mutateComment(ctx, "POST", fmt.Sprintf("/repos/%s/issues/%d/comments", repo, number), body)
	}
	if managed[0].Body != body {
		if err := c.mutateComment(ctx, "PATCH", fmt.Sprintf("/repos/%s/issues/comments/%d", repo, managed[0].ID), body); err != nil {
			return err
		}
	}
	for _, duplicate := range managed[1:] {
		if err := c.deleteComment(ctx, fmt.Sprintf("/repos/%s/issues/comments/%d", repo, duplicate.ID)); err != nil {
			return err
		}
	}
	return nil
}

func (c CLI) viewerLogin(ctx context.Context) (string, error) {
	var viewer struct {
		Login string `json:"login"`
	}
	if err := c.apiJSON(ctx, "/user", &viewer); err != nil {
		return "", err
	}
	if strings.TrimSpace(viewer.Login) == "" {
		return "", fmt.Errorf("GitHub viewer login is empty")
	}
	return viewer.Login, nil
}

func (c CLI) mutateComment(ctx context.Context, method, endpoint, body string) error {
	path := c.Path
	if path == "" {
		path = "gh"
	}
	payload, err := json.Marshal(map[string]string{"body": body})
	if err != nil {
		return err
	}
	command := exec.CommandContext(ctx, path, "api", "--method", method, "-H", "Accept: application/vnd.github+json", "--input", "-", endpoint)
	command.Stdin = strings.NewReader(string(payload))
	if out, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("%s GitHub Issue comment: %w: %s", strings.ToLower(method), err, c.safe(out))
	}
	return nil
}

func (c CLI) deleteComment(ctx context.Context, endpoint string) error {
	path := c.Path
	if path == "" {
		path = "gh"
	}
	if out, err := exec.CommandContext(ctx, path, "api", "--method", "DELETE", "-H", "Accept: application/vnd.github+json", endpoint).CombinedOutput(); err != nil {
		return fmt.Errorf("delete duplicate GitHub Issue comment: %w: %s", err, c.safe(out))
	}
	return nil
}

func IsManagedInputComment(body string) bool {
	return strings.Contains(body, "<!-- codex-issue-loop:input-request:") || strings.Contains(body, "<!-- codex-issue-loop:answer-ack:")
}
