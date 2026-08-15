package app

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ishii1648/codex-issue-loop/internal/compat"
	"github.com/ishii1648/codex-issue-loop/internal/config"
	gh "github.com/ishii1648/codex-issue-loop/internal/github"
	"github.com/ishii1648/codex-issue-loop/internal/layout"
	schema "github.com/ishii1648/codex-issue-loop/internal/migration"
	"github.com/ishii1648/codex-issue-loop/internal/registry"
	"github.com/ishii1648/codex-issue-loop/internal/state"
)

const doctorSchemaVersion = 1

type remediation struct {
	Kind        string `json:"kind"`
	Summary     string `json:"summary"`
	Command     string `json:"command,omitempty"`
	Settings    string `json:"settings,omitempty"`
	Automatic   bool   `json:"automatic"`
	Destructive bool   `json:"destructive"`
}

type diagnostic struct {
	Code         string        `json:"code"`
	OK           bool          `json:"ok"`
	Scope        string        `json:"scope"`
	RepoID       string        `json:"repo_id,omitempty"`
	Summary      string        `json:"summary"`
	Detail       string        `json:"detail,omitempty"`
	Remediations []remediation `json:"remediations,omitempty"`
}

type doctorResult struct {
	SchemaVersion int          `json:"schema_version"`
	OK            bool         `json:"ok"`
	GeneratedAt   time.Time    `json:"generated_at"`
	Diagnostics   []diagnostic `json:"diagnostics"`
}

func (a App) doctor(ctx context.Context, l layout.Layout, args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(a.Err)
	repoPath := fs.String("repo", "", "repository path; omit to diagnose all registered repositories")
	jsonOut := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return exitError{2, err}
	}

	diagnostics := diagnoseHost(ctx, l)
	schemaDiagnostics, schemaReady := diagnoseSchemas(l)
	diagnostics = append(diagnostics, schemaDiagnostics...)
	if !schemaReady {
		result := doctorResult{SchemaVersion: doctorSchemaVersion, OK: false, GeneratedAt: time.Now().UTC(), Diagnostics: diagnostics}
		if err := a.writeDoctorResult(*jsonOut, result); err != nil {
			return err
		}
		return exitError{1, fmt.Errorf("doctor found failing diagnostics")}
	}
	registryStore := registry.Store{Path: l.RegistryPath}
	registered, registryErr := registryStore.Load()
	if registryErr != nil {
		diagnostics = append(diagnostics, failedDiagnostic("REGISTRY_CORRUPT", "host", "", "repository registryを読み込めません", registryErr.Error(),
			instruction("registry fileを退避して内容を確認し、必要なら各repositoryをregisterし直してください")))
	} else {
		diagnostics = append(diagnostics, passedDiagnostic("REGISTRY_VALID", "host", "", "repository registryを読み込めます", fmt.Sprintf("registered_repositories=%d", len(registered.Repos))))
		if *repoPath != "" {
			diagnostics = append(diagnostics, diagnoseExplicitRepository(ctx, l, registryStore, *repoPath)...)
		} else {
			ids := make([]string, 0, len(registered.Repos))
			for id := range registered.Repos {
				ids = append(ids, id)
			}
			sort.Strings(ids)
			if len(ids) == 0 {
				diagnostics = append(diagnostics, diagnostic{
					Code: "REGISTRY_EMPTY", OK: true, Scope: "host", Summary: "登録済みrepositoryはありません",
					Remediations: []remediation{command("対象repositoryを登録します", "agent-loop register --repo /absolute/path/to/repository")},
				})
			}
			for _, id := range ids {
				diagnostics = append(diagnostics, diagnoseRegisteredRepository(ctx, l, registered.Repos[id])...)
			}
		}
	}

	result := doctorResult{SchemaVersion: doctorSchemaVersion, OK: diagnosticsOK(diagnostics), GeneratedAt: time.Now().UTC(), Diagnostics: diagnostics}
	if err := a.writeDoctorResult(*jsonOut, result); err != nil {
		return err
	}
	if !result.OK {
		return exitError{1, fmt.Errorf("doctor found failing diagnostics")}
	}
	return nil
}

