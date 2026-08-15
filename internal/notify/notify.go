package notify

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/ishii1648/codex-issue-loop/internal/config"
	"github.com/ishii1648/codex-issue-loop/internal/state"
)

type Message struct {
	ID          string
	Kind        string
	Title       string
	Body        string
	ClickURL    string
	IssueNumber int
}

type Adapter interface {
	Send(context.Context, Message) error
}

type Ntfy struct {
	Endpoint string
	Topic    string
	Token    string
	Client   *http.Client
}

func (n Ntfy) Send(ctx context.Context, message Message) error {
	if n.Token == "" {
		return errors.New("ntfy credential is empty")
	}
	target := strings.TrimRight(n.Endpoint, "/") + "/" + url.PathEscape(n.Topic)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader(message.Body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+n.Token)
	req.Header.Set("Title", message.Title)
	req.Header.Set("Tags", "warning")
	if message.ClickURL != "" {
		req.Header.Set("Click", message.ClickURL)
	}
	client := n.Client
	if client == nil {
		client = http.DefaultClient
	}
	response, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("send ntfy notification: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return fmt.Errorf("ntfy returned %s: %s", response.Status, strings.TrimSpace(string(body)))
	}
	return nil
}

type Dispatcher struct {
	Config  config.Config
	Store   state.Store
	Adapter Adapter
	Now     func() time.Time
}

func NewDispatcher(cfg config.Config, store state.Store, token string) *Dispatcher {
	if !cfg.Notifications.Enabled {
		return nil
	}
	return &Dispatcher{
		Config: cfg,
		Store:  store,
		Adapter: Ntfy{
			Endpoint: cfg.Notifications.Endpoint,
			Topic:    cfg.Notifications.Topic,
			Token:    token,
			Client:   &http.Client{Timeout: cfg.Notifications.Timeout.Duration},
		},
	}
}

func (d *Dispatcher) Dispatch(ctx context.Context) error {
	if d == nil || d.Adapter == nil || !d.Config.Notifications.Enabled {
		return nil
	}
	now := time.Now().UTC()
	if d.Now != nil {
		now = d.Now().UTC()
	}
	snapshot, err := d.Store.Load()
	if err != nil {
		return fmt.Errorf("load notification outbox: %w", err)
	}
	if err := d.cancelStale(snapshot, now); err != nil {
		return err
	}
	snapshot, err = d.Store.Load()
	if err != nil {
		return err
	}
	if withinRateLimit(snapshot, now, d.Config.Notifications.MinInterval.Duration) {
		return nil
	}
	pending := pendingNotifications(snapshot, now, d.Config.Notifications.MaxAttempts)
	if len(pending) == 0 {
		return nil
	}
	delivery := pending[0]
	message := d.message(snapshot, delivery)
	sendErr := d.Adapter.Send(ctx, message)
	return d.record(delivery, sendErr, now)
}

func pendingNotifications(snapshot state.Snapshot, now time.Time, maxAttempts int) []*state.Notification {
	result := []*state.Notification{}
	for _, delivery := range snapshot.Notifications {
		if delivery != nil && delivery.Status == "pending" && delivery.Attempts < maxAttempts && !delivery.NextAttempt.After(now) {
			copy := *delivery
			result = append(result, &copy)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].CreatedAt.Equal(result[j].CreatedAt) {
			return result[i].ID < result[j].ID
		}
		return result[i].CreatedAt.Before(result[j].CreatedAt)
	})
	return result
}

func withinRateLimit(snapshot state.Snapshot, now time.Time, interval time.Duration) bool {
	if interval <= 0 {
		return false
	}
	var latest time.Time
	for _, delivery := range snapshot.Notifications {
		if delivery != nil && delivery.SentAt != nil && delivery.SentAt.After(latest) {
			latest = *delivery.SentAt
		}
	}
	return !latest.IsZero() && now.Sub(latest) < interval
}

