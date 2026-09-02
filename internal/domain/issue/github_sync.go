package issue

import "fmt"

// GitHubSync is the durable synchronization intent paired with Status.
// A non-empty value means GitHub must converge before lifecycle dispatch.
type GitHubSync string

const (
	GitHubSyncNone            GitHubSync = ""
	GitHubSyncDone            GitHubSync = "done"
	GitHubSyncNeedsInput      GitHubSync = "needs_input"
	GitHubSyncFailed          GitHubSync = "failed"
	GitHubSyncBlocked         GitHubSync = "blocked"
	GitHubSyncConflictRetry   GitHubSync = "conflict_retry"
	GitHubSyncIssueResolution GitHubSync = "issue_resolution"
)

var allGitHubSyncs = [...]GitHubSync{
	GitHubSyncNone, GitHubSyncDone, GitHubSyncNeedsInput, GitHubSyncFailed,
	GitHubSyncBlocked, GitHubSyncConflictRetry, GitHubSyncIssueResolution,
}

func AllGitHubSyncs() []GitHubSync {
	return append([]GitHubSync(nil), allGitHubSyncs[:]...)
}

func (s GitHubSync) Pending() bool { return s != GitHubSyncNone }

func (s GitHubSync) Validate() error {
	for _, candidate := range allGitHubSyncs {
		if s == candidate {
			return nil
		}
	}
	return fmt.Errorf("unknown Issue GitHub synchronization intent %q", s)
}
