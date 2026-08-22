package state

import (
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ishii1648/codex-issue-loop/internal/capability"
)

const RepositoryResource = "repo:*"

const (
	ResourceParkKindEnvironmentBlock = "environment_block"
	ResourceParkKindNeedsInput       = "needs_input"
)

// LeaseConflictError identifies an admission conflict without weakening the
// validation errors used for malformed park provenance. Callers may durably
// retain an answered claim in a waiting state only for this typed error.
type LeaseConflictError struct {
	IssueNumber int
	Slot        int
	Resource    bool
}

func (e LeaseConflictError) Error() string {
	if e.Resource {
		return fmt.Sprintf("parked resources conflict with active lease for Issue #%d", e.IssueNumber)
	}
	return fmt.Sprintf("worker slot %d is already occupied by Issue #%d", e.Slot, e.IssueNumber)
}

type LeaseReservation struct {
	IssueNumber            int
	Title                  string
	RunID                  string
	Slot                   int
	DeclaredResources      []string
	ResolvedResources      []string
	BaseSHA                string
	ReservedAt             time.Time
	CapabilityRequirements *capability.Requirements
	WorkerCapabilities     *capability.Provider
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
	if reservation.CapabilityRequirements != nil {
		payload["capability_requirements"] = reservation.CapabilityRequirements
	}
	if reservation.WorkerCapabilities != nil {
		payload["worker_capabilities"] = reservation.WorkerCapabilities
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
		issue.CapabilityRequirements = reservation.CapabilityRequirements
		issue.WorkerCapabilities = reservation.WorkerCapabilities
		if reservation.CapabilityRequirements != nil {
			issue.ExecutionProfile = reservation.CapabilityRequirements.Profile
		}
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

// ParkIssueLease removes a typed environment block from active resource
// admission while preserving the complete reservation and owner generation.
// The caller must perform this in the same Store.Update transaction that
// records the block.
func ParkIssueLease(issue *Issue, owner LeaseOwner, parkID string, parkedAt time.Time) error {
	if issue == nil || issue.Lease == nil {
		return fmt.Errorf("lease is no longer active")
	}
	if issue.Lease.Owner != owner || strings.TrimSpace(parkID) == "" || parkedAt.IsZero() {
		return fmt.Errorf("invalid parked lease boundary for Issue #%d", issue.Number)
	}
	copy := *issue.Lease
	copy.DeclaredResources = append([]string{}, issue.Lease.DeclaredResources...)
	copy.ResolvedResources = append([]string(nil), issue.Lease.ResolvedResources...)
	copy.ActualResources = append([]string(nil), issue.Lease.ActualResources...)
	issue.ResourcePark = &ResourceLeasePark{
		ID: parkID, Status: "parked", OriginalLease: copy, ParkedAt: parkedAt.UTC(),
	}
	issue.Lease = nil
	return nil
}

// ResumeParkedLease atomically rechecks conflicts and reacquires a parked
// reservation with a new fencing generation. It never steals another Issue's
// lease and is idempotent for an already resumed park.
func ResumeParkedLease(snapshot *Snapshot, issueNumber int, parkID string, slot int, resumedAt time.Time) (LeaseOwner, error) {
	issue := snapshot.Issues[strconv.Itoa(issueNumber)]
	if issue == nil || issue.ResourcePark == nil || issue.ResourcePark.ID != parkID {
		return LeaseOwner{}, fmt.Errorf("Issue #%d has no matching parked resource claim", issueNumber)
	}
	park := issue.ResourcePark
	if issue.RunID == "" || park.OriginalLease.Owner.RunID != issue.RunID ||
		park.OriginalLease.Owner.Generation == 0 || park.OriginalLease.Owner.Generation > issue.LeaseGeneration {
		return LeaseOwner{}, fmt.Errorf("Issue #%d parked resource provenance is inconsistent", issueNumber)
	}
	if issue.Lease != nil {
		if park.ResumeOwner != nil && issue.Lease.Owner == *park.ResumeOwner {
			return issue.Lease.Owner, nil
		}
		return LeaseOwner{}, fmt.Errorf("Issue #%d already has an unrelated active lease", issueNumber)
	}
	if park.Status != "parked" || park.ResumeOwner != nil || !park.ResumedAt.IsZero() {
		return LeaseOwner{}, fmt.Errorf("Issue #%d resource claim is not parked", issueNumber)
	}
	if park.OriginalLease.Owner.Generation != issue.LeaseGeneration {
		return LeaseOwner{}, fmt.Errorf("Issue #%d parked resource generation changed", issueNumber)
	}
	if slot < 0 || resumedAt.IsZero() {
		return LeaseOwner{}, fmt.Errorf("invalid resource resume boundary for Issue #%d", issueNumber)
	}
	claim := park.OriginalLease
	for _, other := range snapshot.Issues {
		if other == nil || other.Number == issueNumber || other.Lease == nil {
			continue
		}
		if issueOccupiesWorkerSlot(other) && other.Lease.Slot == slot {
			return LeaseOwner{}, LeaseConflictError{IssueNumber: other.Number, Slot: slot}
		}
		if resourcesConflict(claim.ResolvedResources, other.Lease.ResolvedResources) {
			return LeaseOwner{}, LeaseConflictError{IssueNumber: other.Number, Resource: true}
		}
	}
	issue.LeaseGeneration++
	owner := LeaseOwner{RunID: issue.RunID, Generation: issue.LeaseGeneration}
	claim.Owner = owner
	claim.Slot = slot
	claim.ReservedAt = resumedAt.UTC()
	issue.Lease = &claim
	park.Status = "resuming"
	park.ResumedAt = resumedAt.UTC()
	park.ResumeOwner = &owner
	return owner, nil
}

// ValidateNeedsInputPark binds an answerable request to the exact park and
// released lease owner captured when the worker stopped. Active claims remain
// bound to the current run. A resumed park is historical provenance, so its
// source run is instead recovered from the immutable request/original/resume
// owner chain while later run changes are authorized by fencing generations.
func ValidateNeedsInputPark(issue *Issue, request *Request) error {
	if issue == nil || request == nil || request.IssueNumber != issue.Number {
		return fmt.Errorf("needs-input request does not match its Issue")
	}
	park := issue.ResourcePark
	if park == nil || park.Kind != ResourceParkKindNeedsInput || park.RequestID != request.ID ||
		request.ResourceParkID != park.ID || request.ReleasedOwner == nil ||
		*request.ReleasedOwner != park.OriginalLease.Owner {
		return fmt.Errorf("Issue #%d needs-input request provenance is inconsistent", issue.Number)
	}
	originalOwner := park.OriginalLease.Owner
	if request.RunID == "" || originalOwner.RunID != request.RunID || originalOwner.Generation == 0 {
		return fmt.Errorf("Issue #%d needs-input resource provenance is inconsistent", issue.Number)
	}
	if park.Status != "resumed" {
		if issue.RunID == "" || request.RunID != issue.RunID {
			return fmt.Errorf("Issue #%d needs-input resource provenance is inconsistent", issue.Number)
		}
		return nil
	}
	if request.Status != "answered" || park.ResumeOwner == nil || park.ResumeOwner.RunID != request.RunID ||
		park.ResumeOwner.Generation <= originalOwner.Generation || park.ResumeOwner.Generation > issue.LeaseGeneration {
		return fmt.Errorf("Issue #%d resumed needs-input provenance is inconsistent", issue.Number)
	}
	if issue.LeaseGeneration == park.ResumeOwner.Generation && issue.RunID != park.ResumeOwner.RunID {
		return fmt.Errorf("Issue #%d resumed needs-input run changed without a fenced lease transfer", issue.Number)
	}
	return nil
}

// ResourcesConflict is used by read-only operator diagnostics. Mutating
// admission paths keep their checks inside the state transaction above.
func ResourcesConflict(left, right []string) bool { return resourcesConflict(left, right) }

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
	case "claiming", "claimed", "running", "resume_pending", "environment_resume_pending", "resolving_conflict":
		return true
	default:
		return false
	}
}