func diagnoseSchemas(l layout.Layout) ([]diagnostic, bool) {
	report, err := schema.Inspect(l)
	if err != nil {
		return []diagnostic{failedDiagnostic("SCHEMA_INSPECTION_FAILED", "host", "", "永続schemaを検査できません", err.Error(), instruction("fileを削除せずbackupし、agent-loop migrate --jsonで再確認してください"))}, false
	}
	if len(report.Unsupported) > 0 {
		parts := make([]string, 0, len(report.Unsupported))
		for _, artifact := range report.Unsupported {
			parts = append(parts, fmt.Sprintf("%s=%d", artifact.Path, artifact.Version))
		}
		return []diagnostic{failedDiagnostic("SCHEMA_VERSION_UNSUPPORTED", "host", "", "このbinaryで扱えない永続schemaがあります", strings.Join(parts, ", "), instruction("対応binaryへ戻すか、対応するmigration手順を確認してください"))}, false
	}
	if report.NeedsMigration {
		return []diagnostic{failedDiagnostic("SCHEMA_MIGRATION_REQUIRED", "host", "", "永続schemaをv2へmigrationする必要があります", "agent-loop migrate --jsonで対象を確認してください", command("全loop停止後にforward migrationを実行します", "agent-loop migrate --apply --json"))}, false
	}
	return []diagnostic{passedDiagnostic("SCHEMA_VERSION_SUPPORTED", "host", "", "永続schemaはこのbinaryと互換です", fmt.Sprintf("version=%d artifacts=%d", report.TargetVersion, len(report.Artifacts)))}, true
}

func diagnoseHost(ctx context.Context, l layout.Layout) []diagnostic {
	diagnostics := diagnoseInstallation(l)
	paths := map[string]string{}
	for _, name := range []string{"git", "gh", "codex", "launchctl", "pmset"} {
		path, err := exec.LookPath(name)
		code := "DEPENDENCY_" + strings.ToUpper(name) + "_AVAILABLE"
		if err != nil {
			diagnostics = append(diagnostics, failedDiagnostic(strings.Replace(code, "_AVAILABLE", "_MISSING", 1), "host", "", name+" commandが見つかりません", err.Error(),
				instruction(name+"をインストールまたはPATHへ追加してからdoctorを再実行してください")))
			continue
		}
		paths[name] = path
		diagnostics = append(diagnostics, passedDiagnostic(code, "host", "", name+" commandを利用できます", path))
	}

	if path := paths["gh"]; path != "" {
		out, err := exec.CommandContext(ctx, path, "auth", "status").CombinedOutput()
		if err != nil {
			diagnostics = append(diagnostics, failedDiagnostic("GITHUB_AUTH_INVALID", "host", "", "GitHub CLIの認証が無効です", truncateTail(strings.TrimSpace(string(out)), 500),
				command("browser flowでGitHub CLIへ再ログインします", "gh auth login")))
		} else {
			diagnostics = append(diagnostics, passedDiagnostic("GITHUB_AUTH_VALID", "host", "", "GitHub CLIは認証済みです", "gh auth status succeeded"))
		}
		report := compat.ProbeGH(ctx, path)
		if report.OK() {
			diagnostics = append(diagnostics, passedDiagnostic("GH_CLI_COMPATIBLE", "host", "", "GitHub CLIの必須capabilityを利用できます", compatibilityDetail(report)))
		} else {
			diagnostics = append(diagnostics, failedDiagnostic("GH_CLI_INCOMPATIBLE", "host", "", "GitHub CLIのversionまたはcapabilityが非対応です", compatibilityDetail(report),
				instruction("GitHub CLIを対応versionへ更新し、doctorを再実行してください")))
		}
	}

	if path := paths["codex"]; path != "" {
		out, err := exec.CommandContext(ctx, path, "login", "status").CombinedOutput()
		if err != nil {
			diagnostics = append(diagnostics, failedDiagnostic("CODEX_AUTH_INVALID", "host", "", "Codex CLIの認証が無効です", truncateTail(strings.TrimSpace(string(out)), 500),
				command("browser flowでCodex CLIへ再ログインします", "codex login"),
				command("headless環境ではdevice code認証を開始します", "codex login --device-auth")))
		} else {
			diagnostics = append(diagnostics, passedDiagnostic("CODEX_AUTH_VALID", "host", "", "Codex CLIは認証済みです", "codex login status succeeded"))
		}
		report := compat.ProbeCodex(ctx, path)
		if report.OK() {
			diagnostics = append(diagnostics, passedDiagnostic("CODEX_CLI_COMPATIBLE", "host", "", "Codex CLIの必須capabilityを利用できます", compatibilityDetail(report)))
		} else {
			diagnostics = append(diagnostics, failedDiagnostic("CODEX_CLI_INCOMPATIBLE", "host", "", "Codex CLIのversionまたはcapabilityが非対応です", compatibilityDetail(report),
				instruction("Codex CLIを対応versionへ更新し、codex login statusとdoctorを再実行してください")))
		}
	}

	if path := paths["pmset"]; path != "" {
		out, err := exec.CommandContext(ctx, path, "-g", "custom").CombinedOutput()
		ok, detail := evaluateSleepSettings(string(out), err)
		if ok {
			diagnostics = append(diagnostics, passedDiagnostic("MACOS_SLEEP_DISABLED", "host", "", "AC電源時の自動sleepは無効です", detail))
		} else {
			code := "MACOS_SLEEP_STATUS_UNKNOWN"
			if detail == "AC sleep is enabled" {
				code = "MACOS_SLEEP_ENABLED"
			}
			diagnostics = append(diagnostics, failedDiagnostic(code, "host", "", "常駐運用に必要なmacOS sleep設定を確認してください", detail,
				settings("Mac miniで自動sleepを無効にします", "System Settings > Energy > Prevent automatic sleeping when the display is off")))
		}
	}
	return diagnostics
}

