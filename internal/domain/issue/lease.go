package issue

type LeaseDisposition uint8

const (
	RetainLease LeaseDisposition = iota
	ReleaseLease
)

// A merge/completion is authoritative. Failed and blocked records release only
// when no Pull Request can still own the branch and no worker boundary must be
// preserved for an operator continuation.
func DecideLease(status Status, pullRequestRecorded, preservesWorkerBoundary bool) LeaseDisposition {
	if status == StatusCompleted {
		return ReleaseLease
	}
	if (status == StatusFailed || status == StatusBlocked) && !pullRequestRecorded && !preservesWorkerBoundary {
		return ReleaseLease
	}
	return RetainLease
}
