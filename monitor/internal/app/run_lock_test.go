package app

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ishii1648/codex-issue-loop/monitor/internal/config"
	"github.com/ishii1648/codex-issue-loop/monitor/internal/model"
	"github.com/ishii1648/codex-issue-loop/monitor/internal/store"
)

type lockTestObserver func(context.Context, config.Repository, int64, bool, time.Time) (model.Observation, error)

func (f lockTestObserver) Observe(ctx context.Context, repo config.Repository, cursor int64, initialized bool, at time.Time) (model.Observation, error) {
	return f(ctx, repo, cursor, initialized, at)
}

func TestRunExcludesConcurrentWriters(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "monitor.yaml")
	if err := os.WriteFile(configPath, []byte("version: 1\nstate_dir: "+root+"\npoll_interval: 1h\nrepositories:\n  - name: owner/repo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	observed := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first := App{Out: io.Discard, Err: io.Discard, Observer: lockTestObserver(func(ctx context.Context, repo config.Repository, _ int64, _ bool, at time.Time) (model.Observation, error) {
		close(observed)
		<-ctx.Done()
		return model.Observation{Repository: repo.Name, ObservedAt: at, CursorInitialized: true}, nil
	})}
	done := make(chan int, 1)
	go func() { done <- first.Run(ctx, []string{"run", "--config", configPath}) }()
	select {
	case <-observed:
	case <-time.After(5 * time.Second):
		t.Fatal("first run did not observe")
	}
	second := App{Out: io.Discard, Observer: lockTestObserver(func(_ context.Context, repo config.Repository, _ int64, _ bool, at time.Time) (model.Observation, error) {
		return model.Observation{Repository: repo.Name, ObservedAt: at, CursorInitialized: true}, nil
	})}
	for _, once := range []bool{false, true} {
		var stderr bytes.Buffer
		second.Err = &stderr
		args := []string{"run", "--config", configPath, "--json"}
		if once {
			args = append(args, "--once")
		}
		attempt, stop := context.WithTimeout(context.Background(), time.Second)
		code := second.Run(attempt, args)
		stop()
		if code != 1 || !strings.Contains(stderr.String(), "another monitor is already running") {
			t.Fatalf("once=%v: code=%d stderr=%s", once, code, &stderr)
		}
	}
	storage := store.Store{Root: root}
	if snapshot, err := storage.Load("owner/repo"); err != nil || snapshot != nil {
		t.Fatalf("competing run committed: %+v, %v", snapshot, err)
	}
	if history, err := storage.History("owner/repo"); err != nil || len(history) != 0 {
		t.Fatalf("competing run wrote history: %+v, %v", history, err)
	}
	if code := second.Run(context.Background(), []string{"status", "--config", configPath, "--json"}); code != 0 {
		t.Fatalf("read command exit=%d", code)
	}
	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("first run exit=%d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("first run did not stop")
	}
	if code := second.Run(context.Background(), []string{"run", "--config", configPath, "--once"}); code != 0 {
		t.Fatalf("run after release exit=%d", code)
	}
}