func diagnoseInstallation(l layout.Layout) []diagnostic {
	manifestPath := filepath.Join(l.Root, "install.json")
	manifest, err := readInstallManifest(manifestPath)
	if errors.Is(err, os.ErrNotExist) {
		if _, binaryErr := os.Stat(filepath.Join(l.BinDir, "agent-loop")); errors.Is(binaryErr, os.ErrNotExist) {
			return []diagnostic{passedDiagnostic("INSTALL_NOT_PRESENT", "host", "", "agent-loopはユーザー領域へ未インストールです", "source buildからdoctorを実行しています")}
		}
		return []diagnostic{failedDiagnostic("INSTALL_MANIFEST_MISSING", "host", "", "install manifestがありません", manifestPath, instruction("同じversionのbinaryからagent-loop installを再実行してください"))}
	}
	if err != nil {
		return []diagnostic{failedDiagnostic("INSTALL_MANIFEST_INVALID", "host", "", "install manifestを読み取れません", err.Error(), instruction("install directoryをbackupし、検証済みreleaseから再installしてください"))}
	}
	manifestSchema := manifest.SchemaVersion
	if manifestSchema == 0 {
		manifestSchema = 1
	}
	if manifestSchema != schema.CurrentVersion {
		return []diagnostic{failedDiagnostic("INSTALL_SCHEMA_INCOMPATIBLE", "host", "", "installed binaryと永続schemaの対応versionが一致しません", fmt.Sprintf("install_schema=%d binary_schema=%d", manifestSchema, schema.CurrentVersion), instruction("全loopを停止し、release手順に従ってbinary updateとschema migrationを組で実行してください"))}
	}
	binaryHash, binaryErr := fileSHA256(filepath.Join(l.BinDir, "agent-loop"))
	skillHash, skillErr := fileSHA256(filepath.Join(l.SkillsDir, "agent-loop", "SKILL.md"))
	skillVersion, versionErr := os.ReadFile(filepath.Join(l.SkillsDir, "agent-loop", "VERSION"))
	if binaryErr != nil || skillErr != nil || versionErr != nil || binaryHash != manifest.BinarySHA256 || skillHash != manifest.SkillSHA256 || strings.TrimSpace(string(skillVersion)) != manifest.Version {
		detail := fmt.Sprintf("manifest_version=%s binary_error=%v skill_error=%v version_error=%v", manifest.Version, binaryErr, skillErr, versionErr)
		return []diagnostic{failedDiagnostic("INSTALL_VERSION_MISMATCH", "host", "", "binaryとSkillのinstall versionまたはchecksumが一致しません", detail, instruction("loopを停止し、検証済みreleaseからagent-loop updateまたはrollbackを実行してください"))}
	}
	return []diagnostic{passedDiagnostic("INSTALL_VERSION_CONSISTENT", "host", "", "binaryとSkillのinstall versionが一致します", fmt.Sprintf("version=%s commit=%s", manifest.Version, manifest.Commit))}
}

