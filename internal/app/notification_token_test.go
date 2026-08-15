package app

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"github.com/ishii1648/codex-issue-loop/internal/registry"
)

func TestNotificationTokenCommandStoresPrivateCredentialWithoutOutputLeak(t *testing.T) {
	repo, l := testEnvironment(t)
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	entry, err := (registry.Store{Path: l.RegistryPath}).Add(mustConfig(t, repo))
	if err != nil {
		t.Fatal(err)
	}
	secret := "not-a-real-token-123"
	var out, stderr bytes.Buffer
	a := App{In: strings.NewReader(secret + "\n"), Out: &out, Err: &stderr}
	if code := a.Run(context.Background(), []string{"notification-token", "--repo", repo, "--token-file", "-", "--json"}); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if strings.Contains(out.String(), secret) || strings.Contains(stderr.String(), secret) {
		t.Fatalf("credential leaked: stdout=%q stderr=%q", out.String(), stderr.String())
	}
	info, err := os.Stat(l.NotificationTokenPath(entry.RepoID))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("mode=%v err=%v", info.Mode().Perm(), err)
	}
	loaded, err := loadNotificationToken(l, entry)
	if err != nil || loaded != secret {
		t.Fatalf("loaded=%q err=%v", loaded, err)
	}

	out.Reset()
	stderr.Reset()
	if code := (App{Out: &out, Err: &stderr}).Run(context.Background(), []string{"notification-token", "--repo", repo, "--clear", "--json"}); code != 0 {
		t.Fatalf("clear code=%d stderr=%s", code, stderr.String())
	}
	if _, err := os.Stat(l.NotificationTokenPath(entry.RepoID)); !os.IsNotExist(err) {
		t.Fatalf("token still exists: %v", err)
	}
}

func TestLoadNotificationTokenRejectsUnsafePermissions(t *testing.T) {
	repo, l := testEnvironment(t)
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	entry, err := (registry.Store{Path: l.RegistryPath}).Add(mustConfig(t, repo))
	if err != nil {
		t.Fatal(err)
	}
	path := l.NotificationTokenPath(entry.RepoID)
	if err := os.MkdirAll(l.RepoDir(entry.RepoID), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("unsafe-token\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadNotificationToken(l, entry); err == nil || !strings.Contains(err.Error(), "0600") {
		t.Fatalf("unsafe token accepted: %v", err)
	}
}
