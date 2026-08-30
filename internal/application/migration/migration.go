package migration

import (
	"bufio"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/ishii1648/codex-issue-loop/internal/adapter/state"
	"github.com/ishii1648/codex-issue-loop/internal/domain/statecontract"
	"github.com/ishii1648/codex-issue-loop/internal/platform/config"
	"github.com/ishii1648/codex-issue-loop/internal/platform/fsutil"
	"github.com/ishii1648/codex-issue-loop/internal/platform/layout"
	"github.com/ishii1648/codex-issue-loop/internal/platform/registry"
	schemaversion "github.com/ishii1648/codex-issue-loop/internal/platform/schema"
	"gopkg.in/yaml.v3"
)

const (
	CurrentVersion = schemaversion.Current
	journalVersion = 1
)

type Artifact struct {
	Kind              string `json:"kind"`
	Path              string `json:"path"`
	Version           int    `json:"version"`
	SemanticMigration bool   `json:"semantic_migration,omitempty"`
}

type SemanticFinding struct {
	RepoID        string `json:"repo_id"`
	IssueNumber   int    `json:"issue_number"`
	Status        string `json:"status"`
	Field         string `json:"field"`
	Code          string `json:"code"`
	Migratable    bool   `json:"migratable"`
	Reason        string `json:"reason"`
	MigrationRule string `json:"migration_rule"`
	OperatorGuide string `json:"operator_guide,omitempty"`
}

type ReleaseCompatibility struct {
	StateSchemaCurrent       int `json:"state_schema_current"`
	StateSchemaMigrationFrom int `json:"state_schema_migration_from"`
	SemanticContractCurrent  int `json:"semantic_contract_current"`
	SemanticContractMinimum  int `json:"semantic_contract_minimum"`
}

type Report struct {
	TargetVersion    int                  `json:"target_version"`
	NeedsMigration   bool                 `json:"needs_migration"`
	Artifacts        []Artifact           `json:"artifacts"`
	Unsupported      []Artifact           `json:"unsupported,omitempty"`
	SemanticFindings []SemanticFinding    `json:"semantic_findings"`
	NonMigratable    []SemanticFinding    `json:"non_migratable,omitempty"`
	Compatibility    ReleaseCompatibility `json:"release_compatibility"`
	Repositories     []registry.Entry     `json:"-"`
}

type Result struct {
	Changed   bool   `json:"changed"`
	Backup    string `json:"backup,omitempty"`
	From      int    `json:"from_version,omitempty"`
	To        int    `json:"to_version"`
	FileCount int    `json:"file_count"`
	Restored  bool   `json:"restored,omitempty"`
	Journal   string `json:"journal,omitempty"`
}

type Migrator struct {
	Layout     layout.Layout
	Now        func() time.Time
	AfterWrite func(path string) error
}

