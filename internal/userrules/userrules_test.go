package userrules

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testConfig(t *testing.T) Config {
	t.Helper()
	root := t.TempDir()
	return Config{
		HomeDir:         root,
		CodexHome:       filepath.Join(root, ".codex"),
		ClaudeConfigDir: filepath.Join(root, ".claude"),
		BackupRoot:      filepath.Join(root, "agent-loop", "user-rules-backups"),
		Now: func() time.Time {
			return time.Date(2026, 8, 16, 1, 2, 3, 4, time.UTC)
		},
	}
}

func TestEnvironmentDefaultsUseUserScopeLocations(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("CODEX_HOME", "")
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("AGENT_LOOP_HOME", "")
	config, err := ConfigFromEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	if config.CodexHome != filepath.Join(root, ".codex") || config.ClaudeConfigDir != filepath.Join(root, ".claude") {
		t.Fatalf("config=%+v", config)
	}
	report, err := Plan(config, []Agent{AgentCodex, AgentClaude})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(report); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		filepath.Join(root, ".codex", "AGENTS.md"),
		filepath.Join(root, ".claude", "rules", "codex-issue-loop.md"),
	} {
		if _, err := os.Stat(path); err != nil {
			t.Fatal(err)
		}
	}
}

func targetFor(t *testing.T, report Report, agent Agent) Target {
	t.Helper()
	for _, target := range report.Targets {
		if target.Agent == agent {
			return target
		}
	}
	t.Fatalf("target for %s not found: %+v", agent, report.Targets)
	return Target{}
}

func TestPlanIsReadOnlyAndApplyCreatesBothRulesIdempotently(t *testing.T) {
	config := testConfig(t)
	report, err := Plan(config, []Agent{AgentCodex, AgentClaude})
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range report.Targets {
		if target.Status != StatusMissing || target.Action != ActionCreate || target.ApplyResult != ResultNotApplied {
			t.Fatalf("preview target=%+v", target)
		}
		if _, err := os.Lstat(target.Path); !os.IsNotExist(err) {
			t.Fatalf("preview created %s: %v", target.Path, err)
		}
	}
	if _, err := os.Lstat(config.BackupRoot); !os.IsNotExist(err) {
		t.Fatalf("preview created backup root: %v", err)
	}

	applied, err := Apply(report)
	if err != nil {
		t.Fatal(err)
	}
	if !applied.Apply || !applied.Changed {
		t.Fatalf("applied=%+v", applied)
	}
	for _, target := range applied.Targets {
		data, err := os.ReadFile(target.Path)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), "agent-loop implementation worker") || !strings.Contains(string(data), ".agent-loop.yaml") || !strings.Contains(string(data), "ready label") {
			t.Fatalf("required behavior missing from %s: %s", target.Path, data)
		}
		if target.Agent == AgentClaude && strings.HasPrefix(string(data), "---\n") {
			t.Fatalf("Claude Code rule unexpectedly has paths frontmatter: %s", data)
		}
		info, err := os.Stat(target.Path)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("mode=%v err=%v", info.Mode().Perm(), err)
		}
	}

	secondPlan, err := Plan(config, []Agent{AgentCodex, AgentClaude})
	if err != nil {
		t.Fatal(err)
	}
	second, err := Apply(secondPlan)
	if err != nil {
		t.Fatal(err)
	}
	if second.Changed {
		t.Fatalf("second apply changed files: %+v", second)
	}
	for _, target := range second.Targets {
		if target.Status != StatusCurrent || target.Action != ActionNone || target.ApplyResult != ResultUnchanged {
			t.Fatalf("second target=%+v", target)
		}
	}
}

