// Package capability defines the versioned, deterministic contract between an
// Issue and the worker profile selected before admission. It deliberately
// contains no filesystem, clock, environment, credential, or network reads.
package capability

import (
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

const ContractVersion = 1

const (
	NetworkNone      = "none"
	NetworkLocalhost = "localhost"
	NetworkPublic    = "public"
)

const (
	CodeMetadataMissing      = "capability_metadata_missing"
	CodeMetadataInvalid      = "capability_metadata_invalid"
	CodeProfileUnknown       = "capability_profile_unknown"
	CodeNetworkMismatch      = "capability_network_mismatch"
	CodeBrowserCDPMismatch   = "capability_browser_cdp_mismatch"
	CodeDownloadMismatch     = "capability_download_mismatch"
	CodeExternalTimeMismatch = "capability_external_time_gate_mismatch"
	CodeWorkerProfileDrift   = "worker_profile_launch_mismatch"
)

type Requirements struct {
	Version          int    `json:"version"`
	Profile          string `json:"profile"`
	Network          string `json:"network"`
	BrowserCDP       bool   `json:"browser_cdp"`
	Download         bool   `json:"download"`
	ExternalTimeGate bool   `json:"external_time_gate"`
}

// Provider is safe to persist and display. It describes capability names and
// booleans only; credential names and values are intentionally absent.
type Provider struct {
	Version          int    `json:"version"`
	Profile          string `json:"profile"`
	Network          string `json:"network"`
	BrowserCDP       bool   `json:"browser_cdp"`
	Download         bool   `json:"download"`
	ExternalTimeGate bool   `json:"external_time_gate"`
}

type Mismatch struct {
	Code     string `json:"code"`
	Field    string `json:"field"`
	Required any    `json:"required,omitempty"`
	Provided any    `json:"provided,omitempty"`
	Detail   string `json:"detail"`
}

type Evaluation struct {
	Compatible   bool          `json:"compatible"`
	Requirements *Requirements `json:"requirements,omitempty"`
	Provided     *Provider     `json:"provided,omitempty"`
	Mismatches   []Mismatch    `json:"mismatches"`
}

// Parse reads exactly one strict capability block. Metadata absence, unknown
// keys, unknown versions/capabilities, YAML aliases, and custom tags all fail
// closed with stable machine-readable codes.
func Parse(body string) (*Requirements, []Mismatch) {
	body = strings.ReplaceAll(body, "\r\n", "\n")
	lines := strings.Split(body, "\n")
	starts := []int{}
	for index, line := range lines {
		if line == "<!-- agent-loop:capabilities" {
			starts = append(starts, index)
		}
	}
	if len(starts) == 0 {
		return nil, []Mismatch{invalid(CodeMetadataMissing, "metadata", "capability metadata block is missing")}
	}
	if len(starts) != 1 {
		return nil, []Mismatch{invalid(CodeMetadataInvalid, "metadata", "multiple capability metadata blocks")}
	}
	end := -1
	for index := starts[0] + 1; index < len(lines); index++ {
		if lines[index] == "-->" {
			end = index
			break
		}
	}
	if end < 0 {
		return nil, []Mismatch{invalid(CodeMetadataInvalid, "metadata", "capability metadata block is not terminated")}
	}
	var document yaml.Node
	if err := yaml.Unmarshal([]byte(strings.Join(lines[starts[0]+1:end], "\n")), &document); err != nil {
		return nil, []Mismatch{invalid(CodeMetadataInvalid, "metadata", "capability metadata YAML is invalid")}
	}
	if len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return nil, []Mismatch{invalid(CodeMetadataInvalid, "metadata", "capability metadata must be a mapping")}
	}
	mapping := document.Content[0]
	if containsUnsafeYAML(mapping) {
		return nil, []Mismatch{invalid(CodeMetadataInvalid, "metadata", "capability metadata aliases and custom tags are not allowed")}
	}
	allowed := map[string]bool{"version": true, "profile": true, "network": true, "browser_cdp": true, "download": true, "external_time_gate": true}
	values := map[string]*yaml.Node{}
	errors := []string{}
	for index := 0; index+1 < len(mapping.Content); index += 2 {
		key, value := mapping.Content[index], mapping.Content[index+1]
		if key.Kind != yaml.ScalarNode || key.Tag != "!!str" {
			errors = append(errors, "metadata key must be a string")
			continue
		}
		if !allowed[key.Value] {
			errors = append(errors, "unknown capability key "+key.Value)
			continue
		}
		if values[key.Value] != nil {
			errors = append(errors, "duplicate capability key "+key.Value)
			continue
		}
		values[key.Value] = value
	}
	requirement := &Requirements{}
	if !decodeInteger(values["version"], &requirement.Version) || requirement.Version != ContractVersion {
		errors = append(errors, fmt.Sprintf("capability version must be unquoted integer %d", ContractVersion))
	}
	if !decodeString(values["profile"], &requirement.Profile) || !validProfile(requirement.Profile) {
		errors = append(errors, "capability profile must be standard or extended")
	}
	if !decodeString(values["network"], &requirement.Network) || !validNetwork(requirement.Network) {
		errors = append(errors, "capability network must be none, localhost, or public")
	}
	if !decodeBool(values["browser_cdp"], &requirement.BrowserCDP) {
		errors = append(errors, "capability browser_cdp must be a boolean")
	}
	if !decodeBool(values["download"], &requirement.Download) {
		errors = append(errors, "capability download must be a boolean")
	}
	if !decodeBool(values["external_time_gate"], &requirement.ExternalTimeGate) {
		errors = append(errors, "capability external_time_gate must be a boolean")
	}
	if len(errors) > 0 {
		sort.Strings(errors)
		mismatches := make([]Mismatch, 0, len(errors))
		for _, detail := range errors {
			mismatches = append(mismatches, invalid(CodeMetadataInvalid, "metadata", detail))
		}
		return nil, mismatches
	}
	return requirement, nil
}