type journal struct {
	Version     int        `json:"version"`
	MigrationID string     `json:"migration_id"`
	Status      string     `json:"status"`
	From        int        `json:"from_version"`
	To          int        `json:"to_version"`
	Backup      string     `json:"backup"`
	StartedAt   time.Time  `json:"started_at"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
}

type backupManifest struct {
	Version         int           `json:"version"`
	From            int           `json:"from_version"`
	To              int           `json:"to_version"`
	CreatedAt       time.Time     `json:"created_at"`
	RepositoryPaths []string      `json:"repository_paths"`
	Entries         []backupEntry `json:"entries"`
}

type backupEntry struct {
	Source  string      `json:"source"`
	Backup  string      `json:"backup"`
	Mode    os.FileMode `json:"mode"`
	SHA256  string      `json:"sha256"`
	Existed bool        `json:"existed"`
}

func RegisteredRepositories(l layout.Layout) ([]registry.Entry, error) {
	repositories, _, err := inspectRegistry(l.RegistryPath)
	return repositories, err
}

func Inspect(l layout.Layout) (Report, error) {
	report := Report{TargetVersion: CurrentVersion, SemanticFindings: []SemanticFinding{}, Compatibility: ReleaseCompatibility{
		StateSchemaCurrent: CurrentVersion, StateSchemaMigrationFrom: schemaversion.Previous,
		SemanticContractCurrent: statecontract.CurrentVersion, SemanticContractMinimum: statecontract.MinimumVersion,
	}}
	repositories, registryArtifact, err := inspectRegistry(l.RegistryPath)
	if err != nil {
		return Report{}, err
	}
	report.Repositories = repositories
	if registryArtifact != nil {
		report.Artifacts = append(report.Artifacts, *registryArtifact)
	}

	seenConfigs := map[string]bool{}
	for _, entry := range repositories {
		path := filepath.Join(entry.RepoPath, config.FileName)
		if seenConfigs[path] {
			continue
		}
		seenConfigs[path] = true
		version, exists, err := yamlVersion(path)
		if err != nil {
			return Report{}, fmt.Errorf("inspect config %s: %w", path, err)
		}
		if exists {
			report.Artifacts = append(report.Artifacts, Artifact{Kind: "config", Path: path, Version: version})
		}
	}

	entries, err := os.ReadDir(l.ReposRoot)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return Report{}, fmt.Errorf("inspect state root: %w", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(l.ReposRoot, entry.Name())
		statePath := filepath.Join(dir, "state.json")
		stateVersion, stateExists, stateErr := jsonVersion(statePath)
		if stateErr != nil {
			return Report{}, fmt.Errorf("inspect state %s: %w", statePath, stateErr)
		}
		semanticMigration := false
		if stateExists && (stateVersion == schemaversion.Previous || stateVersion == CurrentVersion) {
			marker, markerErr := semanticVersion(statePath)
			if markerErr != nil {
				return Report{}, fmt.Errorf("inspect semantic contract marker %s: %w", statePath, markerErr)
			}
			semanticMigration = marker != statecontract.CurrentVersion
			if semanticMigration {
				report.NeedsMigration = true
			}
			findings, findingErr := inspectSemanticState(statePath)
			if findingErr != nil {
				return Report{}, fmt.Errorf("inspect semantic state %s: %w", statePath, findingErr)
			}
			report.SemanticFindings = append(report.SemanticFindings, findings...)
			for _, finding := range findings {
				if !finding.Migratable {
					report.NonMigratable = append(report.NonMigratable, finding)
				}
			}
		}
		for _, item := range []struct {
			kind string
			name string
		}{
			{kind: "state", name: "state.json"},
			{kind: "events", name: "events.jsonl"},
			{kind: "transaction", name: "state.txn.json"},
		} {
			path := filepath.Join(dir, item.name)
			var version int
			var exists bool
			var inspectErr error
			if item.kind == "events" {
				version, exists, inspectErr = eventVersion(path)
			} else {
				version, exists, inspectErr = jsonVersion(path)
			}
			if inspectErr != nil {
				return Report{}, fmt.Errorf("inspect %s %s: %w", item.kind, path, inspectErr)
			}
			if exists {
				artifactVersion := version
				if item.kind == "events" && artifactVersion == 0 && semanticMigration {
					artifactVersion = stateVersion
				}
				report.Artifacts = append(report.Artifacts, Artifact{Kind: item.kind, Path: path, Version: artifactVersion,
					SemanticMigration: semanticMigration && (item.kind == "state" || item.kind == "events")})
			}
			if item.kind == "transaction" && exists && semanticMigration {
				finding := SemanticFinding{RepoID: entry.Name(), Field: "state.txn.json", Code: state.SemanticCodePreparedTransactionPresent,
					Migratable: false, Reason: fmt.Sprintf("prepared transaction must be completed by the v%d runtime before semantic migration", stateVersion),
					MigrationRule: "RECOVER_PREPARED_TRANSACTION_WITH_PRIOR_RUNTIME", OperatorGuide: fmt.Sprintf("run a read-only status with the supported v%d binary, stop the loop, then preview again", stateVersion)}
				report.SemanticFindings = append(report.SemanticFindings, finding)
				report.NonMigratable = append(report.NonMigratable, finding)
			}
		}
		if stateExists && semanticMigration {
			eventsPath := filepath.Join(dir, "events.jsonl")
			if _, err := os.Stat(eventsPath); errors.Is(err, os.ErrNotExist) {
				report.Artifacts = append(report.Artifacts, Artifact{Kind: "events", Path: eventsPath, Version: stateVersion, SemanticMigration: true})
			} else if err != nil {
				return Report{}, fmt.Errorf("inspect events %s: %w", eventsPath, err)
			}
		}
	}

	sort.Slice(report.Artifacts, func(i, j int) bool { return report.Artifacts[i].Path < report.Artifacts[j].Path })
	for _, artifact := range report.Artifacts {
		switch artifact.Version {
		case 0: // Empty event logs do not yet contain a versioned record.
		case schemaversion.Previous:
			report.NeedsMigration = true
		case CurrentVersion:
		default:
			report.Unsupported = append(report.Unsupported, artifact)
		}
	}
	return report, nil
}

func inspectSemanticState(path string) ([]SemanticFinding, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var snapshot state.Snapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return nil, err
	}
	// Legacy v4 has no explicit contract marker. Preview applies the current validator in
	// memory and never writes the source file.
	if snapshot.SemanticContractVersion == statecontract.MinimumVersion {
		snapshot.SemanticContractVersion = statecontract.CurrentVersion
	}
	violations := state.SemanticViolations(snapshot)
	byIssue := map[int]state.SemanticViolation{}
	findings := make([]SemanticFinding, 0, len(snapshot.Issues)+1)
	for _, violation := range violations {
		if violation.IssueNumber == 0 {
			findings = append(findings, SemanticFinding{RepoID: snapshot.RepoID, Field: violation.Field, Code: violation.Code,
				Migratable: false, Reason: violation.Reason, MigrationRule: violation.MigrationRule, OperatorGuide: violation.OperatorGuide})
			continue
		}
		byIssue[violation.IssueNumber] = violation
	}
	keys := make([]string, 0, len(snapshot.Issues))
	for key := range snapshot.Issues {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		issue := snapshot.Issues[key]
		if issue == nil {
			continue
		}
		if violation, found := byIssue[issue.Number]; found {
			findings = append(findings, SemanticFinding{RepoID: snapshot.RepoID, IssueNumber: issue.Number, Status: issue.Status.String(),
				Field: violation.Field, Code: violation.Code, Migratable: false, Reason: violation.Reason,
				MigrationRule: violation.MigrationRule, OperatorGuide: violation.OperatorGuide})
			continue
		}
		findings = append(findings, SemanticFinding{RepoID: snapshot.RepoID, IssueNumber: issue.Number, Status: issue.Status.String(),
			Field: "issues[].workspace", Code: state.SemanticCodeCompatible, Migratable: true,
			Reason: "current execution invariants are satisfied or the worker execution boundary has not been crossed", MigrationRule: "PRESERVE_VERIFIED_PROVENANCE"})
	}
	return findings, nil
}

func semanticVersion(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	var envelope struct {
		SemanticContractVersion int `json:"semantic_contract_version"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return 0, err
	}
	return envelope.SemanticContractVersion, nil
}