func diagnoseExplicitRepository(ctx context.Context, l layout.Layout, registryStore registry.Store, path string) []diagnostic {
	canonical, canonicalErr := config.CanonicalRepoPath(path)
	if canonicalErr != nil {
		return []diagnostic{failedDiagnostic("REPOSITORY_PATH_INVALID", "repository", "", "repository pathを解決できません", canonicalErr.Error(), instruction("存在するGit repository rootを--repoへ指定してください"))}
	}
	entry, resolveErr := registryStore.Resolve(canonical, "")
	diagnostics := []diagnostic{}
	if resolveErr != nil {
		diagnostics = append(diagnostics, failedDiagnostic("REGISTRATION_MISSING", "repository", "", "repositoryが登録されていません", resolveErr.Error(), command("repositoryを登録します", fmt.Sprintf("agent-loop register --repo %q", canonical))))
		entry = registry.Entry{RepoID: registry.RepoID("unregistered", canonical), RepoPath: canonical, Commands: map[string]string{}}
	}
	diagnostics = append(diagnostics, diagnoseRepository(ctx, l, entry)...)
	return diagnostics
}

func diagnoseRegisteredRepository(ctx context.Context, l layout.Layout, entry registry.Entry) []diagnostic {
	return diagnoseRepository(ctx, l, entry)
}

