package retention

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWriterRotatesCompressesAndRetainsGenerations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.log")
	policy := Policy{MaxBytes: 8, MaxAge: time.Hour, Keep: 2}
	w, err := OpenWriter(path, policy)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"first\n", "second\n", "third\n", "fourth\n"} {
		if _, err := w.Write([]byte(value)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	archives, err := archives(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(archives) != 2 {
		t.Fatalf("archives=%v", archives)
	}
	var history bytes.Buffer
	if err := WriteHistory(&history, path); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(history.String(), "second") || !strings.Contains(history.String(), "fourth") {
		t.Fatalf("history=%q", history.String())
	}
}

func TestArchiveAndReplaceIsRecoverableHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	if err := os.WriteFile(path, []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ArchiveAndReplace(path, []byte("checkpoint\n"), Policy{MaxBytes: 1, MaxAge: time.Hour, Keep: 2}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if string(data) != "checkpoint\n" {
		t.Fatalf("active=%q", data)
	}
	var history bytes.Buffer
	if err := WriteHistory(&history, path); err != nil || history.String() != "old\ncheckpoint\n" {
		t.Fatalf("history=%q err=%v", history.String(), err)
	}
}

func TestPruneRunDirsHonorsAgeCountAndExclusion(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	for index, name := range []string{"old", "middle", "active"} {
		path := filepath.Join(root, name)
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		stamp := now.Add(-time.Duration(3-index) * time.Hour)
		if err := os.Chtimes(path, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	removed, err := PruneRunDirs(root, map[string]bool{"active": true}, 90*time.Minute, 1, now)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(removed, ",") != "middle,old" {
		t.Fatalf("removed=%v", removed)
	}
	if _, err := os.Stat(filepath.Join(root, "active")); err != nil {
		t.Fatal(err)
	}
}

func TestLongRunningWriterKeepsBoundedGenerations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "load.log")
	policy := Policy{MaxBytes: 32 * 1024, MaxAge: time.Hour, Keep: 3}
	w, err := OpenWriter(path, policy)
	if err != nil {
		t.Fatal(err)
	}
	block := bytes.Repeat([]byte("x"), 4096)
	for index := 0; index < 500; index++ {
		if _, err := w.Write(block); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Size() > policy.MaxBytes {
		t.Fatalf("active size=%d err=%v", info.Size(), err)
	}
	archives, err := archives(path)
	if err != nil || len(archives) != policy.Keep {
		t.Fatalf("archives=%d err=%v", len(archives), err)
	}
}
