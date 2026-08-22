package app

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
	"os"
	"reflect"
	"strconv"
	"strings"

	gh "github.com/ishii1648/codex-issue-loop/internal/adapter/github"
	"github.com/ishii1648/codex-issue-loop/internal/adapter/state"
	"github.com/ishii1648/codex-issue-loop/internal/adapter/worktree"
	"github.com/ishii1648/codex-issue-loop/internal/application/supervisor"
	"github.com/ishii1648/codex-issue-loop/internal/domain/capability"
	"github.com/ishii1648/codex-issue-loop/internal/platform/config"
	"github.com/ishii1648/codex-issue-loop/internal/platform/layout"
)

func (a App) explainRecovery(ctx context.Context, l layout.Layout, args []string) error {
	fs := flag.NewFlagSet("explain-recovery", flag.ContinueOnError)
	fs.SetOutput(a.Err)
	repo := fs.String("repo", "", "repository path")
	issueNumber := fs.Int("issue", 0, "Issue number")
	operation := fs.String("operation", "resume-blocked", "recovery operation: resume-blocked or recover-checks")
	jsonOut := fs.Bool("json", false, "emit versioned JSON")
	if err := fs.Parse(args); err != nil {
		return exitError{2, err}
	}
	if *issueNumber <= 0 {
		return exitError{2, fmt.Errorf("--issue must be a positive Issue number")}
	}
	switch *operation {
	case "resume-blocked":
		return a.explainResumeBlocked(ctx, l, *repo, *issueNumber, *jsonOut)
	case "recover-checks":
		return a.explainRecoverChecks(ctx, l, *repo, *issueNumber, *jsonOut)
	default:
		return exitError{2, fmt.Errorf("unsupported recovery operation %q", *operation)}
	}
}

