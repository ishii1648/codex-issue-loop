package notify

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ishii1648/codex-issue-loop/internal/config"
	"github.com/ishii1648/codex-issue-loop/internal/state"
)

func TestNtfySendsAuthenticatedMinimalNotification(t *testing.T) {
	var gotPath, gotAuth, gotTitle, gotClick, gotBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		gotPath, gotAuth = request.URL.Path, request.Header.Get("Authorization")
		gotTitle, gotClick = request.Header.Get("Title"), request.Header.Get("Click")
		body, _ := io.ReadAll(request.Body)
		gotBody = string(body)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	adapter := Ntfy{Endpoint: server.URL, Topic: "opaque-topic", Token: "secret-token", Client: server.Client()}
	err := adapter.Send(context.Background(), Message{
		Title: "attention", Body: "owner/repo Issue #3 needs input", ClickURL: "https://github.com/owner/repo/issues/3",
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/opaque-topic" || gotAuth != "Bearer secret-token" || gotTitle != "attention" ||
		gotClick != "https://github.com/owner/repo/issues/3" || gotBody != "owner/repo Issue #3 needs input" {
		t.Fatalf("path=%q auth=%q title=%q click=%q body=%q", gotPath, gotAuth, gotTitle, gotClick, gotBody)
	}
}

type sequenceAdapter struct {
	errors   []error
	messages []Message
}

func (a *sequenceAdapter) Send(_ context.Context, message Message) error {
	a.messages = append(a.messages, message)
	if len(a.errors) == 0 {
		return nil
	}
	err := a.errors[0]
	a.errors = a.errors[1:]
	return err
}

func TestDispatcherDeduplicatesRetriesAndRedactsFailures(t *testing.T) {
	now := time.Date(2026, 8, 16, 0, 0, 0, 0, time.UTC)
	cfg := notificationConfig()
	store := state.Store{Dir: t.TempDir(), RepoID: "repo-id", RepoPath: "/repo", Secrets: []string{"secret-token"}, NotificationsEnabled: true}
	if err := store.Initialize(); err != nil {
		t.Fatal(err)
	}
	request := &state.Request{ID: "req-1", IssueNumber: 3, Status: "pending", Question: "choose channel"}
	for index := 0; index < 2; index++ {
		if _, err := store.Update("input_requested", 3, "run-3", nil, func(snapshot *state.Snapshot) error {
			snapshot.Issues["3"] = &state.Issue{Number: 3, Status: "needs_input"}
			snapshot.PendingRequests[request.ID] = request
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	snapshot, _ := store.Load()
	if len(snapshot.Notifications) != 1 {
		t.Fatalf("duplicate outbox entries: %+v", snapshot.Notifications)
	}
	adapter := &sequenceAdapter{errors: []error{errors.New("provider rejected secret-token"), nil}}
	dispatcher := &Dispatcher{Config: cfg, Store: store, Adapter: adapter, Now: func() time.Time { return now }}
	if err := dispatcher.Dispatch(context.Background()); err == nil {
		t.Fatal("provider failure was not reported to the supervisor logger")
	}
	snapshot, _ = store.Load()
	delivery := snapshot.Notifications["needs_input:req-1"]
	if delivery.Attempts != 1 || delivery.Status != "pending" || delivery.LastError == "" || strings.Contains(delivery.LastError, "secret-token") {
		t.Fatalf("retry state=%+v", delivery)
	}
	now = now.Add(cfg.Notifications.RetryInitial.Duration)
	if err := dispatcher.Dispatch(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := dispatcher.Dispatch(context.Background()); err != nil {
		t.Fatal(err)
	}
	snapshot, _ = store.Load()
	delivery = snapshot.Notifications["needs_input:req-1"]
	if delivery.Attempts != 2 || delivery.Status != "sent" || delivery.SentAt == nil || len(adapter.messages) != 2 {
		t.Fatalf("delivery=%+v messages=%d", delivery, len(adapter.messages))
	}
	if strings.Contains(adapter.messages[0].Body, request.Question) {
		t.Fatalf("details leaked without opt-in: %q", adapter.messages[0].Body)
	}
	events, err := os.ReadFile(filepath.Join(store.Dir, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(events), "secret-token") || !strings.Contains(string(events), "notification_retry_scheduled") || !strings.Contains(string(events), "notification_sent") {
		t.Fatalf("unsafe or incomplete events: %s", events)
	}
}

func TestDispatcherCancelsAnsweredRequestBeforeDelivery(t *testing.T) {
	cfg := notificationConfig()
	store := state.Store{Dir: t.TempDir(), RepoID: "repo-id", RepoPath: "/repo", NotificationsEnabled: true}
	if err := store.Initialize(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update("input_requested", 8, "run-8", nil, func(snapshot *state.Snapshot) error {
		snapshot.PendingRequests["req-8"] = &state.Request{ID: "req-8", IssueNumber: 8, Status: "pending"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update("answer_recorded", 8, "run-8", nil, func(snapshot *state.Snapshot) error {
		snapshot.PendingRequests["req-8"].Status = "answered"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	adapter := &sequenceAdapter{}
	dispatcher := &Dispatcher{Config: cfg, Store: store, Adapter: adapter}
	if err := dispatcher.Dispatch(context.Background()); err != nil {
		t.Fatal(err)
	}
	snapshot, _ := store.Load()
	if snapshot.Notifications["needs_input:req-8"].Status != "canceled" || len(adapter.messages) != 0 {
		t.Fatalf("snapshot=%+v messages=%d", snapshot.Notifications, len(adapter.messages))
	}
}

func TestDisabledNotificationsDoNotAccumulateOutbox(t *testing.T) {
	store := state.Store{Dir: t.TempDir(), RepoID: "repo-id", RepoPath: "/repo"}
	if err := store.Initialize(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update("input_requested", 5, "run-5", nil, func(snapshot *state.Snapshot) error {
		snapshot.PendingRequests["req-5"] = &state.Request{ID: "req-5", IssueNumber: 5, Status: "pending"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Notifications) != 0 {
		t.Fatalf("disabled notification outbox=%+v", snapshot.Notifications)
	}
}

func notificationConfig() config.Config {
	cfg := config.Defaults()
	cfg.GitHub.Repo = "owner/repo"
	cfg.Notifications.Enabled = true
	cfg.Notifications.Topic = "opaque-topic"
	cfg.Notifications.MinInterval = config.Duration{}
	return cfg
}
