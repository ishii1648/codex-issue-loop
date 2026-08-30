// Package recoveryfixture captures sanitized, lossless recovery evidence.
//
// A fixture deliberately stores JSON records as raw messages.  This keeps the
// distinction between an omitted key, null, an empty collection, and a zero
// value -- distinctions which have historically mattered to recovery code.
package recoveryfixture

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	gh "github.com/ishii1648/codex-issue-loop/internal/adapter/github"
	"github.com/ishii1648/codex-issue-loop/internal/adapter/state"
	"github.com/ishii1648/codex-issue-loop/internal/adapter/worktree"
	"github.com/ishii1648/codex-issue-loop/internal/domain/statecontract"
	"github.com/ishii1648/codex-issue-loop/internal/platform/redact"
)

const (
	FormatVersion    = 1
	SanitizerVersion = 1
)

type Omission struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

type Acquisition struct {
	DurableState string `json:"durable_state"`
	Events       string `json:"events"`
	Worktree     string `json:"worktree"`
	Remote       string `json:"remote"`
}

type Manifest struct {
	SourceSchemaVersion  int         `json:"source_schema_version"`
	SourceVersion        string      `json:"source_version"`
	SanitizerVersion     int         `json:"sanitizer_version"`
	CapturedAt           time.Time   `json:"captured_at"`
	Repository           string      `json:"repository"`
	IssueNumber          int         `json:"issue_number"`
	Acquisition          Acquisition `json:"acquisition"`
	IntentionalOmissions []Omission  `json:"intentional_omissions"`
	ContentSHA256        string      `json:"content_sha256"`
}

type DurableCapture struct {
	RepoID          string            `json:"repo_id"`
	RepoPath        string            `json:"repo_path"`
	StateRevision   uint64            `json:"state_revision"`
	Issue           json.RawMessage   `json:"issue"`
	PendingRequests []json.RawMessage `json:"pending_requests"`
}

type Capture struct {
	Durable  DurableCapture      `json:"durable"`
	Events   []json.RawMessage   `json:"events"`
	Worktree worktree.Inspection `json:"worktree"`
	Remote   gh.RemoteState      `json:"remote"`
}

// Completeness is redundant by design.  It makes accidental compression,
// field backfilling, null/empty normalization, reordering, and scalar
// substitution independently visible instead of relying only on one hash.
type Completeness struct {
	EventCount       int      `json:"event_count"`
	EventSequences   []uint64 `json:"event_sequences"`
	EventTypes       []string `json:"event_types"`
	EventShapeSHA256 string   `json:"event_shape_sha256"`
	ValueSHA256      string   `json:"value_sha256"`
	TimestampSHA256  string   `json:"timestamp_sha256"`
	ReferenceSHA256  string   `json:"reference_sha256"`
}

type Bundle struct {
	FormatVersion int          `json:"format_version"`
	Manifest      Manifest     `json:"manifest"`
	Capture       Capture      `json:"capture"`
	Completeness  Completeness `json:"completeness"`
}

type Input struct {
	SourceSchemaVersion  int
	SourceVersion        string
	CapturedAt           time.Time
	Repository           string
	IssueNumber          int
	RepoID               string
	RepoPath             string
	StateRevision        uint64
	Issue                json.RawMessage
	PendingRequests      []json.RawMessage
	Events               []json.RawMessage
	Worktree             worktree.Inspection
	Remote               gh.RemoteState
	Secrets              []string
	IntentionalOmissions []Omission
}

// Replay is a local, non-mutating state store/GitHub/filesystem harness.  It
// decodes exactly the same records that were verified, then returns detached
// values suitable for the production recovery predicates and CLI tests.
type Replay struct {
	Snapshot state.Snapshot
	Events   []state.Event
	Worktree worktree.Inspection
	Remote   gh.RemoteState
}