func TestCodexPreservesUnmanagedContentAndUpdatesOnlyManagedBlock(t *testing.T) {
	config := testConfig(t)
	path := filepath.Join(config.CodexHome, "AGENTS.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	before := "# My instructions\n\nKeep this paragraph.\n\n<!-- agent-loop:rules:start version=0 -->\nold rule\n<!-- agent-loop:rules:end -->\n\nTail remains.\n"
	if err := os.WriteFile(path, []byte(before), 0o640); err != nil {
		t.Fatal(err)
	}
	report, err := Plan(config, []Agent{AgentCodex})
	if err != nil {
		t.Fatal(err)
	}
	target := targetFor(t, report, AgentCodex)
	if target.Status != StatusOutdated || target.Action != ActionUpdate || target.BackupPath == "" {
		t.Fatalf("target=%+v", target)
	}
	applied, err := Apply(report)
	if err != nil {
		t.Fatal(err)
	}
	target = targetFor(t, applied, AgentCodex)
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(after, []byte("# My instructions\n\nKeep this paragraph.")) || !bytes.Contains(after, []byte("\n\nTail remains.\n")) || bytes.Contains(after, []byte("old rule")) {
		t.Fatalf("unmanaged content changed: %s", after)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("mode changed to %o", info.Mode().Perm())
	}
	backup, err := os.ReadFile(target.BackupPath)
	if err != nil || string(backup) != before {
		t.Fatalf("backup=%q err=%v", backup, err)
	}
	// The backup is directly usable for documented restoration.
	if err := os.WriteFile(path, backup, info.Mode().Perm()); err != nil {
		t.Fatal(err)
	}
	restored, _ := os.ReadFile(path)
	if string(restored) != before {
		t.Fatalf("restored=%q", restored)
	}
}

func TestMalformedAndUnownedFilesAreConflictsAndNeverOverwritten(t *testing.T) {
	for _, test := range []struct {
		name  string
		agent Agent
		body  string
	}{
		{name: "codex missing end", agent: AgentCodex, body: "personal\n<!-- agent-loop:rules:start version=1 -->\nbroken\n"},
		{name: "codex duplicate", agent: AgentCodex, body: ManagedBlock() + "\n" + ManagedBlock() + "\n"},
		{name: "codex future version", agent: AgentCodex, body: strings.Replace(ManagedBlock(), "version=1", "version=2", 1) + "\n"},
		{name: "claude unowned", agent: AgentClaude, body: "# personal Claude rule\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := testConfig(t)
			path := filepath.Join(config.CodexHome, "AGENTS.md")
			if test.agent == AgentClaude {
				path = filepath.Join(config.ClaudeConfigDir, "rules", "codex-issue-loop.md")
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(test.body), 0o600); err != nil {
				t.Fatal(err)
			}
			report, err := Plan(config, []Agent{test.agent})
			if err != nil {
				t.Fatal(err)
			}
			target := targetFor(t, report, test.agent)
			if target.Status != StatusConflict || target.Action != ActionNone {
				t.Fatalf("target=%+v", target)
			}
			if _, err := Apply(report); err != nil {
				t.Fatal(err)
			}
			after, _ := os.ReadFile(path)
			if string(after) != test.body {
				t.Fatalf("conflict overwritten: %q", after)
			}
		})
	}
}