func (m Migrator) Apply() (Result, error) {
	unlock, err := m.lock()
	if err != nil {
		return Result{}, err
	}
	defer unlock()

	report, err := Inspect(m.Layout)
	if err != nil {
		return Result{}, err
	}
	if len(report.Unsupported) > 0 {
		return Result{}, unsupportedError(report.Unsupported)
	}
	if len(report.NonMigratable) > 0 {
		return Result{}, nonMigratableError(report.NonMigratable)
	}

	j, journalExists, err := m.loadJournal()
	if err != nil {
		return Result{}, err
	}
	if journalExists && j.Status == "prepared" && j.To != CurrentVersion {
		return Result{}, fmt.Errorf("unfinished migration targets schema version %d; use its matching binary", j.To)
	}
	if journalExists && j.Status == "prepared" {
		if j.MigrationID == "" {
			j.MigrationID = migrationID(j.Backup)
			if err := fsutil.WriteJSON(m.journalPath(), j, 0o600); err != nil {
				return Result{}, err
			}
		}
		if _, _, err := m.verifyBackup(j.Backup); err != nil {
			return Result{}, fmt.Errorf("verify prepared migration backup: %w", err)
		}
	}
	if !report.NeedsMigration {
		if journalExists && j.Status == "prepared" {
			now := m.now()
			j.Status, j.CompletedAt = "completed", &now
			if err := fsutil.WriteJSON(m.journalPath(), j, 0o600); err != nil {
				return Result{}, err
			}
			return Result{Changed: true, Backup: j.Backup, From: j.From, To: j.To, FileCount: len(report.Artifacts), Journal: m.journalPath()}, nil
		}
		return Result{Changed: false, To: CurrentVersion, FileCount: len(report.Artifacts), Journal: m.journalPath()}, nil
	}

	if !journalExists || j.Status != "prepared" {
		from := migrationFrom(report)
		backup, err := m.createBackup(report, from)
		if err != nil {
			return Result{}, err
		}
		startedAt := m.now()
		j = journal{Version: journalVersion, MigrationID: migrationID(backup), Status: "prepared", From: from, To: CurrentVersion, Backup: backup, StartedAt: startedAt}
		if err := fsutil.WriteJSON(m.journalPath(), j, 0o600); err != nil {
			return Result{}, err
		}
	}

	changed := 0
	for _, artifact := range report.Artifacts {
		if artifact.Version != schemaversion.Previous && !artifact.SemanticMigration {
			continue
		}
		if err := migrateArtifact(artifact, j); err != nil {
			return Result{}, err
		}
		changed++
		if m.AfterWrite != nil {
			if err := m.AfterWrite(artifact.Path); err != nil {
				return Result{}, err
			}
		}
	}
	verified, err := Inspect(m.Layout)
	if err != nil {
		return Result{}, err
	}
	if verified.NeedsMigration || len(verified.Unsupported) > 0 {
		return Result{}, fmt.Errorf("migration did not converge to schema version %d", CurrentVersion)
	}
	now := m.now()
	j.Status, j.CompletedAt = "completed", &now
	if err := fsutil.WriteJSON(m.journalPath(), j, 0o600); err != nil {
		return Result{}, err
	}
	return Result{Changed: changed > 0, Backup: j.Backup, From: j.From, To: j.To, FileCount: changed, Journal: m.journalPath()}, nil
}

