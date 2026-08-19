// Package producer validates and persists Codex-proposed Issue admission metadata.
// It is deliberately separate from the supervisor: admission retries consume only
// the GitHub snapshot and never invoke this package or an LLM.
package producer

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/ishii1648/codex-issue-loop/internal/admission"
	"github.com/ishii1648/codex-issue-loop/internal/config"
	gh "github.com/ishii1648/codex-issue-loop/internal/github"
)

const ProposalVersion = 1

type ResourceCandidate struct {
	Name   string   `json:"name"`
	Paths  []string `json:"paths"`
	Reason string   `json:"reason"`
}

type DependencyCandidate struct {
	IssueNumber int    `json:"issue_number"`
	Reason      string `json:"reason"`
}

// Proposal is the local, structured hand-off from Codex to the deterministic
// validator. Reasons are never copied to GitHub by Apply.
type Proposal struct {
	Version          int                   `json:"version"`
	IssueNumber      int                   `json:"issue_number"`
	Resources        []ResourceCandidate   `json:"resources"`
	Dependencies     []DependencyCandidate `json:"dependencies"`
	Exclusive        bool                  `json:"exclusive"`
	ExclusiveReason  string                `json:"exclusive_reason"`
	Confidence       string                `json:"confidence"`
	AmbiguityReasons []string              `json:"ambiguity_reasons"`
}

type DependencySnapshot struct {
	IssueNumber int    `json:"issue_number"`
	State       string `json:"state"`
}

type Report struct {
	Repository          string               `json:"repository"`
	IssueNumber         int                  `json:"issue_number"`
	Mode                string               `json:"mode"`
	Applied             bool                 `json:"applied"`
	Valid               bool                 `json:"valid"`
	Ready               bool                 `json:"ready"`
	Exclusive           bool                 `json:"exclusive"`
	Resources           []string             `json:"resources"`
	Dependencies        []int                `json:"dependencies"`
	DependencySnapshots []DependencySnapshot `json:"dependency_snapshots"`
	FallbackReasons     []string             `json:"fallback_reasons,omitempty"`
	Errors              []string             `json:"errors,omitempty"`
	SnapshotSHA256      string               `json:"snapshot_sha256"`
}

