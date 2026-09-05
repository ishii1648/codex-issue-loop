package incidentloop

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/ishii1648/codex-issue-loop/internal/platform/fsutil"
	"github.com/ishii1648/codex-issue-loop/internal/platform/redact"
	"github.com/ishii1648/codex-issue-loop/internal/platform/retention"
)

var ErrAlreadyRunning = errors.New("incident analysis is already running for this repository")

// File locks provide the cross-process boundary; this keyed lock prevents
// same-process goroutines from entering platform-dependent flock contention.
var decisionProcessLocks sync.Map

type Store struct {
	Dir       string
	Secrets   []string
	Retention retention.Policy
}

func (s Store) SignalsPath() string   { return filepath.Join(s.Dir, "signals.jsonl") }
func (s Store) DecisionsPath() string { return filepath.Join(s.Dir, "decisions.jsonl") }
func (s Store) StatePath() string     { return filepath.Join(s.Dir, "state.json") }
func (s Store) MetricsPath() string   { return filepath.Join(s.Dir, "metrics.json") }
func (s Store) DryRunPath() string    { return filepath.Join(s.Dir, "issue-dry-run.json") }

func (s Store) Ensure() error {
	if s.Dir == "" {
		return errors.New("incident store directory is required")
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	return os.Chmod(s.Dir, 0o700)
}

// Record is an append-only signal boundary. It redacts before validation and
// updates bounded aggregate metrics under the same repository-local lock.
func (s Store) Record(signal Signal) error {
	_, err := s.RecordBatch([]Signal{signal})
	return err
}

func (s Store) RecordBatch(signals []Signal) (int, error) {
	if err := s.Ensure(); err != nil {
		return 0, err
	}
	safeSignals := make([]Signal, len(signals))
	for index, signal := range signals {
		safe, err := sanitizeSignal(signal, s.Secrets)
		if err != nil {
			return 0, err
		}
		safeSignals[index] = safe
	}
	lock, err := s.lock("data.lock", true)
	if err != nil {
		return 0, err
	}
	defer unlockFile(lock)

	existing, err := s.readSignalsUnlocked()
	if err != nil {
		return 0, err
	}
	seen := make(map[string]Signal, len(existing)+len(safeSignals))
	for _, item := range existing {
		seen[item.ID] = item
	}
	policy := s.Retention
	if policy.MaxBytes == 0 {
		policy = retention.Policy{MaxBytes: 16 * 1024 * 1024, MaxAge: 24 * time.Hour, Keep: 7}
	}
	writer, err := retention.OpenWriter(s.SignalsPath(), policy)
	if err != nil {
		return 0, err
	}
	metrics, err := s.loadMetricsUnlocked()
	if err != nil {
		_ = writer.Close()
		return 0, err
	}
	written := 0
	for _, safe := range safeSignals {
		if prior, exists := seen[safe.ID]; exists {
			if !signalsEqual(prior, safe) {
				_ = writer.Close()
				return written, fmt.Errorf("signal %s was replayed with different content", safe.ID)
			}
			continue
		}
		line, err := json.Marshal(safe)
		if err != nil {
			_ = writer.Close()
			return written, err
		}
		if _, err := writer.Write(append(line, '\n')); err != nil {
			_ = writer.Close()
			return written, err
		}
		seen[safe.ID] = safe
		written++
		metrics.UpdatedAt = safe.Timestamp
		metrics.SignalsByName[safe.Name]++
		metrics.Outcomes[safe.OutcomeCode]++
		if safe.Name == "operation_duration" {
			summary := metrics.DurationsMS[safe.OperationCode]
			summary.Count++
			summary.Sum += safe.ElapsedMS
			if safe.ElapsedMS > summary.Max {
				summary.Max = safe.ElapsedMS
			}
			metrics.DurationsMS[safe.OperationCode] = summary
		}
	}
	if err := writer.Close(); err != nil {
		return written, err
	}
	if written == 0 {
		return 0, nil
	}
	return written, fsutil.WriteJSON(s.MetricsPath(), metrics, 0o600)
}

func (s Store) ReadSignals() ([]Signal, error) {
	if err := s.Ensure(); err != nil {
		return nil, err
	}
	lock, err := s.lock("data.lock", false)
	if err != nil {
		return nil, err
	}
	defer unlockFile(lock)
	return s.readSignalsUnlocked()
}

// AppendDecisions preserves existing records byte-for-byte during normal
// operation. It atomically rewrites the file only to remove records strictly
// older than DecisionRetention.
func (s Store) AppendDecisions(decisions []IssueDecision, now time.Time) error {
	if now.IsZero() {
		return errors.New("decision retention timestamp is required")
	}
	if err := s.Ensure(); err != nil {
		return err
	}
	safe := make([]IssueDecision, len(decisions))
	for index, decision := range decisions {
		item, err := sanitizeDecision(decision, s.Secrets)
		if err != nil {
			return fmt.Errorf("sanitize Issue decision %d: %w", index, err)
		}
		safe[index] = item
	}
	processLock := decisionProcessLock(s.DecisionsPath())
	processLock.Lock()
	defer processLock.Unlock()
	lock, err := s.lock("data.lock", true)
	if err != nil {
		return err
	}
	defer unlockFile(lock)

	existing, err := s.readDecisionRecordsUnlocked()
	if err != nil {
		return err
	}
	seen := make(map[string]IssueDecision, len(existing)+len(safe))
	for _, record := range existing {
		seen[record.Decision.ID] = record.Decision
	}
	cutoff := now.UTC().Add(-DecisionRetention)
	var retained bytes.Buffer
	expired := false
	for _, record := range existing {
		if record.Decision.DecidedAt.Before(cutoff) {
			expired = true
			continue
		}
		retained.Write(record.Line)
	}
	newRecords := make([]IssueDecision, 0, len(safe))
	for _, decision := range safe {
		if previous, exists := seen[decision.ID]; exists {
			if !decisionsEqual(previous, decision) {
				return fmt.Errorf("Issue decision %s was replayed with different content", decision.ID)
			}
			continue
		}
		seen[decision.ID] = decision
		if decision.DecidedAt.Before(cutoff) {
			continue
		}
		newRecords = append(newRecords, decision)
	}
	if !expired {
		if len(newRecords) == 0 {
			return nil
		}
		var appended bytes.Buffer
		for _, decision := range newRecords {
			line, err := json.Marshal(decision)
			if err != nil {
				return err
			}
			appended.Write(line)
			appended.WriteByte('\n')
		}
		if _, err := os.Stat(s.DecisionsPath()); errors.Is(err, os.ErrNotExist) {
			if err := fsutil.WriteFile(s.DecisionsPath(), nil, 0o600); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		file, err := os.OpenFile(s.DecisionsPath(), os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return err
		}
		if err := os.Chmod(s.DecisionsPath(), 0o600); err != nil {
			_ = file.Close()
			return err
		}
		if _, err := file.Write(appended.Bytes()); err != nil {
			_ = file.Close()
			return err
		}
		if err := file.Sync(); err != nil {
			_ = file.Close()
			return err
		}
		return file.Close()
	}
	for _, decision := range newRecords {
		line, err := json.Marshal(decision)
		if err != nil {
			return err
		}
		retained.Write(line)
		retained.WriteByte('\n')
	}
	return fsutil.WriteFile(s.DecisionsPath(), retained.Bytes(), 0o600)
}

func (s Store) ReadDecisions() ([]IssueDecision, error) {
	if err := s.Ensure(); err != nil {
		return nil, err
	}
	processLock := decisionProcessLock(s.DecisionsPath())
	processLock.Lock()
	defer processLock.Unlock()
	lock, err := s.lock("data.lock", false)
	if err != nil {
		return nil, err
	}
	defer unlockFile(lock)
	records, err := s.readDecisionRecordsUnlocked()
	if err != nil {
		return nil, err
	}
	decisions := make([]IssueDecision, 0, len(records))
	seen := map[string]bool{}
	for _, record := range records {
		if seen[record.Decision.ID] {
			continue
		}
		seen[record.Decision.ID] = true
		decisions = append(decisions, record.Decision)
	}
	return decisions, nil
}

func decisionProcessLock(path string) *sync.Mutex {
	value, _ := decisionProcessLocks.LoadOrStore(filepath.Clean(path), &sync.Mutex{})
	return value.(*sync.Mutex)
}

type decisionLogRecord struct {
	Decision IssueDecision
	Line     []byte
}

func (s Store) readDecisionRecordsUnlocked() ([]decisionLogRecord, error) {
	raw, err := os.ReadFile(s.DecisionsPath())
	if errors.Is(err, os.ErrNotExist) {
		return []decisionLogRecord{}, nil
	}
	if err != nil {
		return nil, err
	}
	reader := bufio.NewReader(bytes.NewReader(raw))
	records := make([]decisionLogRecord, 0)
	seen := map[string]IssueDecision{}
	for lineNumber := 1; ; lineNumber++ {
		line, readErr := reader.ReadBytes('\n')
		if len(line) > 0 {
			if errors.Is(readErr, io.EOF) {
				return nil, errors.New("decisions.jsonl has a partial final record")
			}
			if len(bytes.TrimSpace(line)) == 0 {
				return nil, fmt.Errorf("decode Issue decision line %d: blank record", lineNumber)
			}
			var decision IssueDecision
			decoder := json.NewDecoder(bytes.NewReader(line))
			decoder.DisallowUnknownFields()
			if decodeErr := decoder.Decode(&decision); decodeErr != nil {
				return nil, fmt.Errorf("decode Issue decision line %d: %w", lineNumber, decodeErr)
			}
			var trailing any
			if decodeErr := decoder.Decode(&trailing); !errors.Is(decodeErr, io.EOF) {
				return nil, fmt.Errorf("decode Issue decision line %d: trailing JSON", lineNumber)
			}
			if validateErr := decision.Validate(); validateErr != nil {
				return nil, fmt.Errorf("validate Issue decision line %d: %w", lineNumber, validateErr)
			}
			if previous, exists := seen[decision.ID]; exists {
				if !decisionsEqual(previous, decision) {
					return nil, fmt.Errorf("Issue decision %s was replayed with different content", decision.ID)
				}
			} else {
				seen[decision.ID] = decision
			}
			records = append(records, decisionLogRecord{Decision: decision, Line: append([]byte(nil), line...)})
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return nil, readErr
		}
	}
	return records, nil
}

func (s Store) readSignalsUnlocked() ([]Signal, error) {
	var history bytes.Buffer
	if err := retention.WriteHistory(&history, s.SignalsPath()); err != nil {
		return nil, err
	}
	reader := bufio.NewReader(bytes.NewReader(history.Bytes()))
	byID := map[string]Signal{}
	for lineNumber := 1; ; lineNumber++ {
		line, err := reader.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			if err != nil && errors.Is(err, io.EOF) {
				return nil, fmt.Errorf("signals.jsonl has a partial final record")
			}
			var signal Signal
			decoder := json.NewDecoder(bytes.NewReader(line))
			decoder.DisallowUnknownFields()
			if decodeErr := decoder.Decode(&signal); decodeErr != nil {
				return nil, fmt.Errorf("decode signal line %d: %w", lineNumber, decodeErr)
			}
			if validateErr := signal.Validate(); validateErr != nil {
				return nil, fmt.Errorf("validate signal line %d: %w", lineNumber, validateErr)
			}
			if previous, exists := byID[signal.ID]; exists && !signalsEqual(previous, signal) {
				return nil, fmt.Errorf("signal %s was replayed with different content", signal.ID)
			}
			byID[signal.ID] = signal
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	result := make([]Signal, 0, len(byID))
	for _, signal := range byID {
		result = append(result, signal)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Timestamp.Equal(result[j].Timestamp) {
			return result[i].ID < result[j].ID
		}
		return result[i].Timestamp.Before(result[j].Timestamp)
	})
	return result, nil
}

func (s Store) LoadState() (DurableState, error) {
	if err := s.Ensure(); err != nil {
		return DurableState{}, err
	}
	lock, err := s.lock("data.lock", false)
	if err != nil {
		return DurableState{}, err
	}
	defer unlockFile(lock)
	return s.loadStateUnlocked()
}

func (s Store) loadStateUnlocked() (DurableState, error) {
	state := DurableState{Version: SchemaVersion, Episodes: map[string]Episode{}}
	data, err := os.ReadFile(s.StatePath())
	if errors.Is(err, os.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		return DurableState{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&state); err != nil {
		return DurableState{}, fmt.Errorf("decode incident state: %w", err)
	}
	if err := state.Validate(); err != nil {
		return DurableState{}, err
	}
	return state, nil
}

func (s Store) SaveState(state DurableState, metrics Metrics) error {
	if err := s.Ensure(); err != nil {
		return err
	}
	lock, err := s.lock("data.lock", true)
	if err != nil {
		return err
	}
	defer unlockFile(lock)
	state.Version = SchemaVersion
	state.Revision++
	if err := state.Validate(); err != nil {
		return err
	}
	stateData, err := redact.Marshal(state, s.Secrets)
	if err != nil {
		return err
	}
	if err := fsutil.WriteFile(s.StatePath(), append(stateData, '\n'), 0o600); err != nil {
		return err
	}
	metrics.Version = SchemaVersion
	ensureMetricMaps(&metrics)
	if err := metrics.Validate(); err != nil {
		return err
	}
	metricsData, err := redact.Marshal(metrics, s.Secrets)
	if err != nil {
		return err
	}
	return fsutil.WriteFile(s.MetricsPath(), append(metricsData, '\n'), 0o600)
}

func (s Store) LoadMetrics() (Metrics, error) {
	if err := s.Ensure(); err != nil {
		return Metrics{}, err
	}
	lock, err := s.lock("data.lock", false)
	if err != nil {
		return Metrics{}, err
	}
	defer unlockFile(lock)
	return s.loadMetricsUnlocked()
}

func (s Store) loadMetricsUnlocked() (Metrics, error) {
	metrics := emptyMetrics()
	data, err := os.ReadFile(s.MetricsPath())
	if errors.Is(err, os.ErrNotExist) {
		return metrics, nil
	}
	if err != nil {
		return Metrics{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&metrics); err != nil {
		return Metrics{}, fmt.Errorf("decode incident metrics: %w", err)
	}
	if metrics.Version != SchemaVersion {
		return Metrics{}, errors.New("unsupported incident metrics version")
	}
	ensureMetricMaps(&metrics)
	if err := metrics.Validate(); err != nil {
		return Metrics{}, err
	}
	return metrics, nil
}

func (s Store) WriteDryRun(drafts []IssueDraft) error {
	safe, err := redact.Marshal(drafts, s.Secrets)
	if err != nil {
		return err
	}
	return fsutil.WriteFile(s.DryRunPath(), append(safe, '\n'), 0o600)
}

// ResetCircuit is the explicit operator recovery boundary for one fingerprint.
// It preserves evidence and Issue identity while resetting only bounded retry state.
func (s Store) ResetCircuit(fingerprint string, at time.Time) (Episode, error) {
	if !validFingerprint(fingerprint) || at.IsZero() {
		return Episode{}, errors.New("valid fingerprint and recovery timestamp are required")
	}
	if err := s.Ensure(); err != nil {
		return Episode{}, err
	}
	lock, err := s.lock("data.lock", true)
	if err != nil {
		return Episode{}, err
	}
	defer unlockFile(lock)
	state, err := s.loadStateUnlocked()
	if err != nil {
		return Episode{}, err
	}
	episode, exists := state.Episodes[fingerprint]
	if !exists {
		return Episode{}, fmt.Errorf("incident fingerprint %s was not found", fingerprint)
	}
	if episode.State == "resolved" {
		return Episode{}, errors.New("resolved incident cannot be retried")
	}
	if !episode.CircuitOpen && !episode.IssueCircuitOpen {
		return Episode{}, errors.New("incident circuit is not open")
	}
	episode.Attempts = 0
	episode.NextAttemptAt = nil
	episode.CircuitOpen = false
	episode.IssueAttempts = 0
	episode.IssueNextAttemptAt = nil
	episode.IssueCircuitOpen = false
	state.Episodes[fingerprint] = episode
	attentionFingerprint := digest("episode", episode.Repository, "automation-circuit-"+episode.ID)
	if attention, ok := state.Episodes[attentionFingerprint]; ok {
		attention.Lifecycle = boundedLifecycle(append(attention.Lifecycle, LifecycleResult{Stage: "recovery_completed", Outcome: "resolved", Timestamp: at.UTC(), Ref: episode.ID}), 128)
		attention.State = "resolved"
		if attention.UpdatedAt.Before(at) {
			attention.UpdatedAt = at.UTC()
		}
		state.Episodes[attentionFingerprint] = attention
	}
	state.Revision++
	state.UpdatedAt = at.UTC()
	if err := state.Validate(); err != nil {
		return Episode{}, err
	}
	stateData, err := redact.Marshal(state, s.Secrets)
	if err != nil {
		return Episode{}, err
	}
	if err := fsutil.WriteFile(s.StatePath(), append(stateData, '\n'), 0o600); err != nil {
		return Episode{}, err
	}
	metrics, err := s.loadMetricsUnlocked()
	if err != nil {
		return Episode{}, err
	}
	recomputeEpisodeMetrics(&metrics, state)
	metrics.UpdatedAt = at.UTC()
	metricsData, err := redact.Marshal(metrics, s.Secrets)
	if err != nil {
		return Episode{}, err
	}
	if err := fsutil.WriteFile(s.MetricsPath(), append(metricsData, '\n'), 0o600); err != nil {
		return Episode{}, err
	}
	return episode, nil
}

// TryProcessLock prevents two schedulers from analyzing or creating an Issue
// concurrently. The lock is advisory and held by the caller for one run.
func (s Store) TryProcessLock() (func(), error) {
	if err := s.Ensure(); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(filepath.Join(s.Dir, "process.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrAlreadyRunning
		}
		return nil, err
	}
	return func() { unlockFile(file) }, nil
}

func (s Store) lock(name string, exclusive bool) (*os.File, error) {
	file, err := os.OpenFile(filepath.Join(s.Dir, name), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	operation := syscall.LOCK_SH
	if exclusive {
		operation = syscall.LOCK_EX
	}
	if err := syscall.Flock(int(file.Fd()), operation); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func unlockFile(file *os.File) {
	_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	_ = file.Close()
}

func sanitizeSignal(signal Signal, secrets []string) (Signal, error) {
	raw, err := redact.Marshal(signal, secrets)
	if err != nil {
		return Signal{}, err
	}
	var safe Signal
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&safe); err != nil {
		return Signal{}, err
	}
	if err := safe.Validate(); err != nil {
		return Signal{}, err
	}
	return safe, nil
}

func signalsEqual(left, right Signal) bool {
	a, _ := json.Marshal(left)
	b, _ := json.Marshal(right)
	return bytes.Equal(a, b)
}

func emptyMetrics() Metrics {
	metrics := Metrics{Version: SchemaVersion}
	ensureMetricMaps(&metrics)
	return metrics
}

func ensureMetricMaps(metrics *Metrics) {
	if metrics.SignalsByName == nil {
		metrics.SignalsByName = map[string]uint64{}
	}
	if metrics.Outcomes == nil {
		metrics.Outcomes = map[string]uint64{}
	}
	if metrics.Classifications == nil {
		metrics.Classifications = map[string]uint64{}
	}
	if metrics.Issues == nil {
		metrics.Issues = map[string]uint64{}
	}
	if metrics.AnalysisAttempts == nil {
		metrics.AnalysisAttempts = map[string]uint64{}
	}
	if metrics.AnalysisFailures == nil {
		metrics.AnalysisFailures = map[string]uint64{}
	}
	if metrics.DurationsMS == nil {
		metrics.DurationsMS = map[string]DurationSummary{}
	}
}