func (m Migrator) Restore(backup string) (Result, error) {
	unlock, err := m.lock()
	if err != nil {
		return Result{}, err
	}
	defer unlock()

	resolved, manifest, err := m.verifyBackup(backup)
	if err != nil {
		return Result{}, err
	}
	if err := ensureRollbackHasNoActiveLeases(manifest); err != nil {
		return Result{}, err
	}
	for _, entry := range manifest.Entries {
		if !backupEntryExisted(entry) {
			if err := os.Remove(entry.Source); err != nil && !errors.Is(err, os.ErrNotExist) {
				return Result{}, fmt.Errorf("remove migration-created %s: %w", entry.Source, err)
			}
			continue
		}
		backupPath, err := backupEntryPath(resolved, entry.Backup)
		if err != nil {
			return Result{}, err
		}
		data, err := os.ReadFile(backupPath)
		if err != nil {
			return Result{}, err
		}
		if err := fsutil.WriteFile(entry.Source, data, entry.Mode.Perm()); err != nil {
			return Result{}, fmt.Errorf("restore %s: %w", entry.Source, err)
		}
	}
	now := m.now()
	j := journal{Version: journalVersion, Status: "rolled_back", From: manifest.From, To: manifest.To, Backup: resolved, StartedAt: manifest.CreatedAt, CompletedAt: &now}
	if err := fsutil.WriteJSON(m.journalPath(), j, 0o600); err != nil {
		return Result{}, err
	}
	return Result{Changed: true, Backup: resolved, From: manifest.To, To: manifest.From, FileCount: len(manifest.Entries), Restored: true, Journal: m.journalPath()}, nil
}

func (m Migrator) verifyBackup(backup string) (string, backupManifest, error) {
	resolved, err := m.validateBackup(backup)
	if err != nil {
		return "", backupManifest{}, err
	}
	manifest, err := readManifest(filepath.Join(resolved, "manifest.json"))
	if err != nil {
		return "", backupManifest{}, err
	}
	for _, entry := range manifest.Entries {
		if err := validateRestoreTarget(m.Layout, manifest.RepositoryPaths, entry.Source); err != nil {
			return "", backupManifest{}, err
		}
		if !backupEntryExisted(entry) {
			continue
		}
		backupPath, err := backupEntryPath(resolved, entry.Backup)
		if err != nil {
			return "", backupManifest{}, err
		}
		data, err := os.ReadFile(backupPath)
		if err != nil {
			return "", backupManifest{}, err
		}
		if hashBytes(data) != entry.SHA256 {
			return "", backupManifest{}, fmt.Errorf("migration backup checksum mismatch: %s", entry.Backup)
		}
	}
	return resolved, manifest, nil
}

// Version 1 manifests created before semantic migration did not emit
// "existed". A non-empty backup path is the unambiguous legacy representation
// of an existing source; only new synthetic event entries have neither.
func backupEntryExisted(entry backupEntry) bool {
	return entry.Existed || entry.Backup != ""
}

func backupEntryPath(root, name string) (string, error) {
	if name == "" || filepath.IsAbs(name) {
		return "", fmt.Errorf("invalid migration backup entry path %q", name)
	}
	clean := filepath.Clean(name)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("invalid migration backup entry path %q", name)
	}
	return filepath.Join(root, clean), nil
}