func TestClaudeOwnedOutdatedRuleIsBackedUpAndUpdated(t *testing.T) {
	config := testConfig(t)
	path := filepath.Join(config.ClaudeConfigDir, "rules", "codex-issue-loop.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	before := "<!-- agent-loop:rules:start version=0 -->\nold\n<!-- agent-loop:rules:end -->\n"
	if err := os.WriteFile(path, []byte(before), 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := Plan(config, []Agent{AgentClaude})
	if err != nil {
		t.Fatal(err)
	}
	target := targetFor(t, report, AgentClaude)
	if target.Status != StatusOutdated || target.Action != ActionUpdate || target.BackupPath == "" {
		t.Fatalf("target=%+v", target)
	}
	applied, err := Apply(report)
	if err != nil {
		t.Fatal(err)
	}
	target = targetFor(t, applied, AgentClaude)
	backup, err := os.ReadFile(target.BackupPath)
	if err != nil || string(backup) != before {
		t.Fatalf("backup=%q err=%v", backup, err)
	}
	after, _ := os.ReadFile(path)
	if string(after) != ManagedBlock()+"\n" {
		t.Fatalf("after=%q", after)
	}
}

func TestSymlinkIsPreservedAndResolvedTargetIsReported(t *testing.T) {
	config := testConfig(t)
	if err := os.MkdirAll(config.CodexHome, 0o700); err != nil {
		t.Fatal(err)
	}
	realDir := filepath.Join(config.HomeDir, "instructions")
	if err := os.MkdirAll(realDir, 0o700); err != nil {
		t.Fatal(err)
	}
	realPath := filepath.Join(realDir, "shared.md")
	before := []byte("shared instructions\n")
	if err := os.WriteFile(realPath, before, 0o644); err != nil {
		t.Fatal(err)
	}
	linkPath := filepath.Join(config.CodexHome, "AGENTS.md")
	if err := os.Symlink(filepath.Join("..", "instructions", "shared.md"), linkPath); err != nil {
		t.Fatal(err)
	}
	report, err := Plan(config, []Agent{AgentCodex})
	if err != nil {
		t.Fatal(err)
	}
	target := targetFor(t, report, AgentCodex)
	canonicalRealPath, err := filepath.EvalSymlinks(realPath)
	if err != nil {
		t.Fatal(err)
	}
	if !target.Symlink || target.ResolvedPath != canonicalRealPath || target.Path != linkPath {
		t.Fatalf("target=%+v", target)
	}
	if _, err := Apply(report); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(linkPath)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("symlink replaced: mode=%v err=%v", info.Mode(), err)
	}
	after, _ := os.ReadFile(realPath)
	if !bytes.HasPrefix(after, before) || !bytes.Contains(after, []byte(startMarker)) {
		t.Fatalf("resolved target not updated: %s", after)
	}
	realInfo, _ := os.Stat(realPath)
	if realInfo.Mode().Perm() != 0o644 {
		t.Fatalf("mode changed to %o", realInfo.Mode().Perm())
	}
}

func TestNonEmptyCodexOverrideReceivesManagedBlock(t *testing.T) {
	config := testConfig(t)
	if err := os.MkdirAll(config.CodexHome, 0o700); err != nil {
		t.Fatal(err)
	}
	agentsPath := filepath.Join(config.CodexHome, "AGENTS.md")
	overridePath := filepath.Join(config.CodexHome, "AGENTS.override.md")
	if err := os.WriteFile(agentsPath, []byte("ordinary\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(overridePath, []byte("active override\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := Plan(config, []Agent{AgentCodex})
	if err != nil {
		t.Fatal(err)
	}
	target := targetFor(t, report, AgentCodex)
	if target.Path != overridePath || !strings.Contains(target.Detail, "takes precedence") {
		t.Fatalf("target=%+v", target)
	}
	if _, err := Apply(report); err != nil {
		t.Fatal(err)
	}
	agentsData, _ := os.ReadFile(agentsPath)
	overrideData, _ := os.ReadFile(overridePath)
	if string(agentsData) != "ordinary\n" || !bytes.Contains(overrideData, []byte(startMarker)) {
		t.Fatalf("AGENTS=%q override=%q", agentsData, overrideData)
	}
}

func TestApplyRejectsChangesMadeAfterPlanning(t *testing.T) {
	config := testConfig(t)
	report, err := Plan(config, []Agent{AgentCodex})
	if err != nil {
		t.Fatal(err)
	}
	path := targetFor(t, report, AgentCodex).Path
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("created concurrently\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(report); err == nil {
		t.Fatal("concurrent change was overwritten")
	}
	after, _ := os.ReadFile(path)
	if string(after) != "created concurrently\n" {
		t.Fatalf("after=%q", after)
	}
}

func TestApplyValidatesAllTargetsBeforeAnyWrite(t *testing.T) {
	config := testConfig(t)
	report, err := Plan(config, []Agent{AgentCodex, AgentClaude})
	if err != nil {
		t.Fatal(err)
	}
	claudePath := targetFor(t, report, AgentClaude).Path
	if err := os.MkdirAll(filepath.Dir(claudePath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(claudePath, []byte("concurrent Claude rule\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(report); err == nil {
		t.Fatal("concurrent change was accepted")
	}
	codexPath := targetFor(t, report, AgentCodex).Path
	if _, err := os.Lstat(codexPath); !os.IsNotExist(err) {
		t.Fatalf("Codex target was written before full validation: %v", err)
	}
}

func TestParseAgentsLimitsTargets(t *testing.T) {
	agents, err := ParseAgents("claude,claude")
	if err != nil || len(agents) != 1 || agents[0] != AgentClaude {
		t.Fatalf("agents=%v err=%v", agents, err)
	}
	if _, err := ParseAgents("cursor"); err == nil {
		t.Fatal("unknown agent accepted")
	}
}
