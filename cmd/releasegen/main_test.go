package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ishii1648/codex-issue-loop/internal/application/delivery"
	"github.com/ishii1648/codex-issue-loop/internal/domain/statecontract"
	"github.com/ishii1648/codex-issue-loop/internal/platform/schema"
)

func TestRunDispatchesSubcommands(t *testing.T) {
	for _, args := range [][]string{{}, {"unknown"}} {
		var stderr bytes.Buffer
		if code := run(args, &stderr); code != 2 {
			t.Fatalf("run(%q) exit code = %d, want 2", args, code)
		}
		if !strings.Contains(stderr.String(), "usage: releasegen <manifest|sbom>") {
			t.Fatalf("run(%q) stderr = %q", args, stderr.String())
		}
	}

	var stderr bytes.Buffer
	if code := run([]string{"help"}, &stderr); code != 0 {
		t.Fatalf("help exit code = %d, want 0", code)
	}
}

func TestGenerateManifest(t *testing.T) {
	dir := t.TempDir()
	artifact := filepath.Join(dir, delivery.BinaryAsset)
	if err := os.WriteFile(artifact, []byte("artifact"), 0o600); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(dir, "release-manifest.json")
	commit := strings.Repeat("a", 40)
	if err := generateManifest(artifact, "v1.2.3", commit, output); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	var manifest delivery.ReleaseManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Version != "v1.2.3" || manifest.Commit != commit || manifest.Artifact != delivery.BinaryAsset {
		t.Fatalf("unexpected release manifest: %+v", manifest)
	}
}

func TestGenerateSBOMProducesDeterministicSPDXDocument(t *testing.T) {
	artifact, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "sbom.spdx.json")
	created := time.Unix(1_700_000_000, 0)
	if err := generateSBOM(artifact, "v1.2.3", output, created); err != nil {
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

func TestReleaseContractConstantsStayAligned(t *testing.T) {
	if delivery.ProtocolVersion != 1 || schema.Current != statecontract.CurrentSchemaVersion || schema.Previous != statecontract.MigrationFromSchema {
		t.Fatalf("protocol/schema contract drift: protocol=%d schema=%d/%d contract=%d/%d", delivery.ProtocolVersion, schema.Current, schema.Previous, statecontract.CurrentSchemaVersion, statecontract.MigrationFromSchema)
	}
}
