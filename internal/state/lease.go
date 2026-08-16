package state

import (
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"
)

const RepositoryResource = "repo:*"

type LeaseReservation struct {
	IssueNumber       int
	Title             string
	RunID             string
	Slot              int
	DeclaredResources []string
	ResolvedResources []string
	BaseSHA           string
	ReservedAt        time.Time
}

// ReserveLease atomically persists the claiming Issue and its slot/resource
// reservation. Repeating the same run is idempotent and returns its owner.
func (s Store) ReserveLease(reservation LeaseReservation) (Snapshot, LeaseOwner, error) {
	declared, err := normalizeResources(reservation.DeclaredResources, true)
	if err != nil {
		return Snapshot{}, LeaseOwner{}, fmt.Errorf("declared resources: %w", err)
	}
	resolved, err := normalizeResources(reservation.ResolvedResources, false)
	if err != nil {
		return Snapshot{}, LeaseOwner{}, fmt.Errorf("resolved resources: %w", err)
	}
	if reservation.IssueNumber < 1 || strings.TrimSpace(reservation.RunID) == "" {
		return Snapshot{}, LeaseOwner{}, fmt.Errorf("lease requires a positive Issue number and non-empty run ID")
	}
	if reservation.Slot < 0 || len(resolved) == 0 {
		return Snapshot{}, LeaseOwner{}, fmt.Errorf("lease requires a non-negative slot and at least one resolved resource")
	}
	if reservation.ReservedAt.IsZero() {
		reservation.ReservedAt = time.Now().UTC()
	} else {
		reservation.ReservedAt = reservation.ReservedAt.UTC()
	}
	var owner LeaseOwner
	payload := map[string]any{
		"slot": reservation.Slot, "declared_resources": declared, "resolved_resources": resolved,
		"base_sha": reservation.BaseSHA, "reserved_at": reservation.ReservedAt,
	}
	snapshot, err := s.Update("lease_reserved", reservation.IssueNumber, reservation.RunID, payload, func(snapshot *Snapshot) error {
		key := strconv.Itoa(reservation.IssueNumber)
		issue := snapshot.Issues[key]
		if issue == nil {
			issue = &Issue{Number: reservation.IssueNumber}
			snapshot.Issues[key] = issue
		}
		if issue.Lease != nil {
			if issue.Lease.Owner.RunID != reservation.RunID {
				return fmt.Errorf("Issue #%d lease is owned by run %s generation %d", issue.Number, issue.Lease.Owner.RunID, issue.Lease.Owner.Generation)
			}
			owner = issue.Lease.Owner
			payload["owner"] = owner
			return nil
		}
		for _, other := range snapshot.Issues {
			if other == nil || other.Number == issue.Number || other.Lease == nil {
				continue
			}
			if issueOccupiesWorkerSlot(other) && other.Lease.Slot == reservation.Slot {
				return fmt.Errorf("worker slot %d is already leased by Issue #%d", reservation.Slot, other.Number)
			}
			if resourcesConflict(resolved, other.Lease.ResolvedResources) {
				return fmt.Errorf("resources conflict with active lease for Issue #%d", other.Number)
			}
		}
		issue.LeaseGeneration++
		owner = LeaseOwner{RunID: reservation.RunID, Generation: issue.LeaseGeneration}
		payload["owner"] = owner
		issue.Title, issue.Status, issue.RunID = reservation.Title, "claiming", reservation.RunID
		issue.DeclaredResources = append([]string(nil), declared...)
		issue.ActualResources = nil
		if issue.Attempts == 0 {
			issue.Attempts = 1
		}
		issue.Lease = &ResourceLease{
			Owner: owner, Slot: reservation.Slot, DeclaredResources: declared,
			ResolvedResources: resolved, BaseSHA: reservation.BaseSHA, ReservedAt: reservation.ReservedAt,
		}
		issue.UpdatedAt = reservation.ReservedAt
		snapshot.Supervisor.State = "running"
		return nil
	})
	return snapshot, owner, err
}

