package delivery

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

const (
	BinaryAsset   = "agent-loop_Darwin_arm64"
	ManifestAsset = "release-manifest.json"
	ChecksumAsset = "checksums.txt"
)

type Runner interface {
	Run(context.Context, string, ...string) ([]byte, error)
}

type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("%s failed: %w", filepath.Base(name), err)
	}
	return out, nil
}

type ReleaseManifest struct {
	ManifestVersion          int    `json:"manifest_version"`
	DeliveryProtocol         int    `json:"delivery_protocol"`
	Version                  string `json:"version"`
	Commit                   string `json:"commit"`
	Target                   string `json:"target"`
	Artifact                 string `json:"artifact"`
	ArtifactSHA256           string `json:"artifact_sha256"`
	StateSchemaCurrent       int    `json:"state_schema_current"`
	StateSchemaMigrationFrom int    `json:"state_schema_migration_from"`
	SemanticContractCurrent  int    `json:"semantic_contract_current"`
	SemanticContractMinimum  int    `json:"semantic_contract_minimum"`
}

type BinaryInfo struct {
	Version                  string `json:"version"`
	Commit                   string `json:"commit"`
	Target                   string `json:"target"`
	DeliveryProtocol         int    `json:"delivery_protocol"`
	StateSchemaCurrent       int    `json:"state_schema_current"`
	StateSchemaMigrationFrom int    `json:"state_schema_migration_from"`
	SemanticContractCurrent  int    `json:"semantic_contract_current"`
	SemanticContractMinimum  int    `json:"semantic_contract_minimum"`
}

type Release struct {
	Tag        string `json:"tagName"`
	Draft      bool   `json:"isDraft"`
	Prerelease bool   `json:"isPrerelease"`
}
type gitObject struct {
	Type string `json:"type"`
	SHA  string `json:"sha"`
}
type gitRef struct {
	Object gitObject `json:"object"`
}
type annotatedTag struct {
	Tag    string    `json:"tag"`
	Object gitObject `json:"object"`
}

type Candidate struct {
	Release  Release         `json:"release"`
	Manifest ReleaseManifest `json:"manifest"`
	Binary   BinaryInfo      `json:"binary"`
	Dir      string          `json:"dir"`
	Digest   string          `json:"digest"`
}

type VerificationProgress struct {
	Phase     Phase
	Desired   VersionRef
	Digest    string
	Candidate string
}

type Verifier struct {
	GH       string
	Runner   Runner
	CacheDir string
	Progress func(VerificationProgress) error
}

func (v Verifier) Discover(ctx context.Context, cfg Config) (VersionRef, error) {
	if v.GH == "" {
		v.GH = "gh"
	}
	if v.Runner == nil {
		v.Runner = ExecRunner{}
	}
	release, commit, err := v.resolveProduction(ctx, cfg.ReleaseRepository)
	if err != nil {
		return VersionRef{}, err
	}
	return VersionRef{Version: release.Tag, Commit: commit}, nil
}