func (d *Dispatcher) cancelStale(snapshot state.Snapshot, now time.Time) error {
	for id, delivery := range snapshot.Notifications {
		if delivery == nil || delivery.Status != "pending" || delivery.Kind != "needs_input" {
			continue
		}
		request := snapshot.PendingRequests[delivery.RequestID]
		if request != nil && request.Status == "pending" {
			continue
		}
		_, err := d.Store.Update("notification_canceled", delivery.IssueNumber, delivery.RunID, map[string]string{"notification_id": id, "reason": "request no longer pending"}, func(current *state.Snapshot) error {
			if item := current.Notifications[id]; item != nil && item.Status == "pending" {
				item.Status = "canceled"
				item.NextAttempt = time.Time{}
			}
			return nil
		})
		if err != nil {
			return fmt.Errorf("cancel stale notification: %w", err)
		}
	}
	return nil
}

func (d *Dispatcher) message(snapshot state.Snapshot, delivery *state.Notification) Message {
	message := Message{
		ID: delivery.ID, Kind: delivery.Kind, IssueNumber: delivery.IssueNumber,
		Title:    "codex-issue-loop attention",
		ClickURL: "https://github.com/" + d.Config.GitHub.Repo,
	}
	switch delivery.Kind {
	case "needs_input":
		message.Body = fmt.Sprintf("%s needs input for Issue #%d (request %s)", d.Config.GitHub.Repo, delivery.IssueNumber, delivery.RequestID)
		message.ClickURL += fmt.Sprintf("/issues/%d", delivery.IssueNumber)
		if d.Config.Notifications.IncludeDetails {
			if request := snapshot.PendingRequests[delivery.RequestID]; request != nil {
				message.Body += ": " + request.Question
			}
		}
	case "issue_blocked":
		message.Body = fmt.Sprintf("%s Issue #%d is blocked", d.Config.GitHub.Repo, delivery.IssueNumber)
		message.ClickURL += fmt.Sprintf("/issues/%d", delivery.IssueNumber)
		if d.Config.Notifications.IncludeDetails {
			if issue := snapshot.Issues[fmt.Sprint(delivery.IssueNumber)]; issue != nil && issue.LastError != "" {
				message.Body += ": " + issue.LastError
			}
		}
	case "supervisor_blocked":
		message.Body = d.Config.GitHub.Repo + " supervisor is blocked"
		if d.Config.Notifications.IncludeDetails && snapshot.Supervisor.Message != "" {
			message.Body += ": " + snapshot.Supervisor.Message
		}
	default:
		message.Body = d.Config.GitHub.Repo + " needs attention"
	}
	return message
}

func (d *Dispatcher) record(pending *state.Notification, sendErr error, now time.Time) error {
	eventType := "notification_sent"
	if sendErr != nil {
		eventType = "notification_retry_scheduled"
		if pending.Attempts+1 >= d.Config.Notifications.MaxAttempts {
			eventType = "notification_failed"
		}
	}
	payload := map[string]any{"notification_id": pending.ID, "attempt": pending.Attempts + 1}
	if sendErr != nil {
		payload["error"] = sendErr.Error()
	}
	_, err := d.Store.Update(eventType, pending.IssueNumber, pending.RunID, payload, func(snapshot *state.Snapshot) error {
		delivery := snapshot.Notifications[pending.ID]
		if delivery == nil || delivery.Status != "pending" {
			return nil
		}
		delivery.Attempts++
		if sendErr == nil {
			delivery.Status = "sent"
			delivery.SentAt = &now
			delivery.NextAttempt = time.Time{}
			delivery.LastError = ""
			return nil
		}
		delivery.LastError = sendErr.Error()
		if delivery.Attempts >= d.Config.Notifications.MaxAttempts {
			delivery.Status = "failed"
			return nil
		}
		delay := d.Config.Notifications.RetryInitial.Duration
		for attempt := 1; attempt < delivery.Attempts && delay < d.Config.Notifications.RetryMax.Duration; attempt++ {
			if delay > d.Config.Notifications.RetryMax.Duration/2 {
				delay = d.Config.Notifications.RetryMax.Duration
			} else {
				delay *= 2
			}
		}
		delivery.NextAttempt = now.Add(delay)
		return nil
	})
	if err != nil {
		return fmt.Errorf("record notification delivery: %w", err)
	}
	if sendErr != nil {
		return sendErr
	}
	return nil
}