func (a App) explainRecoverChecks(ctx context.Context, l layout.Layout, repo string, issueNumber int, jsonOut bool) error {
	entry, err := a.resolvePath(l, repo)
	if err != nil {
		return err
	}
	cfg, err := config.Load(entry.RepoPath)
	if err != nil {
		return err
	}
	store := state.Store{Dir: l.RepoDir(entry.RepoID), RepoID: entry.RepoID, RepoPath: entry.RepoPath, Secrets: cfg.RedactionValues()}
	stateBefore, _ := os.ReadFile(store.StatePath())
	eventsBefore, _ := os.ReadFile(store.EventsPath())
	snapshot, _, err := store.ReadRecoveryInputs()
	if err != nil {
		return err
	}
	current := snapshot.Issues[strconv.Itoa(issueNumber)]
	if current == nil {
		return exitError{4, fmt.Errorf("Issue #%d is missing from durable state", issueNumber)}
	}
	report := state.RecoveryPredicateReport{
		SchemaVersion: state.RecoveryPredicateReportSchemaVersion, Operation: "recover-checks",
		IssueNumber: issueNumber, Eligible: true, Predicates: []state.RecoveryPredicate{},
	}
	add := func(code state.RecoveryPredicateCode, ok bool, source, expected, passed, failed, fixability, remediation string) {
		status, actual := "fail", failed
		if ok {
			status, actual = "pass", passed
		}
		report.AddPredicate(code, status, source, expected, actual, fixability, remediation)
	}
	progress := pullRequestChecksRecoveryProgress(current)
	add(state.RecoveryCodeStatus, progress == recoveryProgressFresh || progress == recoveryProgressIdempotent, "durable.state", "fully synchronized failed state or existing recovery", "status boundary matches", "status or synchronization boundary differs", "operator", "wait for failure synchronization before recovery")
	failureRecord := current.PullRequestChecksFailure
	typedFailure := state.RecoverablePullRequestChecksFailure(current)
	legacyCompatibility := false
	if !typedFailure {
		legacyRecord, legacyErr := store.LegacyPullRequestChecksFailure(*current, cfg.GitHub.Repo, cfg.Git.BaseBranch)
		if legacyErr == nil {
			failureRecord = legacyRecord
			legacyCompatibility = true
		}
	}
	add(state.RecoveryCodeChecksFailureProvenance, typedFailure || legacyCompatibility, "durable.state+events", "typed or exact legacy checks retry exhaustion provenance", "failure provenance matches", "failure provenance is missing or inconsistent", "none", "use reviewed legacy recovery only when exact event provenance is available")
	provenanceOK := failureRecord != nil && current.FailureKind == "issue" && !current.PullRequestMerged &&
		current.PullRequestURL == failureRecord.PullRequestURL && current.PullRequestNumber == failureRecord.PullRequestNumber && current.Branch == failureRecord.Branch
	add(state.RecoveryCodePRFailureIdentity, provenanceOK, "durable.state", "saved PR failure matches Issue lifecycle identity", "failure/PR identity matches", "failure/PR identity differs", "none", "restore the matching failure record and durable Issue snapshot")
	runOK := state.ValidID(current.RunID, "run_") && current.Worktree != "" && current.Branch != "" && current.PullRequestURL != ""
	add(state.RecoveryCodeRunWorkspace, runOK, "durable.state", "valid run, worktree, branch, and Pull Request", "run/workspace identity is complete", "run/workspace identity is incomplete", "none", "do not synthesize missing execution identity")
	leaseOK := current.Lease != nil && current.LeaseGeneration > 0 && current.Lease.Owner.RunID == current.RunID &&
		current.Lease.Owner.Generation == current.LeaseGeneration && (legacyCompatibility || len(current.Lease.DeclaredResources) > 0) && len(current.Lease.ResolvedResources) > 0
	add(state.RecoveryCodeLeasePark, leaseOK, "durable.state", "fenced resource lease retained", "lease fencing matches", "resource lease fencing differs", "none", "restore the original fenced lease evidence")
	compatible := (legacyCompatibility || (current.ConflictRecovery == nil && current.BlockedCause == nil)) && current.PublicationRecovery == nil && current.PublicationFailure == nil
	add(state.RecoveryCodeIncompatibleState, compatible, "durable.state", "no manual, worker, security, publication, or conflict recovery", "no incompatible recovery is active", "another recovery boundary is active", "operator", "complete the active recovery workflow first")
	controller := a.ProcessController
	if controller == nil {
		controller = supervisor.OSProcessGroupController{}
	}
	pgid := current.WorkerPGID
	if pgid <= 1 {
		pgid = current.WorkerPID
	}
	add(state.RecoveryCodeWorkerProcess, !controller.Alive(current.WorkerPID) && !controller.GroupAlive(pgid), "durable.state+process table", "no live worker process", "no worker process is alive", "worker process is still alive", "operator", "let the supervisor stop the worker before recovery")
	pending := false
	for _, request := range snapshot.PendingRequests {
		pending = pending || (request != nil && request.IssueNumber == issueNumber && request.Status == issuedomain.RequestStatusPending)
	}
	add(state.RecoveryCodePendingRequest, !pending, "durable.state.pending_requests", "no pending manual answer", "no pending answer request", "a pending answer request exists", "operator", "resolve the pending answer request first")

	manager := worktree.Manager{StateRoot: l.Root, GitPath: entry.Commands["git"]}
	inspection, inspectionErr := manager.Inspect(ctx, cfg, current.Worktree, current.Branch)
	worktreeOK := inspectionErr == nil && pullRequestChecksWorktreeMatches(current, inspection)
	add(state.RecoveryCodeWorktreeRemote, worktreeOK, "read-only git inspection", "clean saved branch aligned with pushed remote HEAD", "worktree and remote branch align", "worktree, branch, cleanliness, or remote HEAD differs", "operator", "push the external fix and leave the saved worktree clean")

	remote, remoteErr := (gh.CLI{Path: entry.Commands["gh"], Secrets: cfg.RedactionValues()}).Inspect(ctx, cfg, issueNumber, current.Branch)
	remoteOK := false
	var pr gh.PullRequest
	if remoteErr == nil && inspectionErr == nil {
		pr, err = validatePullRequestChecksRecovery(cfg, current, remote, inspection, true)
		remoteOK = err == nil
	}
	add(state.RecoveryCodeGitHubIdentity, remoteOK, "GitHub read API+git inspection", "failed labels and exactly one matching open Pull Request", "GitHub and branch identity match", "GitHub labels, Pull Request, branch, or head identity differs", "operator", "restore the synchronized failed Issue and matching saved Pull Request")
	replacementOK := remoteOK && pullRequestReplacementChecksAllowed(failureRecord, pr, legacyCompatibility)
	add(state.RecoveryCodeReplacementChecks, replacementOK, "GitHub Pull Request checks", "changed PR head with known checks status", "replacement head/checks are observable", "head is unchanged or checks status is unknown", "operator", "push a replacement head and wait for a recognized checks state")

	stateAfter, _ := os.ReadFile(store.StatePath())
	eventsAfter, _ := os.ReadFile(store.EventsPath())
	inspectionAfter, inspectionAfterErr := manager.Inspect(ctx, cfg, current.Worktree, current.Branch)
	readOnlyOK := bytes.Equal(stateBefore, stateAfter) && bytes.Equal(eventsBefore, eventsAfter) && inspectionErr == nil && inspectionAfterErr == nil && reflect.DeepEqual(inspection, inspectionAfter)
	add(state.RecoveryCodeReadOnlyInvariant, readOnlyOK, "state/events/worktree before+after", "unchanged durable files and worktree inspection", "read-only invariants hold", "durable files or worktree observation changed", "operator", "stop concurrent writers and rerun the diagnosis")
	return a.output(jsonOut, report)
}

