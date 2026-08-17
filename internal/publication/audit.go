package publication

import (
	"errors"
	"fmt"
	"time"
)

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

const (
	FailureOriginPublisher        = "publisher"
	FailurePhasePublication       = "publication"
	FailurePhasePrePublication    = "pre_publication"
	FailureCodeDurableBaseMissing = "durable_base_sha_missing"
)

// DurableBaseMissingError identifies the only publisher failure that can be
// recovered by backfilling a verified base before publication mutates Git.
type DurableBaseMissingError struct{}

func (DurableBaseMissingError) Error() string {
	return "inspect publish changes: durable base SHA is missing"
}

// FailureProvenance is the durable, typed boundary used by the operator-only
// publication recovery command. Unknown publisher errors are recorded but are
// deliberately not recoverable.
type FailureProvenance struct {
	Origin      string    `json:"origin"`
	Phase       string    `json:"phase"`
	Code        string    `json:"code"`
	Recoverable bool      `json:"recoverable"`
	Reason      string    `json:"reason"`
	FailedAt    time.Time `json:"failed_at"`

	DeclaredResources []string `json:"declared_resources,omitempty"`
	ResolvedResources []string `json:"resolved_resources,omitempty"`
	ActualResources   []string `json:"actual_resources,omitempty"`
}

func ClassifyFailure(err error, now time.Time) FailureProvenance {
	result := FailureProvenance{
		Origin: FailureOriginPublisher, Phase: FailurePhasePublication,
		Code: "unknown", Reason: err.Error(), FailedAt: now.UTC(),
	}
	var missing DurableBaseMissingError
	if errors.As(err, &missing) {
		result.Phase = FailurePhasePrePublication
		result.Code = FailureCodeDurableBaseMissing
		result.Recoverable = true
		return result
	}
	var formatter FormatterError
	if errors.As(err, &formatter) {
		result.Code = ReasonFormatterFailed
	}
	var mismatch PullRequestMismatchError
	if errors.As(err, &mismatch) {
		result.Code = ReasonPullRequestMismatch
	}
	return result
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