// ValidateProposal validates the structured proposal and applies conservative
// fallback. Unknown resources, unmapped paths, non-high confidence, or any
// ambiguity produce a repository-exclusive result instead of parallel safety.
func ValidateProposal(cfg config.Config, proposal Proposal) (Report, error) {
	report := Report{
		Repository: cfg.GitHub.Repo, IssueNumber: proposal.IssueNumber, Mode: "preview",
		Resources: []string{}, Dependencies: []int{}, DependencySnapshots: []DependencySnapshot{},
		FallbackReasons: []string{}, Errors: []string{},
	}
	if proposal.Version != ProposalVersion {
		report.Errors = append(report.Errors, fmt.Sprintf("proposal version must be %d", ProposalVersion))
	}
	if proposal.IssueNumber < 1 {
		report.Errors = append(report.Errors, "proposal issue_number must be positive")
	}
	if proposal.Resources == nil {
		report.Errors = append(report.Errors, "proposal resources is required")
	}
	if proposal.Dependencies == nil {
		report.Errors = append(report.Errors, "proposal dependencies is required")
	}
	if proposal.AmbiguityReasons == nil {
		report.Errors = append(report.Errors, "proposal ambiguity_reasons is required")
	}
	if proposal.Confidence != "high" && proposal.Confidence != "medium" && proposal.Confidence != "low" {
		report.Errors = append(report.Errors, "proposal confidence must be high, medium, or low")
	}

	known := map[string]bool{}
	for _, definition := range cfg.Resources.Definitions {
		known[strings.ToLower(definition.Name)] = true
	}
	resourceSet := map[string]bool{}
	pathSet := map[string]bool{}
	for index, candidate := range proposal.Resources {
		name := strings.ToLower(candidate.Name)
		candidatePaths := map[string]bool{}
		if strings.TrimSpace(candidate.Reason) == "" {
			report.Errors = append(report.Errors, fmt.Sprintf("resource candidate %d reason is required", index))
		}
		if candidate.Name == "" || candidate.Name != name || !known[name] {
			report.FallbackReasons = append(report.FallbackReasons, "resource_unknown")
			continue
		}
		if resourceSet[name] {
			report.Errors = append(report.Errors, fmt.Sprintf("duplicate resource candidate %q", name))
		}
		resourceSet[name] = true
		if len(candidate.Paths) == 0 {
			report.FallbackReasons = append(report.FallbackReasons, "resource_paths_missing")
		}
		for _, path := range candidate.Paths {
			if path == "" || candidatePaths[path] {
				report.Errors = append(report.Errors, fmt.Sprintf("empty or duplicate proposed path %q", path))
				continue
			}
			candidatePaths[path] = true
			pathSet[path] = true
			actual, err := admission.ResourcesForPaths(cfg.AdmissionSettings(), []string{path})
			if err != nil {
				return report, err
			}
			if len(actual) == 1 && actual[0] == admission.RepositoryResource {
				report.FallbackReasons = append(report.FallbackReasons, "path_unmapped")
				continue
			}
			found := false
			for _, value := range actual {
				if value == name {
					found = true
				}
			}
			if !found {
				report.FallbackReasons = append(report.FallbackReasons, "resource_path_mismatch")
			}
		}
	}
	// Every taxonomy match for a proposed path must be claimed, including
	// overlapping definitions such as docs plus config.
	for path := range pathSet {
		actual, err := admission.ResourcesForPaths(cfg.AdmissionSettings(), []string{path})
		if err != nil {
			return report, err
		}
		for _, value := range actual {
			if value != admission.RepositoryResource && !resourceSet[value] {
				report.FallbackReasons = append(report.FallbackReasons, "overlapping_resource_missing")
			}
		}
	}
	if len(resourceSet) == 0 {
		report.FallbackReasons = append(report.FallbackReasons, "resource_candidates_missing")
	}

	dependencySet := map[int]bool{}
	for index, candidate := range proposal.Dependencies {
		if strings.TrimSpace(candidate.Reason) == "" {
			report.Errors = append(report.Errors, fmt.Sprintf("dependency candidate %d reason is required", index))
		}
		if candidate.IssueNumber < 1 || candidate.IssueNumber == proposal.IssueNumber {
			report.Errors = append(report.Errors, fmt.Sprintf("invalid dependency Issue #%d", candidate.IssueNumber))
			continue
		}
		if dependencySet[candidate.IssueNumber] {
			report.Errors = append(report.Errors, fmt.Sprintf("duplicate dependency Issue #%d", candidate.IssueNumber))
			continue
		}
		dependencySet[candidate.IssueNumber] = true
	}
	for number := range dependencySet {
		report.Dependencies = append(report.Dependencies, number)
	}
	sort.Ints(report.Dependencies)

	for index, reason := range proposal.AmbiguityReasons {
		if strings.TrimSpace(reason) == "" {
			report.Errors = append(report.Errors, fmt.Sprintf("ambiguity reason %d is empty", index))
		}
	}
	if proposal.Confidence != "high" {
		report.FallbackReasons = append(report.FallbackReasons, "confidence_not_high")
	}
	if len(proposal.AmbiguityReasons) > 0 {
		report.FallbackReasons = append(report.FallbackReasons, "issue_ambiguous")
	}
	if proposal.Exclusive {
		if strings.TrimSpace(proposal.ExclusiveReason) == "" {
			report.Errors = append(report.Errors, "proposal exclusive_reason is required when exclusive is true")
		}
		report.FallbackReasons = append(report.FallbackReasons, "exclusive_recommended")
	} else if strings.TrimSpace(proposal.ExclusiveReason) != "" {
		report.Errors = append(report.Errors, "proposal exclusive_reason must be empty when exclusive is false")
	}
	report.FallbackReasons = normalizedStrings(report.FallbackReasons)
	report.Exclusive = len(report.FallbackReasons) > 0
	if !report.Exclusive {
		for resource := range resourceSet {
			report.Resources = append(report.Resources, resource)
		}
		sort.Strings(report.Resources)
	}
	report.Valid = len(report.Errors) == 0
	sort.Strings(report.Errors)
	return report, nil
}