func (bundle Bundle) Replay() (Replay, error) {
	if err := Validate(bundle); err != nil {
		return Replay{}, err
	}
	result := Replay{
		Snapshot: state.Snapshot{
			Version: bundle.Manifest.SourceSchemaVersion, RepoID: bundle.Capture.Durable.RepoID,
			RepoPath: bundle.Capture.Durable.RepoPath, StateRevision: bundle.Capture.Durable.StateRevision,
			Issues: map[string]*state.Issue{}, PendingRequests: map[string]*state.Request{},
		},
		Worktree: bundle.Capture.Worktree,
		Remote:   bundle.Capture.Remote,
	}
	var issue state.Issue
	if err := strictDecode(bundle.Capture.Durable.Issue, &issue); err != nil {
		return Replay{}, err
	}
	result.Snapshot.Issues[strconv.Itoa(issue.Number)] = &issue
	for _, raw := range bundle.Capture.Durable.PendingRequests {
		var request state.Request
		if err := strictDecode(raw, &request); err != nil {
			return Replay{}, err
		}
		result.Snapshot.PendingRequests[request.ID] = &request
	}
	for _, raw := range bundle.Capture.Events {
		var event state.Event
		if err := strictDecode(raw, &event); err != nil {
			return Replay{}, err
		}
		result.Events = append(result.Events, event)
	}
	return result, nil
}

var defaultOmissions = []Omission{
	{Path: "durable.issues[issue_number!=target]", Reason: "unrelated Issue state is outside the acquisition scope"},
	{Path: "durable.pending_requests[issue_number!=target]", Reason: "unrelated manual requests are outside the acquisition scope"},
	{Path: "events[issue_number!=target]", Reason: "unrelated repository events are outside the acquisition scope"},
	{Path: "remote.issue.body", Reason: "Issue body is not a recovery predicate and may contain sensitive production text"},
	{Path: "remote.issue.comments[].non_marker_text", Reason: "only exact automation marker lines and required failure reasons are retained"},
	{Path: "worktree.contents", Reason: "worktree content is not exported; only read-only inspection results are captured"},
	{Path: "worker_run_artifacts", Reason: "worker prompts, logs, and results are not required by the selected recovery predicates"},
}

func Build(input Input) (Bundle, error) {
	if input.SourceSchemaVersion <= 0 || strings.TrimSpace(input.SourceVersion) == "" || input.IssueNumber <= 0 ||
		len(bytes.TrimSpace(input.Issue)) == 0 || len(input.Events) == 0 || input.CapturedAt.IsZero() {
		return Bundle{}, errors.New("recovery fixture input is incomplete")
	}
	bundle := Bundle{
		FormatVersion: FormatVersion,
		Manifest: Manifest{
			SourceSchemaVersion: input.SourceSchemaVersion,
			SourceVersion:       input.SourceVersion,
			SanitizerVersion:    SanitizerVersion,
			CapturedAt:          input.CapturedAt.UTC(),
			Repository:          input.Repository,
			IssueNumber:         input.IssueNumber,
			Acquisition: Acquisition{
				DurableState: "target Issue plus its pending-request records from state.json",
				Events:       "all events.jsonl records whose issue_number equals the target",
				Worktree:     "one read-only git inspection of the saved worktree and branch",
				Remote:       "one read-only GitHub Issue/labels/comments/PR identity inspection",
			},
			IntentionalOmissions: append([]Omission(nil), input.IntentionalOmissions...),
		},
		Capture: Capture{
			Durable: DurableCapture{
				RepoID: input.RepoID, RepoPath: input.RepoPath, StateRevision: input.StateRevision,
				Issue: append(json.RawMessage(nil), input.Issue...), PendingRequests: cloneRaw(input.PendingRequests),
			},
			Events: cloneRaw(input.Events), Worktree: input.Worktree, Remote: input.Remote,
		},
	}
	if len(bundle.Manifest.IntentionalOmissions) == 0 {
		bundle.Manifest.IntentionalOmissions = append([]Omission(nil), defaultOmissions...)
	}

	sanitized, err := sanitizeBundle(bundle, input.Secrets)
	if err != nil {
		return Bundle{}, err
	}
	sanitized.Completeness, err = summarize(sanitized)
	if err != nil {
		return Bundle{}, err
	}
	sanitized.Manifest.ContentSHA256, err = contentHash(sanitized)
	if err != nil {
		return Bundle{}, err
	}
	if err := Validate(sanitized); err != nil {
		return Bundle{}, fmt.Errorf("validate generated recovery fixture: %w", err)
	}
	return sanitized, nil
}

