package operatorcontrol

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestTransactionJSONCompletedAt(t *testing.T) {
	for _, tc := range []struct {
		name string
		at   time.Time
		want string
	}{
		{name: "unset", want: `"0001-01-01T00:00:00Z"`},
		{name: "set", at: time.Date(2026, 9, 11, 12, 30, 0, 0, time.UTC), want: `"2026-09-11T12:30:00Z"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data, err := json.Marshal(Transaction{CompletedAt: tc.at})
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(data, &fields); err != nil {
				t.Fatal(err)
			}
			if got := string(fields["completed_at"]); got != tc.want {
				t.Errorf("completed_at = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestTransactionAndFenceRoundTrip(t *testing.T) {
	root := t.TempDir()
	now := time.Now().UTC()
	tx := Transaction{Generation: "operator_fixture", Operation: OperationRestart, Phase: PhaseDraining, RequestedAt: now, DrainDeadline: now.Add(time.Hour), UpdatedAt: now}
	path := filepath.Join(root, "operator-control.json")
	if err := Save(path, tx); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil || loaded.Generation != tx.Generation || !loaded.Active() {
		t.Fatalf("loaded=%+v err=%v", loaded, err)
	}
	fencePath := filepath.Join(root, "operator-maintenance.json")
	if err := WriteFence(fencePath, Fence{Generation: tx.Generation, Operation: tx.Operation, RequestedAt: now}); err != nil {
		t.Fatal(err)
	}
	if fence, err := LoadFence(fencePath); err != nil || fence.Generation != tx.Generation {
		t.Fatalf("fence=%+v err=%v", fence, err)
	}
	if err := ClearFence(fencePath); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(fencePath); !os.IsNotExist(err) {
		t.Fatalf("fence still exists: %v", err)
	}
}

func TestLoadRejectsNonPrivateTransaction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "operator-control.json")
	if err := os.WriteFile(path, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("non-private transaction was accepted")
	}
}