func validateResourceLeases(snapshot Snapshot) error {
	active := []*Issue{}
	for key, issue := range snapshot.Issues {
		if issue != nil {
			if err := issue.Status.Validate(); err != nil {
				return fmt.Errorf("Issue #%d lifecycle: %w", issue.Number, err)
			}
		}
		if issue != nil && issue.ResourcePark != nil {
			park := issue.ResourcePark
			original := &park.OriginalLease
			if issue.Number < 1 || strconv.Itoa(issue.Number) != key || strings.TrimSpace(park.ID) == "" || park.ID != strings.TrimSpace(park.ID) || park.ParkedAt.IsZero() ||
				original.Owner.RunID == "" || original.Owner.Generation == 0 || original.Owner.Generation > issue.LeaseGeneration ||
				original.Slot < 0 || original.ReservedAt.IsZero() {
				return fmt.Errorf("Issue #%d has invalid parked resource claim", issue.Number)
			}
			declared, err := normalizeResources(original.DeclaredResources, true)
			if err != nil || !reflect.DeepEqual(declared, original.DeclaredResources) {
				return fmt.Errorf("Issue #%d parked declared resources are not canonical", issue.Number)
			}
			resolved, err := normalizeResources(original.ResolvedResources, false)
			if err != nil || !reflect.DeepEqual(resolved, original.ResolvedResources) {
				return fmt.Errorf("Issue #%d parked resolved resources are not canonical", issue.Number)
			}
			if len(original.ActualResources) > 0 {
				actual, err := normalizeResources(original.ActualResources, true)
				if err != nil || !reflect.DeepEqual(actual, original.ActualResources) {
					return fmt.Errorf("Issue #%d parked actual resources are not canonical", issue.Number)
				}
			}
			if park.Status != "parked" && park.Status != "resuming" && park.Status != "resumed" {
				return fmt.Errorf("Issue #%d has invalid resource park status %q", issue.Number, park.Status)
			}
			if park.Kind != "" && park.Kind != ResourceParkKindEnvironmentBlock && park.Kind != ResourceParkKindNeedsInput {
				return fmt.Errorf("Issue #%d has unknown resource park kind %q", issue.Number, park.Kind)
			}
			if park.Kind == ResourceParkKindNeedsInput {
				request := snapshot.PendingRequests[park.RequestID]
				if err := ValidateNeedsInputPark(issue, request); err != nil {
					return err
				}
			} else if park.RequestID != "" {
				return fmt.Errorf("Issue #%d non-input resource park has a request ID", issue.Number)
			}
			if park.Status == "parked" && (original.Owner.RunID != issue.RunID || issue.LeaseGeneration != original.Owner.Generation || issue.Lease != nil || park.ResumeOwner != nil || !park.ResumedAt.IsZero()) {
				return fmt.Errorf("Issue #%d parked resource claim is still active", issue.Number)
			}
			if park.ResumeOwner != nil && (park.ResumeOwner.RunID != original.Owner.RunID || park.ResumeOwner.Generation <= original.Owner.Generation || park.ResumeOwner.Generation > issue.LeaseGeneration) {
				return fmt.Errorf("Issue #%d resource park resume owner is invalid", issue.Number)
			}
			if park.Status == "resuming" && (original.Owner.RunID != issue.RunID || issue.Lease == nil || park.ResumeOwner == nil || park.ResumeOwner.RunID != issue.RunID || issue.Lease.Owner != *park.ResumeOwner || issue.LeaseGeneration != park.ResumeOwner.Generation || park.ResumedAt.IsZero()) {
				return fmt.Errorf("Issue #%d resumed resource claim is inconsistent", issue.Number)
			}
			if park.Status == "resumed" && (park.ResumeOwner == nil || park.ResumedAt.IsZero()) {
				return fmt.Errorf("Issue #%d completed resource park lacks resume provenance", issue.Number)
			}
			if park.Status == "resumed" && issue.LeaseGeneration == park.ResumeOwner.Generation && issue.RunID != park.ResumeOwner.RunID {
				return fmt.Errorf("Issue #%d completed resource park changed run without a fenced lease transfer", issue.Number)
			}
			// A completed park is historical provenance. A fresh retry transfers
			// the active lease to a new run and advances its fencing generation;
			// retaining the earlier ResumeOwner must not turn that valid transfer
			// back into an active parked claim. At the resume generation, however,
			// the owner still has to match exactly so ambiguous state fails closed.
			if park.Status == "resumed" && issue.Lease != nil &&
				(issue.Lease.Owner.Generation < park.ResumeOwner.Generation ||
					(issue.Lease.Owner.Generation == park.ResumeOwner.Generation && issue.Lease.Owner != *park.ResumeOwner)) {
				return fmt.Errorf("Issue #%d completed resource park has an unrelated active lease", issue.Number)
			}
		}
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