func (m Migrator) createBackup(report Report, from int) (string, error) {
	root := filepath.Join(m.Layout.Root, "migrations")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", err
	}
	suffix := fmt.Sprintf("-v%d-to-v%d", from, CurrentVersion)
	if from == CurrentVersion {
		suffix = fmt.Sprintf("-v%d-semantic-v%d", CurrentVersion, statecontract.CurrentVersion)
	}
	backup := filepath.Join(root, m.now().UTC().Format("20060102T150405.000000000Z")+suffix)
	if err := os.Mkdir(backup, 0o700); err != nil {
		return "", err
	}
	filesDir := filepath.Join(backup, "files")
	if err := os.Mkdir(filesDir, 0o700); err != nil {
		return "", err
	}
	manifest := backupManifest{Version: 1, From: from, To: CurrentVersion, CreatedAt: m.now()}
	for _, repository := range report.Repositories {
		manifest.RepositoryPaths = append(manifest.RepositoryPaths, repository.RepoPath)
	}
	sort.Strings(manifest.RepositoryPaths)
	for index, artifact := range report.Artifacts {
		data, err := os.ReadFile(artifact.Path)
		if errors.Is(err, os.ErrNotExist) && artifact.Kind == "events" {
			manifest.Entries = append(manifest.Entries, backupEntry{Source: artifact.Path, Existed: false})
			continue
		}
		if err != nil {
			return "", err
		}
		info, err := os.Stat(artifact.Path)
		if err != nil {
			return "", err
		}
		name := fmt.Sprintf("files/%04d", index)
		if err := fsutil.WriteFile(filepath.Join(backup, name), data, 0o600); err != nil {
			return "", err
		}
		manifest.Entries = append(manifest.Entries, backupEntry{Source: artifact.Path, Backup: name, Mode: info.Mode().Perm(), SHA256: hashBytes(data), Existed: true})
	}
	if err := fsutil.WriteJSON(filepath.Join(backup, "manifest.json"), manifest, 0o600); err != nil {
		return "", err
	}
	return backup, nil
}

func inspectRegistry(path string) ([]registry.Entry, *Artifact, error) {
	version, exists, err := jsonVersion(path)
	if err != nil || !exists {
		return nil, nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	var raw registry.Registry
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, nil, err
	}
	repositories := make([]registry.Entry, 0, len(raw.Repos))
	for _, entry := range raw.Repos {
		repositories = append(repositories, entry)
	}
	sort.Slice(repositories, func(i, j int) bool { return repositories[i].RepoID < repositories[j].RepoID })
	artifact := Artifact{Kind: "registry", Path: path, Version: version}
	return repositories, &artifact, nil
}

func jsonVersion(path string) (int, bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	var envelope struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return 0, true, err
	}
	return envelope.Version, true, nil
}

func yamlVersion(path string) (int, bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	var envelope struct {
		Version int `yaml:"version"`
	}
	if err := yaml.Unmarshal(data, &envelope); err != nil {
		return 0, true, err
	}
	return envelope.Version, true, nil
}

