package monitor_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestPublishedSchemasAreValidJSONAndVersioned(t *testing.T) {
	entries, err := os.ReadDir("schemas")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 4 {
		t.Fatalf("schema count = %d, want 4", len(entries))
	}
	for _, entry := range entries {
		data, err := os.ReadFile(filepath.Join("schemas", entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		var schema map[string]any
		if err := json.Unmarshal(data, &schema); err != nil {
			t.Fatalf("%s: %v", entry.Name(), err)
		}
		if schema["$schema"] == nil || schema["$id"] == nil {
			t.Fatalf("%s is not a published JSON schema", entry.Name())
		}
	}
}
