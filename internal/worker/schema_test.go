package worker

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestPublishedSchemaReferencesRuntimeSchema(t *testing.T) {
	var runtimeSchema map[string]any
	if err := json.Unmarshal(schema, &runtimeSchema); err != nil {
		t.Fatalf("embedded runtime schema is invalid JSON: %v", err)
	}

	publishedPath := filepath.Join("..", "..", "schemas", "worker-result.schema.json")
	published, err := os.ReadFile(publishedPath)
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Reference string `json:"$ref"`
	}
	if err := json.Unmarshal(published, &document); err != nil {
		t.Fatalf("published schema is invalid JSON: %v", err)
	}
	if document.Reference == "" {
		t.Fatal("published schema must reference the runtime schema")
	}

	referencedPath := filepath.Clean(filepath.Join(filepath.Dir(publishedPath), document.Reference))
	runtimePath := filepath.Clean("worker-result.schema.json")
	referencedAbsolute, err := filepath.Abs(referencedPath)
	if err != nil {
		t.Fatal(err)
	}
	runtimeAbsolute, err := filepath.Abs(runtimePath)
	if err != nil {
		t.Fatal(err)
	}
	if referencedAbsolute != runtimeAbsolute {
		t.Fatalf("published schema references %q, want %q", referencedAbsolute, runtimeAbsolute)
	}
	onDisk, err := os.ReadFile(referencedPath)
	if err != nil {
		t.Fatalf("read referenced runtime schema: %v", err)
	}
	if string(onDisk) != string(schema) {
		t.Fatal("embedded runtime schema differs from the referenced file")
	}
}