func Load(path string) (Bundle, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Bundle{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var bundle Bundle
	if err := decoder.Decode(&bundle); err != nil {
		return Bundle{}, fmt.Errorf("decode recovery fixture: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return Bundle{}, errors.New("recovery fixture contains trailing JSON")
	}
	if err := Validate(bundle); err != nil {
		return Bundle{}, err
	}
	return bundle, nil
}

func Validate(bundle Bundle) error {
	if bundle.FormatVersion != FormatVersion {
		return fmt.Errorf("unsupported recovery fixture format %d", bundle.FormatVersion)
	}
	if bundle.Manifest.SanitizerVersion != SanitizerVersion {
		return fmt.Errorf("unsupported recovery fixture sanitizer %d", bundle.Manifest.SanitizerVersion)
	}
	if bundle.Manifest.SourceSchemaVersion <= 0 || bundle.Manifest.SourceVersion == "" || bundle.Manifest.CapturedAt.IsZero() ||
		bundle.Manifest.Repository == "" || bundle.Manifest.IssueNumber <= 0 {
		return errors.New("recovery fixture manifest is incomplete")
	}
	if bundle.Manifest.Acquisition.DurableState == "" || bundle.Manifest.Acquisition.Events == "" ||
		bundle.Manifest.Acquisition.Worktree == "" || bundle.Manifest.Acquisition.Remote == "" ||
		bundle.Capture.Durable.RepoID == "" || bundle.Capture.Durable.RepoPath == "" || bundle.Capture.Durable.StateRevision == 0 {
		return errors.New("recovery fixture acquisition scope or durable identity is incomplete")
	}
	if len(bundle.Manifest.IntentionalOmissions) == 0 {
		return errors.New("recovery fixture does not declare intentional omissions")
	}
	for _, omission := range bundle.Manifest.IntentionalOmissions {
		if strings.TrimSpace(omission.Path) == "" || strings.TrimSpace(omission.Reason) == "" {
			return errors.New("recovery fixture contains an undocumented omission")
		}
	}
	var issue state.Issue
	if err := strictDecode(bundle.Capture.Durable.Issue, &issue); err != nil {
		return fmt.Errorf("decode durable Issue: %w", err)
	}
	if issue.Number != bundle.Manifest.IssueNumber {
		return fmt.Errorf("durable Issue number %d does not match manifest %d", issue.Number, bundle.Manifest.IssueNumber)
	}
	requestIDs := map[string]bool{}
	for index, raw := range bundle.Capture.Durable.PendingRequests {
		var request state.Request
		if err := strictDecode(raw, &request); err != nil || request.IssueNumber != issue.Number || request.ID == "" || requestIDs[request.ID] {
			return fmt.Errorf("pending request %d is invalid or belongs to another Issue", index)
		}
		requestIDs[request.ID] = true
	}
	if err := validateReconstructedSnapshot(bundle, issue); err != nil {
		return err
	}
	if bundle.Capture.Remote.Issue.Number != issue.Number {
		return errors.New("remote Issue identity does not match durable state")
	}
	want, err := summarize(bundle)
	if err != nil {
		return err
	}
	if !equalCompleteness(bundle.Completeness, want) {
		return errors.New("recovery fixture completeness metadata does not match its records")
	}
	hash, err := contentHash(bundle)
	if err != nil {
		return err
	}
	if !strings.EqualFold(bundle.Manifest.ContentSHA256, hash) {
		return errors.New("recovery fixture content hash mismatch")
	}
	return nil
}

func validateReconstructedSnapshot(bundle Bundle, issue state.Issue) error {
	snapshot := state.Snapshot{
		Version: state.CurrentVersion, SemanticContractVersion: statecontract.CurrentVersion,
		RepoID: bundle.Capture.Durable.RepoID, RepoPath: bundle.Capture.Durable.RepoPath,
		StateRevision: bundle.Capture.Durable.StateRevision,
		Supervisor:    state.Supervisor{State: state.SupervisorStateStopped, UpdatedAt: bundle.Manifest.CapturedAt},
		Issues:        map[string]*state.Issue{strconv.Itoa(issue.Number): &issue}, PendingRequests: map[string]*state.Request{},
	}
	for _, raw := range bundle.Capture.Durable.PendingRequests {
		var request state.Request
		if err := strictDecode(raw, &request); err != nil {
			return err
		}
		snapshot.PendingRequests[request.ID] = &request
	}
	err := snapshot.Validate()
	if err == nil {
		return nil
	}
	var compatibility state.SemanticCompatibilityError
	if !errors.As(err, &compatibility) || len(compatibility.Violations) != 1 ||
		compatibility.Violations[0].Code != state.SemanticCodeWorkspaceProvenanceMissing {
		return fmt.Errorf("reconstructed production snapshot violates aggregate invariants: %w", err)
	}
	return nil
}

func strictDecode(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	return decoder.Decode(target)
}

func summarize(bundle Bundle) (Completeness, error) {
	result := Completeness{EventCount: len(bundle.Capture.Events)}
	shapes := make([]string, 0, len(bundle.Capture.Events)+4)
	values := make([]string, 0)
	timestamps := make([]string, 0)
	references := make([]string, 0)
	all := []json.RawMessage{bundle.Capture.Durable.Issue}
	all = append(all, bundle.Capture.Durable.PendingRequests...)
	all = append(all, bundle.Capture.Events...)
	extra, err := json.Marshal(struct {
		Worktree worktree.Inspection `json:"worktree"`
		Remote   gh.RemoteState      `json:"remote"`
	}{bundle.Capture.Worktree, bundle.Capture.Remote})
	if err != nil {
		return result, err
	}
	all = append(all, extra)
	for recordIndex, raw := range all {
		var value any
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		if err := decoder.Decode(&value); err != nil {
			return result, fmt.Errorf("decode fixture record %d: %w", recordIndex, err)
		}
		walkSummary(value, "$", recordIndex, &shapes, &values, &timestamps, &references)
	}
	var previous uint64
	for index, raw := range bundle.Capture.Events {
		var event state.Event
		if err := strictDecode(raw, &event); err != nil {
			return result, fmt.Errorf("decode event %d: %w", index, err)
		}
		if event.Version != bundle.Manifest.SourceSchemaVersion || event.RepoID != bundle.Capture.Durable.RepoID ||
			event.IssueNumber != bundle.Manifest.IssueNumber || event.EventID == "" || event.Type == "" || event.Timestamp.IsZero() ||
			event.Sequence == 0 || (index > 0 && event.Sequence <= previous) {
			return result, fmt.Errorf("event %d is outside scope or not strictly ordered", index)
		}
		previous = event.Sequence
		result.EventSequences = append(result.EventSequences, event.Sequence)
		result.EventTypes = append(result.EventTypes, event.Type)
	}
	result.EventShapeSHA256 = digestLines(shapes)
	result.ValueSHA256 = digestLines(values)
	result.TimestampSHA256 = digestLines(timestamps)
	result.ReferenceSHA256 = digestLines(references)
	return result, nil
}

func walkSummary(value any, path string, record int, shapes, values, timestamps, references *[]string) {
	switch current := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(current))
		for key := range current {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		*shapes = append(*shapes, fmt.Sprintf("%d:%s:object:%s", record, path, strings.Join(keys, ",")))
		for _, key := range keys {
			walkSummary(current[key], path+"."+key, record, shapes, values, timestamps, references)
		}
	case []any:
		*shapes = append(*shapes, fmt.Sprintf("%d:%s:array:%d", record, path, len(current)))
		for index, item := range current {
			walkSummary(item, path+"["+strconv.Itoa(index)+"]", record, shapes, values, timestamps, references)
		}
	case nil:
		*values = append(*values, fmt.Sprintf("%d:%s:null", record, path))
	case string:
		*values = append(*values, fmt.Sprintf("%d:%s:string:%s", record, path, current))
		if _, err := time.Parse(time.RFC3339Nano, current); err == nil {
			*timestamps = append(*timestamps, fmt.Sprintf("%d:%s:%s", record, path, current))
		}
		matches := referencePattern.FindAllString(current, -1)
		for _, match := range matches {
			*references = append(*references, fmt.Sprintf("%d:%s:%s", record, path, match))
		}
		if len(matches) == 0 && isIdentityKey(pathKey(path)) && idValuePattern.MatchString(current) {
			*references = append(*references, fmt.Sprintf("%d:%s:%s", record, path, current))
		}
	case json.Number:
		*values = append(*values, fmt.Sprintf("%d:%s:number:%s", record, path, current))
	case bool:
		*values = append(*values, fmt.Sprintf("%d:%s:bool:%t", record, path, current))
	default:
		*values = append(*values, fmt.Sprintf("%d:%s:%T:%v", record, path, current, current))
	}
}

func equalCompleteness(left, right Completeness) bool {
	return left.EventCount == right.EventCount && slicesEqual(left.EventSequences, right.EventSequences) &&
		stringsEqual(left.EventTypes, right.EventTypes) && left.EventShapeSHA256 == right.EventShapeSHA256 &&
		left.ValueSHA256 == right.ValueSHA256 && left.TimestampSHA256 == right.TimestampSHA256 &&
		left.ReferenceSHA256 == right.ReferenceSHA256
}

func slicesEqual[T comparable](left, right []T) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func stringsEqual(left, right []string) bool { return slicesEqual(left, right) }

func contentHash(bundle Bundle) (string, error) {
	bundle.Manifest.ContentSHA256 = ""
	data, err := json.Marshal(bundle)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

func digestLines(lines []string) string {
	hash := sha256.New()
	for _, line := range lines {
		hash.Write([]byte(line))
		hash.Write([]byte{0})
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func cloneRaw(values []json.RawMessage) []json.RawMessage {
	result := make([]json.RawMessage, len(values))
	for index := range values {
		result[index] = append(json.RawMessage(nil), values[index]...)
	}
	return result
}

var (
	absolutePathPattern   = regexp.MustCompile(`(?:^|[\s=:"'(])(/(?:[^\s:"'()<>]|\\ )+)`)
	urlPattern            = regexp.MustCompile(`https?://[^\s"'<>]+`)
	shaPattern            = regexp.MustCompile(`\b[0-9a-fA-F]{40}\b`)
	referencePattern      = regexp.MustCompile(`\b(?:run|resume|req|park|conflict|session|event|evt|checks_recovery|publication_recovery)_[A-Za-z0-9._-]+\b|<!-- codex-issue-loop:[^>]+ -->`)
	idValuePattern        = regexp.MustCompile(`^(?:run|resume|req|park|retry|conflict|session|event|evt|checks_recovery|publication_recovery|merged_pr_adoption)_[A-Za-z0-9._-]+$`)
	markerPattern         = regexp.MustCompile(`<!-- codex-issue-loop:(?:done|failed:[0-9]+|failure:[0-9a-f]{16}|claim:run_[A-Za-z0-9._-]+|request:req_[A-Za-z0-9._-]+|conflict-retry:retry_[A-Za-z0-9._-]+|environment-resume:resume_[A-Za-z0-9._-]+|publication-recovery:publication_recovery_[A-Za-z0-9._-]+|checks-recovery:checks_recovery_[A-Za-z0-9._-]+) -->`)
	markerExactPattern    = regexp.MustCompile(`^<!-- codex-issue-loop:(?:done|failed:[0-9]+|failure:[0-9a-f]{16}|claim:run_[A-Za-z0-9._-]+|request:req_[A-Za-z0-9._-]+|conflict-retry:retry_[A-Za-z0-9._-]+|environment-resume:resume_[A-Za-z0-9._-]+|publication-recovery:publication_recovery_[A-Za-z0-9._-]+|checks-recovery:checks_recovery_[A-Za-z0-9._-]+) -->$`)
	markerIDPattern       = regexp.MustCompile(`((?:claim|request|conflict-retry|environment-resume|publication-recovery|checks-recovery):)((?:run|resume|req|retry|publication_recovery|checks_recovery)_[A-Za-z0-9._-]+)`)
	failureCommentPattern = regexp.MustCompile(`(?s)^(<!-- codex-issue-loop:failed:[0-9]+ -->)\n<!-- codex-issue-loop:failure:[0-9a-f]+ -->\nAutomation stopped: (.*)$`)
)

type sanitizer struct {
	secrets      []string
	replacements map[string]string
}

func sanitizeBundle(bundle Bundle, secrets []string) (Bundle, error) {
	s := &sanitizer{secrets: append([]string(nil), secrets...), replacements: map[string]string{}}
	data, err := json.Marshal(bundle)
	if err != nil {
		return Bundle{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return Bundle{}, err
	}
	value = s.walk(value, "")
	safe, err := json.Marshal(value)
	if err != nil {
		return Bundle{}, err
	}
	var result Bundle
	if err := json.Unmarshal(safe, &result); err != nil {
		return Bundle{}, err
	}
	return result, nil
}

func (s *sanitizer) walk(value any, key string) any {
	switch current := value.(type) {
	case map[string]any:
		for childKey, child := range current {
			current[childKey] = s.walk(child, childKey)
		}
	case []any:
		for index := range current {
			current[index] = s.walk(current[index], key)
		}
	case string:
		return s.string(current, key)
	}
	return value
}

func (s *sanitizer) string(value, key string) string {
	key = strings.ToLower(key)
	value = redact.StringWithSecrets(value, s.secrets)
	if key == "body" {
		return "sanitized-body-" + shortDigest(value)
	}
	if key == "title" || key == "question" || key == "answer" {
		return "sanitized-" + key + "-" + shortDigest(value)
	}
	if key == "comments" {
		return sanitizeComment(value, s)
	}
	if isIdentityKey(key) && idValuePattern.MatchString(value) {
		return s.replace("id", value, identityPrefix(value))
	}
	value = urlPattern.ReplaceAllStringFunc(value, func(match string) string { return s.replace("url", match, "https://example.invalid/") })
	value = shaPattern.ReplaceAllStringFunc(value, func(match string) string { return s.replace("sha", strings.ToLower(match), "") })
	value = referencePattern.ReplaceAllStringFunc(value, func(match string) string {
		if strings.HasPrefix(match, "<!--") {
			if !markerExactPattern.MatchString(match) {
				return "sanitized-marker-" + shortDigest(match)
			}
			return markerIDPattern.ReplaceAllStringFunc(match, func(token string) string {
				parts := markerIDPattern.FindStringSubmatch(token)
				return parts[1] + s.replace("id", parts[2], identityPrefix(parts[2]))
			})
		}
		return s.replace("id", match, identityPrefix(match))
	})
	value = absolutePathPattern.ReplaceAllStringFunc(value, func(fragment string) string {
		prefix := fragment[:len(fragment)-len(strings.TrimLeft(fragment, " \t=:"+"\"'("))]
		path := strings.TrimPrefix(fragment, prefix)
		return prefix + s.replace("path", path, "/sanitized/")
	})
	return value
}

func sanitizeComment(value string, s *sanitizer) string {
	if match := failureCommentPattern.FindStringSubmatch(value); len(match) == 3 {
		reason := s.string(match[2], "reason")
		digest := sha256.Sum256([]byte(reason))
		return match[1] + "\n<!-- codex-issue-loop:failure:" + hex.EncodeToString(digest[:8]) + " -->\nAutomation stopped: " + reason
	}
	markers := markerPattern.FindAllString(value, -1)
	if len(markers) == 0 {
		return "sanitized-comment-" + shortDigest(value)
	}
	for index := range markers {
		markers[index] = s.string(markers[index], "marker")
	}
	return strings.Join(markers, "\n")
}

func (s *sanitizer) replace(kind, value, prefix string) string {
	lookup := kind + "\x00" + value
	if replacement, ok := s.replacements[lookup]; ok {
		return replacement
	}
	length := 16
	if kind == "sha" {
		length = len(value)
	}
	replacement := prefix + deterministicHex(kind+"\x00"+value, length)
	s.replacements[lookup] = replacement
	return replacement
}

func deterministicHex(value string, length int) string {
	var output strings.Builder
	for counter := 0; output.Len() < length; counter++ {
		digest := sha256.Sum256([]byte(fmt.Sprintf("recovery-fixture-v%d\x00%s\x00%d", SanitizerVersion, value, counter)))
		output.WriteString(hex.EncodeToString(digest[:]))
	}
	return output.String()[:length]
}

func shortDigest(value string) string { return deterministicHex(value, 16) }

func identityPrefix(value string) string {
	for _, prefix := range []string{"publication_recovery_", "checks_recovery_", "merged_pr_adoption_"} {
		if strings.HasPrefix(value, prefix) {
			return prefix
		}
	}
	return strings.SplitN(value, "_", 2)[0] + "_"
}

func pathKey(path string) string {
	if index := strings.LastIndexByte(path, '.'); index >= 0 {
		return path[index+1:]
	}
	return path
}

func isIdentityKey(key string) bool {
	key = strings.TrimSuffix(strings.ToLower(key), "]")
	return key == "id" || strings.HasSuffix(key, "_id") || key == "eventid" || key == "runid" || key == "sessionid"
}