func (v Verifier) Check(ctx context.Context, cfg Config) (Candidate, error) {
	if v.GH == "" {
		v.GH = "gh"
	}
	if v.Runner == nil {
		v.Runner = ExecRunner{}
	}
	if err := os.MkdirAll(v.CacheDir, 0o700); err != nil {
		return Candidate{}, err
	}
	release, commit, err := v.resolveProduction(ctx, cfg.ReleaseRepository)
	if err != nil {
		return Candidate{}, err
	}
	desired := VersionRef{Version: release.Tag, Commit: commit}
	if err := v.reportProgress(VerificationProgress{Phase: PhaseDiscovered, Desired: desired}); err != nil {
		return Candidate{}, err
	}
	tmp, err := os.MkdirTemp(v.CacheDir, ".candidate-")
	if err != nil {
		return Candidate{}, err
	}
	keep := false
	defer func() {
		if !keep {
			_ = os.RemoveAll(tmp)
		}
	}()
	args := []string{"release", "download", release.Tag, "--repo", cfg.ReleaseRepository, "--dir", tmp, "--pattern", BinaryAsset, "--pattern", ManifestAsset, "--pattern", ChecksumAsset}
	if _, err := v.Runner.Run(ctx, v.GH, args...); err != nil {
		return Candidate{}, fmt.Errorf("download release assets: %w", err)
	}
	if err := v.reportProgress(VerificationProgress{Phase: PhaseDownloaded, Desired: desired, Candidate: tmp}); err != nil {
		return Candidate{}, err
	}

	manifest, digest, err := verifyStaticAssets(tmp, release.Tag, commit)
	if err != nil {
		return Candidate{}, err
	}
	workflow := cfg.WorkflowIdentity()
	for _, asset := range []string{BinaryAsset, ManifestAsset} {
		if _, err := v.Runner.Run(ctx, v.GH, "attestation", "verify", filepath.Join(tmp, asset), "--repo", cfg.ReleaseRepository, "--signer-workflow", workflow, "--source-ref", "refs/tags/"+release.Tag, "--deny-self-hosted-runners"); err != nil {
			return Candidate{}, fmt.Errorf("verify GitHub artifact attestation for %s: %w", asset, err)
		}
	}
	// The candidate is executed only after its checksum, trusted workflow
	// attestation, tag and machine-readable manifest have all been verified.
	if err := os.Chmod(filepath.Join(tmp, BinaryAsset), 0o700); err != nil {
		return Candidate{}, err
	}
	out, err := v.Runner.Run(ctx, filepath.Join(tmp, BinaryAsset), "version", "--json")
	if err != nil {
		return Candidate{}, fmt.Errorf("inspect verified candidate binary: %w", err)
	}
	var binary BinaryInfo
	if err := decodeStrictJSON(out, &binary); err != nil {
		return Candidate{}, fmt.Errorf("decode candidate binary metadata: %w", err)
	}
	if err := compareBinaryManifest(binary, manifest); err != nil {
		return Candidate{}, err
	}
	// Re-resolve after verification so an edited Release or replaced tag cannot
	// race the candidate into the apply transaction.
	after, afterCommit, err := v.resolveProduction(ctx, cfg.ReleaseRepository)
	if err != nil {
		return Candidate{}, err
	}
	if after.Tag != release.Tag || afterCommit != commit {
		return Candidate{}, errors.New("release changed while it was being verified; retry with a fresh candidate")
	}
	final := filepath.Join(v.CacheDir, release.Tag+"-"+digest[:16])
	if info, statErr := os.Lstat(final); statErr == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return Candidate{}, errors.New("existing delivery cache entry is not a regular directory")
		}
		existingManifest, existingDigest, verifyErr := verifyStaticAssets(final, release.Tag, commit)
		if verifyErr != nil || existingDigest != digest || existingManifest != manifest {
			return Candidate{}, errors.New("existing delivery cache entry does not match the verified candidate")
		}
		if err := os.RemoveAll(tmp); err != nil {
			return Candidate{}, err
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return Candidate{}, statErr
	} else if err := os.Rename(tmp, final); err != nil {
		return Candidate{}, err
	} else {
		keep = true
	}
	candidate := Candidate{Release: release, Manifest: manifest, Binary: binary, Dir: final, Digest: digest}
	if err := v.reportProgress(VerificationProgress{Phase: PhaseVerified, Desired: desired, Digest: digest, Candidate: final}); err != nil {
		return Candidate{}, err
	}
	return candidate, nil
}

func (v Verifier) reportProgress(progress VerificationProgress) error {
	if v.Progress == nil {
		return nil
	}
	if err := v.Progress(progress); err != nil {
		return fmt.Errorf("persist %s delivery phase: %w", progress.Phase, err)
	}
	return nil
}

