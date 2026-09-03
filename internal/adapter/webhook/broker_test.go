package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	gh "github.com/ishii1648/codex-issue-loop/internal/adapter/github"
	"github.com/ishii1648/codex-issue-loop/internal/platform/config"
	"github.com/ishii1648/codex-issue-loop/internal/platform/registry"
)

type fakePagedSweepClient struct {
	pages map[int]gh.ConditionalQueueResult
	calls []int
}

type fakeSweepClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeSweepTimer
}

type fakeSweepTimer struct {
	clock    *fakeSweepClock
	deadline time.Time
	channel  chan time.Time
	fired    bool
	stopped  bool
}

func (c *fakeSweepClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeSweepClock) NewTimer(delay time.Duration) SweepTimer {
	c.mu.Lock()
	defer c.mu.Unlock()
	timer := &fakeSweepTimer{clock: c, deadline: c.now.Add(delay), channel: make(chan time.Time, 1)}
	c.timers = append(c.timers, timer)
	return timer
}

func (c *fakeSweepClock) Advance(duration time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(duration)
	for _, timer := range c.timers {
		if !timer.fired && !timer.stopped && !timer.deadline.After(c.now) {
			timer.fired = true
			timer.channel <- c.now
		}
	}
	c.mu.Unlock()
}

func (c *fakeSweepClock) timerCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.timers)
}

func (t *fakeSweepTimer) C() <-chan time.Time { return t.channel }

func (t *fakeSweepTimer) Stop() bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	wasActive := !t.fired && !t.stopped
	t.stopped = true
	return wasActive
}

type repositoryCountingSweepClient struct {
	mu    sync.Mutex
	calls map[string]int
}

func (c *repositoryCountingSweepClient) ListReadyConditionalPage(_ context.Context, cfg config.Config, _ int, _, _ string) (gh.ConditionalQueueResult, error) {
	c.mu.Lock()
	c.calls[cfg.GitHub.Repo]++
	c.mu.Unlock()
	return gh.ConditionalQueueResult{StatusCode: http.StatusNotModified, NotModified: true, ETag: `"stable"`, RateRemaining: "4999"}, nil
}

func (c *repositoryCountingSweepClient) count(repo string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls[repo]
}

func (f *fakePagedSweepClient) ListReadyConditionalPage(_ context.Context, _ config.Config, page int, _, _ string) (gh.ConditionalQueueResult, error) {
	f.calls = append(f.calls, page)
	return f.pages[page], nil
}

func sweepIssues(first, last int) []gh.Issue {
	issues := make([]gh.Issue, 0, last-first+1)
	for number := first; number <= last; number++ {
		issues = append(issues, gh.Issue{Number: number, State: "open", Labels: []string{"codex-loop:ready"}})
	}
	return issues
}

func sweepPage(issues []gh.Issue, etag string) gh.ConditionalQueueResult {
	return gh.ConditionalQueueResult{Issues: issues, ItemCount: len(issues), StatusCode: http.StatusOK, ETag: etag, RateRemaining: "4999"}
}

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

func TestSafetySweepCoalescesRepeatedReadyCollectionIntent(t *testing.T) {
	b, _ := testBroker(t)
	registration := b.Registrations[0]
	repoDir := filepath.Join(b.Root, "repos", registration.Entry.RepoID)
	client := &fakePagedSweepClient{pages: map[int]gh.ConditionalQueueResult{
		1: sweepPage(sweepIssues(1, 1), `"ready"`),
	}}
	b.SweepClient = func(Registration) gh.PagedConditionalQueueClient { return client }
	if _, err := b.sweepOnce(context.Background(), registration); err != nil {
		t.Fatal(err)
	}
	if _, err := b.sweepOnce(context.Background(), registration); err != nil {
		t.Fatal(err)
	}
	deliveries, err := ReadMailbox(repoDir)
	if err != nil || len(deliveries) != 1 || deliveries[0].DeliveryID != "sweep-1-reconciled" {
		t.Fatalf("deliveries=%v err=%v", deliveries, err)
	}
}

