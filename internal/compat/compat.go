package compat

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

const (
	MinimumCodexVersion = "0.136.0"
	MinimumGHVersion    = "2.69.0"
)

type Report struct {
	Tool         string          `json:"tool"`
	Version      string          `json:"version,omitempty"`
	Minimum      string          `json:"minimum"`
	VersionOK    bool            `json:"version_ok"`
	Capabilities map[string]bool `json:"capabilities"`
	Missing      []string        `json:"missing,omitempty"`
	Detail       string          `json:"detail,omitempty"`
}

func (r Report) OK() bool { return r.VersionOK && len(r.Missing) == 0 }

func (r Report) Has(name string) bool { return r.Capabilities[name] }

func ProbeCodex(ctx context.Context, path string) Report {
	if path == "" {
		path = "codex"
	}
	report := Report{Tool: "codex", Minimum: MinimumCodexVersion, Capabilities: map[string]bool{}}
	versionOutput, err := exec.CommandContext(ctx, path, "--version").CombinedOutput()
	if err != nil {
		report.Detail = safeDetail(versionOutput, err)
		report.Missing = []string{"version"}
		return report
	}
	report.Version = parseVersion(string(versionOutput))
	report.VersionOK = AtLeast(report.Version, report.Minimum)

	execHelp, execErr := exec.CommandContext(ctx, path, "exec", "--help").CombinedOutput()
	resumeHelp, resumeErr := exec.CommandContext(ctx, path, "exec", "resume", "--help").CombinedOutput()
	base := execErr == nil && containsAll(string(execHelp), "--json", "--output-schema", "--output-last-message", "--sandbox", "--cd")
	resume := resumeErr == nil && containsAll(string(resumeHelp), "--json", "--output-schema", "--output-last-message")
	report.Capabilities["exec_structured"] = base
	report.Capabilities["session_resume"] = resume
	report.Capabilities["session_event_thread_id"] = true // Accepted by the tolerant JSONL parser.
	if !base {
		report.Missing = append(report.Missing, "exec_structured")
	}
	if !report.VersionOK {
		report.Missing = append(report.Missing, "minimum_version")
	}
	if execErr != nil || resumeErr != nil {
		report.Detail = safeDetail(append(execHelp, resumeHelp...), firstError(execErr, resumeErr))
	}
	return report
}

func ProbeGH(ctx context.Context, path string) Report {
	if path == "" {
		path = "gh"
	}
	report := Report{Tool: "gh", Minimum: MinimumGHVersion, Capabilities: map[string]bool{}}
	versionOutput, err := exec.CommandContext(ctx, path, "--version").CombinedOutput()
	if err != nil {
		report.Detail = safeDetail(versionOutput, err)
		report.Missing = []string{"version"}
		return report
	}
	report.Version = parseVersion(string(versionOutput))
	report.VersionOK = AtLeast(report.Version, report.Minimum)

	listHelp, listErr := exec.CommandContext(ctx, path, "issue", "list", "--help").CombinedOutput()
	editHelp, editErr := exec.CommandContext(ctx, path, "issue", "edit", "--help").CombinedOutput()
	commentHelp, commentErr := exec.CommandContext(ctx, path, "issue", "comment", "--help").CombinedOutput()
	report.Capabilities["issue_json_pagination"] = listErr == nil && containsAll(string(listHelp), "--json", "--limit", "--label", "--assignee", "--milestone")
	report.Capabilities["issue_label_edit"] = editErr == nil && containsAll(string(editHelp), "--add-label", "--remove-label")
	report.Capabilities["issue_comment"] = commentErr == nil && containsAll(string(commentHelp), "--body")
	for _, name := range []string{"issue_json_pagination", "issue_label_edit", "issue_comment"} {
		if !report.Capabilities[name] {
			report.Missing = append(report.Missing, name)
		}
	}
	if !report.VersionOK {
		report.Missing = append(report.Missing, "minimum_version")
	}
	if listErr != nil || editErr != nil || commentErr != nil {
		report.Detail = safeDetail(append(append(listHelp, editHelp...), commentHelp...), firstError(listErr, editErr, commentErr))
	}
	return report
}

var versionPattern = regexp.MustCompile(`\b([0-9]+)\.([0-9]+)\.([0-9]+)(?:[-+][0-9A-Za-z.-]+)?\b`)

func parseVersion(value string) string {
	match := versionPattern.FindStringSubmatch(value)
	if len(match) == 0 {
		return ""
	}
	return strings.Join(match[1:4], ".")
}

func AtLeast(current, minimum string) bool {
	currentParts, currentOK := numericVersion(current)
	minimumParts, minimumOK := numericVersion(minimum)
	if !currentOK || !minimumOK {
		return false
	}
	for index := range currentParts {
		if currentParts[index] != minimumParts[index] {
			return currentParts[index] > minimumParts[index]
		}
	}
	return true
}

func numericVersion(value string) ([3]int, bool) {
	var result [3]int
	parts := strings.Split(value, ".")
	if len(parts) != 3 {
		return result, false
	}
	for index, part := range parts {
		parsed, err := strconv.Atoi(part)
		if err != nil {
			return result, false
		}
		result[index] = parsed
	}
	return result, true
}

func containsAll(value string, required ...string) bool {
	for _, item := range required {
		if !strings.Contains(value, item) {
			return false
		}
	}
	return true
}

func safeDetail(output []byte, err error) string {
	value := strings.TrimSpace(string(output))
	if len(value) > 300 {
		value = value[:300] + "..."
	}
	if err != nil {
		return fmt.Sprintf("%v: %s", err, value)
	}
	return value
}

func firstError(errors ...error) error {
	for _, err := range errors {
		if err != nil {
			return err
		}
	}
	return nil
}