func (v Verifier) resolveProduction(ctx context.Context, repository string) (Release, string, error) {
	out, err := v.Runner.Run(ctx, v.GH, "release", "view", "--repo", repository, "--json", "tagName,isDraft,isPrerelease")
	if err != nil {
		return Release{}, "", fmt.Errorf("discover latest production release: %w", err)
	}
	var release Release
	if err := json.Unmarshal(out, &release); err != nil {
		return Release{}, "", fmt.Errorf("decode release metadata: %w", err)
	}
	if release.Draft || release.Prerelease {
		return Release{}, "", errors.New("latest release is not a production release")
	}
	if _, err := ParseSemVer(release.Tag); err != nil {
		return Release{}, "", fmt.Errorf("release tag: %w", err)
	}
	out, err = v.Runner.Run(ctx, v.GH, "api", "repos/"+repository+"/git/ref/tags/"+release.Tag)
	if err != nil {
		return Release{}, "", fmt.Errorf("resolve release tag ref: %w", err)
	}
	var ref gitRef
	if err := json.Unmarshal(out, &ref); err != nil {
		return Release{}, "", err
	}
	if ref.Object.Type != "tag" || !validSHA(ref.Object.SHA) {
		return Release{}, "", errors.New("release tag must be an annotated Git tag")
	}
	out, err = v.Runner.Run(ctx, v.GH, "api", "repos/"+repository+"/git/tags/"+ref.Object.SHA)
	if err != nil {
		return Release{}, "", fmt.Errorf("peel annotated release tag: %w", err)
	}
	var tag annotatedTag
	if err := json.Unmarshal(out, &tag); err != nil {
		return Release{}, "", err
	}
	if tag.Tag != release.Tag || tag.Object.Type != "commit" || !validSHA(tag.Object.SHA) {
		return Release{}, "", errors.New("annotated release tag does not peel uniquely to a commit")
	}
	return release, tag.Object.SHA, nil
}

func verifyStaticAssets(dir, tag, commit string) (ReleaseManifest, string, error) {
	data, err := os.ReadFile(filepath.Join(dir, ManifestAsset))
	if err != nil {
		return ReleaseManifest{}, "", fmt.Errorf("read release manifest: %w", err)
	}
	var manifest ReleaseManifest
	if err := decodeStrictJSON(data, &manifest); err != nil {
		return manifest, "", fmt.Errorf("decode release manifest: %w", err)
	}
	if manifest.ManifestVersion != 1 || manifest.DeliveryProtocol != ProtocolVersion {
		return manifest, "", errors.New("unknown release manifest version or delivery protocol")
	}
	if manifest.Version != tag || manifest.Commit != commit {
		return manifest, "", errors.New("release tag, commit, and manifest metadata do not match")
	}
	if manifest.Target != "darwin/arm64" || manifest.Artifact != BinaryAsset {
		return manifest, "", errors.New("release target or artifact name is not darwin/arm64")
	}
	if !validSHA(manifest.Commit) || !validDigest(manifest.ArtifactSHA256) {
		return manifest, "", errors.New("release manifest contains an invalid commit or digest")
	}
	if manifest.StateSchemaCurrent <= 0 || manifest.StateSchemaMigrationFrom <= 0 || manifest.StateSchemaMigrationFrom > manifest.StateSchemaCurrent ||
		manifest.SemanticContractCurrent <= 0 || manifest.SemanticContractMinimum < 0 || manifest.SemanticContractMinimum > manifest.SemanticContractCurrent {
		return manifest, "", errors.New("release manifest contains an invalid compatibility range")
	}
	checksums, err := parseChecksums(filepath.Join(dir, ChecksumAsset))
	if err != nil {
		return manifest, "", err
	}
	for _, name := range []string{BinaryAsset, ManifestAsset} {
		want, ok := checksums[name]
		if !ok {
			return manifest, "", fmt.Errorf("checksums.txt does not cover %s", name)
		}
		got, err := fileDigest(filepath.Join(dir, name))
		if err != nil {
			return manifest, "", err
		}
		if got != want {
			return manifest, "", fmt.Errorf("checksum mismatch for %s", name)
		}
		if name == BinaryAsset && got != manifest.ArtifactSHA256 {
			return manifest, "", errors.New("artifact digest differs between manifest and checksums.txt")
		}
	}
	return manifest, manifest.ArtifactSHA256, nil
}

func compareBinaryManifest(binary BinaryInfo, manifest ReleaseManifest) error {
	if binary.Version != manifest.Version || binary.Commit != manifest.Commit || binary.Target != manifest.Target || binary.DeliveryProtocol != manifest.DeliveryProtocol ||
		binary.StateSchemaCurrent != manifest.StateSchemaCurrent || binary.StateSchemaMigrationFrom != manifest.StateSchemaMigrationFrom ||
		binary.SemanticContractCurrent != manifest.SemanticContractCurrent || binary.SemanticContractMinimum != manifest.SemanticContractMinimum {
		return errors.New("binary embedded metadata does not match release manifest")
	}
	return nil
}

