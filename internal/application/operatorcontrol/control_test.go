package operatorcontrol

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

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