func diagnoseRepository(ctx context.Context, l layout.Layout, entry registry.Entry) []diagnostic {
	diagnostics := []diagnostic{}
	cfg, err := config.Load(entry.RepoPath)
	if err != nil {
		return append(diagnostics, failedDiagnostic("CONFIG_INVALID", "repository", entry.RepoID, ".agent-loop.yamlが無効です", err.Error(),
			instruction(filepath.Join(entry.RepoPath, config.FileName)+"を修正し、doctorを再実行してください")))
	}
	diagnostics = append(diagnostics, passedDiagnostic("CONFIG_VALID", "repository", entry.RepoID, ".agent-loop.yamlを読み込めます", cfg.GitHub.Repo))

	if len(entry.Commands) > 0 {
		missing := []string{}
		for name, path := range entry.Commands {
			info, statErr := os.Stat(path)
			if statErr != nil || info.IsDir() || info.Mode()&0o111 == 0 {
				missing = append(missing, name+"="+path)
			}
		}
		sort.Strings(missing)
		if len(missing) > 0 {
			diagnostics = append(diagnostics, failedDiagnostic("REGISTERED_BINARY_MISSING", "repository", entry.RepoID, "登録時のcommand pathが無効です", strings.Join(missing, ", "),
				command("command pathを再解決して登録します", fmt.Sprintf("agent-loop register --repo %q", entry.RepoPath))))
		} else {
			diagnostics = append(diagnostics, passedDiagnostic("REGISTERED_BINARIES_VALID", "repository", entry.RepoID, "登録済みcommand pathを利用できます", ""))
		}
	}

	if _, statErr := os.Stat(l.PlistPath(entry.RepoID)); statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		diagnostics = append(diagnostics, failedDiagnostic("LAUNCH_AGENT_UNREADABLE", "repository", entry.RepoID, "LaunchAgent plistを読み取れません", statErr.Error(), instruction("plistの所有者とpermissionを確認してください")))
	} else if errors.Is(statErr, os.ErrNotExist) && len(entry.Commands) > 0 {
		diagnostics = append(diagnostics, failedDiagnostic("LAUNCH_AGENT_MISSING", "repository", entry.RepoID, "LaunchAgent plistがありません", l.PlistPath(entry.RepoID),
			command("plistを再生成します", fmt.Sprintf("agent-loop register --repo %q", entry.RepoPath))))
	} else if statErr == nil {
		diagnostics = append(diagnostics, passedDiagnostic("LAUNCH_AGENT_PRESENT", "repository", entry.RepoID, "LaunchAgent plistがあります", l.PlistPath(entry.RepoID)))
	}

	ghPath := entry.Commands["gh"]
	if ghPath == "" {
		ghPath = "gh"
	}
	out, repoErr := exec.CommandContext(ctx, ghPath, "repo", "view", cfg.GitHub.Repo, "--json", "nameWithOwner").CombinedOutput()
	if repoErr != nil {
		diagnostics = append(diagnostics, failedDiagnostic("GITHUB_REPOSITORY_INACCESSIBLE", "repository", entry.RepoID, "GitHub repositoryへアクセスできません", truncateTail(strings.TrimSpace(string(out)), 500),
			command("GitHub認証を確認します", "gh auth status"), instruction("tokenまたはGitHub Appに対象repositoryのIssuesとPull requestsのread/write権限を付与してください")))
	} else {
		diagnostics = append(diagnostics, passedDiagnostic("GITHUB_REPOSITORY_ACCESSIBLE", "repository", entry.RepoID, "GitHub repositoryへアクセスできます", cfg.GitHub.Repo))
		plan, labelErr := (gh.CLI{Path: ghPath, Secrets: cfg.RedactionValues()}).BootstrapLabels(ctx, cfg, false)
		if labelErr != nil {
			diagnostics = append(diagnostics, failedDiagnostic("GITHUB_LABEL_CHECK_FAILED", "repository", entry.RepoID, "GitHub labelを確認できません", labelErr.Error(), command("GitHub認証を確認します", "gh auth status")))
		} else {
			missing := []string{}
			for _, action := range plan.Actions {
				if action.Action == "create" {
					missing = append(missing, action.Desired.Name)
				}
			}
			if len(missing) > 0 {
				diagnostics = append(diagnostics, failedDiagnostic("GITHUB_LABELS_MISSING", "repository", entry.RepoID, "必須GitHub labelが不足しています", strings.Join(missing, ", "),
					command("作成計画をpreviewします", fmt.Sprintf("agent-loop bootstrap-labels --repo %q --json", entry.RepoPath)),
					command("確認後に不足labelを作成します", fmt.Sprintf("agent-loop bootstrap-labels --repo %q --apply", entry.RepoPath))))
			} else {
				diagnostics = append(diagnostics, passedDiagnostic("GITHUB_LABELS_PRESENT", "repository", entry.RepoID, "必須GitHub labelが揃っています", ""))
			}
		}
	}

	diagnostics = append(diagnostics, diagnoseDurableState(l, entry, cfg)...)
	return diagnostics
}