func parseChecksums(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	result := map[string]string{}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 2 || !validDigest(fields[0]) {
			return nil, errors.New("invalid checksums.txt format")
		}
		name := strings.TrimPrefix(fields[1], "*")
		if filepath.Base(name) != name || name == "." {
			return nil, errors.New("checksum asset name must be a basename")
		}
		if _, exists := result[name]; exists {
			return nil, fmt.Errorf("duplicate checksum for %s", name)
		}
		result[name] = strings.ToLower(fields[0])
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

func fileDigest(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}
func validDigest(value string) bool {
	_, err := hex.DecodeString(value)
	return len(value) == 64 && err == nil && value == strings.ToLower(value)
}
func validSHA(value string) bool { return len(value) == 40 && validHex(value) }
func validHex(value string) bool {
	_, err := hex.DecodeString(value)
	return err == nil && value == strings.ToLower(value)
}

type SemVer struct{ Major, Minor, Patch int }

var semverPattern = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

func ParseSemVer(value string) (SemVer, error) {
	match := semverPattern.FindStringSubmatch(value)
	if match == nil {
		return SemVer{}, errors.New("production release tag must be vMAJOR.MINOR.PATCH")
	}
	major, majorErr := strconv.Atoi(match[1])
	minor, minorErr := strconv.Atoi(match[2])
	patch, patchErr := strconv.Atoi(match[3])
	if majorErr != nil || minorErr != nil || patchErr != nil {
		return SemVer{}, errors.New("production release version component is too large")
	}
	return SemVer{major, minor, patch}, nil
}
func (v SemVer) Compare(other SemVer) int {
	if v.Major != other.Major {
		if v.Major < other.Major {
			return -1
		}
		return 1
	}
	if v.Minor != other.Minor {
		if v.Minor < other.Minor {
			return -1
		}
		return 1
	}
	if v.Patch < other.Patch {
		return -1
	}
	if v.Patch > other.Patch {
		return 1
	}
	return 0
}

type CompatibilityPlan struct {
	Allowed bool       `json:"allowed"`
	Result  string     `json:"result"`
	Reason  string     `json:"reason,omitempty"`
	Current VersionRef `json:"current"`
	Desired VersionRef `json:"desired"`
}

func PlanCompatibility(current VersionRef, currentSchema, currentSemantic int, candidate Candidate) CompatibilityPlan {
	plan := CompatibilityPlan{Allowed: false, Result: "blocked", Current: current, Desired: VersionRef{Version: candidate.Manifest.Version, Commit: candidate.Manifest.Commit}}
	want, err := ParseSemVer(candidate.Manifest.Version)
	if err != nil {
		plan.Reason = err.Error()
		return plan
	}
	if current.Version == "" {
		plan.Reason = "no managed installation exists"
		return plan
	}
	have, err := ParseSemVer(current.Version)
	if err != nil {
		plan.Reason = "installed version is not a production SemVer"
		return plan
	}
	cmp := want.Compare(have)
	if cmp < 0 {
		plan.Reason = "implicit downgrade is not allowed"
		return plan
	}
	if cmp == 0 {
		if current.Commit != candidate.Manifest.Commit {
			plan.Reason = "same version resolves to a different commit"
		} else {
			plan.Result = "current"
			plan.Reason = "already installed"
		}
		return plan
	}
	if want.Major != have.Major {
		plan.Result = "blocked_for_approval"
		plan.Reason = "SemVer major update requires explicit migration approval"
		return plan
	}
	if candidate.Manifest.StateSchemaCurrent != currentSchema {
		plan.Result = "blocked_for_approval"
		plan.Reason = "durable schema migration requires explicit approval"
		return plan
	}
	if currentSemantic < candidate.Manifest.SemanticContractMinimum || currentSemantic > candidate.Manifest.SemanticContractCurrent {
		plan.Result = "blocked_for_approval"
		plan.Reason = "durable semantic contract is outside the candidate compatibility range"
		return plan
	}
	plan.Allowed = true
	plan.Result = "ready"
	return plan
}
