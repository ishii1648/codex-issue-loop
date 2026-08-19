package main

import (
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"github.com/ishii1648/codex-issue-loop/internal/delivery"
	"github.com/ishii1648/codex-issue-loop/internal/fsutil"
	"github.com/ishii1648/codex-issue-loop/internal/schema"
	"github.com/ishii1648/codex-issue-loop/internal/statecontract"
)

var releaseVersion = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+([.-][0-9A-Za-z.-]+)?$`)
var commitSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

func main() {
	artifact := flag.String("artifact", "", "release artifact")
	version := flag.String("version", "", "release version")
	commit := flag.String("commit", "", "release commit")
	output := flag.String("output", "", "manifest output")
	flag.Parse()
	if *artifact == "" || *output == "" || !releaseVersion.MatchString(*version) || !commitSHA.MatchString(*commit) {
		fmt.Fprintln(os.Stderr, "--artifact, --output, v-prefixed --version, and 40-character --commit are required")
		os.Exit(2)
	}
	data, err := os.ReadFile(*artifact)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	sum := sha256.Sum256(data)
	manifest := delivery.ReleaseManifest{
		ManifestVersion: 1, DeliveryProtocol: delivery.ProtocolVersion, Version: *version, Commit: *commit,
		Target: "darwin/arm64", Artifact: filepath.Base(*artifact), ArtifactSHA256: hex.EncodeToString(sum[:]),
		StateSchemaCurrent: schema.Current, StateSchemaMigrationFrom: schema.Previous,
		SemanticContractCurrent: statecontract.CurrentVersion, SemanticContractMinimum: statecontract.MinimumVersion,
	}
	if manifest.Artifact != delivery.BinaryAsset {
		fmt.Fprintln(os.Stderr, "artifact must be named "+delivery.BinaryAsset)
		os.Exit(2)
	}
	if err := fsutil.WriteJSON(*output, manifest, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