// AssignLeaseSlot records the scheduler slot selected for the next worker
// invocation. Retained leases in retry/attention/PR states protect resources,
// but do not occupy a bounded worker slot.
func (s Store) AssignLeaseSlot(issueNumber int, owner LeaseOwner, slot int) (Snapshot, error) {
	if slot < 0 {
		return Snapshot{}, fmt.Errorf("worker slot must not be negative")
	}
	return s.Update("lease_slot_assigned", issueNumber, owner.RunID, map[string]any{"owner": owner, "slot": slot}, func(snapshot *Snapshot) error {
		issue, err := ownedIssue(snapshot, issueNumber, owner)
		if err != nil {
			return err
		}
		for _, other := range snapshot.Issues {
			if other == nil || other.Number == issueNumber || other.Lease == nil || !issueOccupiesWorkerSlot(other) {
				continue
			}
			if other.Lease.Slot == slot {
				return fmt.Errorf("worker slot %d is already occupied by Issue #%d", slot, other.Number)
			}
		}
		issue.Lease.Slot = slot
		return nil
	})
}

// ExpandLease adds resources without changing the owner generation. The
// expansion and its event are committed by the normal state transaction.
func (s Store) ExpandLease(issueNumber int, owner LeaseOwner, resources []string) (Snapshot, error) {
	additional, err := normalizeResources(resources, false)
	if err != nil {
		return Snapshot{}, err
	}
	return s.Update("lease_expanded", issueNumber, owner.RunID, map[string]any{"owner": owner, "resources": additional}, func(snapshot *Snapshot) error {
		issue, err := ownedIssue(snapshot, issueNumber, owner)
		if err != nil {
			return err
		}
		expanded, _ := normalizeResources(append(append([]string(nil), issue.Lease.ResolvedResources...), additional...), false)
		for _, other := range snapshot.Issues {
			if other == nil || other.Number == issueNumber || other.Lease == nil {
				continue
			}
			if resourcesConflict(expanded, other.Lease.ResolvedResources) {
				return fmt.Errorf("resources conflict with active lease for Issue #%d", other.Number)
			}
		}
		issue.Lease.ResolvedResources = expanded
		return nil
	})
}

func (s Store) ReleaseLease(issueNumber int, owner LeaseOwner, reason string) (Snapshot, error) {
	return s.Update("lease_released", issueNumber, owner.RunID, map[string]any{"owner": owner, "reason": reason}, func(snapshot *Snapshot) error {
		issue, err := ownedIssue(snapshot, issueNumber, owner)
		if err != nil {
			return err
		}
		issue.Lease = nil
		return nil
	})
}

// ReleaseIssueLease is used by lifecycle transitions that must release the
// lease in the same snapshot/event transaction as their terminal status.
func ReleaseIssueLease(issue *Issue, owner LeaseOwner) error {
	if issue == nil || issue.Lease == nil {
		if owner == (LeaseOwner{}) {
			return nil
		}
		return fmt.Errorf("lease is no longer active")
	}
	if issue.Lease.Owner != owner {
		return fmt.Errorf("stale lease owner for Issue #%d: got run %s generation %d", issue.Number, owner.RunID, owner.Generation)
	}
	issue.Lease = nil
	return nil
}

// TransferIssueLease fences a retained lease to a new worker run without
// releasing its slot or resources between attempts.
func TransferIssueLease(issue *Issue, owner LeaseOwner, newRunID string) (LeaseOwner, error) {
	if issue == nil || issue.Lease == nil {
		return LeaseOwner{}, nil // pre-v3 in-memory fixtures remain readable
	}
	if issue.Lease.Owner != owner || strings.TrimSpace(newRunID) == "" {
		return LeaseOwner{}, fmt.Errorf("stale lease owner for Issue #%d", issue.Number)
	}
	issue.LeaseGeneration++
	next := LeaseOwner{RunID: newRunID, Generation: issue.LeaseGeneration}
	issue.Lease.Owner = next
	return next, nil
}

func ownedIssue(snapshot *Snapshot, issueNumber int, owner LeaseOwner) (*Issue, error) {
	issue := snapshot.Issues[strconv.Itoa(issueNumber)]
	if issue == nil || issue.Lease == nil {
		return nil, fmt.Errorf("Issue #%d has no active lease", issueNumber)
	}
	if issue.Lease.Owner != owner {
		return nil, fmt.Errorf("stale lease owner for Issue #%d: got run %s generation %d", issueNumber, owner.RunID, owner.Generation)
	}
	return issue, nil
}

