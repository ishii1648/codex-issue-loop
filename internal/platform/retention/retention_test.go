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

func TestWriterHistoryWithLiteralMetacharacters(t *testing.T) {
	for _, name := range []string{"proj[wip]", "proj[1", "proj*", "proj?"} {
		for _, inFilename := range []bool{false, true} {
			relative := filepath.Join(name, "service.log")
			if inFilename {
				relative = name + ".log"
			}
			t.Run(relative, func(t *testing.T) {
				path := filepath.Join(t.TempDir(), relative)
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("initial!\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				w, err := OpenWriter(path, Policy{MaxBytes: 8, MaxAge: time.Hour, Keep: 2})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = w.Close() })
				var initial bytes.Buffer
				if err := WriteHistory(&initial, path); err != nil || initial.String() != "initial!\n" {
					t.Fatalf("initial history=%q err=%v", initial.String(), err)
				}
				for _, value := range []string{"first\n", "second\n", "third\n", "fourth\n"} {
					if _, err := w.Write([]byte(value)); err != nil {
						t.Fatal(err)
					}
				}
				if err := w.Close(); err != nil {
					t.Fatal(err)
				}
				entries, err := os.ReadDir(filepath.Dir(path))
				if err != nil {
					t.Fatal(err)
				}
				if len(entries) != 3 {
					t.Fatalf("expected active log and two archives, got %v", entries)
				}
				var history bytes.Buffer
				if err := WriteHistory(&history, path); err != nil || history.String() != "second\nthird\nfourth\n" {
					t.Fatalf("history=%q err=%v", history.String(), err)
				}
			})
		}
	}
}

func TestWriteHistoryMissingDirectory(t *testing.T) {
	var history bytes.Buffer
	if err := WriteHistory(&history, filepath.Join(t.TempDir(), "missing", "service.log")); err != nil {
		t.Fatal(err)
	}
	if history.Len() != 0 {
		t.Fatalf("history=%q", history.String())
	}
}

func TestArchivesMatchesLiteralFilename(t *testing.T) {
	dir := t.TempDir()
	base := "service[1]*?.log"
	for _, name := range []string{base + ".002.gz", base + ".001.gz", base + ".003.gz.tmp", base + "x.001.gz", "service1ab.log.001.gz"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, base+".000.gz"), 0o700); err != nil {
		t.Fatal(err)
	}
	matches, err := archives(filepath.Join(dir, base))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 2 || matches[0] != filepath.Join(dir, base+".001.gz") || matches[1] != filepath.Join(dir, base+".002.gz") {
		t.Fatalf("archives=%v", matches)
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
