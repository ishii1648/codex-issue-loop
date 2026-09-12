package supervisor

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"

	gh "github.com/ishii1648/codex-issue-loop/internal/adapter/github"
	"github.com/ishii1648/codex-issue-loop/internal/adapter/state"
	"github.com/ishii1648/codex-issue-loop/internal/adapter/statusapi"
	"github.com/ishii1648/codex-issue-loop/internal/adapter/worker"
	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
	"github.com/ishii1648/codex-issue-loop/internal/platform/config"
)

type startupBlockedGitHub struct {
	gh.Client
	entered chan struct{}
	once    *sync.Once
}

func (g startupBlockedGitHub) Inspect(ctx context.Context, _ config.Config, _ int, _ string) (gh.RemoteState, error) {
	g.once.Do(func() { close(g.entered) })
	<-ctx.Done()
	return gh.RemoteState{}, ctx.Err()
}

func TestStatusAPIBeforeGitHubStartupAndFailureIsolation(t *testing.T) {
	for _, unavailable := range []bool{false, true} {
		t.Run(map[bool]string{false: "available", true: "socket_failure"}[unavailable], func(t *testing.T) {
			loop, _ := testLoop(t, worker.Result{})
			loop.Config.GitHub.Repo = "test/" + state.NewID("repo")
			_, err := loop.Store.Update("fixture", 1, "run_1", nil, func(s *state.Snapshot) error {
				s.Issues["1"] = &state.Issue{Number: 1, Status: issuedomain.StatusBlocked, RunID: "run_1", UpdatedAt: time.Now().UTC()}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			path := statusapi.SocketPath(loop.Config.GitHub.Repo)
			if unavailable {
				if err := os.MkdirAll(path, 0700); err != nil {
					t.Fatal(err)
				}
				defer os.Remove(path)
			}
			entered := make(chan struct{})
			loop.GitHub = startupBlockedGitHub{loop.GitHub, entered, &sync.Once{}}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- loop.Run(ctx) }()
			select {
			case <-entered:
			case err := <-done:
				t.Fatalf("runtime exited before GitHub: %v", err)
			case <-time.After(3 * time.Second):
				t.Fatal("startup did not reach GitHub")
			}
			if !unavailable {
				tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return (&net.Dialer{}).DialContext(ctx, "unix", path)
				}}
				defer tr.CloseIdleConnections()
				client := &http.Client{Transport: tr, Timeout: time.Second}
				res, err := client.Get("http://runtime/v1/status")
				if err != nil {
					t.Fatal(err)
				}
				var response statusapi.Response
				err = json.NewDecoder(res.Body).Decode(&response)
				_ = res.Body.Close()
				if err != nil || res.StatusCode != 200 || response.RuntimePhase != "starting" || response.Supervisor.State != "polling" || response.RuntimeID == "" {
					t.Fatalf("response=%+v status=%d err=%v", response, res.StatusCode, err)
				}
				if lock, err := loop.Store.AcquireSupervisorLock(); err == nil {
					state.ReleaseSupervisorLock(lock)
					t.Error("API outside runtime lock")
				}
			}
			cancel()
			select {
			case err := <-done:
				if err != nil && !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("runtime failed to stop")
			}
			if !unavailable {
				if _, err := os.Lstat(path); !os.IsNotExist(err) {
					t.Fatalf("socket remains: %v", err)
				}
			} else if info, err := os.Lstat(path); err != nil || !info.IsDir() {
				t.Fatal("API removed foreign file")
			}
		})
	}
}
