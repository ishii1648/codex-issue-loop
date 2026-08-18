package compat

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

const (
	MinimumCodexVersion      = "0.136.0"
	MinimumClaudeCodeVersion = "2.1.119"
	MinimumOpenCodeVersion   = "1.1.1"
	MinimumGHVersion         = "2.69.0"
)

func ProbeBackend(ctx context.Context, backend, path string) Report {
	switch backend {
	case "", "codex":
		return ProbeCodex(ctx, path)
	case "claude-code":
		return ProbeClaudeCode(ctx, path)
	case "opencode":
		return ProbeOpenCode(ctx, path)
	default:
		return Report{Tool: backend, Capabilities: map[string]bool{}, Missing: []string{"known_backend"}, Detail: "unknown backend"}
	}
}

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
	// Probe the exact parent-option placement used by the adapter. --cd belongs
	// to `exec`, not `exec resume`, and must be accepted before the subcommand.
	resumeHelp, resumeErr := exec.CommandContext(ctx, path, "exec", "--cd", ".", "resume", "--help").CombinedOutput()
	features, featuresErr := exec.CommandContext(ctx, path, "features", "list").CombinedOutput()
	base := execErr == nil && containsAll(string(execHelp), "--json", "--output-schema", "--output-last-message", "--sandbox", "--cd")
	resume := resumeErr == nil && containsAll(string(resumeHelp), "--json", "--output-schema", "--output-last-message")
	report.Capabilities["exec_structured"] = base
	report.Capabilities["session_resume"] = resume
	report.Capabilities["session_event_thread_id"] = true // Accepted by the tolerant JSONL parser.
	report.Capabilities["app_server_goal"] = probeCodexAppServerGoal(ctx, path)
	report.Capabilities["localhost_network_proxy"] = execErr == nil && featuresErr == nil &&
		containsAll(string(execHelp), "--ignore-user-config", "--strict-config", "--disable") &&
		containsAll(string(features), "network_proxy", "apps", "browser_use", "computer_use", "plugins", "remote_plugin", "skill_search", "tool_suggest")
	if !base {
		report.Missing = append(report.Missing, "exec_structured")
	}
	if !report.VersionOK {
		report.Missing = append(report.Missing, "minimum_version")
	}
	if execErr != nil || resumeErr != nil || featuresErr != nil {
		report.Detail = safeDetail(append(append(execHelp, resumeHelp...), features...), firstError(execErr, resumeErr, featuresErr))
	}
	return report
}

func probeCodexAppServerGoal(ctx context.Context, path string) bool {
	help, err := exec.CommandContext(ctx, path, "app-server", "generate-json-schema", "--help").CombinedOutput()
	if err != nil || !containsAll(string(help), "--out", "--experimental") {
		return false
	}
	dir, err := os.MkdirTemp("", "agent-loop-codex-app-server-schema-")
	if err != nil {
		return false
	}
	defer os.RemoveAll(dir)
	if _, err := exec.CommandContext(ctx, path, "app-server", "generate-json-schema", "--experimental", "--out", dir).CombinedOutput(); err != nil {
		return false
	}
	client, err := os.ReadFile(filepath.Join(dir, "ClientRequest.json"))
	if err != nil {
		return false
	}
	serverRequests, err := os.ReadFile(filepath.Join(dir, "ServerRequest.json"))
	if err != nil {
		return false
	}
	notifications, err := os.ReadFile(filepath.Join(dir, "ServerNotification.json"))
	if err != nil {
		return false
	}
	return containsAll(string(client), "thread/start", "thread/resume", "thread/goal/set", "thread/goal/get", "thread/goal/clear", "turn/start", "turn/steer") &&
		containsAll(string(serverRequests), "item/tool/requestUserInput", "item/commandExecution/requestApproval", "item/fileChange/requestApproval") &&
		containsAll(string(notifications), "thread/tokenUsage/updated", "turn/completed")
}