func diagnoseDurableState(l layout.Layout, entry registry.Entry, cfg config.Config) []diagnostic {
	store := state.Store{Dir: l.RepoDir(entry.RepoID), RepoID: entry.RepoID, RepoPath: entry.RepoPath, Secrets: cfg.RedactionValues()}
	data, err := os.ReadFile(store.StatePath())
	if errors.Is(err, os.ErrNotExist) {
		if len(entry.Commands) == 0 {
			return nil
		}
		return []diagnostic{failedDiagnostic("STATE_MISSING", "repository", entry.RepoID, "durable stateがありません", store.StatePath(), command("repositoryを再登録します", fmt.Sprintf("agent-loop register --repo %q", entry.RepoPath)))}
	}
	if err != nil {
		return []diagnostic{failedDiagnostic("STATE_UNREADABLE", "repository", entry.RepoID, "durable stateを読み取れません", err.Error(), instruction("state directoryの所有者とpermissionを確認してください"))}
	}
	var snapshot state.Snapshot
	if err := json.Unmarshal(data, &snapshot); err != nil || snapshot.Version != state.CurrentVersion || snapshot.RepoID != entry.RepoID || snapshot.RepoPath != entry.RepoPath {
		detail := errorText(err)
		if err == nil {
			detail = fmt.Sprintf("version=%d repo_id=%q repo_path=%q", snapshot.Version, snapshot.RepoID, snapshot.RepoPath)
		}
		return []diagnostic{failedDiagnostic("STATE_CORRUPT", "repository", entry.RepoID, "durable stateが破損または対象repositoryと不一致です", detail,
			command("supervisorを停止します", fmt.Sprintf("agent-loop stop --repo %q", entry.RepoPath)),
			instruction("state directoryを削除せず別の場所へbackupし、events.jsonlとlogを確認してから復旧してください"))}
	}

	diagnostics := []diagnostic{passedDiagnostic("STATE_VALID", "repository", entry.RepoID, "durable stateを読み込めます", fmt.Sprintf("revision=%d", snapshot.StateRevision))}
	event, eventErr := latestEvent(store.EventsPath())
	if eventErr != nil {
		diagnostics = append(diagnostics, failedDiagnostic("EVENT_LOG_INVALID", "repository", entry.RepoID, "event logの末尾を解釈できません", eventErr.Error(), instruction("events.jsonlを削除せずbackupし、supervisor再起動前に内容を確認してください")))
	}
	contextDetail := fmt.Sprintf("state=%s message=%s", snapshot.Supervisor.State, snapshot.Supervisor.Message)
	if event.Type != "" {
		contextDetail += fmt.Sprintf(" latest_event=%s@%s", event.Type, event.Timestamp.Format(time.RFC3339))
	}
	line, logErr := tailFile(filepath.Join(store.Dir, "launchd.stderr.log"), 4096)
	if line != "" {
		contextDetail += " latest_log=" + truncate(line, 500)
	} else if logErr == nil {
		line, logErr = tailFile(filepath.Join(store.Dir, "supervisor.log"), 4096)
		if line != "" {
			contextDetail += " latest_log=" + truncate(line, 500)
		}
	}
	if logErr != nil {
		diagnostics = append(diagnostics, failedDiagnostic("LOG_UNREADABLE", "repository", entry.RepoID, "supervisor logを読み取れません", logErr.Error(), instruction("log fileの所有者とpermissionを確認してください")))
	}
	switch snapshot.Supervisor.State {
	case "blocked":
		diagnostics = append(diagnostics, failedDiagnostic("SUPERVISOR_BLOCKED", "repository", entry.RepoID, "supervisorがblockedです", contextDetail,
			command("現在状態を確認します", fmt.Sprintf("agent-loop status --repo %q --json", entry.RepoPath)),
			command("直近stderr logを確認します", fmt.Sprintf("agent-loop logs --repo %q --stderr", entry.RepoPath)),
			instruction("先に原因となった認証・設定・binary・stateを修復し、確認後にrestartしてください")))
	case "stopped":
		diagnostics = append(diagnostics, failedDiagnostic("SUPERVISOR_STOPPED", "repository", entry.RepoID, "supervisorは停止しています", contextDetail,
			command("停止が意図したものか状態を確認します", fmt.Sprintf("agent-loop status --repo %q --json", entry.RepoPath)),
			command("処理を再開します", fmt.Sprintf("agent-loop start --repo %q", entry.RepoPath))))
	default:
		diagnostics = append(diagnostics, passedDiagnostic("SUPERVISOR_STATE_HEALTHY", "repository", entry.RepoID, "supervisor stateに停止障害はありません", contextDetail))
	}
	return diagnostics
}

