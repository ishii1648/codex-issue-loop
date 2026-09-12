package statusapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ishii1648/codex-issue-loop/internal/adapter/state"
	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
)

func testHandler(t *testing.T) *Handler {
	t.Helper()
	store := state.Store{Dir: t.TempDir(), RepoID: "repo-test", RepoPath: "/repo"}
	if err := store.Initialize(); err != nil {
		t.Fatal(err)
	}
	return &Handler{Store: store, Repository: "test/" + state.NewID("repo"), RuntimeID: state.NewID("runtime_"), StartedAt: time.Now().UTC()}
}

func TestHTTPContractAndFreshState(t *testing.T) {
	h := testHandler(t)
	cleanup, err := Start(context.Background(), h, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", SocketPath(h.Repository))
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: time.Second}
	get := func() Response {
		t.Helper()
		res, err := client.Get("http://runtime/v1/status")
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var v Response
		if err := json.NewDecoder(res.Body).Decode(&v); err != nil {
			t.Fatal(err)
		}
		if res.StatusCode != 200 {
			t.Fatalf("status %d", res.StatusCode)
		}
		return v
	}
	first := get()
	if first.RuntimePhase != "starting" || first.RuntimeID != h.RuntimeID || first.ActiveExecution != nil {
		t.Fatalf("%+v", first)
	}
	next, err := h.Store.Update("external_operation", 0, "", nil, func(s *state.Snapshot) error { s.Supervisor.State = state.SupervisorStatePolling; return nil })
	if err != nil {
		t.Fatal(err)
	}
	h.Ready.Store(true)
	second := get()
	if second.Revision != next.StateRevision || second.Revision <= first.Revision || second.RuntimePhase != "serving" {
		t.Fatalf("stale: %+v", second)
	}
	for _, test := range []struct {
		method, path, code string
		status             int
	}{{"POST", "/v1/status", "method_not_allowed", 405}, {"GET", "/v2/status", "unsupported_endpoint", 404}, {"GET", "/v1/events", "unsupported_endpoint", 404}} {
		req, _ := http.NewRequest(test.method, "http://runtime"+test.path, nil)
		res, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(res.Body)
		_ = res.Body.Close()
		if res.StatusCode != test.status || !strings.Contains(string(body), test.code) {
			t.Fatalf("%d %s", res.StatusCode, body)
		}
	}
	cleanup()
	if _, err := os.Lstat(SocketPath(h.Repository)); !os.IsNotExist(err) {
		t.Fatalf("socket remains: %v", err)
	}
	restarted := testHandler(t)
	restarted.Store = h.Store
	restarted.Repository = h.Repository
	stop, err := Start(context.Background(), restarted, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	stop()
	if restarted.RuntimeID == first.RuntimeID {
		t.Fatal("reused startup identity")
	}
}

func TestProjectionFixture(t *testing.T) {
	now := time.Date(2026, 9, 13, 0, 0, 0, 0, time.UTC)
	s := state.Snapshot{RepoID: "repo-fixture", StateRevision: 42, Supervisor: state.Supervisor{State: state.SupervisorStatePolling, UpdatedAt: now}, ActiveExecution: &state.ActiveExecution{IssueNumber: 1, RunID: "run_1", Generation: 2}, Issues: map[string]*state.Issue{}, PendingRequests: map[string]*state.Request{}, QuarantinedIssues: map[string]*state.QuarantineRecord{}}
	for i, status := range []issuedomain.Status{issuedomain.StatusRunning, issuedomain.StatusResumePending, issuedomain.StatusNeedsInput, issuedomain.StatusAwaitingChecks, issuedomain.StatusBlocked, issuedomain.StatusCanceled} {
		n := i + 1
		s.Issues[fmt.Sprint(n)] = &state.Issue{Number: n, Status: status, RunID: fmt.Sprint("run_", n), Generation: 2, UpdatedAt: now, LastError: "SECRET", Answers: []state.AnswerRecord{{Answer: "SECRET"}}}
	}
	s.PendingRequests["req_1"] = &state.Request{IssueNumber: 3, Status: issuedomain.RequestStatusPending, Question: "SECRET"}
	s.Issues["5"].Suspension = &state.Suspension{ReasonCode: "workspace_missing", Reason: "SECRET", Recoverability: issuedomain.RecoverabilityOperator, Status: issuedomain.SuspensionActive}
	s.Issues["6"].Cancellation = &state.Cancellation{Source: "operator"}
	s.QuarantinedIssues["7"] = &state.QuarantineRecord{IssueNumber: 7, ReasonCode: "invalid_lifecycle", Reason: "SECRET", QuarantinedAt: now}
	response := project(s, true)
	response.Repository = "owner/repo"
	response.RuntimeID = "runtime_fixture"
	response.RuntimeStartedAt = now
	response.ObservedAt = now
	response.RuntimePhase = "starting"
	data, err := json.MarshalIndent(response, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	expected, err := os.ReadFile("testdata/status-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(data)) != strings.TrimSpace(string(expected)) {
		t.Fatalf("fixture mismatch:\n%s", data)
	}
	var decoded Response
	if err := json.Unmarshal(expected, &decoded); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(response, decoded) || strings.Contains(string(data), "SECRET") {
		t.Fatal("projection changed or leaked private data")
	}
}

type stalledWriter struct {
	entered, release chan struct{}
	header           http.Header
}

func (w *stalledWriter) Header() http.Header { return w.header }
func (w *stalledWriter) WriteHeader(int)     {}
func (w *stalledWriter) Write(p []byte) (int, error) {
	close(w.entered)
	<-w.release
	return len(p), nil
}
func TestSlowResponseReleasesStateLock(t *testing.T) {
	h := testHandler(t)
	w := &stalledWriter{make(chan struct{}), make(chan struct{}), http.Header{}}
	done := make(chan struct{})
	go func() { h.ServeHTTP(w, httptest.NewRequest("GET", "/v1/status", nil)); close(done) }()
	<-w.entered
	updated := make(chan error, 1)
	go func() {
		_, err := h.Store.Update("while_sending", 0, "", nil, func(*state.Snapshot) error { return nil })
		updated <- err
	}()
	select {
	case err := <-updated:
		if err != nil {
			t.Error(err)
		}
	case <-time.After(time.Second):
		t.Error("slow response blocks writer")
	}
	close(w.release)
	<-done
}

func TestSocketOwnership(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "status-test-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "status.sock")
	if len(SocketPath(strings.Repeat("long", 1000))) >= 104 {
		t.Fatal("socket path exceeds macOS limit")
	}
	listener, info, err := listen(path)
	if err != nil {
		t.Fatal(err)
	}
	current, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if current.Mode().Perm() != 0600 {
		t.Fatalf("mode=%v", info.Mode())
	}
	if other, _, err := listen(path); err == nil {
		_ = other.Close()
		t.Fatal("replaced live listener")
	}
	_ = listener.Close()
	listener, info, err = listen(path)
	if err != nil {
		t.Fatal("stale socket:", err)
	}
	_ = listener.Close()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("replacement"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := removeOwned(path, info); err == nil {
		t.Fatal("removed replacement")
	}
	if _, _, err := listen(path); err == nil {
		t.Fatal("accepted regular file")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "target"), path); err != nil {
		t.Fatal(err)
	}
	if _, _, err := listen(path); err == nil {
		t.Fatal("accepted symlink")
	}
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if _, _, err := listen(path); err == nil {
		t.Fatal("accepted public directory")
	}
}

func TestHTTPUnavailableDoesNotExposeState(t *testing.T) {
	for _, test := range []struct{ name, body, code string }{
		{"snapshot", "{SECRET", "invalid_state"},
		{"snapshot", `{"version":999,"issues":"SECRET"}`, "unsupported_snapshot_version"},
		{"transaction", "SECRET", "state_unconfirmed"},
	} {
		t.Run(test.code, func(t *testing.T) {
			h := testHandler(t)
			path := h.Store.StatePath()
			if test.name == "transaction" {
				path = h.Store.TransactionPath()
			}
			if err := os.WriteFile(path, []byte(test.body), 0600); err != nil {
				t.Fatal(err)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest("GET", "/v1/status", nil))
			if w.Code != 503 || w.Body.String() != fmt.Sprintf("{\"api_version\":1,\"error_code\":%q}\n", test.code) {
				t.Fatalf("%d %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestSocketClientReadTimeoutAndContextCleanup(t *testing.T) {
	h := testHandler(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop, err := Start(ctx, h, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	conn, err := net.DialTimeout("unix", SocketPath(h.Repository), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(4 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(conn, "GET /v1/status HTTP/1.1\r\n"); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("server did not close slow headers: %v, %s", err, data)
	}
	cancel()
	deadline := time.Now().Add(time.Second)
	for {
		if _, err := os.Lstat(SocketPath(h.Repository)); os.IsNotExist(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("context cancellation left socket")
		}
		time.Sleep(time.Millisecond)
	}
}
