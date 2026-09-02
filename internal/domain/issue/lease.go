package issue

type LeaseDisposition uint8

const (
	RetainLease LeaseDisposition = iota
	ReleaseLease
)

// Terminal lifecycle state never owns execution capacity. Continuation and
// publication authority must be persisted independently from the active lease.
func DecideLease(status Status, pullRequestRecorded, preservesWorkerBoundary bool) LeaseDisposition {
	if status.Terminal() {
		return ReleaseLease
	}
	return RetainLease
}
