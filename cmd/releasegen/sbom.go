package main

import (
	"crypto/sha256"
	"debug/buildinfo"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"time"
)

type document struct {
	SPDXVersion       string         `json:"spdxVersion"`
	DataLicense       string         `json:"dataLicense"`
	SPDXID            string         `json:"SPDXID"`
	Name              string         `json:"name"`
	DocumentNamespace string         `json:"documentNamespace"`
	CreationInfo      creationInfo   `json:"creationInfo"`
	Packages          []spdxPackage  `json:"packages"`
	Files             []spdxFile     `json:"files"`
	Relationships     []relationship `json:"relationships"`
}

type creationInfo struct {
	Created  string   `json:"created"`
	Creators []string `json:"creators"`
}

type spdxPackage struct {
	Name             string `json:"name"`
	SPDXID           string `json:"SPDXID"`
	VersionInfo      string `json:"versionInfo,omitempty"`
	DownloadLocation string `json:"downloadLocation"`
	FilesAnalyzed    bool   `json:"filesAnalyzed"`
	LicenseConcluded string `json:"licenseConcluded"`
	LicenseDeclared  string `json:"licenseDeclared"`
}

type checksum struct {
	Algorithm     string `json:"algorithm"`
	ChecksumValue string `json:"checksumValue"`
}

type spdxFile struct {
	FileName         string     `json:"fileName"`
	SPDXID           string     `json:"SPDXID"`
	Checksums        []checksum `json:"checksums"`
	LicenseConcluded string     `json:"licenseConcluded"`
}

type relationship struct {
	Element string `json:"spdxElementId"`
	Type    string `json:"relationshipType"`
	Related string `json:"relatedSpdxElement"`
}

var unsafeID = regexp.MustCompile(`[^A-Za-z0-9.-]+`)

func runSBOM(args []string, stderr io.Writer) int {
	flags := flag.NewFlagSet("releasegen sbom", flag.ContinueOnError)
	flags.SetOutput(stderr)
	artifact := flags.String("artifact", "", "built agent-loop binary")
	version := flags.String("version", "", "release version")
	output := flags.String("output", "", "SPDX JSON output")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() != 0 || *artifact == "" || *version == "" || *output == "" {
		fmt.Fprintln(stderr, "--artifact, --version, and --output are required")
		return 2
	}
	if err := generateSBOM(*artifact, *version, *output, sourceTime()); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

func generateSBOM(artifact, version, output string, created time.Time) error {
	info, err := buildinfo.ReadFile(artifact)
	if err != nil {
		return fmt.Errorf("read Go build information: %w", err)
	}
	data, err := os.ReadFile(artifact)
	if err != nil {
		return err
	}
	hash := fmt.Sprintf("%x", sha256.Sum256(data))
	mainID := packageID(info.Main.Path)
	packages := []spdxPackage{{Name: info.Main.Path, SPDXID: mainID, VersionInfo: version, DownloadLocation: "NOASSERTION", FilesAnalyzed: true, LicenseConcluded: "NOASSERTION", LicenseDeclared: "NOASSERTION"}}
	relations := []relationship{
		{Element: "SPDXRef-DOCUMENT", Type: "DESCRIBES", Related: mainID},
		{Element: mainID, Type: "CONTAINS", Related: "SPDXRef-File-agent-loop"},
	}
	for _, dependency := range info.Deps {
		module := dependency
		if dependency.Replace != nil {
			module = dependency.Replace
		}
		id := packageID(module.Path + "-" + module.Version)
		packages = append(packages, spdxPackage{Name: module.Path, SPDXID: id, VersionInfo: module.Version, DownloadLocation: "NOASSERTION", FilesAnalyzed: false, LicenseConcluded: "NOASSERTION", LicenseDeclared: "NOASSERTION"})
		relations = append(relations, relationship{Element: mainID, Type: "DEPENDS_ON", Related: id})
	}
	dependencies := packages[1:]
	sort.Slice(dependencies, func(i, j int) bool { return dependencies[i].Name < dependencies[j].Name })
	doc := document{
		SPDXVersion: "SPDX-2.3", DataLicense: "CC0-1.0", SPDXID: "SPDXRef-DOCUMENT",
		Name:              "agent-loop-" + version,
		DocumentNamespace: "https://github.com/ishii1648/codex-issue-loop/releases/" + version + "/spdx/" + hash,
		CreationInfo:      creationInfo{Created: created.UTC().Format("2006-01-02T15:04:05Z"), Creators: []string{"Tool: codex-issue-loop-releasegen"}},
		Packages:          packages,
		Files:             []spdxFile{{FileName: "./" + filepath.Base(artifact), SPDXID: "SPDXRef-File-agent-loop", Checksums: []checksum{{Algorithm: "SHA256", ChecksumValue: hash}}, LicenseConcluded: "NOASSERTION"}},
		Relationships:     relations,
	}
	encoded, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	return os.WriteFile(output, encoded, 0o600)
}

func packageID(value string) string {
	clean := unsafeID.ReplaceAllString(value, "-")
	if clean == "" {
		clean = "unknown"
	}
	return "SPDXRef-Package-" + clean
}

func sourceTime() time.Time {
	if value := os.Getenv("SOURCE_DATE_EPOCH"); value != "" {
		if seconds, err := strconv.ParseInt(value, 10, 64); err == nil {
			return time.Unix(seconds, 0)
		}
	}
	return time.Now().UTC()
}
