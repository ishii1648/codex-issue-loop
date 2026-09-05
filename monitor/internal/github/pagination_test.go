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
	events, head, err := (CLI{Path: script}).eventsSince(context.Background(), repo, 150)
	if err != nil {
		t.Fatal(err)
	}
	if head != 300 || len(events) != 1 || events[0].ID != 200 {
		t.Fatalf("events=%+v head=%d", events, head)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 || strings.Contains(string(data), "page=3") {
		t.Fatalf("requests = %q", data)
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