func latestEvent(path string) (state.Event, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return state.Event{}, nil
	}
	if err != nil {
		return state.Event{}, err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	var latest state.Event
	for scanner.Scan() {
		if len(strings.TrimSpace(scanner.Text())) == 0 {
			continue
		}
		var event state.Event
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			return latest, fmt.Errorf("decode event sequence after %d: %w", latest.Sequence, err)
		}
		latest = event
	}
	return latest, scanner.Err()
}

func tailFile(path string, limit int64) (string, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	start := info.Size() - limit
	if start < 0 {
		start = 0
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return "", err
	}
	data, err := io.ReadAll(io.LimitReader(f, limit))
	if err != nil {
		return "", err
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) == 0 {
		return "", nil
	}
	return strings.TrimSpace(lines[len(lines)-1]), nil
}

func truncateTail(value string, max int) string {
	if len(value) <= max {
		return value
	}
	return value[len(value)-max:]
}

func (a App) writeDoctorResult(asJSON bool, result doctorResult) error {
	if asJSON {
		return a.output(true, result)
	}
	status := "OK"
	if !result.OK {
		status = "FAILED"
	}
	fmt.Fprintf(a.Out, "doctor: %s (schema %d)\n", status, result.SchemaVersion)
	for _, item := range result.Diagnostics {
		mark := "PASS"
		if !item.OK {
			mark = "FAIL"
		}
		scope := item.Scope
		if item.RepoID != "" {
			scope += "/" + item.RepoID
		}
		fmt.Fprintf(a.Out, "[%s] %s %s: %s\n", mark, item.Code, scope, item.Summary)
		if item.Detail != "" {
			fmt.Fprintf(a.Out, "  detail: %s\n", item.Detail)
		}
		for _, fix := range item.Remediations {
			fmt.Fprintf(a.Out, "  next: %s", fix.Summary)
			if fix.Command != "" {
				fmt.Fprintf(a.Out, " — %s", fix.Command)
			}
			if fix.Settings != "" {
				fmt.Fprintf(a.Out, " — %s", fix.Settings)
			}
			fmt.Fprintln(a.Out)
		}
	}
	return nil
}

func diagnosticsOK(items []diagnostic) bool {
	for _, item := range items {
		if !item.OK {
			return false
		}
	}
	return true
}

func passedDiagnostic(code, scope, repoID, summary, detail string) diagnostic {
	return diagnostic{Code: code, OK: true, Scope: scope, RepoID: repoID, Summary: summary, Detail: detail}
}

func failedDiagnostic(code, scope, repoID, summary, detail string, fixes ...remediation) diagnostic {
	return diagnostic{Code: code, OK: false, Scope: scope, RepoID: repoID, Summary: summary, Detail: detail, Remediations: fixes}
}

func command(summary, value string) remediation {
	return remediation{Kind: "command", Summary: summary, Command: value, Automatic: false, Destructive: false}
}

func instruction(summary string) remediation {
	return remediation{Kind: "instruction", Summary: summary, Automatic: false, Destructive: false}
}

func settings(summary, path string) remediation {
	return remediation{Kind: "settings", Summary: summary, Settings: path, Automatic: false, Destructive: false}
}

func evaluateSleepSettings(output string, commandErr error) (bool, string) {
	if commandErr != nil {
		return false, commandErr.Error()
	}
	inAC := false
	for _, line := range strings.Split(output, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasSuffix(trimmed, "Power:") {
			inAC = strings.HasPrefix(trimmed, "AC ")
			continue
		}
		if !inAC {
			continue
		}
		fields := strings.Fields(trimmed)
		if len(fields) == 2 && fields[0] == "sleep" {
			if fields[1] == "0" {
				return true, "AC sleep is disabled"
			}
			return false, "AC sleep is enabled"
		}
	}
	return false, "could not determine AC sleep setting from pmset"
}
