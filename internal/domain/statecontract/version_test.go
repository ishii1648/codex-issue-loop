package statecontract

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestSnapshotVersionIsTheOnlyRuntimeCompatibilityIdentifier(t *testing.T) {
	snapshot := validSnapshotForInvariantTest()
	data, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "semantic_contract_version") || strings.Contains(string(data), "issue_lifecycle_api_version") {
		t.Fatalf("legacy identifiers persisted: %s", data)
	}
	for _, version := range []int{0, 4, 5, 6, 7} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			candidate := snapshot
			candidate.Version = version
			err := candidate.Validate()
			if (err == nil) != (version == CurrentVersion) {
				t.Fatalf("version %d: %v", version, err)
			}
		})
	}
	for _, field := range []string{`"semantic_contract_version":0`, `"semantic_contract_version":4`, `"issue_lifecycle_api_version":""`, `"issue_lifecycle_api_version":"2.1"`} {
		var decoded Snapshot
		if err := json.Unmarshal([]byte(`{"version":6,`+field+`}`), &decoded); err == nil {
			t.Fatalf("mixed envelope accepted: %s", field)
		}
	}
}

func TestLegacySourceRequiresTheExactThreePartEnvelope(t *testing.T) {
	for _, tc := range []struct {
		version, semantic int
		lifecycle         string
		want              bool
	}{
		{5, 4, "2.0", true}, {5, 4, "2.1", true}, {5, 3, "2.1", false}, {5, 4, "2.2", false}, {4, 4, "2.1", false}, {6, 4, "2.1", false}, {5, 4, "", false},
	} {
		snapshot := Snapshot{Version: tc.version, SemanticContractVersion: tc.semantic, IssueLifecycleAPIVersion: tc.lifecycle}
		if snapshot.LegacySource() != tc.want {
			t.Fatalf("source classification: %+v", tc)
		}
		if snapshot.ValidateVersion() == nil {
			t.Fatalf("legacy source accepted by runtime: %+v", tc)
		}
	}
}

func TestTransactionValidatesItsSnapshotAndRevisionThroughContract(t *testing.T) {
	snapshot := validSnapshotForInvariantTest()
	snapshot.StateRevision = 1
	txn := Transaction{Version: CurrentVersion, Snapshot: snapshot, Event: Event{Version: CurrentVersion, RepoID: snapshot.RepoID, Sequence: 1}}
	if err := txn.Validate(snapshot.RepoID); err != nil {
		t.Fatal(err)
	}
	txn.Snapshot.Issues["1"].Attempts = -1
	if err := txn.Validate(snapshot.RepoID); err == nil {
		t.Fatal("invalid snapshot accepted by transaction")
	}
	txn.Snapshot.Issues["1"].Attempts = 0
	txn.Event.Sequence = 2
	if err := txn.Validate(snapshot.RepoID); err == nil {
		t.Fatal("revision mismatch accepted")
	}
	for _, raw := range []string{`{"version":5,"snapshot":{}}`, `{"version":6,"snapshot":{"version":5,"semantic_contract_version":"uninterpreted"}}`} {
		if err := json.Unmarshal([]byte(raw), &txn); err == nil {
			t.Fatalf("legacy transaction accepted: %s", raw)
		}
	}
}
