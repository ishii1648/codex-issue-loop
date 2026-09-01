package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"

	"github.com/ishii1648/codex-issue-loop/internal/application/delivery"
	"github.com/ishii1648/codex-issue-loop/internal/domain/statecontract"
	"github.com/ishii1648/codex-issue-loop/internal/platform/fsutil"
	"github.com/ishii1648/codex-issue-loop/internal/platform/schema"
)

var releaseVersion = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+([.-][0-9A-Za-z.-]+)?$`)
var commitSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

func runManifest(args []string, stderr io.Writer) int {
	flags := flag.NewFlagSet("releasegen manifest", flag.ContinueOnError)
	flags.SetOutput(stderr)
	artifact := flags.String("artifact", "", "release artifact")
	version := flags.String("version", "", "release version")
	commit := flags.String("commit", "", "release commit")
	output := flags.String("output", "", "manifest output")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() != 0 || *artifact == "" || *output == "" || !releaseVersion.MatchString(*version) || !commitSHA.MatchString(*commit) {
		fmt.Fprintln(stderr, "--artifact, --output, v-prefixed --version, and 40-character --commit are required")
		return 2
	}
	if err := generateManifest(*artifact, *version, *commit, *output); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

func generateManifest(artifact, version, commit, output string) error {
	data, err := os.ReadFile(artifact)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	manifest := delivery.ReleaseManifest{
		ManifestVersion: 1, DeliveryProtocol: delivery.ProtocolVersion, AssignmentProtocol: delivery.AssignmentProtocolVersion, Version: version, Commit: commit,
		Target: "darwin/arm64", Artifact: filepath.Base(artifact), ArtifactSHA256: hex.EncodeToString(sum[:]),
		StateSchemaCurrent: schema.Current, StateSchemaMigrationFrom: schema.Previous,
		SemanticContractCurrent: statecontract.CurrentVersion, SemanticContractMinimum: statecontract.MinimumVersion,
	}
	if manifest.Artifact != delivery.BinaryAsset {
		return fmt.Errorf("artifact must be named %s", delivery.BinaryAsset)
	}
	return fsutil.WriteJSON(output, manifest, 0o644)
}