// Audit validates metadata already persisted on GitHub. A deliberate
// resource_claim_missing fallback is valid exclusive metadata; malformed or
// unknown persisted claims are not ready-safe.
func Audit(cfg config.Config, issue gh.Issue, dependencies []DependencySnapshot, configBytes []byte) (Report, error) {
	evaluation, err := admission.EvaluateCandidate(cfg.AdmissionSettings(), admission.Candidate{
		Number: issue.Number, CreatedAt: issue.CreatedAt, Labels: issue.Labels, Body: issue.Body,
	})
	if err != nil {
		return Report{}, err
	}
	report := Report{
		Repository: cfg.GitHub.Repo, IssueNumber: issue.Number, Mode: "audit", Resources: append([]string(nil), evaluation.Resources...),
		Dependencies: append([]int{}, evaluation.Dependencies...), DependencySnapshots: append([]DependencySnapshot{}, dependencies...),
		FallbackReasons: []string{}, Errors: append([]string(nil), evaluation.Errors...),
	}
	report.Ready = hasAllLabels(issue.Labels, cfg.GitHub.ReadyLabels)
	report.Exclusive = len(evaluation.Resources) == 1 && evaluation.Resources[0] == admission.RepositoryResource
	switch evaluation.FallbackReason {
	case "":
		report.Valid = true
	case admission.FallbackResourceMissing:
		report.Valid = true
		report.FallbackReasons = []string{evaluation.FallbackReason}
		report.Errors = []string{}
	default:
		report.FallbackReasons = []string{evaluation.FallbackReason}
	}
	for _, dependency := range dependencies {
		if dependency.State == "" {
			report.Valid = false
			report.Errors = append(report.Errors, fmt.Sprintf("dependency Issue #%d does not exist or is inaccessible", dependency.IssueNumber))
		}
	}
	report.SnapshotSHA256 = snapshotDigest(configBytes, issue, dependencies)
	report.Errors = normalizedStrings(report.Errors)
	return report, nil
}

func MetadataBody(body string, dependencies []int) (string, error) {
	dependencies = append([]int(nil), dependencies...)
	sort.Ints(dependencies)
	lines := strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n")
	start, end := -1, -1
	for index, line := range lines {
		if line != "<!-- agent-loop:metadata" {
			continue
		}
		if start >= 0 {
			return "", fmt.Errorf("Issue body contains multiple metadata blocks")
		}
		start = index
		for next := index + 1; next < len(lines); next++ {
			if lines[next] == "-->" {
				end = next
				break
			}
		}
		if end < 0 {
			return "", fmt.Errorf("Issue body metadata block is not terminated")
		}
	}
	block := []string{"<!-- agent-loop:metadata", "version: 1"}
	if len(dependencies) == 0 {
		block = append(block, "depends_on: []")
	} else {
		block = append(block, "depends_on:")
		for _, number := range dependencies {
			block = append(block, fmt.Sprintf("  - %d", number))
		}
	}
	block = append(block, "-->")
	if start >= 0 {
		lines = append(append(append([]string{}, lines[:start]...), block...), lines[end+1:]...)
		return strings.Join(lines, "\n"), nil
	}
	trimmed := strings.TrimRight(strings.Join(lines, "\n"), "\n")
	if trimmed == "" {
		return strings.Join(block, "\n") + "\n", nil
	}
	return trimmed + "\n\n" + strings.Join(block, "\n") + "\n", nil
}

func normalizedStrings(values []string) []string {
	set := map[string]bool{}
	for _, value := range values {
		if value != "" {
			set[value] = true
		}
	}
	result := make([]string, 0, len(set))
	for value := range set {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func hasAllLabels(labels, required []string) bool {
	set := map[string]bool{}
	for _, label := range labels {
		set[label] = true
	}
	for _, label := range required {
		if !set[label] {
			return false
		}
	}
	return true
}

func snapshotDigest(configBytes []byte, issue gh.Issue, dependencies []DependencySnapshot) string {
	labels := normalizedStrings(issue.Labels)
	deps := append([]DependencySnapshot(nil), dependencies...)
	sort.Slice(deps, func(i, j int) bool { return deps[i].IssueNumber < deps[j].IssueNumber })
	canonical, _ := json.Marshal(struct {
		Config       string               `json:"config_sha256"`
		IssueNumber  int                  `json:"issue_number"`
		Body         string               `json:"body"`
		Labels       []string             `json:"labels"`
		Dependencies []DependencySnapshot `json:"dependencies"`
	}{hex.EncodeToString(sum(configBytes)), issue.Number, issue.Body, labels, deps})
	digest := sha256.Sum256(canonical)
	return hex.EncodeToString(digest[:])
}

func sum(value []byte) []byte {
	digest := sha256.Sum256(value)
	return digest[:]
}
