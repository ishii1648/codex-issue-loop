package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ishii1648/codex-issue-loop/internal/config"
	"github.com/ishii1648/codex-issue-loop/internal/registry"
)

func testBroker(t *testing.T) (*Broker, []byte) {
	t.Helper()
	t.Setenv("TEST_WEBHOOK_SECRET", "fixture-secret-value")
	cfg := config.Defaults()
	cfg.RepoPath = filepath.Join(t.TempDir(), "repo")
	cfg.GitHub.Repo = "owner/repo"
	cfg.GitHub.RepositoryID = 1234
	cfg.Webhook.Mode = "webhook"
	cfg.Webhook.ListenerAddress = "127.0.0.1:0"
	cfg.Webhook.PublicURLIdentifier = "fixture.example/webhook"
	cfg.Webhook.SecretSource.Env = "TEST_WEBHOOK_SECRET"
	cfg.Webhook.InstallationIDs = []int64{99}
	b := &Broker{
		Root: t.TempDir(), Registrations: []Registration{{
			Entry: registry.Entry{RepoID: "repo-123", RepoPath: cfg.RepoPath, GitHubRepo: cfg.GitHub.Repo}, Config: cfg,
		}},
		Now: func() time.Time { return time.Date(2026, 8, 17, 1, 2, 3, 0, time.UTC) },
	}
	if err := b.initialize(); err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"action":"labeled","repository":{"id":1234,"full_name":"owner/repo"},"installation":{"id":99},"issue":{"number":109}}`)
	return b, body
}

func signedRequest(body []byte, delivery string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/github/webhook", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Delivery", delivery)
	req.Header.Set("X-GitHub-Event", "issues")
	mac := hmac.New(sha256.New, []byte("fixture-secret-value"))
	_, _ = mac.Write(body)
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	return req
}

func TestSignedDeliveryIsDurableDeduplicatedAndRouted(t *testing.T) {
	b, body := testBroker(t)
	recorder := httptest.NewRecorder()
	b.ServeHTTP(recorder, signedRequest(body, "delivery-1"))
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if err := b.route("delivery-1"); err != nil {
		t.Fatal(err)
	}
	mailbox := filepath.Join(b.Root, "repos", "repo-123", "webhook-mailbox", "delivery-1.json")
	data, err := os.ReadFile(mailbox)
	if err != nil {
		t.Fatal(err)
	}
	var delivery Delivery
	if err := json.Unmarshal(data, &delivery); err != nil || delivery.IssueNumber != 109 {
		t.Fatalf("delivery=%+v err=%v", delivery, err)
	}

	duplicate := httptest.NewRecorder()
	b.ServeHTTP(duplicate, signedRequest(body, "delivery-1"))
	if duplicate.Code != http.StatusAccepted || b.status.Accepted != 1 || b.status.Duplicates != 1 {
		t.Fatalf("status=%d metrics=%+v", duplicate.Code, b.status)
	}
}

func TestWebhookRejectsBeforeDurableStateChange(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*http.Request)
		want   int
	}{
		{name: "missing signature", mutate: func(r *http.Request) { r.Header.Del("X-Hub-Signature-256") }, want: http.StatusUnauthorized},
		{name: "unknown event", mutate: func(r *http.Request) { r.Header.Set("X-GitHub-Event", "deployment") }, want: http.StatusBadRequest},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			b, body := testBroker(t)
			req := signedRequest(body, "rejected-1")
			test.mutate(req)
			recorder := httptest.NewRecorder()
			b.ServeHTTP(recorder, req)
			if recorder.Code != test.want {
				t.Fatalf("status=%d want=%d", recorder.Code, test.want)
			}
			entries, _ := os.ReadDir(b.inboxDir())
			if len(entries) != 0 {
				t.Fatalf("rejected request changed inbox: %v", entries)
			}
		})
	}
}

func TestWebhookRejectsOversizePayload(t *testing.T) {
	b, _ := testBroker(t)
	b.Registrations[0].Config.Webhook.MaxBodyBytes = 1024
	body := bytes.Repeat([]byte("x"), 1025)
	recorder := httptest.NewRecorder()
	b.ServeHTTP(recorder, signedRequest(body, "oversize-1"))
	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status=%d", recorder.Code)
	}
}

func TestWebhookRejectsUnknownActionAndRepositoryMismatch(t *testing.T) {
	tests := []struct {
		name string
		body []byte
		want int
	}{
		{name: "unknown action", body: []byte(`{"action":"assigned","repository":{"id":1234,"full_name":"owner/repo"},"installation":{"id":99},"issue":{"number":109}}`), want: http.StatusUnprocessableEntity},
		{name: "repository mismatch", body: []byte(`{"action":"labeled","repository":{"id":1234,"full_name":"other/repo"},"installation":{"id":99},"issue":{"number":109}}`), want: http.StatusForbidden},
		{name: "installation mismatch", body: []byte(`{"action":"labeled","repository":{"id":1234,"full_name":"owner/repo"},"installation":{"id":100},"issue":{"number":109}}`), want: http.StatusForbidden},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			b, _ := testBroker(t)
			recorder := httptest.NewRecorder()
			b.ServeHTTP(recorder, signedRequest(test.body, "rejected-routing"))
			if recorder.Code != test.want {
				t.Fatalf("status=%d want=%d", recorder.Code, test.want)
			}
			entries, _ := os.ReadDir(b.inboxDir())
			if len(entries) != 0 {
				t.Fatalf("rejected request changed inbox: %v", entries)
			}
		})
	}
}

func TestReadMailboxReturnsEveryDeliveryForAtomicAck(t *testing.T) {
	dir := t.TempDir()
	mailbox := MailboxDir(dir)
	if err := os.MkdirAll(mailbox, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"first", "second"} {
		value := Delivery{Version: 1, DeliveryID: id, Event: "issues", IssueNumber: 1, AcceptedAt: time.Now()}
		data, _ := json.Marshal(value)
		if err := os.WriteFile(filepath.Join(mailbox, id+".json"), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	values, err := ReadMailbox(dir)
	if err != nil || len(values) != 2 {
		t.Fatalf("deliveries=%v err=%v", values, err)
	}
	if err := AckMailbox(dir, values); err != nil {
		t.Fatal(err)
	}
	remaining, _ := os.ReadDir(mailbox)
	if len(remaining) != 0 {
		t.Fatalf("remaining=%v", remaining)
	}
}

func TestSharedBrokerSafetySweepPersists304(t *testing.T) {
	b, _ := testBroker(t)
	ghPath := filepath.Join(t.TempDir(), "gh")
	script := "#!/bin/sh\nprintf 'HTTP/2 304 Not Modified\\r\\nETag: W/\"fixture\"\\r\\nX-RateLimit-Remaining: 4999\\r\\n\\r\\n'\n"
	if err := os.WriteFile(ghPath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	registration := b.Registrations[0]
	registration.Entry.Commands = map[string]string{"gh": ghPath}
	if _, err := b.sweepOnce(context.Background(), registration); err != nil {
		t.Fatal(err)
	}
	state, err := LoadSweepState(filepath.Join(b.Root, "repos", registration.Entry.RepoID))
	if err != nil || state.NotModified304 != 1 || state.ETag != `W/"fixture"` || state.LastSuccessful.IsZero() {
		t.Fatalf("state=%+v err=%v", state, err)
	}
	if b.status.NotModified304 != 1 || b.status.LastSuccessfulSafetySweep.IsZero() {
		t.Fatalf("broker status=%+v", b.status)
	}
}
