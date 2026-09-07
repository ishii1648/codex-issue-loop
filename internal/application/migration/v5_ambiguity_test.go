package migration

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/ishii1648/codex-issue-loop/internal/adapter/state"
	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
)

func TestV4AmbiguitiesArePreviewedAndQuarantined(t *testing.T) {
	for _, scenario := range []string{"multiple active", "incomplete lease", "unknown stage", "unknown kind"} {
		t.Run(scenario, func(t *testing.T) {
			l, _, _ := writeV4Fixture(t, false)
			var object map[string]json.RawMessage
			if err := json.Unmarshal(legacyRunningLaunchFixture("", false), &object); err != nil {
				t.Fatal(err)
			}
			var issues map[string]map[string]json.RawMessage
			if err := json.Unmarshal(object["issues"], &issues); err != nil {
				t.Fatal(err)
			}
			item := issues["477"]
			want := []string{"477"}
			switch scenario {
			case "multiple active":
				var other map[string]json.RawMessage
				if err := json.Unmarshal(mustMarshal(item), &other); err != nil {
					t.Fatal(err)
				}
				other["number"] = mustMarshal(478)
				issues["478"] = other
				want = append(want, "478")
			case "incomplete lease":
				item["lease"] = mustMarshal(map[string]any{"owner": map[string]any{"run_id": "run_477"}})
			case "unknown stage", "unknown kind":
				checkpoint := map[string]any{"id": "park_477", "stage": "resume", "original_lease": item["lease"]}
				if scenario == "unknown stage" {
					checkpoint["stage"] = "unknown"
				} else {
					checkpoint["kind"] = "unknown"
				}
				item["resource_park"] = mustMarshal(checkpoint)
			}
			issues["479"] = map[string]json.RawMessage{"number": mustMarshal(479), "status": mustRaw("completed"), "updated_at": item["updated_at"]}
			object["issues"] = mustMarshal(issues)
			path := filepath.Join(l.RepoDir("repo-1"), "state.json")
			original := mustMarshal(object)
			if err := os.WriteFile(path, original, 0o600); err != nil {
				t.Fatal(err)
			}
			report, err := Inspect(l)
			if err != nil {
				t.Fatal(err)
			}
			for _, key := range want {
				found := false
				for _, finding := range report.SemanticFindings {
					if finding.IssueNumber == rawInt(issues[key]["number"]) && finding.Code == "V4_RECOVERY_CONTRACT_QUARANTINED" && finding.Migratable {
						found = true
					}
				}
				if !found {
					t.Fatalf("missing quarantine preview for %s: %+v", key, report)
				}
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(after, original) {
				t.Fatal("preview changed source")
			}
			migrator := Migrator{Layout: l}
			if _, err := migrator.Apply(); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var snapshot state.Snapshot
			if err := json.Unmarshal(data, &snapshot); err != nil {
				t.Fatal(err)
			}
			if err := snapshot.Validate(); err != nil {
				t.Fatal(err)
			}
			for _, key := range want {
				got := snapshot.Issues[key]
				if got.Status != issuedomain.StatusBlocked || got.Suspension == nil || got.Suspension.Status != issuedomain.SuspensionQuarantined || string(got.Suspension.Recoverability) != "ambiguous" {
					t.Fatalf("Issue %s was not quarantined: %+v", key, got)
				}
				actions := mustMarshal(got.Suspension.AllowedActions)
				if string(actions) != `["cancel"]` {
					t.Fatalf("actions=%s", actions)
				}
			}
			if snapshot.ActiveExecution != nil || snapshot.Issues["479"].Status != issuedomain.StatusCompleted {
				t.Fatalf("unexpected migrated snapshot: %+v", snapshot)
			}
			j, exists, err := migrator.loadJournal()
			if err != nil || !exists || j.Status != "completed" {
				t.Fatalf("journal=%+v err=%v", j, err)
			}
		})
	}
}

func TestMigrationWritesStateBeforeEvents(t *testing.T) {
	l, _, original := writeV4Fixture(t, false)
	statePath := filepath.Join(l.RepoDir("repo-1"), "state.json")
	eventsPath := filepath.Join(l.RepoDir("repo-1"), "events.jsonl")
	stopped := errors.New("stop after state")
	m := Migrator{Layout: l, AfterWrite: func(path string) error {
		if path == statePath {
			return stopped
		}
		return nil
	}}
	if _, err := m.Apply(); !errors.Is(err, stopped) {
		t.Fatalf("err=%v", err)
	}
	data, err := os.ReadFile(eventsPath)
	if err != nil || !bytes.Equal(data, original[eventsPath]) {
		t.Fatal("events changed before state migration completed")
	}
}

func TestV4InvalidSnapshotFailsBeforeWritingArtifacts(t *testing.T) {
	l, _, original := writeV4Fixture(t, false)
	path := filepath.Join(l.RepoDir("repo-1"), "state.json")
	var object map[string]json.RawMessage
	if err := json.Unmarshal(original[path], &object); err != nil {
		t.Fatal(err)
	}
	object["repo_id"] = mustRaw("")
	original[path] = mustMarshal(object)
	if err := os.WriteFile(path, original[path], 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Inspect(l); err == nil {
		t.Fatal("preview accepted an invalid snapshot")
	}
	m := Migrator{Layout: l}
	if _, err := m.Apply(); err == nil {
		t.Fatal("apply accepted an invalid snapshot")
	}
	for path, before := range original {
		after, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(before, after) {
			t.Fatalf("artifact changed: %s, err=%v", path, err)
		}
	}
	if _, exists, err := m.loadJournal(); err != nil || exists {
		t.Fatalf("unexpected journal: exists=%v err=%v", exists, err)
	}
}
