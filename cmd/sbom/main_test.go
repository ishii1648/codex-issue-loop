package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestGenerateProducesDeterministicSPDXDocument(t *testing.T) {
	artifact, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "sbom.spdx.json")
	created := time.Unix(1_700_000_000, 0)
	if err := generate(artifact, "v1.2.3", output, created); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	var doc document
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.SPDXVersion != "SPDX-2.3" || doc.Name != "agent-loop-v1.2.3" || doc.CreationInfo.Created != "2023-11-14T22:13:20Z" || len(doc.Packages) == 0 || len(doc.Files) != 1 {
		t.Fatalf("unexpected SPDX document: %+v", doc)
	}
}
