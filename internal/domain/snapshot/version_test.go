package snapshot

import (
	"encoding/json"
	"fmt"
	"testing"

	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
)

func TestVersionIdentifiesTheEntireSnapshotContract(t *testing.T) {
	for _, tc := range []struct {
		version, semantic int
		lifecycle         string
		current, legacy   bool
	}{
		{6, 0, "", true, false},
		{5, 4, "2.0", false, true},
		{5, 4, "2.1", false, true},
		{5, 3, "2.1", false, false},
		{5, 4, "3.0", false, false},
		{6, 4, "2.1", false, false},
		{7, 0, "", false, false},
		{0, 0, "", false, false},
	} {
		t.Run(fmt.Sprintf("%d/%d/%s", tc.version, tc.semantic, tc.lifecycle), func(t *testing.T) {
			snapshot := validSnapshotForInvariantTest()
			snapshot.Version, snapshot.SemanticContractVersion, snapshot.IssueLifecycleAPIVersion = tc.version, tc.semantic, tc.lifecycle
			if err := snapshot.Validate(); (err == nil) != tc.current {
				t.Fatalf("current reader: %v", err)
			}
			if err := snapshot.ValidateLegacy(); (err == nil) != tc.legacy {
				t.Fatalf("legacy migration contract: %v", err)
			}
		})
	}
}

func TestV6JSONContainsOnlyOneCompatibilityVersion(t *testing.T) {
	data, err := json.Marshal(validSnapshotForInvariantTest())
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"semantic_contract_version", "issue_lifecycle_api_version"} {
		if _, exists := fields[field]; exists {
			t.Fatalf("serialized legacy field %s", field)
		}
		for _, value := range []string{"0", `""`, "null"} {
			fields[field] = json.RawMessage(value)
			contaminated, err := json.Marshal(fields)
			if err != nil {
				t.Fatal(err)
			}
			var snapshot Snapshot
			if err := json.Unmarshal(contaminated, &snapshot); err == nil {
				t.Fatalf("accepted %s=%s", field, value)
			}
		}
		delete(fields, field)
	}
}

func TestVersionPrecedesVersionSpecificStructure(t *testing.T) {
	for _, data := range []string{`{"version":5,"issues":[]}`, `{"version":999,"issues":false}`} {
		if _, ok := CheckVersion([]byte(data)).(SchemaVersionError); !ok {
			t.Fatalf("input was not rejected by version: %s", data)
		}
	}
}

func TestTransitionCommitUsesExistingLifecycleDecision(t *testing.T) {
	item := &Issue{Status: issuedomain.StatusRunning}
	decision, err := issuedomain.Complete(item.Status, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateIssueTransition(item, decision.Transition); err != nil {
		t.Fatal(err)
	}
	item.Status = issuedomain.StatusCanceled
	if err := ValidateIssueTransition(item, decision.Transition); err == nil {
		t.Fatal("stale decision accepted")
	}
	if _, err := issuedomain.Complete(item.Status, ""); err == nil {
		t.Fatal("canceled Issue can complete")
	}
}

func TestPreparedTransactionUsesAggregateAndVersionContract(t *testing.T) {
	snapshot := validSnapshotForInvariantTest()
	snapshot.StateRevision = 1
	txn := Transaction{Version: CurrentVersion, Snapshot: snapshot, Event: Event{Version: CurrentVersion, RepoID: snapshot.RepoID, Sequence: 1}}
	if err := txn.Validate(snapshot.RepoID); err != nil {
		t.Fatal(err)
	}
	txn.Snapshot.PendingRequests["req_bad"] = &Request{ID: "req_bad", IssueNumber: 999}
	if err := txn.Validate(snapshot.RepoID); err == nil {
		t.Fatal("invalid request accepted in transaction")
	}
	delete(txn.Snapshot.PendingRequests, "req_bad")
	txn.Snapshot.Version = 5
	if err := txn.Validate(snapshot.RepoID); err == nil {
		t.Fatal("legacy snapshot accepted in transaction")
	}
}
