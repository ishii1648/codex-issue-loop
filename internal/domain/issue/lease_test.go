package issue

import "testing"

func TestDecideLease(t *testing.T) {
	tests := []struct {
		name                        string
		status                      Status
		pullRequest, workerBoundary bool
		want                        LeaseDisposition
	}{
		{"completed", StatusCompleted, true, true, ReleaseLease},
		{"failed without pull request", StatusFailed, false, false, ReleaseLease},
		{"blocked worker boundary", StatusBlocked, false, true, ReleaseLease},
		{"failed pull request", StatusFailed, true, false, ReleaseLease},
		{"running", StatusRunning, false, false, RetainLease},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DecideLease(tt.status, tt.pullRequest, tt.workerBoundary); got != tt.want {
				t.Fatalf("DecideLease()=%v want %v", got, tt.want)
			}
		})
	}
}