func ProbeClaudeCode(ctx context.Context, path string) Report {
	if path == "" {
		path = "claude"
	}
	report := Report{Tool: "claude-code", Minimum: MinimumClaudeCodeVersion, Capabilities: map[string]bool{}}
	versionOutput, err := exec.CommandContext(ctx, path, "--version").CombinedOutput()
	if err != nil {
		report.Detail = safeDetail(versionOutput, err)
		report.Missing = []string{"version"}
		return report
	}
	report.Version = parseVersion(string(versionOutput))
	report.VersionOK = AtLeast(report.Version, report.Minimum)
	help, helpErr := exec.CommandContext(ctx, path, "--help").CombinedOutput()
	value := string(help)
	report.Capabilities["structured_output"] = helpErr == nil && containsAll(value, "--output-format", "--json-schema")
	report.Capabilities["session_resume"] = helpErr == nil && strings.Contains(value, "--resume")
	report.Capabilities["model_selection"] = helpErr == nil && strings.Contains(value, "--model")
	report.Capabilities["variant_selection"] = helpErr == nil && strings.Contains(value, "--effort")
	report.Capabilities["non_interactive_policy"] = helpErr == nil && containsAll(value, "--permission-mode", "--settings", "--disallowedTools", "--strict-mcp-config", "--mcp-config")
	report.Capabilities["workspace_isolation"] = report.VersionOK // sandbox.failIfUnavailable is passed by the adapter.
	for _, name := range []string{"structured_output", "model_selection", "non_interactive_policy", "workspace_isolation"} {
		if !report.Capabilities[name] {
			report.Missing = append(report.Missing, name)
		}
	}
	if !report.VersionOK {
		report.Missing = append(report.Missing, "minimum_version")
	}
	if helpErr != nil {
		report.Detail = safeDetail(help, helpErr)
	}
	return report
}

func ProbeOpenCode(ctx context.Context, path string) Report {
	if path == "" {
		path = "opencode"
	}
	report := Report{Tool: "opencode", Minimum: MinimumOpenCodeVersion, Capabilities: map[string]bool{}}
	versionOutput, err := exec.CommandContext(ctx, path, "--version").CombinedOutput()
	if err != nil {
		report.Detail = safeDetail(versionOutput, err)
		report.Missing = []string{"version"}
		return report
	}
	report.Version = parseVersion(string(versionOutput))
	report.VersionOK = AtLeast(report.Version, report.Minimum)
	serveHelp, serveErr := exec.CommandContext(ctx, path, "serve", "--help").CombinedOutput()
	modelsHelp, modelsErr := exec.CommandContext(ctx, path, "models", "--help").CombinedOutput()
	globalHelp, globalErr := exec.CommandContext(ctx, path, "--help").CombinedOutput()
	report.Capabilities["structured_output"] = serveErr == nil // Adapter requests JSON Schema through the server message API.
	report.Capabilities["session_resume"] = serveErr == nil
	report.Capabilities["model_selection"] = modelsErr == nil
	report.Capabilities["variant_selection"] = serveErr == nil
	report.Capabilities["non_interactive_policy"] = report.VersionOK && globalErr == nil && strings.Contains(string(globalHelp), "--pure") // OPENCODE_CONFIG_CONTENT deny policy.
	report.Capabilities["workspace_isolation"] = report.VersionOK                                                                          // Application-enforced external_directory deny.
	for _, name := range []string{"structured_output", "session_resume", "model_selection", "variant_selection", "non_interactive_policy", "workspace_isolation"} {
		if !report.Capabilities[name] {
			report.Missing = append(report.Missing, name)
		}
	}
	if !report.VersionOK {
		report.Missing = append(report.Missing, "minimum_version")
	}
	if serveErr != nil || modelsErr != nil || globalErr != nil {
		report.Detail = safeDetail(append(append(serveHelp, modelsHelp...), globalHelp...), firstError(serveErr, modelsErr, globalErr))
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

// ProbeGofmt verifies the fixed stdin formatting contract without reading or
// writing repository files. It deliberately accepts no repository-provided
// arguments or source.
func ProbeGofmt(ctx context.Context, path string) error {
	if path == "" {
		return fmt.Errorf("gofmt path is missing")
	}
	command := exec.CommandContext(ctx, path)
	command.Stdin = strings.NewReader("package probe\nfunc f( ){}\n")
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("gofmt capability probe: %s", safeDetail(output, err))
	}
	if string(output) != "package probe\n\nfunc f() {}\n" {
		return fmt.Errorf("gofmt capability probe returned unexpected output")
	}
	return nil
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
