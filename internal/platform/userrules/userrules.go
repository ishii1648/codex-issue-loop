// Package userrules plans and applies the user-scoped instructions used to
// delegate implementation work to codex-issue-loop.
package userrules

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/ishii1648/codex-issue-loop/internal/platform/fsutil"
)

const (
	SchemaVersion = 1
	RuleVersion   = 1
)

type Agent string

const (
	AgentCodex  Agent = "codex"
	AgentClaude Agent = "claude"
)

type Status string

const (
	StatusMissing  Status = "missing"
	StatusCurrent  Status = "current"
	StatusOutdated Status = "outdated"
	StatusConflict Status = "conflict"
)

type Action string

const (
	ActionCreate Action = "create"
	ActionUpdate Action = "update"
	ActionNone   Action = "none"
)

type ApplyResult string

const (
	ResultNotApplied ApplyResult = "not_applied"
	ResultCreated    ApplyResult = "created"
	ResultUpdated    ApplyResult = "updated"
	ResultUnchanged  ApplyResult = "unchanged"
	ResultSkipped    ApplyResult = "skipped"
	ResultFailed     ApplyResult = "failed"
)

// Config contains resolved locations. Constructing it and calling Plan are
// read-only; directories are created only by Apply.
type Config struct {
	HomeDir         string
	CodexHome       string
	ClaudeConfigDir string
	BackupRoot      string
	Now             func() time.Time
}

// ConfigFromEnvironment resolves the same user-level locations as Codex and
// Claude Code without creating them.
func ConfigFromEnvironment() (Config, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Config{}, fmt.Errorf("resolve user home: %w", err)
	}
	codexHome := os.Getenv("CODEX_HOME")
	if codexHome == "" {
		codexHome = filepath.Join(home, ".codex")
	}
	claudeDir := os.Getenv("CLAUDE_CONFIG_DIR")
	if claudeDir == "" {
		claudeDir = filepath.Join(home, ".claude")
	}
	agentLoopHome := os.Getenv("AGENT_LOOP_HOME")
	if agentLoopHome == "" {
		agentLoopHome = filepath.Join(home, "Library", "Application Support", "codex-issue-loop")
	}
	return Config{
		HomeDir:         home,
		CodexHome:       codexHome,
		ClaudeConfigDir: claudeDir,
		BackupRoot:      filepath.Join(agentLoopHome, "user-rules-backups"),
		Now:             time.Now,
	}, nil
}

type Target struct {
	Agent        Agent       `json:"agent"`
	Path         string      `json:"path"`
	ResolvedPath string      `json:"resolved_path"`
	Symlink      bool        `json:"symlink"`
	Status       Status      `json:"status"`
	Action       Action      `json:"action"`
	Applied      bool        `json:"applied"`
	ApplyResult  ApplyResult `json:"apply_result"`
	BackupPath   string      `json:"backup_path"`
	Detail       string      `json:"detail,omitempty"`

	desired       []byte
	originalHash  [sha256.Size]byte
	originalMode  os.FileMode
	originalExist bool
	writePath     string
}

type Report struct {
	SchemaVersion int      `json:"schema_version"`
	RuleVersion   int      `json:"rule_version"`
	Apply         bool     `json:"apply"`
	Changed       bool     `json:"changed"`
	Targets       []Target `json:"targets"`
}

var startMarkerPattern = regexp.MustCompile(`<!-- agent-loop:rules:start version=([0-9]+) -->`)

const (
	startMarker = "<!-- agent-loop:rules:start version=1 -->"
	endMarker   = "<!-- agent-loop:rules:end -->"
)

const ruleBody = "# codex-issue-loop Issue作成ルール\n\n" +
	"- 変更依頼では、最初に対象repositoryを確定し、そのrootまたはdefault branchにある `.agent-loop.yaml` を確認する。\n" +
	"- `.agent-loop.yaml` があり、現在のセッションがagent-loop implementation workerでなければ、自ら実装しない。open Issueとの重複を確認してからIssueを起票する。設定がなければ通常どおり作業する。\n" +
	"- Issue起票時は、対象repositoryの `.agent-loop.yaml` の `github.ready_labels` に設定されたready labelだけを使用する。作成後のIssueを再取得してlabelを確認する。不明または不足があれば推測やlabel作成をせず、その状態を報告する。\n" +
	"- agent-loop implementation workerは、割り当てられたIssueを実装し、同じ依頼のIssueを新たに起票しない。\n" +
	"- 説明、レビュー、調査などの読み取り専用タスクではIssueを起票しない。ユーザーがagent-loopを使わないよう明示した場合は、変更依頼を直接実装してよい。"

