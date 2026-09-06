package github

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ishii1648/codex-issue-loop/monitor/internal/config"
)

func TestObservationFailureWaitsForNextPoll(t *testing.T) {
	for _, tc := range []struct {
		mode  string
		calls int
	}{
		{"closed", 1}, {"duplicate", 1}, {"snapshot", 2},
		{"head", 2}, {"persistent-snapshot", 2}, {"persistent", 1}, {"http", 1},
		{"malformed", 1}, {"invalid", 1},
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
			if err == nil {
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
			if obs.CurrentVerified || obs.Cursor != 0 {
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
