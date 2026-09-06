package github

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ishii1648/codex-issue-loop/monitor/internal/config"
	"github.com/ishii1648/codex-issue-loop/monitor/internal/model"
)

func TestTransientObservationRetry(t *testing.T) {
	for _, tc := range []struct {
		mode     string
		calls    int
		succeeds bool
	}{
		{"closed", 3, true}, {"duplicate", 3, true}, {"snapshot", 4, true},
		{"head", 4, true}, {"persistent-snapshot", 6, false}, {"persistent", 3, false}, {"http", 1, false},
		{"malformed", 1, false}, {"invalid", 1, false},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			script := filepath.Join(dir, "gh")
			body := fmt.Sprintf(`#!/bin/sh
cd '%s' || exit 9
printf '%%s\n' "$*" >> calls
case "$*" in
 *issues/events*)
 n=0; [ ! -f head ] || n=$(cat head); n=$((n+1)); echo "$n" > head
 if [ '%s' = head ] && [ "$n" = 2 ]; then printf '%%s' '[{"id":2}]'; else printf '%%s' '[{"id":1}]'; fi ;;
 *issues\?*)
 n=0; [ ! -f issues ] || n=$(cat issues); n=$((n+1)); echo "$n" > issues
 case '%s' in
  http) exit 7 ;;
  malformed) printf '{'; exit 0 ;;
  invalid) printf '%%s' '[[{"number":0}]]'; exit 0 ;;
  persistent) printf '%%s' '[[{"number":1,"state":"closed"}]]'; exit 0 ;;
 esac
 if [ "$n" = 1 ] || { [ '%s' = persistent-snapshot ] && [ "$((n%%2))" = 1 ]; }; then
 case '%s' in
  closed) printf '%%s' '[[{"number":1,"state":"closed"}]]'; exit 0 ;;
  duplicate) printf '%%s' '[[{"number":1},{"number":1}]]'; exit 0 ;;
  snapshot|persistent-snapshot) printf '%%s' '[[{"number":1}]]'; exit 0 ;;
 esac
 fi
 printf '%%s' '[[]]' ;;
 *) exit 9 ;;
esac
`, dir, tc.mode, tc.mode, tc.mode, tc.mode)
			if err := os.WriteFile(script, []byte(body), 0700); err != nil {
				t.Fatal(err)
			}
			repo := config.Repository{Name: "owner/repo"}
			at := time.Date(2026, 9, 6, 15, 0, 0, 0, time.UTC)
			obs, err := (CLI{Path: script}).Observe(context.Background(), repo, 1, true, at)
			if (err == nil) != tc.succeeds {
				t.Fatalf("observation=%+v error=%v", obs, err)
			}
			data, readErr := os.ReadFile(filepath.Join(dir, "issues"))
			if readErr != nil {
				t.Fatal(readErr)
			}
			count, _ := strconv.Atoi(strings.TrimSpace(string(data)))
			if count != tc.calls {
				t.Fatalf("open issue reads=%d want=%d", count, tc.calls)
			}
			if tc.succeeds {
				if !obs.CurrentVerified || obs.Cursor != 1 || !obs.ObservedAt.Equal(at) {
					t.Fatalf("observation=%+v", obs)
				}
				previous, _, applyErr := model.Apply(nil, model.Observation{Repository: repo.Name, ObservedAt: at.Add(-time.Minute), Cursor: 1, CursorInitialized: true})
				if applyErr != nil {
					t.Fatal(applyErr)
				}
				next, closed, applyErr := model.Apply(&previous, obs)
				if applyErr != nil || next.Current.Status != model.Idle || len(closed) != 0 {
					t.Fatalf("state=%+v intervals=%+v error=%v", next, closed, applyErr)
				}
			} else if obs.CurrentVerified || obs.Cursor != 0 {
				t.Fatalf("failed attempt leaked observation: %+v", obs)
			}
			calls, _ := os.ReadFile(filepath.Join(dir, "calls"))
			for _, line := range strings.Split(strings.TrimSpace(string(calls)), "\n") {
				if !strings.HasPrefix(line, "api --method GET ") {
					t.Fatalf("non-read-only call: %s", line)
				}
			}
		})
	}
}

func TestObservationRetryCancellation(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "gh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ncase \"$*\" in\n *issues/events*) printf '%s' '[{\"id\":1}]' ;;\n *) printf '%s' '[[{\"number\":1,\"state\":\"closed\"}]]' ;;\nesac\n"), 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := (CLI{Path: script}).Observe(ctx, config.Repository{Name: "owner/repo"}, 1, true, start)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error=%v", err)
	}
	if time.Since(start) >= time.Second {
		t.Fatal("retry ignored cancellation")
	}
}
