package incidentloop

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

type CanarySeedReport struct {
	Version     int      `json:"version"`
	Repository  string   `json:"repository"`
	CanaryID    string   `json:"canary_id"`
	Fingerprint string   `json:"fingerprint"`
	SignalIDs   []string `json:"signal_ids"`
	Status      string   `json:"status"`
}

func SeedCanary(store Store, repository, canaryID string, at time.Time) (CanarySeedReport, error) {
	if !repositoryPattern.MatchString(repository) || !strings.HasSuffix(repository, "-canary") {
		return CanarySeedReport{}, errors.New("incident canary requires a repository whose name ends with -canary")
	}
	if !identifierPattern.MatchString(canaryID) || len(canaryID) > 80 {
		return CanarySeedReport{}, errors.New("incident canary ID must be a bounded identifier of at most 80 characters")
	}
	if at.IsZero() {
		return CanarySeedReport{}, errors.New("incident canary timestamp is required")
	}
	release, err := store.TryProcessLock()
	if err != nil {
		return CanarySeedReport{}, err
	}
	defer release()
	episodeID := "incident-canary-" + canaryID
	signalIDs := []string{"canary-" + canaryID + "-run-1", "canary-" + canaryID + "-run-2"}
	fingerprint := digest("episode", repository, episodeID)
	report := CanarySeedReport{Version: SchemaVersion, Repository: repository, CanaryID: canaryID, Fingerprint: fingerprint, SignalIDs: signalIDs, Status: "created"}

	existing, err := store.ReadSignals()
	if err != nil {
		return CanarySeedReport{}, err
	}
	found := map[string]bool{}
	for _, signal := range existing {
		for _, id := range signalIDs {
			if signal.ID == id {
				found[id] = true
			}
		}
	}
	if len(found) == len(signalIDs) {
		report.Status = "reused"
		return report, nil
	}
	if len(found) != 0 {
		return CanarySeedReport{}, fmt.Errorf("incident canary %q has a partial signal set", canaryID)
	}

	signals := make([]Signal, 0, len(signalIDs))
	for index, id := range signalIDs {
		signals = append(signals, Signal{
			Version: SchemaVersion, ID: id, Timestamp: at.UTC().Add(time.Duration(index) * time.Second),
			Repository: repository, CorrelationID: "canary-" + canaryID, RunID: fmt.Sprintf("canary-run-%d-%s", index+1, canaryID), EpisodeID: episodeID,
			Kind: "event", Name: "failure_classified", Component: "scheduler", Phase: "poll", OutcomeCode: "failed", ReasonCode: "live_canary_invariant",
			FailureKind: "product", FailureCode: "live_canary_invariant", InvariantViolation: true,
			Evidence: []EvidenceRef{{Source: "incident-canary", Ref: canaryID}},
		})
	}
	if written, err := store.RecordBatch(signals); err != nil {
		return CanarySeedReport{}, err
	} else if written != len(signals) {
		return CanarySeedReport{}, fmt.Errorf("incident canary wrote %d of %d signals", written, len(signals))
	}
	return report, nil
}
