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

	"github.com/ishii1648/codex-issue-loop/internal/config"
	"github.com/ishii1648/codex-issue-loop/internal/fsutil"
	"github.com/ishii1648/codex-issue-loop/internal/layout"
	"github.com/ishii1648/codex-issue-loop/internal/registry"
	schemaversion "github.com/ishii1648/codex-issue-loop/internal/schema"
	"gopkg.in/yaml.v3"
)

const (
	CurrentVersion = schemaversion.Current
	journalVersion = 1
)

type Artifact struct {
	Kind    string `json:"kind"`
	Path    string `json:"path"`
	Version int    `json:"version"`
}

type Report struct {
	TargetVersion  int              `json:"target_version"`
	NeedsMigration bool             `json:"needs_migration"`
	Artifacts      []Artifact       `json:"artifacts"`
	Unsupported    []Artifact       `json:"unsupported,omitempty"`
	Repositories   []registry.Entry `json:"-"`
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
	Source string      `json:"source"`
	Backup string      `json:"backup"`
	Mode   os.FileMode `json:"mode"`
	SHA256 string      `json:"sha256"`
}

func RegisteredRepositories(l layout.Layout) ([]registry.Entry, error) {
	repositories, _, err := inspectRegistry(l.RegistryPath)
	return repositories, err
}

func Inspect(l layout.Layout) (Report, error) {
	report := Report{TargetVersion: CurrentVersion}
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
				report.Artifacts = append(report.Artifacts, Artifact{Kind: item.kind, Path: path, Version: version})
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

	j, journalExists, err := m.loadJournal()
	if err != nil {
		return Result{}, err
	}
	if journalExists && j.Status == "prepared" && j.To != CurrentVersion {
		return Result{}, fmt.Errorf("unfinished migration targets schema version %d; use its matching binary", j.To)
	}
	if journalExists && j.Status == "prepared" {
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
		backup, err := m.createBackup(report)
		if err != nil {
			return Result{}, err
		}
		j = journal{Version: journalVersion, Status: "prepared", From: schemaversion.Previous, To: CurrentVersion, Backup: backup, StartedAt: m.now()}
		if err := fsutil.WriteJSON(m.journalPath(), j, 0o600); err != nil {
			return Result{}, err
		}
	}

	changed := 0
	for _, artifact := range report.Artifacts {
		if artifact.Version != schemaversion.Previous {
			continue
		}
		if err := migrateArtifact(artifact); err != nil {
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

func (m Migrator) createBackup(report Report) (string, error) {
	root := filepath.Join(m.Layout.Root, "migrations")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", err
	}
	backup := filepath.Join(root, m.now().UTC().Format("20060102T150405.000000000Z")+fmt.Sprintf("-v%d-to-v%d", schemaversion.Previous, CurrentVersion))
	if err := os.Mkdir(backup, 0o700); err != nil {
		return "", err
	}
	filesDir := filepath.Join(backup, "files")
	if err := os.Mkdir(filesDir, 0o700); err != nil {
		return "", err
	}
	manifest := backupManifest{Version: 1, From: schemaversion.Previous, To: CurrentVersion, CreatedAt: m.now()}
	for _, repository := range report.Repositories {
		manifest.RepositoryPaths = append(manifest.RepositoryPaths, repository.RepoPath)
	}
	sort.Strings(manifest.RepositoryPaths)
	for index, artifact := range report.Artifacts {
		data, err := os.ReadFile(artifact.Path)
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
		manifest.Entries = append(manifest.Entries, backupEntry{Source: artifact.Path, Backup: name, Mode: info.Mode().Perm(), SHA256: hashBytes(data)})
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

func migrateArtifact(artifact Artifact) error {
	switch artifact.Kind {
	case "config":
		return migrateYAML(artifact.Path)
	case "events":
		return migrateEvents(artifact.Path)
	case "transaction":
		return migrateTransaction(artifact.Path)
	case "state":
		return migrateState(artifact.Path)
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

func migrateState(path string) error {
	object, err := readRawObject(path)
	if err != nil {
		return err
	}
	object["version"] = json.RawMessage(fmt.Sprint(CurrentVersion))
	delete(object, "notifications")
	return writeRawObject(path, object)
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
			Lease json.RawMessage `json:"lease"`
		}
		if err := json.Unmarshal(object["issues"], &issues); err != nil {
			return err
		}
		for number, issue := range issues {
			if len(issue.Lease) > 0 && string(issue.Lease) != "null" {
				return fmt.Errorf("rollback blocked: active resource lease for Issue #%s in %s", number, entry.Source)
			}
		}
	}
	return nil
}

func migrateEvents(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	lines := strings.Split(string(data), "\n")
	output := make([]byte, 0, len(data))
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var object map[string]json.RawMessage
		if err := json.Unmarshal([]byte(line), &object); err != nil {
			return err
		}
		object["version"] = json.RawMessage(fmt.Sprint(CurrentVersion))
		removeLegacyDeliveryEvent(object)
		encoded, err := json.Marshal(object)
		if err != nil {
			return err
		}
		output = append(output, encoded...)
		output = append(output, '\n')
	}
	return fsutil.WriteFile(path, output, 0o600)
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
	if manifest.Version != 1 || manifest.From != schemaversion.Previous || manifest.To != CurrentVersion {
		return backupManifest{}, fmt.Errorf("unsupported migration backup manifest")
	}
	return manifest, nil
}

func unsupportedError(artifacts []Artifact) error {
	parts := make([]string, 0, len(artifacts))
	for _, artifact := range artifacts {
		parts = append(parts, fmt.Sprintf("%s=%d", artifact.Path, artifact.Version))
	}
	return fmt.Errorf("unsupported schema version; supported migration is v%d to v%d: %s", schemaversion.Previous, CurrentVersion, strings.Join(parts, ", "))
}

func hashBytes(data []byte) string { return fmt.Sprintf("%x", sha256.Sum256(data)) }

func (m Migrator) now() time.Time {
	if m.Now != nil {
		return m.Now().UTC()
	}
	return time.Now().UTC()
}
