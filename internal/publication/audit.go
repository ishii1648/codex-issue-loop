package publication

import "fmt"

const (
	ReasonResourceClaimMismatch = "resource_claim_mismatch"
	ReasonFormatterFailed       = "formatter_failed"
	ReasonPullRequestMismatch   = "pull_request_mismatch"
)

type FormatterAudit struct {
	Name        string `json:"name"`
	Enabled     bool   `json:"enabled"`
	FileCount   int    `json:"file_count"`
	Changed     bool   `json:"changed"`
	Result      string `json:"result"`
	FailureCode string `json:"failure_code,omitempty"`
}

// Audit is persisted before publication so restart and status output retain
// the exact resource decision even when publication is refused.
type Audit struct {
	BaseSHA           string         `json:"base_sha"`
	ChangedPaths      []string       `json:"changed_paths"`
	DeclaredResources []string       `json:"declared_resources"`
	ActualResources   []string       `json:"actual_resources"`
	Formatter         FormatterAudit `json:"formatter"`
	Reason            string         `json:"reason,omitempty"`
}

type ClaimMismatchError struct {
	Declared []string
	Actual   []string
}

func (e ClaimMismatchError) Error() string {
	return fmt.Sprintf("%s: actual resources %v are not covered by declared resources %v", ReasonResourceClaimMismatch, e.Actual, e.Declared)
}

// FormatterError is safe to persist. Detail contains only a bounded, redacted
// process error and never source contents or repository-provided commands.
type FormatterError struct {
	Code   string
	Detail string
}

func (e FormatterError) Error() string {
	if e.Detail == "" {
		return fmt.Sprintf("%s: %s", ReasonFormatterFailed, e.Code)
	}
	return fmt.Sprintf("%s: %s: %s", ReasonFormatterFailed, e.Code, e.Detail)
}

type PullRequestMismatchError struct{ Detail string }

func (e PullRequestMismatchError) Error() string {
	return fmt.Sprintf("%s: %s", ReasonPullRequestMismatch, e.Detail)
}