func eventVersion(path string) (int, bool, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	defer file.Close()
	version := 0
	scanner := bufio.NewScanner(file)
	buffer := make([]byte, 64*1024)
	scanner.Buffer(buffer, 4*1024*1024)
	for scanner.Scan() {
		if strings.TrimSpace(scanner.Text()) == "" {
			continue
		}
		var envelope struct {
			Version int `json:"version"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &envelope); err != nil {
			return 0, true, err
		}
		if envelope.Version == 0 {
			return 0, true, fmt.Errorf("event schema version is missing")
		}
		if version != 0 && version != envelope.Version {
			return 0, true, fmt.Errorf("event log contains mixed schema versions %d and %d", version, envelope.Version)
		}
		version = envelope.Version
	}
	if err := scanner.Err(); err != nil {
		return 0, true, err
	}
	return version, true, nil
}

func migrateArtifact(artifact Artifact, migration journal) error {
	switch artifact.Kind {
	case "config":
		return migrateYAML(artifact.Path)
	case "events":
		return migrateEvents(artifact.Path, migration, artifact.Version)
	case "transaction":
		return migrateTransaction(artifact.Path)
	case "state":
		return migrateState(artifact.Path, migration)
	case "registry":
		return migrateJSONObject(artifact.Path)
	default:
		return fmt.Errorf("unsupported migration artifact kind %q", artifact.Kind)
	}
}

func migrateYAML(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var node yaml.Node
	if err := yaml.Unmarshal(data, &node); err != nil {
		return err
	}
	if len(node.Content) == 0 || node.Content[0].Kind != yaml.MappingNode {
		return fmt.Errorf("config must be a YAML mapping")
	}
	mapping := node.Content[0]
	found := false
	for index := 0; index+1 < len(mapping.Content); {
		if mapping.Content[index].Value == "notifications" {
			mapping.Content = append(mapping.Content[:index], mapping.Content[index+2:]...)
			continue
		}
		if mapping.Content[index].Value == "version" {
			mapping.Content[index+1].Value = fmt.Sprint(CurrentVersion)
			found = true
		}
		index += 2
	}
	if !found {
		return fmt.Errorf("config version is missing")
	}
	encoded, err := yaml.Marshal(&node)
	if err != nil {
		return err
	}
	return fsutil.WriteFile(path, encoded, 0o600)
}

func migrateJSONObject(path string) error {
	object, err := readRawObject(path)
	if err != nil {
		return err
	}
	object["version"] = json.RawMessage(fmt.Sprint(CurrentVersion))
	return writeRawObject(path, object)
}

func migrateTransaction(path string) error {
	object, err := readRawObject(path)
	if err != nil {
		return err
	}
	object["version"] = json.RawMessage(fmt.Sprint(CurrentVersion))
	for _, key := range []string{"snapshot", "event"} {
		var nested map[string]json.RawMessage
		if err := json.Unmarshal(object[key], &nested); err != nil {
			return fmt.Errorf("decode transaction %s: %w", key, err)
		}
		nested["version"] = json.RawMessage(fmt.Sprint(CurrentVersion))
		if key == "snapshot" {
			delete(nested, "notifications")
		} else {
			removeLegacyDeliveryEvent(nested)
		}
		encoded, err := json.Marshal(nested)
		if err != nil {
			return err
		}
		object[key] = encoded
	}
	return writeRawObject(path, object)
}

func migrateState(path string, migration journal) error {
	object, err := readRawObject(path)
	if err != nil {
		return err
	}
	object["version"] = json.RawMessage(fmt.Sprint(CurrentVersion))
	object["semantic_contract_version"] = json.RawMessage(fmt.Sprint(statecontract.CurrentVersion))
	var revision uint64
	if err := json.Unmarshal(object["state_revision"], &revision); err != nil {
		return fmt.Errorf("decode state revision: %w", err)
	}
	revision++
	object["state_revision"] = json.RawMessage(fmt.Sprint(revision))
	var supervisor map[string]json.RawMessage
	if err := json.Unmarshal(object["supervisor"], &supervisor); err != nil {
		return fmt.Errorf("decode state supervisor: %w", err)
	}
	updatedAt, _ := json.Marshal(migration.StartedAt)
	supervisor["updated_at"] = updatedAt
	encodedSupervisor, err := json.Marshal(supervisor)
	if err != nil {
		return err
	}
	object["supervisor"] = encodedSupervisor
	delete(object, "notifications")
	if err := normalizeMigratedSessions(object); err != nil {
		return err
	}
	encoded, err := json.Marshal(object)
	if err != nil {
		return err
	}
	var snapshot state.Snapshot
	if err := json.Unmarshal(encoded, &snapshot); err != nil {
		return fmt.Errorf("decode migrated state for invariant validation: %w", err)
	}
	if err := snapshot.Validate(); err != nil {
		return fmt.Errorf("validate migrated state before commit: %w", err)
	}
	return writeRawObject(path, object)
}

func normalizeMigratedSessions(object map[string]json.RawMessage) error {
	var issues map[string]json.RawMessage
	if err := json.Unmarshal(object["issues"], &issues); err != nil {
		return fmt.Errorf("decode migrated state Issues: %w", err)
	}
	for key, raw := range issues {
		var issue map[string]json.RawMessage
		if err := json.Unmarshal(raw, &issue); err != nil {
			return fmt.Errorf("decode migrated Issue %s: %w", key, err)
		}
		var sessionID string
		_ = json.Unmarshal(issue["session_id"], &sessionID)
		if sessionID == "" || len(issue["session"]) > 0 && string(issue["session"]) != "null" {
			continue
		}
		session, err := json.Marshal(state.WorkerSession{Backend: "codex", ID: sessionID})
		if err != nil {
			return err
		}
		issue["session"] = session
		issues[key], err = json.Marshal(issue)
		if err != nil {
			return err
		}
	}
	encoded, err := json.Marshal(issues)
	if err != nil {
		return err
	}
	object["issues"] = encoded
	return nil
}

func ensureRollbackHasNoActiveLeases(manifest backupManifest) error {
	for _, entry := range manifest.Entries {
		if filepath.Base(entry.Source) != "state.json" && filepath.Base(entry.Source) != "state.txn.json" {
			continue
		}
		object, err := readRawObject(entry.Source)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if filepath.Base(entry.Source) == "state.txn.json" {
			if err := json.Unmarshal(object["snapshot"], &object); err != nil {
				return err
			}
		}
		var issues map[string]struct {
			Lease        json.RawMessage `json:"lease"`
			ResourcePark *struct {
				Status string `json:"status"`
			} `json:"resource_park"`
		}
		if err := json.Unmarshal(object["issues"], &issues); err != nil {
			return err
		}
		for number, issue := range issues {
			if len(issue.Lease) > 0 && string(issue.Lease) != "null" {
				return fmt.Errorf("rollback blocked: active resource lease for Issue #%s in %s", number, entry.Source)
			}
			if issue.ResourcePark != nil && (issue.ResourcePark.Status == "parked" || issue.ResourcePark.Status == "resuming") {
				return fmt.Errorf("rollback blocked: parked resource continuation for Issue #%s in %s", number, entry.Source)
			}
		}
	}
	return nil
}

func migrateEvents(path string, migration journal, fromSchema int) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		data = nil
		err = nil
	}
	if err != nil {
		return err
	}
	lines := strings.Split(string(data), "\n")
	output := make([]byte, 0, len(data))
	var lastSequence uint64
	repoID := ""
	hasAudit := false
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var object map[string]json.RawMessage
		if err := json.Unmarshal([]byte(line), &object); err != nil {
			return err
		}
		object["version"] = json.RawMessage(fmt.Sprint(CurrentVersion))
		var sequence uint64
		if err := json.Unmarshal(object["sequence"], &sequence); err != nil {
			return fmt.Errorf("decode event sequence: %w", err)
		}
		if sequence > lastSequence {
			lastSequence = sequence
		}
		if repoID == "" {
			_ = json.Unmarshal(object["repo_id"], &repoID)
		}
		var eventType string
		_ = json.Unmarshal(object["type"], &eventType)
		if eventType == "semantic_migration_applied" {
			var payload struct {
				MigrationID string `json:"migration_id"`
			}
			if json.Unmarshal(object["payload"], &payload) == nil && payload.MigrationID == migration.MigrationID {
				hasAudit = true
			}
		}
		removeLegacyDeliveryEvent(object)
		encoded, err := json.Marshal(object)
		if err != nil {
			return err
		}
		output = append(output, encoded...)
		output = append(output, '\n')
	}
	if repoID == "" {
		stateData, readErr := os.ReadFile(filepath.Join(filepath.Dir(path), "state.json"))
		if readErr != nil {
			return fmt.Errorf("resolve repository for migration audit: %w", readErr)
		}
		var envelope struct {
			RepoID        string `json:"repo_id"`
			StateRevision uint64 `json:"state_revision"`
		}
		if err := json.Unmarshal(stateData, &envelope); err != nil {
			return err
		}
		repoID, lastSequence = envelope.RepoID, envelope.StateRevision
	}
	if hasAudit {
		return fsutil.WriteFile(path, output, 0o600)
	}
	payload := map[string]any{
		"migration_id":           migration.MigrationID,
		"authority":              "operator",
		"source":                 "agent-loop migrate --apply",
		"before":                 map[string]int{"state_schema_version": fromSchema, "semantic_contract_version": statecontract.MinimumVersion},
		"after":                  map[string]int{"state_schema_version": CurrentVersion, "semantic_contract_version": statecontract.CurrentVersion},
		"operator_confirmation":  map[string]bool{"apply": true},
		"provenance_synthesized": false,
	}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	eventID := migrationAuditEventID(migration.MigrationID, repoID)
	audit := map[string]any{
		"version": CurrentVersion, "event_id": eventID, "sequence": lastSequence + 1,
		"timestamp": migration.StartedAt, "repo_id": repoID, "type": "semantic_migration_applied", "payload": json.RawMessage(payloadJSON),
	}
	auditJSON, err := json.Marshal(audit)
	if err != nil {
		return err
	}
	output = append(output, auditJSON...)
	output = append(output, '\n')
	return fsutil.WriteFile(path, output, 0o600)
}

func migrationID(backup string) string {
	digest := sha256.Sum256([]byte(backup))
	return fmt.Sprintf("migration_%x", digest[:12])
}

func migrationAuditEventID(id, repoID string) string {
	digest := sha256.Sum256([]byte(id + "\x00" + repoID))
	return fmt.Sprintf("evt_migration_%x", digest[:12])
}

func removeLegacyDeliveryEvent(object map[string]json.RawMessage) {
	var eventType string
	_ = json.Unmarshal(object["type"], &eventType)
	if !strings.HasPrefix(eventType, "notification_") {
		return
	}
	object["type"] = json.RawMessage(`"legacy_external_delivery_removed"`)
	delete(object, "payload")
}

func readRawObject(path string) (map[string]json.RawMessage, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil {
		return nil, err
	}
	return object, nil
}

func writeRawObject(path string, object map[string]json.RawMessage) error {
	data, err := json.MarshalIndent(object, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return fsutil.WriteFile(path, data, 0o600)
}

func (m Migrator) lock() (func(), error) {
	path := filepath.Join(m.Layout.Root, "migration.lock")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		return nil, fmt.Errorf("another migration holds %s: %w", path, err)
	}
	return func() {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		_ = file.Close()
	}, nil
}

func (m Migrator) journalPath() string { return filepath.Join(m.Layout.Root, "migration.json") }

func (m Migrator) loadJournal() (journal, bool, error) {
	data, err := os.ReadFile(m.journalPath())
	if errors.Is(err, os.ErrNotExist) {
		return journal{}, false, nil
	}
	if err != nil {
		return journal{}, false, err
	}
	var value journal
	if err := json.Unmarshal(data, &value); err != nil {
		return journal{}, false, err
	}
	if value.Version != journalVersion {
		return journal{}, false, fmt.Errorf("unsupported migration journal version %d", value.Version)
	}
	return value, true, nil
}

func (m Migrator) validateBackup(path string) (string, error) {
	if path == "" || !filepath.IsAbs(path) {
		return "", fmt.Errorf("--backup must be an absolute migration backup path")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	root, err := filepath.EvalSymlinks(filepath.Join(m.Layout.Root, "migrations"))
	if err != nil {
		return "", err
	}
	relative, err := filepath.Rel(root, resolved)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("backup must be a child of %s", root)
	}
	return resolved, nil
}

func validateRestoreTarget(l layout.Layout, repositories []string, path string) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("migration backup contains non-absolute restore target: %s", path)
	}
	if path == l.RegistryPath {
		return nil
	}
	if relative, err := filepath.Rel(l.ReposRoot, path); err == nil && relative != "." && relative != ".." && !strings.HasPrefix(relative, ".."+string(os.PathSeparator)) {
		return nil
	}
	for _, repository := range repositories {
		if path == filepath.Join(repository, config.FileName) {
			return nil
		}
	}
	return fmt.Errorf("migration backup contains unmanaged restore target: %s", path)
}

func readManifest(path string) (backupManifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return backupManifest{}, err
	}
	var manifest backupManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return backupManifest{}, err
	}
	if manifest.Version != 1 || (manifest.From != schemaversion.Previous && manifest.From != CurrentVersion) || manifest.To != CurrentVersion {
		return backupManifest{}, fmt.Errorf("unsupported migration backup manifest")
	}
	return manifest, nil
}

func migrationFrom(report Report) int {
	for _, artifact := range report.Artifacts {
		if artifact.Version == schemaversion.Previous {
			return schemaversion.Previous
		}
	}
	return CurrentVersion
}

func unsupportedError(artifacts []Artifact) error {
	parts := make([]string, 0, len(artifacts))
	for _, artifact := range artifacts {
		parts = append(parts, fmt.Sprintf("%s=%d", artifact.Path, artifact.Version))
	}
	return fmt.Errorf("unsupported schema version; supported migration is v%d to v%d: %s", schemaversion.Previous, CurrentVersion, strings.Join(parts, ", "))
}

func nonMigratableError(findings []SemanticFinding) error {
	parts := make([]string, 0, len(findings))
	for _, finding := range findings {
		target := finding.RepoID
		if finding.IssueNumber > 0 {
			target = fmt.Sprintf("%s/Issue#%d", finding.RepoID, finding.IssueNumber)
		}
		parts = append(parts, fmt.Sprintf("%s=%s", target, finding.Code))
	}
	sort.Strings(parts)
	return fmt.Errorf("semantic migration refused; non-migratable recovery state requires the reported operator procedure: %s", strings.Join(parts, ", "))
}

func hashBytes(data []byte) string { return fmt.Sprintf("%x", sha256.Sum256(data)) }

func (m Migrator) now() time.Time {
	if m.Now != nil {
		return m.Now().UTC()
	}
	return time.Now().UTC()
}
