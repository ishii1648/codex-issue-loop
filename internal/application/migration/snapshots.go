package migration

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"

	"github.com/ishii1648/codex-issue-loop/internal/adapter/state"
	contract "github.com/ishii1648/codex-issue-loop/internal/domain/snapshot"
	"github.com/ishii1648/codex-issue-loop/internal/domain/statecontract"
	"github.com/ishii1648/codex-issue-loop/internal/platform/config"
	"github.com/ishii1648/codex-issue-loop/internal/platform/fsutil"
	"github.com/ishii1648/codex-issue-loop/internal/platform/layout"
)

func InspectSnapshots(l layout.Layout) (Report, error) {
	m := Migrator{Layout: l}
	j, exists, err := m.loadJournal()
	if err != nil {
		return Report{}, err
	}
	var sources map[string][]byte
	if exists && j.Status == "prepared" {
		if j.To != state.CurrentVersion {
			return Report{}, fmt.Errorf("unfinished migration targets version %d; use its matching binary", j.To)
		}
		root, manifest, err := m.verifyBackup(j.Backup)
		if err != nil {
			return Report{}, err
		}
		sources = map[string][]byte{}
		for _, entry := range manifest.Entries {
			if !backupEntryExisted(entry) {
				sources[entry.Source] = nil
				continue
			}
			path, err := backupEntryPath(root, entry.Backup)
			if err != nil {
				return Report{}, err
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return Report{}, err
			}
			sources[entry.Source] = data
		}
	} else {
		j = journal{StartedAt: time.Now().UTC()}
	}
	report, _, err := planSnapshots(l, sources, j)
	return report, err
}

