package store

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/ishii1648/codex-issue-loop/monitor/internal/model"
)

func TestCompletionStorageRestartIsolationAndCorruption(t *testing.T) {
	disk := Store{Root: t.TempDir()}
	at := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)
	if got, err := disk.Completions("owner/repo"); err != nil || got != nil {
		t.Fatalf("old state=%+v error=%v", got, err)
	}
	history := model.CompletionHistory{SchemaVersion: 1, Repository: "owner/repo"}
	if err := history.Apply("done", model.CompletionObservation{At: at, Cursor: 1, Verified: true, Result: "verified"}); err != nil {
		t.Fatal(err)
	}
	if err := history.Apply("done", model.CompletionObservation{At: at.Add(time.Minute), Cursor: 2, FromCursor: 1, Verified: true, Continuous: true, Result: "verified", Events: []model.CompletionLabelEvent{{CompletionEvent: model.CompletionEvent{ID: 2, IssueNumber: 7, At: at}, Label: "done"}}}); err != nil {
		t.Fatal(err)
	}
	if err := disk.CommitCompletions(history); err != nil {
		t.Fatal(err)
	}
	restarted := Store{Root: disk.Root}
	got, err := restarted.Completions(history.Repository)
	if err != nil || !reflect.DeepEqual(*got, history) {
		t.Fatalf("history=%+v error=%v", got, err)
	}
	if other, err := restarted.Completions("owner/other"); err != nil || other != nil {
		t.Fatalf("other=%+v error=%v", other, err)
	}
	path := filepath.Join(disk.repoDir(history.Repository), "completions.json")
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("permissions=%v error=%v", info, err)
	}
	if err := os.WriteFile(path, []byte(`{"schema_version":1,"repository":"owner/repo","epochs":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := disk.Completions(history.Repository); err == nil {
		t.Fatal("incomplete state accepted")
	}
	if err := os.WriteFile(path, []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := disk.Completions(history.Repository); err == nil {
		t.Fatal("truncated state accepted")
	}
}