func normalizeResources(resources []string, allowEmpty bool) ([]string, error) {
	set := map[string]bool{}
	for _, resource := range resources {
		resource = strings.TrimSpace(resource)
		if resource == "" {
			return nil, fmt.Errorf("resource names must not be empty")
		}
		if resource != RepositoryResource {
			for index, char := range resource {
				if !((char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') || (char == '-' && index > 0)) {
					return nil, fmt.Errorf("invalid resource name %q", resource)
				}
			}
			if len(resource) > 63 || strings.HasSuffix(resource, "-") {
				return nil, fmt.Errorf("invalid resource name %q", resource)
			}
		}
		set[resource] = true
	}
	result := make([]string, 0, len(set))
	for resource := range set {
		result = append(result, resource)
	}
	sort.Strings(result)
	if len(result) == 0 && !allowEmpty {
		return nil, fmt.Errorf("resource set must not be empty")
	}
	return result, nil
}

func resourcesConflict(left, right []string) bool {
	set := map[string]bool{}
	for _, resource := range left {
		if resource == RepositoryResource {
			return true
		}
		set[resource] = true
	}
	for _, resource := range right {
		if resource == RepositoryResource || set[resource] {
			return true
		}
	}
	return false
}

func issueOccupiesWorkerSlot(issue *Issue) bool {
	if issue == nil {
		return false
	}
	switch issue.Status {
	case "claiming", "claimed", "running", "resolving_conflict":
		return true
	default:
		return false
	}
}

func validateResourceLeases(snapshot Snapshot) error {
	active := []*Issue{}
	for key, issue := range snapshot.Issues {
		if issue == nil || issue.Lease == nil {
			continue
		}
		lease := issue.Lease
		if issue.Number < 1 || strconv.Itoa(issue.Number) != key {
			return fmt.Errorf("resource lease has invalid Issue key %q", key)
		}
		if lease.Owner.RunID == "" || lease.Owner.Generation == 0 || lease.Owner.Generation != issue.LeaseGeneration || lease.Owner.RunID != issue.RunID {
			return fmt.Errorf("Issue #%d resource lease owner does not match its run and generation", issue.Number)
		}
		if lease.Slot < 0 || lease.ReservedAt.IsZero() {
			return fmt.Errorf("Issue #%d resource lease has invalid slot or reserved timestamp", issue.Number)
		}
		declared, err := normalizeResources(lease.DeclaredResources, true)
		if err != nil || !reflect.DeepEqual(declared, lease.DeclaredResources) {
			return fmt.Errorf("Issue #%d declared resources are not canonical", issue.Number)
		}
		resolved, err := normalizeResources(lease.ResolvedResources, false)
		if err != nil || !reflect.DeepEqual(resolved, lease.ResolvedResources) {
			return fmt.Errorf("Issue #%d resolved resources are not canonical", issue.Number)
		}
		if len(lease.ActualResources) > 0 {
			actual, err := normalizeResources(lease.ActualResources, true)
			if err != nil || !reflect.DeepEqual(actual, lease.ActualResources) {
				return fmt.Errorf("Issue #%d actual resources are not canonical", issue.Number)
			}
		}
		if len(issue.DeclaredResources) > 0 {
			persistedDeclared, err := normalizeResources(issue.DeclaredResources, true)
			if err != nil || !reflect.DeepEqual(persistedDeclared, issue.DeclaredResources) {
				return fmt.Errorf("Issue #%d persisted declared resources are not canonical", issue.Number)
			}
		}
		if len(issue.ActualResources) > 0 {
			persistedActual, err := normalizeResources(issue.ActualResources, true)
			if err != nil || !reflect.DeepEqual(persistedActual, issue.ActualResources) {
				return fmt.Errorf("Issue #%d persisted actual resources are not canonical", issue.Number)
			}
		}
		for _, other := range active {
			if issueOccupiesWorkerSlot(issue) && issueOccupiesWorkerSlot(other) && lease.Slot == other.Lease.Slot {
				return fmt.Errorf("worker slot %d is leased by Issues #%d and #%d", lease.Slot, other.Number, issue.Number)
			}
			if resourcesConflict(lease.ResolvedResources, other.Lease.ResolvedResources) {
				return fmt.Errorf("resource leases for Issues #%d and #%d conflict", other.Number, issue.Number)
			}
		}
		active = append(active, issue)
	}
	return nil
}