func ManagedBlock() string {
	return startMarker + "\n" + ruleBody + "\n" + endMarker
}

func ParseAgents(value string) ([]Agent, error) {
	if strings.TrimSpace(value) == "" {
		return nil, fmt.Errorf("--agents must contain codex, claude, or both")
	}
	seen := map[Agent]bool{}
	agents := make([]Agent, 0, 2)
	for _, raw := range strings.Split(value, ",") {
		agent := Agent(strings.ToLower(strings.TrimSpace(raw)))
		if agent != AgentCodex && agent != AgentClaude {
			return nil, fmt.Errorf("unsupported agent %q; expected codex or claude", raw)
		}
		if !seen[agent] {
			seen[agent] = true
			agents = append(agents, agent)
		}
	}
	return agents, nil
}

func Plan(config Config, agents []Agent) (Report, error) {
	if config.CodexHome == "" || config.ClaudeConfigDir == "" || config.BackupRoot == "" {
		return Report{}, fmt.Errorf("Codex, Claude Code, and backup locations must be configured")
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	stamp := config.Now().UTC().Format("20060102T150405.000000000Z")
	report := Report{SchemaVersion: SchemaVersion, RuleVersion: RuleVersion, Targets: make([]Target, 0, len(agents))}
	for _, agent := range agents {
		var target Target
		var err error
		switch agent {
		case AgentCodex:
			target, err = planCodex(config, stamp)
		case AgentClaude:
			target, err = planClaude(config, stamp)
		default:
			err = fmt.Errorf("unsupported agent %q", agent)
		}
		if err != nil {
			return Report{}, err
		}
		report.Targets = append(report.Targets, target)
	}
	return report, nil
}

func planCodex(config Config, stamp string) (Target, error) {
	path := filepath.Join(config.CodexHome, "AGENTS.md")
	detail := ""
	overridePath := filepath.Join(config.CodexHome, "AGENTS.override.md")
	if _, err := os.Lstat(overridePath); err == nil {
		data, readErr := os.ReadFile(overridePath)
		if readErr != nil || len(bytesTrimSpace(data)) > 0 {
			path = overridePath
			detail = "non-empty AGENTS.override.md takes precedence; the managed block is planned for the active override file"
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		path = overridePath
		detail = "AGENTS.override.md exists but could not be inspected"
	}
	target, data, err := inspectTarget(AgentCodex, path)
	if err != nil {
		return Target{}, err
	}
	if detail != "" {
		target.Detail = detail
	}
	if target.Status == StatusConflict {
		return target, nil
	}
	blockStart, blockEnd, markerVersion, markerStatus, markerDetail := inspectMarkers(data)
	if markerVersion > RuleVersion {
		target.Status, target.Action, target.ApplyResult = StatusConflict, ActionNone, ResultSkipped
		target.Detail = joinDetail(target.Detail, fmt.Sprintf("managed rule version %d is newer than supported version %d", markerVersion, RuleVersion))
		return target, nil
	}
	switch markerStatus {
	case StatusConflict:
		target.Status, target.Action, target.ApplyResult = StatusConflict, ActionNone, ResultSkipped
		target.Detail = joinDetail(target.Detail, markerDetail)
		return target, nil
	case StatusMissing:
		target.Status, target.Action = StatusMissing, ActionCreate
		if target.originalExist {
			target.Action = ActionUpdate
		}
		target.desired = appendCodexBlock(data)
	case StatusCurrent, StatusOutdated:
		existing := string(data[blockStart:blockEnd])
		if existing == ManagedBlock() {
			target.Status, target.Action, target.desired = StatusCurrent, ActionNone, data
			target.ApplyResult = ResultNotApplied
		} else {
			target.Status, target.Action = StatusOutdated, ActionUpdate
			target.desired = replaceRange(data, blockStart, blockEnd, []byte(ManagedBlock()))
		}
	}
	if target.Action == ActionUpdate {
		target.BackupPath = filepath.Join(config.BackupRoot, stamp, string(AgentCodex), filepath.Base(path))
	}
	return target, nil
}

func planClaude(config Config, stamp string) (Target, error) {
	path := filepath.Join(config.ClaudeConfigDir, "rules", "codex-issue-loop.md")
	target, data, err := inspectTarget(AgentClaude, path)
	if err != nil {
		return Target{}, err
	}
	if target.Status == StatusConflict {
		return target, nil
	}
	if !target.originalExist {
		target.Status, target.Action = StatusMissing, ActionCreate
		target.desired = []byte(ManagedBlock() + "\n")
		return target, nil
	}
	start, end, markerVersion, markerStatus, markerDetail := inspectMarkers(data)
	if markerVersion > RuleVersion {
		target.Status, target.Action, target.ApplyResult = StatusConflict, ActionNone, ResultSkipped
		target.Detail = fmt.Sprintf("managed rule version %d is newer than supported version %d", markerVersion, RuleVersion)
		return target, nil
	}
	if markerStatus == StatusConflict || markerStatus == StatusMissing || len(bytesTrimSpace(append(append([]byte{}, data[:start]...), data[end:]...))) != 0 {
		target.Status, target.Action, target.ApplyResult = StatusConflict, ActionNone, ResultSkipped
		target.Detail = "existing Claude Code rule is not wholly owned by agent-loop"
		if markerDetail != "" {
			target.Detail = joinDetail(target.Detail, markerDetail)
		}
		return target, nil
	}
	desired := []byte(ManagedBlock() + "\n")
	if string(data) == string(desired) {
		target.Status, target.Action, target.desired = StatusCurrent, ActionNone, data
		return target, nil
	}
	target.Status, target.Action, target.desired = StatusOutdated, ActionUpdate, desired
	target.BackupPath = filepath.Join(config.BackupRoot, stamp, string(AgentClaude), filepath.Base(path))
	return target, nil
}

func inspectTarget(agent Agent, path string) (Target, []byte, error) {
	target := Target{Agent: agent, Path: path, ResolvedPath: path, writePath: path, ApplyResult: ResultNotApplied}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		target.Status, target.Action, target.originalMode = StatusMissing, ActionCreate, 0o600
		return target, nil, nil
	}
	if err != nil {
		target.Status, target.Action, target.ApplyResult, target.Detail = StatusConflict, ActionNone, ResultSkipped, err.Error()
		return target, nil, nil
	}
	target.originalExist = true
	if info.Mode()&os.ModeSymlink != 0 {
		target.Symlink = true
		resolved, resolveErr := filepath.EvalSymlinks(path)
		if resolveErr != nil {
			target.Status, target.Action, target.ApplyResult, target.Detail = StatusConflict, ActionNone, ResultSkipped, "cannot resolve symlink: "+resolveErr.Error()
			return target, nil, nil
		}
		target.ResolvedPath, target.writePath = resolved, resolved
		info, err = os.Stat(resolved)
		if err != nil {
			target.Status, target.Action, target.ApplyResult, target.Detail = StatusConflict, ActionNone, ResultSkipped, "cannot inspect symlink target: "+err.Error()
			return target, nil, nil
		}
	}
	if !info.Mode().IsRegular() {
		target.Status, target.Action, target.ApplyResult, target.Detail = StatusConflict, ActionNone, ResultSkipped, "target is not a regular file"
		return target, nil, nil
	}
	data, err := os.ReadFile(target.writePath)
	if err != nil {
		target.Status, target.Action, target.ApplyResult, target.Detail = StatusConflict, ActionNone, ResultSkipped, "cannot read target: "+err.Error()
		return target, nil, nil
	}
	target.originalMode = info.Mode().Perm()
	target.originalHash = sha256.Sum256(data)
	return target, data, nil
}

func inspectMarkers(data []byte) (int, int, int, Status, string) {
	text := string(data)
	startPrefixCount := strings.Count(text, "<!-- agent-loop:rules:start")
	endPrefixCount := strings.Count(text, "<!-- agent-loop:rules:end")
	if startPrefixCount == 0 && endPrefixCount == 0 {
		return 0, 0, 0, StatusMissing, ""
	}
	matches := startMarkerPattern.FindAllStringIndex(text, -1)
	if startPrefixCount != 1 || endPrefixCount != 1 || len(matches) != 1 {
		return 0, 0, 0, StatusConflict, "managed markers are malformed or duplicated"
	}
	var version int
	if _, err := fmt.Sscanf(text[matches[0][0]:matches[0][1]], "<!-- agent-loop:rules:start version=%d -->", &version); err != nil {
		return 0, 0, 0, StatusConflict, "managed rule version is invalid"
	}
	start := matches[0][0]
	endRelative := strings.Index(text[matches[0][1]:], endMarker)
	if endRelative < 0 {
		return 0, 0, 0, StatusConflict, "managed start and end markers are inconsistent"
	}
	end := matches[0][1] + endRelative + len(endMarker)
	return start, end, version, StatusOutdated, ""
}

func appendCodexBlock(data []byte) []byte {
	block := []byte(ManagedBlock())
	if len(data) == 0 {
		return append(block, '\n')
	}
	if data[len(data)-1] == '\n' {
		result := append([]byte{}, data...)
		if len(data) < 2 || data[len(data)-2] != '\n' {
			result = append(result, '\n')
		}
		result = append(result, block...)
		return append(result, '\n')
	}
	result := append([]byte{}, data...)
	result = append(result, '\n', '\n')
	return append(result, block...)
}

func replaceRange(data []byte, start, end int, replacement []byte) []byte {
	result := make([]byte, 0, len(data)-(end-start)+len(replacement))
	result = append(result, data[:start]...)
	result = append(result, replacement...)
	return append(result, data[end:]...)
}

func bytesTrimSpace(data []byte) []byte { return []byte(strings.TrimSpace(string(data))) }

func joinDetail(first, second string) string {
	if first == "" {
		return second
	}
	if second == "" {
		return first
	}
	return first + "; " + second
}

// Apply revalidates every target before any backup or atomic write begins.
func Apply(report Report) (Report, error) {
	report.Apply = true
	report.Changed = false
	// Revalidate the complete plan before writing any target. This prevents a
	// concurrent edit in one target from leaving another target updated from a
	// stale multi-agent plan.
	for i := range report.Targets {
		target := &report.Targets[i]
		if target.Action == ActionNone {
			if target.Status == StatusCurrent {
				target.ApplyResult = ResultUnchanged
			} else {
				target.ApplyResult = ResultSkipped
			}
			continue
		}
		if err := validateSnapshot(*target); err != nil {
			target.Status, target.Action = StatusConflict, ActionNone
			target.ApplyResult, target.Detail = ResultFailed, joinDetail(target.Detail, err.Error())
			return report, fmt.Errorf("%s changed after planning: %w", target.Path, err)
		}
	}
	// Back up every target first so a later backup failure cannot leave only part
	// of the multi-agent settings updated.
	for i := range report.Targets {
		target := &report.Targets[i]
		if target.Action == ActionNone || !target.originalExist {
			continue
		}
		original, err := os.ReadFile(target.writePath)
		if err != nil {
			target.ApplyResult = ResultFailed
			return report, fmt.Errorf("read %s for backup: %w", target.writePath, err)
		}
		if err := fsutil.WriteFile(target.BackupPath, original, target.originalMode); err != nil {
			target.ApplyResult = ResultFailed
			return report, fmt.Errorf("backup %s: %w", target.writePath, err)
		}
	}
	for i := range report.Targets {
		target := &report.Targets[i]
		if target.Action == ActionNone {
			continue
		}
		if err := fsutil.WriteFile(target.writePath, target.desired, target.originalMode); err != nil {
			target.ApplyResult = ResultFailed
			return report, fmt.Errorf("write %s: %w", target.writePath, err)
		}
		target.Applied, report.Changed = true, true
		if target.Action == ActionCreate {
			target.ApplyResult = ResultCreated
		} else {
			target.ApplyResult = ResultUpdated
		}
	}
	return report, nil
}

func validateSnapshot(target Target) error {
	info, err := os.Lstat(target.Path)
	if !target.originalExist {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		return fmt.Errorf("target was created")
	}
	if err != nil {
		return err
	}
	if (info.Mode()&os.ModeSymlink != 0) != target.Symlink {
		return fmt.Errorf("symlink state changed")
	}
	writePath := target.Path
	if target.Symlink {
		resolved, err := filepath.EvalSymlinks(target.Path)
		if err != nil {
			return err
		}
		if resolved != target.ResolvedPath {
			return fmt.Errorf("symlink target changed")
		}
		writePath = resolved
	}
	writeInfo, err := os.Stat(writePath)
	if err != nil {
		return err
	}
	if !writeInfo.Mode().IsRegular() {
		return fmt.Errorf("target is no longer a regular file")
	}
	if writeInfo.Mode().Perm() != target.originalMode {
		return fmt.Errorf("file permission changed")
	}
	data, err := os.ReadFile(writePath)
	if err != nil {
		return err
	}
	if sha256.Sum256(data) != target.originalHash {
		return fmt.Errorf("file content changed")
	}
	return nil
}
