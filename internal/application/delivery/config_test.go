package delivery

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigSecureAtomicWriteAndValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "delivery.yaml")
	cfg := DefaultConfig("owner/repository")
	if err := WriteConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("unsafe config mode: %v", info.Mode())
	}
	loaded, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ReleaseRepository != cfg.ReleaseRepository || loaded.PollDuration() != cfg.PollDuration() {
		t.Fatalf("loaded config=%+v", loaded)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".agent-loop-") {
			t.Fatalf("atomic temporary file leaked: %s", entry.Name())
		}
	}

	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil || !strings.Contains(err.Error(), "0600") {
		t.Fatalf("mode was not rejected: %v", err)
	}
}

func TestConfigRejectsSymlinkAndRelativeOverride(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("version: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(link); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("symlink was not rejected: %v", err)
	}
	if _, err := ResolveConfigPath("relative.yaml"); err == nil {
		t.Fatal("relative --config was accepted")
	}
	if !errors.Is(unwrapConfigPathError(filepath.Join(dir, "missing")), os.ErrNotExist) {
		t.Fatal("missing config did not preserve os.ErrNotExist")
	}
}

func TestConfigUnknownFieldFailsClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "delivery.yaml")
	data := []byte("version: 1\nenabled: true\nrelease_repository: owner/repo\nchannel: stable\npoll_interval: 15m\ndrain_timeout: 2h\nauto_apply: schema_compatible\nfuture_protocol: 2\n")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil || !strings.Contains(err.Error(), "future_protocol") {
		t.Fatalf("unknown field accepted: %v", err)
	}
}

func unwrapConfigPathError(path string) error {
	_, err := LoadConfig(path)
	var pathErr *os.PathError
	if errors.As(err, &pathErr) {
		return pathErr.Err
	}
	return err
}

func TestTransactionUnknownPhaseFailsClosedAndLockIsExclusive(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "transaction.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"phase":"future","updated_at":"2026-01-01T00:00:00Z"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadTransaction(path); err == nil || !strings.Contains(err.Error(), "unknown delivery transaction phase") {
		t.Fatalf("unknown phase accepted: %v", err)
	}
	lock, err := AcquireLock(filepath.Join(dir, "lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if _, err := AcquireLock(filepath.Join(dir, "lock")); err == nil {
		t.Fatal("concurrent delivery lock was accepted")
	}
}

func TestRuntimePathsStayUnderManagedDeliveryRoot(t *testing.T) {
	managed := filepath.Join(t.TempDir(), "Library", "Application Support", "codex-issue-loop")
	paths := RuntimePaths(managed)
	want := filepath.Join(managed, "delivery")
	if paths.Root != want {
		t.Fatalf("root=%s want=%s", paths.Root, want)
	}
	for _, path := range []string{paths.Transaction, paths.Maintenance, paths.Cache, paths.Log, paths.Lock} {
		relative, err := filepath.Rel(want, path)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(os.PathSeparator)) {
			t.Fatalf("runtime path escaped managed root: %s", path)
		}
	}
}

func TestRuntimePathsRejectSymlinkedDeliveryRoot(t *testing.T) {
	managed := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(managed, "delivery")); err != nil {
		t.Fatal(err)
	}
	if err := RuntimePaths(managed).Ensure(); err == nil {
		t.Fatal("symlinked runtime root accepted")
	}
}