func TestMailboxRedeliveryRetainsOriginalAcceptanceTime(t *testing.T) {
	dir := t.TempDir()
	first := Delivery{Version: 1, DeliveryID: "same", RepoID: "repo", Event: "issues", Action: "reconciled", IssueNumber: 42, AcceptedAt: time.Unix(1, 0).UTC()}
	second := first
	second.AcceptedAt = time.Unix(2, 0).UTC()
	if err := EnqueueMailbox(dir, first); err != nil {
		t.Fatal(err)
	}
	if err := EnqueueMailbox(dir, second); err != nil {
		t.Fatal(err)
	}
	deliveries, err := ReadMailbox(dir)
	if err != nil || len(deliveries) != 1 || !deliveries[0].AcceptedAt.Equal(first.AcceptedAt) {
		t.Fatalf("deliveries=%+v err=%v", deliveries, err)
	}
}

func TestTwoRepositorySafetySweepsStayWithinConfiguredRateAcrossFakeHour(t *testing.T) {
	base := time.Date(2026, 8, 17, 0, 0, 0, 0, time.UTC)
	clock := &fakeSweepClock{now: base}
	client := &repositoryCountingSweepClient{calls: map[string]int{}}
	registrations := make([]Registration, 0, 2)
	for index, interval := range []time.Duration{10 * time.Minute, 15 * time.Minute} {
		cfg := config.Defaults()
		cfg.RepoPath = filepath.Join(t.TempDir(), fmt.Sprintf("repo-%d", index+1))
		cfg.GitHub.Repo = fmt.Sprintf("owner/repo-%d", index+1)
		cfg.GitHub.RepositoryID = int64(100 + index)
		cfg.Webhook.Mode = "webhook"
		cfg.Webhook.ListenerAddress = "127.0.0.1:0"
		cfg.Webhook.PublicURLIdentifier = "fixture.example/webhook"
		cfg.Webhook.SecretSource.Env = "TEST_WEBHOOK_SECRET"
		cfg.Webhook.SafetySweepInterval.Duration = interval
		cfg.Webhook.SafetySweepJitter = 0
		registrations = append(registrations, Registration{
			Entry: registry.Entry{RepoID: fmt.Sprintf("repo-%d", index+1), RepoPath: cfg.RepoPath, GitHubRepo: cfg.GitHub.Repo}, Config: cfg,
		})
	}
	t.Setenv("TEST_WEBHOOK_SECRET", "fixture-secret-value")
	b := &Broker{
		Root: t.TempDir(), Registrations: registrations, Now: clock.Now, SweepTimers: clock,
		SweepClient: func(Registration) gh.PagedConditionalQueueClient { return client },
	}
	if err := b.initialize(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	b.startSafetySweeps(ctx)
	defer cancel()
	deadline := time.Now().Add(2 * time.Second)
	for clock.timerCount() < len(registrations) {
		if time.Now().After(deadline) {
			t.Fatal("safety sweep timers were not registered")
		}
		time.Sleep(time.Millisecond)
	}
	for minute := 1; minute <= 60; minute++ {
		clock.Advance(time.Minute)
		wantFirst := 1 + (minute-1)/10
		wantSecond := 1 + (minute-1)/15
		deadline := time.Now().Add(2 * time.Second)
		for client.count("owner/repo-1") < wantFirst || client.count("owner/repo-2") < wantSecond {
			if time.Now().After(deadline) {
				t.Fatalf("minute=%d calls=%v,%v want_at_least=%d,%d", minute, client.count("owner/repo-1"), client.count("owner/repo-2"), wantFirst, wantSecond)
			}
			time.Sleep(time.Millisecond)
		}
		wantTimers := len(registrations) + client.count("owner/repo-1") + client.count("owner/repo-2")
		for clock.timerCount() < wantTimers {
			if time.Now().After(deadline) {
				t.Fatalf("minute=%d next sweep timers were not registered: got=%d want=%d", minute, clock.timerCount(), wantTimers)
			}
			time.Sleep(time.Millisecond)
		}
	}
	first := client.count("owner/repo-1")
	second := client.count("owner/repo-2")
	if first > 1+60/10 || second > 1+60/15 {
		t.Fatalf("sweep rate exceeded: repo-1=%d repo-2=%d", first, second)
	}
}

func TestSharedBrokerSafetySweepPaginatesAndWarmsWith304(t *testing.T) {
	b, _ := testBroker(t)
	dir := t.TempDir()
	for page, count := range map[int]int{1: 100, 2: 100, 3: 50} {
		items := make([]map[string]any, 0, count)
		for index := 0; index < count; index++ {
			number := (page-1)*100 + index + 1
			items = append(items, map[string]any{
				"number": number, "title": fmt.Sprintf("Issue %d", number), "body": "body",
				"html_url":   fmt.Sprintf("https://example.test/issues/%d", number),
				"created_at": "2026-08-17T00:00:00Z", "state": "open",
				"labels": []map[string]string{{"name": b.Registrations[0].Config.GitHub.ReadyLabels[0]}},
			})
		}
		data, err := json.Marshal(items)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, fmt.Sprintf("page-%d.json", page))
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv(fmt.Sprintf("SWEEP_PAGE_%d", page), path)
	}
	ghPath := filepath.Join(dir, "gh")
	script := `#!/bin/sh
page=1
case "$*" in *page=2*) page=2;; *page=3*) page=3;; esac
eval body=\$SWEEP_PAGE_$page
if echo "$*" | /usr/bin/grep -q If-None-Match; then
  printf 'HTTP/2 304 Not Modified\r\nETag: "page-%s"\r\nX-RateLimit-Remaining: 4999\r\n\r\n' "$page"
  exit 0
fi
printf 'HTTP/2 200 OK\r\nETag: "page-%s"\r\nX-RateLimit-Remaining: 4999\r\n\r\n' "$page"
/bin/cat "$body"
`
	if err := os.WriteFile(ghPath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	registration := b.Registrations[0]
	registration.Entry.Commands = map[string]string{"gh": ghPath}
	if _, err := b.sweepOnce(context.Background(), registration); err != nil {
		t.Fatal(err)
	}
	mailbox := MailboxDir(filepath.Join(b.Root, "repos", registration.Entry.RepoID))
	entries, err := os.ReadDir(mailbox)
	if err != nil || len(entries) != 250 {
		t.Fatalf("mailbox entries=%d err=%v", len(entries), err)
	}
	if _, err := b.sweepOnce(context.Background(), registration); err != nil {
		t.Fatal(err)
	}
	entries, _ = os.ReadDir(mailbox)
	state, err := LoadSweepState(filepath.Join(b.Root, "repos", registration.Entry.RepoID))
	if err != nil || len(entries) != 250 || len(state.Pages) != 3 || state.REST200 != 3 || state.NotModified304 != 3 {
		t.Fatalf("entries=%d state=%+v err=%v", len(entries), state, err)
	}
}

func TestSafetySweepDiffsComposedRepositoryCollectionAcrossPageMovement(t *testing.T) {
	b, _ := testBroker(t)
	registration := b.Registrations[0]
	repoDir := filepath.Join(b.Root, "repos", registration.Entry.RepoID)
	previous := SweepState{Version: InboxVersion, Pages: map[int]SweepPageState{
		1: {ETag: `"old-1"`, ItemCount: 100, Issues: sweepIssues(1, 100)},
		2: {ETag: `"old-2"`, ItemCount: 50, Issues: sweepIssues(101, 150)},
	}}
	if err := SaveSweepState(repoDir, previous); err != nil {
		t.Fatal(err)
	}
	page1 := append(sweepIssues(1, 99), sweepIssues(101, 101)...)
	page2 := append(sweepIssues(100, 100), sweepIssues(102, 150)...)
	client := &fakePagedSweepClient{pages: map[int]gh.ConditionalQueueResult{
		1: sweepPage(page1, `"new-1"`),
		2: sweepPage(page2, `"new-2"`),
	}}
	b.SweepClient = func(Registration) gh.PagedConditionalQueueClient { return client }
	if _, err := b.sweepOnce(context.Background(), registration); err != nil {
		t.Fatal(err)
	}
	deliveries, err := ReadMailbox(repoDir)
	if err != nil || len(deliveries) != 150 {
		t.Fatalf("deliveries=%d err=%v", len(deliveries), err)
	}
	for _, delivery := range deliveries {
		if delivery.Action == "collection_exited" {
			t.Fatalf("Issue #%d was falsely treated as exiting after a page move", delivery.IssueNumber)
		}
	}
}

func TestSafetySweepComposes304And200BeforeShrinkingPages(t *testing.T) {
	b, _ := testBroker(t)
	registration := b.Registrations[0]
	repoDir := filepath.Join(b.Root, "repos", registration.Entry.RepoID)
	previous := SweepState{Version: InboxVersion, Pages: map[int]SweepPageState{
		1: {ETag: `"page-1"`, ItemCount: 100, Issues: sweepIssues(1, 100)},
		2: {ETag: `"page-2"`, ItemCount: 100, Issues: sweepIssues(101, 200)},
		3: {ETag: `"page-3"`, ItemCount: 50, Issues: sweepIssues(201, 250)},
	}}
	if err := SaveSweepState(repoDir, previous); err != nil {
		t.Fatal(err)
	}
	client := &fakePagedSweepClient{pages: map[int]gh.ConditionalQueueResult{
		1: {StatusCode: http.StatusNotModified, NotModified: true, ETag: `"page-1"`, RateRemaining: "4999"},
		2: sweepPage(sweepIssues(101, 150), `"page-2-short"`),
	}}
	b.SweepClient = func(Registration) gh.PagedConditionalQueueClient { return client }
	if _, err := b.sweepOnce(context.Background(), registration); err != nil {
		t.Fatal(err)
	}
	deliveries, err := ReadMailbox(repoDir)
	if err != nil {
		t.Fatal(err)
	}
	exited := make([]int, 0)
	for _, delivery := range deliveries {
		if delivery.Action == "collection_exited" {
			exited = append(exited, delivery.IssueNumber)
		}
	}
	sort.Ints(exited)
	if len(exited) != 100 || exited[0] != 151 || exited[len(exited)-1] != 250 {
		t.Fatalf("exited=%v", exited)
	}
	state, err := LoadSweepState(repoDir)
	if err != nil || len(state.Pages) != 2 || len(composeSweepIssues(state.Pages)) != 150 || state.NotModified304 != 1 || state.REST200 != 1 {
		t.Fatalf("state=%+v err=%v", state, err)
	}
}

func TestSafetySweepDoesNotAdvanceCacheBeforeExitIntentIsDurable(t *testing.T) {
	b, _ := testBroker(t)
	registration := b.Registrations[0]
	repoDir := filepath.Join(b.Root, "repos", registration.Entry.RepoID)
	previous := SweepState{Version: InboxVersion, Pages: map[int]SweepPageState{
		1: {ETag: `"old"`, ItemCount: 1, Issues: sweepIssues(1, 1)},
	}}
	if err := SaveSweepState(repoDir, previous); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(MailboxDir(repoDir), []byte("not-a-directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	client := &fakePagedSweepClient{pages: map[int]gh.ConditionalQueueResult{1: sweepPage(nil, `"empty"`)}}
	b.SweepClient = func(Registration) gh.PagedConditionalQueueClient { return client }
	if _, err := b.sweepOnce(context.Background(), registration); err == nil {
		t.Fatal("sweep succeeded without a durable collection-exit intent")
	}
	state, err := LoadSweepState(repoDir)
	if err != nil || len(composeSweepIssues(state.Pages)) != 1 || state.Pages[1].ETag != `"old"` {
		t.Fatalf("cache advanced before mailbox durability: state=%+v err=%v", state, err)
	}
}

func TestRouteMovesPendingDeliveryToReceiptAndRecoversPartialRoute(t *testing.T) {
	b, body := testBroker(t)
	recorder := httptest.NewRecorder()
	b.ServeHTTP(recorder, signedRequest(body, "crash-route"))
	data, err := os.ReadFile(b.inboxPath("crash-route"))
	if err != nil {
		t.Fatal(err)
	}
	var delivery Delivery
	if err := json.Unmarshal(data, &delivery); err != nil {
		t.Fatal(err)
	}
	// Simulate a crash after the durable mailbox write but before receipt.
	repoDir := filepath.Join(b.Root, "repos", delivery.RepoID)
	if err := EnqueueMailbox(repoDir, delivery); err != nil {
		t.Fatal(err)
	}
	if err := b.route(delivery.DeliveryID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(b.inboxPath(delivery.DeliveryID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pending delivery remains: %v", err)
	}
	if _, err := os.Stat(b.receiptPath(delivery.DeliveryID)); err != nil {
		t.Fatalf("receipt missing: %v", err)
	}
	// Simulate a crash after receipt creation but before pending removal.
	if err := os.WriteFile(b.inboxPath(delivery.DeliveryID), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := b.route(delivery.DeliveryID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(b.inboxPath(delivery.DeliveryID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("replayed pending delivery remains: %v", err)
	}
}

func TestReceiptCompactionAppliesRetentionWithoutReadingReceiptBodies(t *testing.T) {
	b, _ := testBroker(t)
	oldPath := b.receiptPath("old")
	recentPath := b.receiptPath("recent")
	for _, path := range []string{oldPath, recentPath} {
		if err := os.WriteFile(path, []byte("not-json"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	old := b.Now().Add(-receiptRetention - time.Hour)
	if err := os.Chtimes(oldPath, old, old); err != nil {
		t.Fatal(err)
	}
	if err := b.compactReceipts(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(oldPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expired receipt remains: %v", err)
	}
	if _, err := os.Stat(recentPath); err != nil {
		t.Fatalf("recent receipt removed: %v", err)
	}
}

type logicalDirEntry struct{ name string }

func (e logicalDirEntry) Name() string      { return e.name }
func (e logicalDirEntry) IsDir() bool       { return false }
func (e logicalDirEntry) Type() os.FileMode { return 0 }
func (e logicalDirEntry) Info() (os.FileInfo, error) {
	return nil, errors.New("body metadata must not be read")
}

func TestReplayWorkIsProportionalToPendingWithOneHundredThousandLogicalReceipts(t *testing.T) {
	b, _ := testBroker(t)
	b.work = make(chan string, 10)
	for _, name := range []string{"invalid.json", "unreadable.json"} {
		if err := os.WriteFile(filepath.Join(b.receiptDir(), name), []byte("not-json"), 0o000); err != nil {
			t.Fatal(err)
		}
	}
	receiptReads := 0
	b.ReadDir = func(path string) ([]os.DirEntry, error) {
		switch path {
		case b.inboxDir():
			return []os.DirEntry{logicalDirEntry{"pending-1.json"}, logicalDirEntry{"ignored.tmp"}, logicalDirEntry{"pending-2.json"}}, nil
		case b.receiptDir():
			receiptReads++
			entries := make([]os.DirEntry, 100000)
			for index := range entries {
				entries[index] = logicalDirEntry{fmt.Sprintf("receipt-%06d.json", index)}
			}
			return entries, nil
		default:
			return nil, fmt.Errorf("unexpected directory read: %s", path)
		}
	}
	if err := b.replayOnce(); err != nil {
		t.Fatal(err)
	}
	if receiptReads != 0 || len(b.work) != 2 {
		t.Fatalf("receipt_reads=%d queued_pending=%d", receiptReads, len(b.work))
	}
	queued := []string{<-b.work, <-b.work}
	if strings.Join(queued, ",") != "pending-1,pending-2" {
		t.Fatalf("queued=%v", queued)
	}
}
