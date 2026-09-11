package incidentloop

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/ishii1648/codex-issue-loop/internal/adapter/state"
	"github.com/ishii1648/codex-issue-loop/internal/platform/retention"
)

func TestCollectorDoesNotReplayExpiredSignals(t *testing.T) {
	target := testStore(t)
	target.Retention = retention.Policy{MaxBytes: 1, MaxAge: time.Hour, Keep: 1}
	collector := StateEventCollector{Repository: "owner/repo", Source: state.Store{Dir: t.TempDir(), RepoID: "repoid"}, Target: target}
	now := time.Now().UTC()
	events := []state.Event{
		{Version: 4, EventID: "ready", Sequence: 1, Timestamp: now, RepoID: "repoid", IssueNumber: 42, Type: "pull_request_ready"},
		{Version: 4, EventID: "ignored", Sequence: 2, Timestamp: now.Add(time.Second), RepoID: "repoid", Type: "lease_reserved"},
	}
	writeEvents := func() {
		t.Helper()
		var data bytes.Buffer
		for _, event := range events {
			if err := json.NewEncoder(&data).Encode(event); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(collector.Source.EventsPath(), data.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeEvents()
	if written, err := collector.Collect(); err != nil || written != 3 {
		t.Fatalf("initial collection: written=%d err=%v", written, err)
	}
	for i := 0; i < 3; i++ {
		signal := signalAt(now, fmt.Sprintf("scheduler-%d", i), "cycle", "scheduler_cycle", "started", func(s *Signal) {
			s.Component, s.Phase, s.CycleID, s.Trigger = "scheduler", "poll", "cycle", "poll_timer"
		})
		if err := target.Record(signal); err != nil {
			t.Fatal(err)
		}
	}
	signals, err := target.ReadSignals()
	if err != nil {
		t.Fatal(err)
	}
	for _, signal := range signals {
		if signal.SourceKind == "state_events" || signal.EventSequence != 0 {
			t.Fatalf("collected signal survived rotation: %+v", signal)
		}
	}
	before, err := os.ReadFile(target.SignalsPath())
	if err != nil {
		t.Fatal(err)
	}
	metricsBefore, err := target.LoadMetrics()
	if err != nil {
		t.Fatal(err)
	}
	collector = StateEventCollector{Repository: collector.Repository, Source: collector.Source, Target: target}
	for i := 0; i < 3; i++ {
		if written, err := collector.Collect(); err != nil || written != 0 {
			t.Fatalf("recollection: written=%d err=%v", written, err)
		}
	}
	after, err := os.ReadFile(target.SignalsPath())
	if err != nil {
		t.Fatal(err)
	}
	metricsAfter, err := target.LoadMetrics()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) || !reflect.DeepEqual(metricsBefore, metricsAfter) {
		t.Fatalf("recollection changed signals or metrics: before=%+v after=%+v", metricsBefore, metricsAfter)
	}
	events = append(events, state.Event{Version: 4, EventID: "merged", Sequence: 3, Timestamp: now.Add(2 * time.Second), RepoID: "repoid", IssueNumber: 42, Type: "issue_completed"})
	writeEvents()
	if written, err := collector.Collect(); err != nil || written != 2 {
		t.Fatalf("incremental collection: written=%d err=%v", written, err)
	}
	metricsAfter, err = target.LoadMetrics()
	if err != nil {
		t.Fatal(err)
	}
	if metricsAfter.SignalsByName["lifecycle_outcome"] != 3 || metricsAfter.SignalsByName["evidence_coverage"] != 2 || metricsAfter.Outcomes["succeeded"] != 2 {
		t.Fatalf("unexpected incremental metrics: %+v", metricsAfter)
	}
}

func TestCollectorDoesNotAdvanceCursorOnRecordFailure(t *testing.T) {
	target := testStore(t)
	collector := StateEventCollector{Repository: "owner/repo", Source: state.Store{Dir: t.TempDir(), RepoID: "repoid"}, Target: target}
	event := state.Event{Version: 4, EventID: "started", Sequence: 1, Timestamp: time.Now().UTC(), RepoID: "repoid", IssueNumber: 42, Type: "worker_started"}
	data, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(collector.Source.EventsPath(), append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target.MetricsPath(), []byte("invalid"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := collector.Collect(); err == nil {
		t.Fatal("expected metrics decode failure")
	}
	if _, err := os.Stat(filepath.Join(target.Dir, "state-event-cursor.json")); !os.IsNotExist(err) {
		t.Fatalf("cursor persisted after failed collection: %v", err)
	}
	if err := os.Remove(target.MetricsPath()); err != nil {
		t.Fatal(err)
	}
	if written, err := collector.Collect(); err != nil || written != 2 {
		t.Fatalf("collection after repair: written=%d err=%v", written, err)
	}
}
