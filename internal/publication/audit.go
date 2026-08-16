package publication

import "fmt"

const ReasonResourceClaimMismatch = "resource_claim_mismatch"

// Audit is persisted before publication so restart and status output retain
// the exact resource decision even when publication is refused.
type Audit struct {
	BaseSHA           string   `json:"base_sha"`
	ChangedPaths      []string `json:"changed_paths"`
	DeclaredResources []string `json:"declared_resources"`
	ActualResources   []string `json:"actual_resources"`
	Reason            string   `json:"reason,omitempty"`
}

type ClaimMismatchError struct {
	Declared []string
	Actual   []string
}

func (e ClaimMismatchError) Error() string {
	return fmt.Sprintf("%s: actual resources %v are not covered by declared resources %v", ReasonResourceClaimMismatch, e.Actual, e.Declared)
}
