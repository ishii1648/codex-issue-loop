package github

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ishii1648/codex-issue-loop/monitor/internal/config"
)

func TestEventsSinceStopsOnPageContainingCursor(t *testing.T) {
	dir := t.TempDir()
	page1 := make([]rawEvent, 100)
	for index := range page1 {
		page1[index].ID = int64(300 - index)
	}
	page2 := []rawEvent{{ID: 200}, {ID: 150}}
	page2[0].Event = "labeled"
	page2[0].CreatedAt = time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	page2[0].Label.Name = "ready"
	page2[0].Issue.Number = 7
	writeEventPage(t, filepath.Join(dir, "page1.json"), page1)
	writeEventPage(t, filepath.Join(dir, "page2.json"), page2)
	logPath := filepath.Join(dir, "args")
	script := filepath.Join(dir, "gh")
	body := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"" + logPath + "\"\ncase \"$*\" in\n  *\\&page=1) cat \"" + filepath.Join(dir, "page1.json") + "\" ;;\n  *\\&page=2) cat \"" + filepath.Join(dir, "page2.json") + "\" ;;\n  *) exit 9 ;;\nesac\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	repo := config.Repository{Name: "owner/repo", ReadyLabels: []string{"ready"}, RunningLabel: "running", TerminalLabels: []string{"done"}}
	for _, test := range []struct {
		name       string
		cursor     int64
		wantPages  int
		wantEvents int
	}{
		{name: "first page", cursor: 250, wantPages: 1},
		{name: "second page", cursor: 150, wantPages: 2, wantEvents: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := os.WriteFile(logPath, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			events, head, err := (CLI{Path: script}).eventsSince(context.Background(), repo, test.cursor)
			if err != nil {
				t.Fatal(err)
			}
			if head != 300 || len(events) != test.wantEvents {
				t.Fatalf("events=%+v head=%d", events, head)
			}
			if len(events) > 0 && events[0].ID != 200 {
				t.Fatalf("events=%+v", events)
			}
			data, err := os.ReadFile(logPath)
			if err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(strings.TrimSpace(string(data)), "\n")
			if len(lines) != test.wantPages {
				t.Fatalf("requests = %q", data)
			}
		})
	}
}

func TestEventsSinceRejectsMissingCursor(t *testing.T) {
	dir := t.TempDir()
	writeEventPage(t, filepath.Join(dir, "page1.json"), []rawEvent{{ID: 300}})
	script := filepath.Join(dir, "gh")
	body := "#!/bin/sh\ncat \"" + filepath.Join(dir, "page1.json") + "\"\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	repo := config.Repository{Name: "owner/repo"}
	if _, _, err := (CLI{Path: script}).eventsSince(context.Background(), repo, 150); err == nil {
		t.Fatal("missing cursor was accepted")
	}
}

func writeEventPage(t *testing.T, path string, events []rawEvent) {
	t.Helper()
	data, err := json.Marshal(events)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestEventsSinceBoundsRequestsWhenCursorIsMissing(t *testing.T) {
	dir := t.TempDir()
	events := make([]rawEvent, 100)
	for index := range events {
		events[index].ID = int64(300 - index)
	}
	pagePath := filepath.Join(dir, "page.json")
	writeEventPage(t, pagePath, events)
	logPath := filepath.Join(dir, "args")
	script := filepath.Join(dir, "gh")
	body := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"" + logPath + "\"\ncat \"" + pagePath + "\"\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, cursor := range []int64{0, 150} {
		if err := os.WriteFile(logPath, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		batch, head, err := (CLI{Path: script}).eventsSince(context.Background(), config.Repository{Name: "owner/repo"}, cursor)
		if err == nil || batch != nil || head != cursor {
			t.Fatalf("incomplete history: batch=%+v head=%d err=%v", batch, head, err)
		}
		data, err := os.ReadFile(logPath)
		if err != nil {
			t.Fatal(err)
		}
		if lines := strings.Split(strings.TrimSpace(string(data)), "\n"); len(lines) != maxEventPages {
			t.Fatalf("requests = %q", data)
		}
	}
}
