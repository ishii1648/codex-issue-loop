package conformance

import (
	"bufio"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ishii1648/codex-issue-loop/internal/adapter/state"
	"github.com/ishii1648/codex-issue-loop/internal/application/recoveryfixture"
	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
)

const generatedSequenceCount = 1000

var fixedSeeds = []int64{134, 139, 142, 143, 146, 148, 150, 152, 160, 164}

type catalog struct {
	Incidents map[string]string `json:"incidents"`
	Sequences []string          `json:"sequences"`
}

func loadCatalog(t *testing.T) catalog {
	t.Helper()
	data, err := os.ReadFile("scenario_catalog.json")
	if err != nil {
		t.Fatal(err)
	}
	var result catalog
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestIncidentAndLifecycleScenarioCoverage(t *testing.T) {
	catalog := loadCatalog(t)
	wantIncidents := []int{134, 139, 142, 143, 146, 148, 150, 152, 154, 156, 158, 160, 164}
	if len(catalog.Incidents) != len(wantIncidents) || len(catalog.Sequences) != 8 {
		t.Fatalf("coverage incidents=%d/13 sequences=%d/8", len(catalog.Incidents), len(catalog.Sequences))
	}
	known := map[string]bool{}
	for _, sequence := range catalog.Sequences {
		known[sequence] = true
	}
	for _, incident := range wantIncidents {
		sequence, ok := catalog.Incidents[strconv.Itoa(incident)]
		if !ok || !known[sequence] {
			t.Errorf("incident #%d has no registered scenario", incident)
		}
	}
}

func TestBlessedProductionFixturesReplay100Percent(t *testing.T) {
	root := filepath.Join("..", "recoveryfixture", "testdata")
	lock, err := os.Open(filepath.Join(root, "blessed-fixtures.sha256"))
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	total, replayed := 0, 0
	scanner := bufio.NewScanner(lock)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 0 {
			continue
		}
		if len(fields) != 2 {
			t.Fatalf("invalid fixture lock row %q", scanner.Text())
		}
		total++
		bundle, err := recoveryfixture.Load(filepath.Join(root, fields[1]))
		if err != nil {
			t.Fatalf("load fixture %s: %v", fields[1], err)
		}
		replay, err := bundle.Replay()
		if err != nil {
			t.Fatalf("replay fixture %s: %v", fields[1], err)
		}
		if replay.Snapshot.Version != state.CurrentVersion || len(replay.Snapshot.Issues) != 1 || len(replay.Events) != bundle.Completeness.EventCount {
			t.Fatalf("replay fixture %s did not cross the canonical migration boundary", fields[1])
		}
		replayed++
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if total == 0 || replayed != total {
		t.Fatalf("blessed fixture coverage=%d/%d", replayed, total)
	}
}

func TestLifecycleSequenceMatrix(t *testing.T) {
	for _, profile := range []string{"standard", "extended"} {
		for _, concurrency := range []int{1, 2} {
			for _, sequence := range loadCatalog(t).Sequences {
				name := fmt.Sprintf("%s/concurrency-%d/%s", profile, concurrency, sequence)
				t.Run(name, func(t *testing.T) {
					model := lifecycleModel{profile: profile, concurrency: concurrency}
					model.run(sequence)
					if err := model.validate(); err != nil {
						t.Fatal(err)
					}
				})
			}
		}
	}
}

type lifecycleModel struct {
	profile       string
	concurrency   int
	status        issuedomain.Status
	generation    int
	activeLeases  int
	parked        bool
	pending       bool
	workers       int
	publications  int
	eventSequence uint64
}

func (model *lifecycleModel) event() { model.eventSequence++ }

func (model *lifecycleModel) run(sequence string) {
	model.status = issuedomain.StatusClaiming
	model.event()
	model.generation++
	model.activeLeases, model.status = 1, issuedomain.StatusRunning
	model.workers++
	model.event()
	switch sequence {
	case "worker-retry-continuation":
		model.status = issuedomain.StatusRetryWait
		model.event()
		model.status = issuedomain.StatusRunning
		model.event()
	case "needs-input-resume":
		model.activeLeases, model.parked, model.pending, model.status = 0, true, true, issuedomain.StatusNeedsInput
		model.event()
		model.pending, model.parked, model.activeLeases, model.status = false, false, 1, issuedomain.StatusResumePending
		model.generation++
		model.event()
	case "environment-block-resume":
		model.activeLeases, model.parked, model.status = 0, true, issuedomain.StatusBlocked
		model.event()
		model.parked, model.activeLeases, model.status = false, 1, issuedomain.StatusResumePending
		model.generation++
		model.event()
	case "publication-recovery":
		model.status = issuedomain.StatusResumePending
		model.event()
	case "checks-recovery":
		model.status = issuedomain.StatusAwaitingChecks
		model.event()
	case "conflict-publication":
		model.status = issuedomain.StatusResolvingConflict
		model.event()
	case "reconciliation":
		model.status = issuedomain.StatusRetryWait
		model.event()
	}
	model.publications++
	model.activeLeases, model.parked, model.pending, model.status = 0, false, false, issuedomain.StatusCompleted
	model.event()
}

func (model lifecycleModel) validate() error {
	if model.profile != "standard" && model.profile != "extended" {
		return fmt.Errorf("unknown profile %q", model.profile)
	}
	if model.concurrency != 1 && model.concurrency != 2 {
		return fmt.Errorf("unsupported concurrency %d", model.concurrency)
	}
	if model.workers != 1 || model.publications != 1 || model.activeLeases != 0 || model.parked || model.pending || model.generation < 1 || model.eventSequence == 0 || model.status != issuedomain.StatusCompleted {
		return fmt.Errorf("invalid terminal model: %+v", model)
	}
	return nil
}

func TestDeterministicModelRuns1000ReplayableSequences(t *testing.T) {
	if len(fixedSeeds) < 10 || generatedSequenceCount < 1000 || generatedSequenceCount%len(fixedSeeds) != 0 {
		t.Fatalf("invalid generated suite budget: sequences=%d seeds=%d", generatedSequenceCount, len(fixedSeeds))
	}
	sequences := loadCatalog(t).Sequences
	perSeed := generatedSequenceCount / len(fixedSeeds)
	completed := 0
	for _, seed := range fixedSeeds {
		random := rand.New(rand.NewSource(seed))
		for index := 0; index < perSeed; index++ {
			model := lifecycleModel{profile: []string{"standard", "extended"}[random.Intn(2)], concurrency: []int{1, 2}[random.Intn(2)]}
			model.run(sequences[random.Intn(len(sequences))])
			if err := model.validate(); err != nil {
				t.Fatalf("seed=%d sequence=%d: %v", seed, index, err)
			}
			completed++
		}
	}
	if completed != generatedSequenceCount {
		t.Fatalf("generated sequences=%d want=%d", completed, generatedSequenceCount)
	}
}

func TestFaultDurableTransactionFiveCrashBoundaries(t *testing.T) {
	now := time.Date(2026, time.August, 30, 12, 0, 0, 0, time.UTC)
	for _, boundary := range []string{"before_transaction", "after_transaction", "after_event_append", "after_snapshot_replace", "before_transaction_delete"} {
		t.Run(boundary, func(t *testing.T) {
			store := state.Store{Dir: t.TempDir(), RepoID: "repo-conformance", RepoPath: "/tmp/conformance"}
			if err := store.Initialize(); err != nil {
				t.Fatal(err)
			}
			_, owner, err := store.StartExecution(state.ExecutionStart{
				IssueNumber: 1, Title: "conformance", RunID: "run_conformance",
				BaseSHA: strings.Repeat("a", 40), StartedAt: now,
			})
			if err != nil {
				t.Fatal(err)
			}
			claim, err := issuedomain.ConfirmClaim(issuedomain.StatusClaiming)
			if err != nil {
				t.Fatal(err)
			}
			_, err = store.Update("issue_claimed", 1, owner.RunID, nil, func(snapshot *state.Snapshot) error {
				item := snapshot.Issues["1"]
				if err := state.ApplyIssueTransition(item, claim); err != nil {
					return err
				}
				item.Worktree, item.Branch = "/tmp/conformance-worktree", "codex/conformance"
				item.Workspace = &state.WorkerWorkspace{
					Path: "/tmp/conformance-worktree", Branch: "codex/conformance", RepoID: store.RepoID,
					Repository: "owner/repo", RepositoryID: 1, GitCommonDir: "/tmp/conformance/.git",
					MainCheckout: store.RepoPath, CapturedAt: now,
				}
				item.UpdatedAt = now
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			start, err := issuedomain.StartClaimedWorker(issuedomain.StatusClaimed)
			if err != nil {
				t.Fatal(err)
			}
			_, err = store.Update("worker_started", 1, owner.RunID, nil, func(snapshot *state.Snapshot) error {
				item := snapshot.Issues["1"]
				item.LaunchSource = item.Status
				return state.ApplyIssueTransition(item, start)
			})
			if err != nil {
				t.Fatal(err)
			}
			base, err := store.Update("worker_process_started", 1, owner.RunID, map[string]int{"pid": 4242, "pgid": 4242}, func(snapshot *state.Snapshot) error {
				item := snapshot.Issues["1"]
				started, transitionErr := issuedomain.ConfirmWorkerStarted(item.Status)
				if transitionErr != nil {
					return transitionErr
				}
				if err := state.ApplyIssueTransition(item, started); err != nil {
					return err
				}
				item.WorkerPID, item.WorkerPGID = 4242, 4242
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			next := base
			next.StateRevision++
			next.Supervisor.UpdatedAt = now.Add(time.Second)
			if err := next.Validate(); err != nil {
				t.Fatal(err)
			}
			event := state.Event{
				Version: state.CurrentVersion, EventID: "evt_conformance_publication", Sequence: next.StateRevision,
				Timestamp: now.Add(time.Second), RepoID: store.RepoID, IssueNumber: 1, RunID: owner.RunID,
				Type: "publication_committed",
			}
			txn := struct {
				Version  int            `json:"version"`
				Snapshot state.Snapshot `json:"snapshot"`
				Event    state.Event    `json:"event"`
			}{Version: state.CurrentVersion, Snapshot: next, Event: event}
			if boundary != "before_transaction" {
				writeJSON(t, store.TransactionPath(), txn)
			}
			if boundary == "after_event_append" || boundary == "after_snapshot_replace" || boundary == "before_transaction_delete" {
				appendJSONLine(t, store.EventsPath(), event)
			}
			if boundary == "after_snapshot_replace" || boundary == "before_transaction_delete" {
				writeJSON(t, store.StatePath(), next)
			}
			loaded, err := store.Load()
			if err != nil {
				t.Fatal(err)
			}
			wantRevision := base.StateRevision
			if boundary != "before_transaction" {
				wantRevision = next.StateRevision
			}
			if loaded.StateRevision != wantRevision {
				t.Fatalf("boundary=%s snapshot=%+v", boundary, loaded)
			}
			item := loaded.Issues["1"]
			if item == nil || item.Status != issuedomain.StatusRunning || item.Generation != owner.Generation ||
				item.WorkerPID != 4242 || item.WorkerPGID != 4242 || !state.OwnsActiveExecution(&loaded, 1, owner) {
				t.Fatalf("boundary=%s lost worker or execution ownership: issue=%+v owner=%+v", boundary, item, owner)
			}
			activeWorkers := 0
			for _, issue := range loaded.Issues {
				if issue != nil && issue.Status.RequiresActiveExecution() && loaded.ActiveExecution != nil && loaded.ActiveExecution.IssueNumber == issue.Number {
					activeWorkers++
				}
			}
			if activeWorkers != 1 {
				t.Fatalf("boundary=%s active workers=%d want=1", boundary, activeWorkers)
			}
			events := assertContiguousEvents(t, store.EventsPath(), wantRevision)
			workerStarts, publications := 0, 0
			for _, storedEvent := range events {
				switch storedEvent.Type {
				case "worker_process_started":
					workerStarts++
				case "publication_committed":
					publications++
				}
			}
			wantPublications := 1
			if boundary == "before_transaction" {
				wantPublications = 0
			}
			if workerStarts != 1 || publications != wantPublications {
				t.Fatalf("boundary=%s worker starts=%d publications=%d want=1/%d", boundary, workerStarts, publications, wantPublications)
			}
			if _, err := os.Stat(store.TransactionPath()); !os.IsNotExist(err) {
				t.Fatalf("prepared transaction remains after recovery: %v", err)
			}
		})
	}
}

func writeJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

func appendJSONLine(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(append(data, '\n')); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func assertContiguousEvents(t *testing.T, path string, revision uint64) []state.Event {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != int(revision) {
		t.Fatalf("event count=%d revision=%d", len(lines), revision)
	}
	events := make([]state.Event, 0, len(lines))
	seenIDs := make(map[string]bool, len(lines))
	for index, line := range lines {
		var event state.Event
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatal(err)
		}
		if event.Sequence != uint64(index+1) {
			t.Fatalf("event sequence gap: got=%d want=%d", event.Sequence, index+1)
		}
		if event.EventID == "" || seenIDs[event.EventID] {
			t.Fatalf("missing or duplicate event ID at sequence %d: %q", event.Sequence, event.EventID)
		}
		seenIDs[event.EventID] = true
		events = append(events, event)
	}
	return events
}
