package incidentloop

import (
	"strings"
	"testing"
	"time"

	"github.com/ishii1648/codex-issue-loop/internal/application/incidentanalysis"
)

func TestSeedCanaryCreatesTwoIndependentInvariantSignalsAndReusesThem(t *testing.T) {
	store := testStore(t)
	at := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	created, err := SeedCanary(store, "owner/product-canary", "release-v1-2-3", at)
	if err != nil {
		t.Fatal(err)
	}
	reused, err := SeedCanary(store, "owner/product-canary", "release-v1-2-3", at.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if created.Status != "created" || reused.Status != "reused" || created.Fingerprint != reused.Fingerprint {
		t.Fatalf("created=%+v reused=%+v", created, reused)
	}
	signals, err := store.ReadSignals()
	if err != nil {
		t.Fatal(err)
	}
	if len(signals) != 2 || signals[0].RunID == signals[1].RunID {
		t.Fatalf("signals=%+v", signals)
	}
	rules, err := incidentanalysis.LoadRules(rulesPath())
	if err != nil {
		t.Fatal(err)
	}
	state, err := BuildEpisodes(signals, DurableState{}, rules)
	if err != nil {
		t.Fatal(err)
	}
	episode := state.Episodes[created.Fingerprint]
	if episode.PrimaryClassification != "suspected_bug" || !episode.Features.DocumentedInvariantViolation || !episode.Features.RepeatedIndependentRuns || !strings.Contains(strings.Join(episode.InvariantCodes, ","), "live_canary_invariant") {
		t.Fatalf("episode=%+v", episode)
	}
}

func TestSeedCanaryFailsClosedOutsideDedicatedRepository(t *testing.T) {
	_, err := SeedCanary(testStore(t), "owner/production", "release-v1-2-3", time.Now().UTC())
	if err == nil || !strings.Contains(err.Error(), "ends with -canary") {
		t.Fatalf("err=%v", err)
	}
}