func (a App) explainResumeBlocked(ctx context.Context, l layout.Layout, repo string, issueNumber int, jsonOut bool) error {
	entry, err := a.resolvePath(l, repo)
	if err != nil {
		return err
	}
	cfg, err := config.Load(entry.RepoPath)
	if err != nil {
		return err
	}
	store := state.Store{Dir: l.RepoDir(entry.RepoID), RepoID: entry.RepoID, RepoPath: entry.RepoPath, Secrets: cfg.RedactionValues()}
	stateBefore, stateErr := os.ReadFile(store.StatePath())
	eventsBefore, eventsErr := os.ReadFile(store.EventsPath())
	if stateErr != nil || (eventsErr != nil && !os.IsNotExist(eventsErr)) {
		return fmt.Errorf("capture read-only recovery inputs: state=%v events=%v", stateErr, eventsErr)
	}
	snapshot, events, err := store.ReadRecoveryInputs()
	if err != nil {
		return err
	}
	current := snapshot.Issues[strconv.Itoa(issueNumber)]
	if current == nil {
		return exitError{4, fmt.Errorf("Issue #%d is missing from durable state", issueNumber)}
	}

	legacyFullHistory := current.EnvironmentResume != nil && current.EnvironmentResume.Status == issuedomain.EnvironmentResumeStatusRunning && current.BlockedCause != nil &&
		current.BlockedCause.Origin == "supervisor" && current.BlockedCause.Kind == "worker_workspace"
	var report state.RecoveryPredicateReport
	if legacyFullHistory {
		report = state.InterruptedWorkspaceResumePredicateReportFromEvents(*current, events)
	} else {
		report = state.RecoveryPredicateReport{
			SchemaVersion: state.RecoveryPredicateReportSchemaVersion,
			Operation:     "resume-blocked", IssueNumber: issueNumber, Eligible: true,
			Predicates: []state.RecoveryPredicate{},
		}
	}

	add := func(code state.RecoveryPredicateCode, ok bool, source, expected, passed, failed, fixability, remediation string) {
		status, actual := "fail", failed
		if ok {
			status, actual = "pass", passed
		}
		report.AddPredicate(code, status, source, expected, actual, fixability, remediation)
	}
	statusOK := legacyFullHistory || environmentResumeProgress(current) != recoveryProgressInvalid
	add(state.RecoveryCodeStatus, statusOK, "durable.state", "fully synchronized blocked state", "status boundary matches", "status or GitHub synchronization boundary differs", "operator", "wait for synchronization or inspect the active lifecycle operation")
	blockedCauseOK := legacyFullHistory || (current.BlockedCause != nil && current.BlockedCause.Origin == "worker" && current.BlockedCause.Kind == "environment" && current.BlockedCause.Resumable)
	add(state.RecoveryCodeBlockedCause, blockedCauseOK, "durable.state.blocked_cause", "resumable worker environment cause or exact legacy workspace interruption", "blocked cause is eligible", "blocked cause is not resumable by this operation", "none", "use the command named by the durable blocked cause")

	controller := a.ProcessController
	if controller == nil {
		controller = supervisor.OSProcessGroupController{}
	}
	pgid := current.WorkerPGID
	if pgid <= 1 {
		pgid = current.WorkerPID
	}
	processOK := !controller.Alive(current.WorkerPID) && !controller.GroupAlive(pgid)
	add(state.RecoveryCodeWorkerProcess, processOK, "durable.state+process table", "no live worker PID or process group", "no live worker process", "worker PID or process group is still alive", "operator", "allow the worker to exit or stop it through the normal supervisor workflow")

	parkedClaim := current.ResourcePark != nil && current.ResourcePark.Status == issuedomain.ResourceParkStatusParked && current.ResourcePark.ID != ""
	leaseOK := current.RunID != "" && current.Worktree != "" && current.Branch != "" && (current.Lease != nil || parkedClaim || legacyFullHistory)
	add(state.RecoveryCodeLeasePark, leaseOK, "durable.state", "fenced lease or exact parked/legacy authority", "lease/park authority is retained", "run, lease, park, worktree, or branch authority is incomplete", "none", "do not synthesize a lease; restore matching durable evidence")

	pending := false
	for _, request := range snapshot.PendingRequests {
		pending = pending || (request != nil && request.IssueNumber == issueNumber && request.Status == issuedomain.RequestStatusPending)
	}
	add(state.RecoveryCodePendingRequest, !pending, "durable.state.pending_requests", "no pending manual answer", "no pending answer request", "a pending answer request exists", "operator", "answer or cancel the request through its normal workflow")

	capabilityOK := true
	if current.CapabilityRequirements != nil {
		capabilityOK = capability.EvaluateRequirement(current.CapabilityRequirements, cfg.WorkerCapabilityProfiles()).Compatible
	}
	add(state.RecoveryCodeCapability, capabilityOK, "durable.state+config", "current worker profile satisfies saved requirements", "capability requirements match", "worker capability requirements differ", "operator", "select a compatible configured worker profile")

	manager := worktree.Manager{StateRoot: l.Root, GitPath: entry.Commands["git"]}
	inspection, inspectionErr := manager.Inspect(ctx, cfg, current.Worktree, current.Branch)
	workspaceOK := inspectionErr == nil && inspection.Exists && inspection.Valid && inspection.Branch == current.Branch && inspection.LocalBranchExists
	if inspectionErr != nil {
		report.AddPredicate(state.RecoveryCodeWorkspace, "unknown", "git inspection", "saved linked worktree and local branch identity", "read-only git inspection failed", "operator", "repair read access and rerun the diagnosis")
	} else {
		add(state.RecoveryCodeWorkspace, workspaceOK, "git inspection", "saved linked worktree and local branch identity", "worktree identity matches", "worktree existence, validity, or branch identity differs", "operator", "restore the saved worktree/branch without rewriting durable state")
	}
	if legacyFullHistory {
		expectedHead := state.InterruptedWorkspaceResumeReconciliationHead(events, *current)
		exactWorktreeOK := inspectionErr == nil && interruptedWorkspaceInspectionMatches(expectedHead, inspection)
		add(state.RecoveryCodeWorktreeHeadRemote, exactWorktreeOK, "durable.events+git inspection", "stable dirty local-only reconciliation HEAD", "local-only HEAD and remote absence match", "HEAD, dirty state, or remote branch identity differs", "operator", "restore the exact local-only worktree boundary; do not push or rewrite it")
	}

	launch, launchErr := manager.ValidateLaunch(ctx, cfg, current.Worktree, current.Branch)
	launchOK := launchErr == nil && launch.Valid
	add(state.RecoveryCodeWorkspaceProvenance, launchOK, "local launch validation", "canonical managed worktree and repository identity", "launch provenance matches", "launch provenance could not be established", "operator", "repair the managed worktree boundary and rerun the diagnosis")
	if legacyFullHistory && inspectionErr == nil {
		baseSHA, baseErr := environmentResumeBaseSHA(ctx, entry.Commands["git"], cfg, current, inspection)
		currentBaseSHA, currentBaseErr := currentRemoteBaseSHA(ctx, entry.Commands["git"], current.Worktree, cfg.Git.BaseBranch)
		baseOK := baseErr == nil && currentBaseErr == nil && current.EnvironmentResume != nil &&
			baseSHA == current.EnvironmentResume.BaseSHA && currentBaseSHA == current.EnvironmentResume.CurrentBaseSHA
		add(state.RecoveryCodeBaseSHAIdentity, baseOK, "durable.state+read-only git", "saved publication and current base identities", "base identities match", "publication or current base identity differs", "operator", "restore the original branch/base boundary or abandon this recovery")
	}

	remote, remoteErr := (gh.CLI{Path: entry.Commands["gh"], Secrets: cfg.RedactionValues()}).Inspect(ctx, cfg, issueNumber, current.Branch)
	if remoteErr != nil {
		report.AddPredicate(state.RecoveryCodeGitHubIdentity, "unknown", "GitHub read API", "open Issue, expected labels, and matching PR identity", "read-only GitHub inspection failed", "operator", "restore read access and rerun the diagnosis")
	} else {
		githubOK := strings.EqualFold(remote.Issue.State, "open") && recoveryRemoteIdentityMatches(cfg, current, remote)
		add(state.RecoveryCodeGitHubIdentity, githubOK, "GitHub read API", "open Issue, expected labels, and matching PR identity", "Issue/PR identity matches", "Issue state, labels, PR, or branch identity differs", "operator", "restore the authoritative GitHub state or abandon this recovery")
		if legacyFullHistory {
			markerOK := interruptedWorkspaceRemoteMarkersMatch(cfg, current, remote)
			add(state.RecoveryCodeGitHubCommentMarkers, markerOK, "GitHub Issue comments", "exact automation marker cardinality and identities", "marker cardinality matches", "marker cardinality or identity differs", "none", "do not expose or recreate comment text; restore authoritative automation evidence")
		}
	}

	stateAfter, stateAfterErr := os.ReadFile(store.StatePath())
	eventsAfter, eventsAfterErr := os.ReadFile(store.EventsPath())
	inspectionAfter, inspectionAfterErr := manager.Inspect(ctx, cfg, current.Worktree, current.Branch)
	readOnlyOK := stateAfterErr == nil && (eventsAfterErr == nil || os.IsNotExist(eventsAfterErr)) &&
		bytes.Equal(stateBefore, stateAfter) && bytes.Equal(eventsBefore, eventsAfter) &&
		inspectionErr == nil && inspectionAfterErr == nil && reflect.DeepEqual(inspection, inspectionAfter)
	add(state.RecoveryCodeReadOnlyInvariant, readOnlyOK, "state/events/worktree before+after", "byte-identical durable files and unchanged worktree inspection", "read-only invariants hold", "a durable file or worktree observation changed during diagnosis", "operator", "stop concurrent writers and rerun the diagnosis")
	return a.output(jsonOut, report)
}

