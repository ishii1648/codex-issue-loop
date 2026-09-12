package app

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/ishii1648/codex-issue-loop/internal/platform/runtimemetadata"
)

func TestStatusRuntimeIsLatestResponseMetadataOnly(t *testing.T) {
	cfg, _, at := dashboardFixture(t)
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	metadata := runtimemetadata.Store{Root: root, Inspect: func(pid int) (runtimemetadata.Process, error) {
		return runtimemetadata.Process{PID: pid, UID: uint32(os.Geteuid()), BootSessionID: "boot", StartedAt: runtimemetadata.StartTime{Seconds: at.Add(-time.Hour).Unix()}}, nil
	}}
	now := at
	a := App{RuntimeMetadata: &metadata, Now: func() time.Time { return now }}
	publish := func(version string) func() error {
		cleanup, err := metadata.Publish("owner/repo", "test-repo", version, now)
		if err != nil {
			t.Fatal(err)
		}
		return cleanup
	}
	read := func(path string) map[string]json.RawMessage {
		response := request(t, a.monitorHandler(cfg), path)
		if response.Code != 200 {
			t.Fatalf("HTTP %d: %s", response.Code, response.Body.String())
		}
		var result struct {
			Repositories []map[string]json.RawMessage `json:"repositories"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return result.Repositories[0]
	}
	before := read("/api/status")
	if string(before["runtime"]) != "null" {
		t.Fatal("missing runtime was supplemented")
	}
	publish("1.8.2")
	current := read("/api/status")
	delete(before, "runtime")
	var observed runtimemetadata.Observation
	if err := json.Unmarshal(current["runtime"], &observed); err != nil {
		t.Fatal(err)
	}
	if observed.Version != "1.8.2" || !observed.ObservedAt.Equal(at) {
		t.Fatalf("runtime=%+v", observed)
	}
	now = now.Add(time.Minute)
	cleanup := publish("1.8.3")
	historical := read("/api/status?at=" + at.Format(time.RFC3339))
	if err := json.Unmarshal(historical["runtime"], &observed); err != nil {
		t.Fatal(err)
	}
	if observed.Version != "1.8.3" || !observed.ObservedAt.Equal(now) || !observed.ExpiresAt.Equal(now.Add(cfg.ObservationTimeout.Duration)) {
		t.Fatalf("historical selection changed runtime=%+v", observed)
	}
	delete(historical, "runtime")
	if !reflect.DeepEqual(before, historical) {
		t.Fatal("runtime affected GitHub state")
	}
	if err := cleanup(); err != nil {
		t.Fatal(err)
	}
	if got := read("/api/status"); string(got["runtime"]) != "null" {
		t.Fatal("stopped runtime retained")
	}
	publish("1.8.4")
	metadata.Inspect = func(int) (runtimemetadata.Process, error) { return runtimemetadata.Process{}, os.ErrPermission }
	if got := read("/api/status"); string(got["runtime"]) != "null" {
		t.Fatal("failed observation retained version")
	}
}