func planSnapshots(l layout.Layout, sources map[string][]byte, j journal) (Report, map[string][]byte, error) {
	report := Report{TargetVersion: state.CurrentVersion, SemanticFindings: []SemanticFinding{}, Compatibility: ReleaseCompatibility{StateSchemaCurrent: state.CurrentVersion, StateSchemaMigrationFrom: statecontract.MigrationFromSchema, SemanticContractCurrent: statecontract.CurrentVersion, SemanticContractMinimum: statecontract.MinimumVersion}}
	output := map[string][]byte{}
	repos, registryArtifact, err := inspectRegistry(l.RegistryPath)
	if err != nil {
		return report, nil, err
	}
	report.Repositories = repos
	if registryArtifact != nil && registryArtifact.Version != CurrentVersion {
		report.Unsupported = append(report.Unsupported, *registryArtifact)
	}
	for _, repo := range repos {
		path := filepath.Join(repo.RepoPath, config.FileName)
		version, exists, err := yamlVersion(path)
		if err != nil {
			return report, nil, err
		}
		if exists && version != config.CurrentVersion {
			report.Unsupported = append(report.Unsupported, Artifact{Kind: "config", Path: path, Version: version})
		}
	}
	paths, err := filepath.Glob(filepath.Join(l.ReposRoot, "*", "state.json"))
	if err != nil {
		return report, nil, err
	}
	if sources != nil {
		paths = nil
		for path := range sources {
			if filepath.Base(path) == "state.json" {
				paths = append(paths, path)
			}
		}
		sort.Strings(paths)
	}
	read := func(path string) ([]byte, error) {
		if sources != nil {
			return sources[path], nil
		}
		data, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return data, err
	}
	for _, path := range paths {
		data, err := read(path)
		if err != nil {
			return report, nil, err
		}
		snapshot, err := decodeV6Snapshot(data)
		if err != nil {
			return report, nil, fmt.Errorf("inspect %s: %w", path, err)
		}
		if snapshot.RepoID != filepath.Base(filepath.Dir(path)) {
			return report, nil, fmt.Errorf("snapshot repository identity differs from directory: %s", path)
		}
		var envelope struct {
			Version int `json:"version"`
			Issues  map[string]struct {
				Status       string          `json:"status"`
				Cancellation json.RawMessage `json:"cancellation"`
			} `json:"issues"`
		}
		_ = json.Unmarshal(data, &envelope)
		migrating := envelope.Version != state.CurrentVersion
		report.NeedsMigration = report.NeedsMigration || migrating
		if migrating {
			keys := make([]string, 0, len(snapshot.Issues))
			for key := range snapshot.Issues {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				item := snapshot.Issues[key]
				previous := envelope.Issues[key]
				if item.Cancellation != nil && item.Cancellation.Source == "legacy_cancel_migration" && (len(previous.Cancellation) == 0 || string(previous.Cancellation) == "null") {
					report.SemanticFindings = append(report.SemanticFindings, SemanticFinding{RepoID: snapshot.RepoID, IssueNumber: item.Number, Status: previous.Status, Field: "issues[].cancellation", Code: "LEGACY_CANCEL_MIGRATABLE", Migratable: true, Reason: fmt.Sprintf("preserve cancellation at %s; reconcile retained PR after startup", item.Cancellation.CanceledAt.Format(time.RFC3339Nano)), MigrationRule: "NORMALIZE_RESOLVED_CANCEL"})
				}
			}
		}
		eventsPath := filepath.Join(filepath.Dir(path), "events.jsonl")
		txnPath := filepath.Join(filepath.Dir(path), "state.txn.json")
		txnData, err := read(txnPath)
		if err != nil {
			return report, nil, err
		}
		if len(txnData) > 0 && migrating {
			return report, nil, fmt.Errorf("prepared transaction requires matching old runtime recovery: %s", txnPath)
		}
		eventsData, err := read(eventsPath)
		if err != nil {
			return report, nil, err
		}
		var events []state.Event
		var encodedEvents bytes.Buffer
		for _, line := range bytes.Split(eventsData, []byte("\n")) {
			if len(bytes.TrimSpace(line)) == 0 {
				continue
			}
			var event state.Event
			if err := json.Unmarshal(line, &event); err != nil {
				return report, nil, fmt.Errorf("decode %s: %w", eventsPath, err)
			}
			if event.Version != envelope.Version || event.RepoID != snapshot.RepoID {
				return report, nil, fmt.Errorf("event contract or repository mismatch: %s", eventsPath)
			}
			event.Version = state.CurrentVersion
			events = append(events, event)
			encoded, err := json.Marshal(event)
			if err != nil {
				return report, nil, err
			}
			encodedEvents.Write(encoded)
			encodedEvents.WriteByte('\n')
		}
		if len(txnData) > 0 {
			var txn contract.Transaction
			if err := json.Unmarshal(txnData, &txn); err != nil {
				return report, nil, err
			}
			if err := txn.Validate(snapshot.RepoID); err != nil {
				return report, nil, err
			}
			if txn.Snapshot.StateRevision == snapshot.StateRevision {
				current, _ := json.Marshal(snapshot)
				pending, _ := json.Marshal(txn.Snapshot)
				if !bytes.Equal(current, pending) {
					return report, nil, fmt.Errorf("prepared transaction snapshot conflicts at the same revision")
				}
			}
			if txn.Snapshot.StateRevision < snapshot.StateRevision || txn.Snapshot.StateRevision > snapshot.StateRevision+1 {
				return report, nil, fmt.Errorf("prepared transaction revision does not follow snapshot")
			}
			pendingEvents := append([]state.Event(nil), events...)
			if len(pendingEvents) == 0 || pendingEvents[len(pendingEvents)-1].Sequence < txn.Event.Sequence {
				pendingEvents = append(pendingEvents, txn.Event)
			} else {
				last, _ := json.Marshal(pendingEvents[len(pendingEvents)-1])
				expected, _ := json.Marshal(txn.Event)
				if !bytes.Equal(last, expected) {
					return report, nil, fmt.Errorf("prepared transaction event conflicts")
				}
			}
			if err := contract.ValidateEventSequence(txn.Snapshot, pendingEvents); err != nil {
				return report, nil, err
			}
		} else if err := contract.ValidateEventSequence(snapshot, events); err != nil {
			return report, nil, fmt.Errorf("validate %s: %w", path, err)
		}
		report.Artifacts = append(report.Artifacts, Artifact{Kind: "state", Path: path, Version: envelope.Version, SemanticMigration: migrating}, Artifact{Kind: "events", Path: eventsPath, Version: envelope.Version, SemanticMigration: migrating})
		if !migrating {
			continue
		}
		snapshot.StateRevision++
		snapshot.Supervisor.UpdatedAt = j.StartedAt
		payload, _ := json.Marshal(map[string]any{"migration_id": j.MigrationID, "before_version": 5, "after_version": state.CurrentVersion, "provenance_synthesized": false})
		audit := state.Event{Version: state.CurrentVersion, EventID: migrationAuditEventID(j.MigrationID, snapshot.RepoID), Sequence: snapshot.StateRevision, Timestamp: j.StartedAt, RepoID: snapshot.RepoID, Type: "snapshot_migration_applied", Payload: payload}
		events = append(events, audit)
		if err := contract.ValidateEventSequence(snapshot, events); err != nil {
			return report, nil, err
		}
		encoded, err := json.Marshal(audit)
		if err != nil {
			return report, nil, err
		}
		encodedEvents.Write(encoded)
		encodedEvents.WriteByte('\n')
		encoded, err = json.MarshalIndent(snapshot, "", "  ")
		if err != nil {
			return report, nil, err
		}
		output[path] = append(encoded, '\n')
		output[eventsPath] = encodedEvents.Bytes()
	}
	return report, output, nil
}