func recoveryRemoteIdentityMatches(cfg config.Config, current *state.Issue, remote gh.RemoteState) bool {
	labels := lowerLabelSet(remote.Issue.Labels)
	blocked := false
	for _, label := range cfg.GitHub.ExcludeLabels {
		if strings.EqualFold(label, "blocked") {
			blocked = labels[strings.ToLower(label)]
			break
		}
	}
	running := labels[strings.ToLower(cfg.GitHub.RunningLabel)]
	if !blocked || running {
		return false
	}
	if current.PullRequestURL == "" {
		return len(remote.PullRequests) == 0
	}
	return len(remote.PullRequests) == 1 && remote.PullRequests[0].URL == current.PullRequestURL &&
		strings.EqualFold(remote.PullRequests[0].State, "open") && remote.PullRequests[0].HeadRefName == current.Branch
}

func interruptedWorkspaceInspectionMatches(expectedHead string, inspection worktree.Inspection) bool {
	return expectedHead != "" && inspection.Exists && inspection.Valid && inspection.Head == expectedHead && inspection.Dirty &&
		inspection.LocalBranchExists && !inspection.RemoteBranchExists && inspection.RemoteHead == "" && !inspection.RemoteConsistent
}

func interruptedWorkspaceRemoteMarkersMatch(cfg config.Config, current *state.Issue, remote gh.RemoteState) bool {
	if current == nil || current.EnvironmentResume == nil || current.BlockedCause == nil {
		return false
	}
	labels := lowerLabelSet(remote.Issue.Labels)
	blockedLabel := ""
	for _, label := range cfg.GitHub.ExcludeLabels {
		if strings.EqualFold(label, "blocked") {
			blockedLabel = strings.ToLower(label)
		}
	}
	if blockedLabel == "" || !labels[blockedLabel] || labels[strings.ToLower(cfg.GitHub.RunningLabel)] {
		return false
	}
	resumeMarker := "<!-- codex-issue-loop:environment-resume:" + current.EnvironmentResume.ID + " -->"
	failureMarker := fmt.Sprintf("<!-- codex-issue-loop:failed:%d -->", current.Number)
	resumeMarkers, expectedResumeMarkers, failureMarkers, expectedFailureMarkers := 0, 0, 0, 0
	failureIDMarkers, originalFailureIDMarkers, workspaceFailureIDMarkers := 0, 0, 0
	originalFailureComments, workspaceFailureComments := 0, 0
	originalFailureReason := "worker blocked: " + current.EnvironmentResume.PreviousReason
	workspaceFailureReason := current.BlockedCause.Reason
	originalFailureIDMarker := failureIDMarker(originalFailureReason)
	workspaceFailureIDMarker := failureIDMarker(workspaceFailureReason)
	for _, comment := range remote.Issue.Comments {
		resumeMarkers += strings.Count(comment, "<!-- codex-issue-loop:environment-resume:")
		expectedResumeMarkers += strings.Count(comment, resumeMarker)
		failureMarkers += strings.Count(comment, "<!-- codex-issue-loop:failed:")
		expectedFailureMarkers += strings.Count(comment, failureMarker)
		failureIDMarkers += strings.Count(comment, "<!-- codex-issue-loop:failure:")
		originalFailureIDMarkers += strings.Count(comment, originalFailureIDMarker)
		workspaceFailureIDMarkers += strings.Count(comment, workspaceFailureIDMarker)
		if exactFailureComment(comment, current.Number, originalFailureReason) {
			originalFailureComments++
		}
		if exactFailureComment(comment, current.Number, workspaceFailureReason) {
			workspaceFailureComments++
		}
	}
	return resumeMarkers == 2 && expectedResumeMarkers == 2 && failureMarkers == 2 && expectedFailureMarkers == 2 &&
		failureIDMarkers == 2 && originalFailureIDMarkers == 1 && workspaceFailureIDMarkers == 1 &&
		originalFailureComments == 1 && workspaceFailureComments == 1
}