func Evaluate(body string, profiles map[string]Provider) Evaluation {
	requirement, mismatches := Parse(body)
	if requirement == nil {
		return Evaluation{Requirements: nil, Mismatches: mismatches}
	}
	return evaluateRequirement(requirement, profiles, mismatches)
}

// EvaluateRequirement rechecks persisted, already-validated requirements
// against the current worker launch profile without reparsing mutable Issue
// text. Resumes and retries therefore use the same predicate as admission.
func EvaluateRequirement(requirement *Requirements, profiles map[string]Provider) Evaluation {
	if requirement == nil {
		return Evaluation{Mismatches: []Mismatch{invalid(CodeMetadataMissing, "metadata", "persisted capability requirements are missing")}}
	}
	return evaluateRequirement(requirement, profiles, nil)
}

func evaluateRequirement(requirement *Requirements, profiles map[string]Provider, mismatches []Mismatch) Evaluation {
	result := Evaluation{Requirements: requirement, Mismatches: mismatches}
	if requirement == nil {
		return result
	}
	provided, ok := profiles[requirement.Profile]
	if !ok {
		result.Mismatches = append(result.Mismatches, Mismatch{Code: CodeProfileUnknown, Field: "profile", Required: requirement.Profile, Detail: "selected worker profile is not configured"})
		return result
	}
	copy := provided
	result.Provided = &copy
	if !networkSatisfies(provided.Network, requirement.Network) {
		result.Mismatches = append(result.Mismatches, Mismatch{Code: CodeNetworkMismatch, Field: "network", Required: requirement.Network, Provided: provided.Network, Detail: "worker network scope does not satisfy the Issue requirement"})
	}
	if requirement.BrowserCDP && !provided.BrowserCDP {
		result.Mismatches = append(result.Mismatches, Mismatch{Code: CodeBrowserCDPMismatch, Field: "browser_cdp", Required: true, Provided: false, Detail: "worker profile does not provide browser/CDP"})
	}
	if requirement.Download && !provided.Download {
		result.Mismatches = append(result.Mismatches, Mismatch{Code: CodeDownloadMismatch, Field: "download", Required: true, Provided: false, Detail: "worker profile does not provide downloads"})
	}
	if requirement.ExternalTimeGate && !provided.ExternalTimeGate {
		result.Mismatches = append(result.Mismatches, Mismatch{Code: CodeExternalTimeMismatch, Field: "external_time_gate", Required: true, Provided: false, Detail: "worker profile cannot satisfy an external time gate"})
	}
	result.Compatible = len(result.Mismatches) == 0
	return result
}

func ProfileDrift(configured, launched Provider) []Mismatch {
	result := []Mismatch{}
	appendMismatch := func(field string, left, right any) {
		result = append(result, Mismatch{Code: CodeWorkerProfileDrift, Field: field, Required: left, Provided: right, Detail: "configured worker profile exceeds the effective launch path"})
	}
	if configured.Network != "" && !networkSatisfies(launched.Network, configured.Network) {
		appendMismatch("network", configured.Network, launched.Network)
	}
	if configured.BrowserCDP && !launched.BrowserCDP {
		appendMismatch("browser_cdp", true, false)
	}
	if configured.Download && !launched.Download {
		appendMismatch("download", true, false)
	}
	// external_time_gate is an operator-provided orchestration property and has
	// no secret-bearing or argv-derived representation to compare.
	return result
}

func networkSatisfies(provided, required string) bool {
	if provided == required {
		return true
	}
	return provided == NetworkPublic && (required == NetworkLocalhost || required == NetworkNone) || provided == NetworkLocalhost && required == NetworkNone
}

func validNetwork(value string) bool {
	return value == NetworkNone || value == NetworkLocalhost || value == NetworkPublic
}
func validProfile(value string) bool { return value == "standard" || value == "extended" }

func decodeString(node *yaml.Node, target *string) bool {
	return node != nil && node.Kind == yaml.ScalarNode && node.Tag == "!!str" && node.Style == 0 && node.Decode(target) == nil && *target != ""
}

func decodeInteger(node *yaml.Node, target *int) bool {
	return node != nil && node.Kind == yaml.ScalarNode && node.Tag == "!!int" && node.Style == 0 && node.Decode(target) == nil
}

func decodeBool(node *yaml.Node, target *bool) bool {
	return node != nil && node.Kind == yaml.ScalarNode && node.Tag == "!!bool" && node.Style == 0 && node.Decode(target) == nil
}

func containsUnsafeYAML(node *yaml.Node) bool {
	if node.Kind == yaml.AliasNode || strings.HasPrefix(node.Tag, "!") && !strings.HasPrefix(node.Tag, "!!") {
		return true
	}
	for _, child := range node.Content {
		if containsUnsafeYAML(child) {
			return true
		}
	}
	return false
}

func invalid(code, field, detail string) Mismatch {
	return Mismatch{Code: code, Field: field, Detail: detail}
}