// supervisorDir is non-empty only while the caller owns that supervisor lock.
func (m Migrator) ApplySnapshots(supervisorDir string) (Result, error) {
	release, err := m.lock()
	if err != nil {
		return Result{}, err
	}
	defer release()
	if supervisorDir != "" {
		j, exists, err := m.loadJournal()
		if err != nil {
			return Result{}, err
		}
		if !exists || j.Status != "prepared" {
			data, err := os.ReadFile(filepath.Join(supervisorDir, "state.json"))
			if errors.Is(err, os.ErrNotExist) {
				return Result{To: state.CurrentVersion}, nil
			}
			if err != nil {
				return Result{}, err
			}
			var envelope struct {
				Version int `json:"version"`
			}
			if err := json.Unmarshal(data, &envelope); err != nil {
				return Result{}, err
			}
			if envelope.Version == state.CurrentVersion {
				_, err := decodeV6Snapshot(data)
				return Result{To: state.CurrentVersion}, err
			}
		}
	}
	releaseStores, err := m.lockSnapshotStores(supervisorDir)
	if err != nil {
		return Result{}, err
	}
	defer releaseStores()
	j, exists, err := m.loadJournal()
	if err != nil {
		return Result{}, err
	}
	var sources map[string][]byte
	if exists && j.Status == "prepared" {
		if j.To != state.CurrentVersion {
			return Result{}, fmt.Errorf("unfinished migration targets version %d; use its matching binary", j.To)
		}
		root, manifest, err := m.verifyBackup(j.Backup)
		if err != nil {
			return Result{}, err
		}
		sources = map[string][]byte{}
		for _, entry := range manifest.Entries {
			if !backupEntryExisted(entry) {
				sources[entry.Source] = nil
				continue
			}
			path, err := backupEntryPath(root, entry.Backup)
			if err != nil {
				return Result{}, err
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return Result{}, err
			}
			sources[entry.Source] = data
		}
	} else {
		j = journal{Version: journalVersion, From: 5, To: state.CurrentVersion, StartedAt: m.now()}
	}
	report, output, err := planSnapshots(m.Layout, sources, j)
	if err != nil {
		return Result{}, err
	}
	if len(report.Unsupported) > 0 {
		return Result{}, fmt.Errorf("snapshot migration requires config/registry version %d; unsupported artifacts: %v", config.CurrentVersion, report.Unsupported)
	}
	if !report.NeedsMigration {
		return Result{To: state.CurrentVersion}, nil
	}
	if sources == nil {
		for _, artifact := range report.Artifacts {
			if artifact.Kind != "state" || !artifact.SemanticMigration {
				continue
			}
			data, err := os.ReadFile(artifact.Path)
			if err != nil {
				return Result{}, err
			}
			var snapshot state.Snapshot
			if err := json.Unmarshal(data, &snapshot); err != nil {
				return Result{}, err
			}
			if snapshot.Supervisor.PID > 0 && (snapshot.Supervisor.PID != os.Getpid() || filepath.Dir(artifact.Path) != supervisorDir) {
				err := syscall.Kill(snapshot.Supervisor.PID, 0)
				if err == nil || !errors.Is(err, syscall.ESRCH) {
					return Result{}, fmt.Errorf("migration cannot prove supervisor PID %d is stopped", snapshot.Supervisor.PID)
				}
			}
		}
		backup, err := m.createBackup(report, 5)
		if err != nil {
			return Result{}, err
		}
		j.Backup, j.MigrationID, j.Status = backup, migrationID(backup), "prepared"
		_, output, err = planSnapshots(m.Layout, nil, j)
		if err != nil {
			return Result{}, err
		}
		if err := fsutil.WriteJSON(m.journalPath(), j, 0600); err != nil {
			return Result{}, err
		}
		if m.AfterWrite != nil {
			if err := m.AfterWrite(m.journalPath()); err != nil {
				return Result{}, err
			}
		}
	} else {
		for path, before := range sources {
			current, err := os.ReadFile(path)
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return Result{}, err
			}
			after, changed := output[path]
			if !bytes.Equal(current, before) && (!changed || !bytes.Equal(current, after)) {
				return Result{}, fmt.Errorf("migration input progressed or changed after preparation: %s", path)
			}
		}
	}
	paths := make([]string, 0, len(output))
	for path := range output {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		if err := fsutil.WriteFile(path, output[path], 0600); err != nil {
			return Result{}, err
		}
		if m.AfterWrite != nil {
			if err := m.AfterWrite(path); err != nil {
				return Result{}, err
			}
		}
	}
	verified, _, err := planSnapshots(m.Layout, nil, j)
	if err != nil {
		return Result{}, err
	}
	if verified.NeedsMigration || len(verified.Unsupported) > 0 {
		return Result{}, fmt.Errorf("snapshot migration did not converge")
	}
	now := m.now()
	j.Status, j.CompletedAt = "completed", &now
	if err := fsutil.WriteJSON(m.journalPath(), j, 0600); err != nil {
		return Result{}, err
	}
	return Result{Changed: true, Backup: j.Backup, From: 5, To: state.CurrentVersion, FileCount: len(output), Journal: m.journalPath()}, nil
}

func (m Migrator) lockSnapshotStores(supervisorDir string) (func(), error) {
	dirs, err := filepath.Glob(filepath.Join(m.Layout.ReposRoot, "*"))
	if err != nil {
		return nil, err
	}
	var files []*os.File
	release := func() {
		for i := len(files) - 1; i >= 0; i-- {
			_ = syscall.Flock(int(files[i].Fd()), syscall.LOCK_UN)
			_ = files[i].Close()
		}
	}
	for _, dir := range dirs {
		info, err := os.Stat(dir)
		if err != nil {
			release()
			return nil, err
		}
		if !info.IsDir() {
			continue
		}
		names := []string{"supervisor.lock", "state.lock"}
		if filepath.Clean(dir) == filepath.Clean(supervisorDir) {
			names = names[1:]
		}
		for _, name := range names {
			f, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_RDWR, 0600)
			if err != nil {
				release()
				return nil, err
			}
			if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
				f.Close()
				release()
				return nil, fmt.Errorf("migration blocked by %s: %w", filepath.Join(dir, name), err)
			}
			files = append(files, f)
		}
	}
	return release, nil
}
