package app

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadCommandsRejectUnconfiguredRepository(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "monitor.yaml")
	configBody := "version: 1\nstate_dir: " + dir + "\nrepositories:\n  - name: owner/repo\n"
	if err := os.WriteFile(configPath, []byte(configBody), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := [][]string{
		{"status", "--config", configPath, "--repo", "owner/missing"},
		{"history", "--config", configPath, "--repo", "owner/missing"},
		{"report", "--config", configPath, "--repo", "owner/missing", "--from", "2026-09-01T00:00:00Z"},
	}
	for _, args := range tests {
		var stderr bytes.Buffer
		application := App{Out: &bytes.Buffer{}, Err: &stderr}
		if code := application.Run(context.Background(), args); code != 1 {
			t.Fatalf("%s exit code = %d", args[0], code)
		}
		if !strings.Contains(stderr.String(), `repository "owner/missing" is not configured`) {
			t.Fatalf("%s stderr = %q", args[0], stderr.String())
		}
	}
}
