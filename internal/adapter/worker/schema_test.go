package worker

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestPublishedSchemaReferencesRuntimeSchema(t *testing.T) {
	var runtimeSchema map[string]any
	if err := json.Unmarshal(schema, &runtimeSchema); err != nil {
		t.Fatalf("embedded runtime schema is invalid JSON: %v", err)
	}

	publishedPath := filepath.Join("..", "..", "..", "schemas", "worker-result.schema.json")
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

func TestRuntimeSchemaUsesStructuredOutputsSubset(t *testing.T) {
	var runtimeSchema map[string]any
	if err := json.Unmarshal(schema, &runtimeSchema); err != nil {
		t.Fatalf("embedded runtime schema is invalid JSON: %v", err)
	}
	properties, ok := runtimeSchema["properties"].(map[string]any)
	if !ok {
		t.Fatal("runtime schema must define object properties")
	}
	for name, want := range map[string]string{
		"version": "integer", "status": "string", "execution_profile": "string",
	} {
		property, ok := properties[name].(map[string]any)
		if !ok || property["type"] != want {
			t.Fatalf("property %q type=%v, want %q", name, property["type"], want)
		}
	}
	assertNoUnsupportedOneOf(t, runtimeSchema, "$")
}

func assertNoUnsupportedOneOf(t *testing.T, value any, path string) {
	t.Helper()
	switch current := value.(type) {
	case map[string]any:
		if _, exists := current["oneOf"]; exists {
			t.Fatalf("unsupported oneOf at %s; use anyOf for Codex Structured Outputs", path)
		}
		for key, child := range current {
			assertNoUnsupportedOneOf(t, child, path+"."+key)
		}
	case []any:
		for index, child := range current {
			assertNoUnsupportedOneOf(t, child, path+"["+fmt.Sprint(index)+"]")
		}
	}
}
