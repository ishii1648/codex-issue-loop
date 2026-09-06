package conformance

import (
	"bytes"
	"fmt"
	"math/rand"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ishii1648/codex-issue-loop/internal/adapter/state"
	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
)

const (
	noSuspension issuedomain.SuspensionStatus = ""
	noRequest    issuedomain.RequestStatus    = ""
)

type lifecycleModel struct {
	status               issuedomain.Status
	generation           uint64
	active, continuation bool
	suspension           issuedomain.SuspensionStatus
	request              issuedomain.RequestStatus
	answers              int
	effect               issuedomain.EffectKind
}

func lifecycleOperations(sequence string) []string {
	ops := []string{"start", "claim", "launch", "running"}
	switch sequence {
	case "worker-retry-continuation", "reconciliation":
		op := "retry"
		if sequence == "reconciliation" {
			op = "reconcile"
		}
		ops = append(ops, op, "reload", "retry-launch", "running")
	case "needs-input-resume":
		ops = append(ops, "input", "prepare", "reload", "answer", "answer", "different-answer", "prepare", "prepare", "resume-launch", "running")
	case "environment-block-resume":
		ops = append(ops, "block", "reload", "resolve", "resume-launch", "running")
	case "publication-recovery", "checks-recovery":
		ops = append(ops, "checks", "reload", "merge")
	case "conflict-publication":
		ops = append(ops, "checks", "conflict", "conflict-launch", "running")
	}
	return append(ops, "complete", "repeat-effect", "stale-effect", "reload", "clear-effect", "clear-effect", "complete", "reload")
}

func TestLifecycleSequenceMatrix(t *testing.T) {
	for _, sequence := range loadCatalog(t).Sequences {
		t.Run(sequence, func(t *testing.T) { runLifecycleSequence(t, lifecycleOperations(sequence)) })
	}
}

func TestDeterministicModelRuns1000ReplayableSequences(t *testing.T) {
	sequences := loadCatalog(t).Sequences
	for index := 0; index < generatedSequenceCount; index++ {
		seed := fixedSeeds[index%len(fixedSeeds)] + int64(index/len(fixedSeeds))*1000
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			t.Parallel()
			random := rand.New(rand.NewSource(seed))
			base := lifecycleOperations(sequences[random.Intn(len(sequences))])
			var ops []string
			for _, op := range base {
				ops = append(ops, op)
				noise := []string{"reload", "stale-run", "stale-generation", "stale-decision", "unknown-answer", "out-of-order"}
				ops = append(ops, noise[random.Intn(len(noise))])
				if op == "running" {
					ops = append(ops, "competing-start", "duplicate-start")
				}
			}
			runLifecycleSequence(t, ops)
		})
	}
}

func (m *lifecycleModel) apply(op string) (reject, unchanged bool) {
	switch op {
	case "start":
		m.status, m.generation, m.active = issuedomain.StatusClaiming, 1, true
	case "claim":
		m.status = issuedomain.StatusClaimed
	case "launch":
		m.status = issuedomain.StatusLaunching
	case "running":
		m.status = issuedomain.StatusRunning
	case "retry", "reconcile":
		m.status, m.active, m.continuation = issuedomain.StatusRetryWait, false, true
	case "retry-launch", "resume-launch", "conflict-launch":
		m.status, m.active = issuedomain.StatusLaunching, true
		m.generation++
	case "input":
		m.status, m.active, m.continuation, m.request, m.effect = issuedomain.StatusNeedsInput, false, true, issuedomain.RequestStatusPending, issuedomain.EffectMarkNeedsInput
	case "answer":
		if m.request == issuedomain.RequestStatusAnswered {
			return false, true
		}
		m.request = issuedomain.RequestStatusAnswered
	case "prepare":
		if m.request != issuedomain.RequestStatusAnswered || m.answers != 0 {
			return false, true
		}
		m.status, m.answers, m.effect = issuedomain.StatusResumePending, 1, issuedomain.EffectNone
	case "block":
		m.status, m.active, m.continuation, m.suspension, m.effect = issuedomain.StatusBlocked, false, true, issuedomain.SuspensionActive, issuedomain.EffectMarkBlocked
	case "resolve":
		m.status, m.suspension, m.effect = issuedomain.StatusResumePending, issuedomain.SuspensionResolved, issuedomain.EffectNone
	case "checks":
		m.status, m.active, m.continuation = issuedomain.StatusAwaitingChecks, false, true
	case "merge":
		m.status = issuedomain.StatusAwaitingMerge
	case "conflict":
		m.status = issuedomain.StatusResolvingConflict
	case "complete":
		if m.status == issuedomain.StatusCompleted {
			return true, true
		}
		m.status, m.active, m.continuation, m.suspension, m.effect = issuedomain.StatusCompleted, false, false, noSuspension, issuedomain.EffectMarkDone
	case "clear-effect":
		m.effect = issuedomain.EffectNone
	case "repeat-effect", "duplicate-start":
	case "reload":
		return false, true
	case "stale-run", "stale-generation", "stale-decision", "competing-start", "different-answer", "unknown-answer", "stale-effect", "out-of-order":
		return true, true
	default:
		panic(op)
	}
	return false, false
}

func runLifecycleSequence(t *testing.T, ops []string) {
	t.Helper()
	store := state.Store{Dir: t.TempDir(), RepoID: "repo-conformance", RepoPath: "/tmp/conformance"}
	if err := store.Initialize(); err != nil {
		t.Fatal(err)
	}
	model := lifecycleModel{}
	now := time.Date(2026, 9, 4, 0, 0, 0, 0, time.UTC)
	var effectID string
	for step, op := range ops {
		t.Run(fmt.Sprintf("step-%02d-%s", step, op), func(t *testing.T) {
			t.Cleanup(func() {
				if t.Failed() {
					t.Logf("first mismatch step=%d operations=%q", step, ops[:step+1])
				}
			})
			before, err := store.Load()
			if err != nil {
				t.Fatal(err)
			}
			eventBytes, err := os.ReadFile(store.EventsPath())
			if err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			owner := state.ExecutionIdentity{RunID: "run_model", Generation: model.generation}
			reject, unchanged := model.apply(op)
			var result state.Snapshot
			switch op {
			case "start", "competing-start", "duplicate-start":
				number, run := 1, "run_model"
				if op == "competing-start" {
					number, run = 2, "run_other"
				}
				var actual state.ExecutionIdentity
				result, actual, err = store.StartExecution(state.ExecutionStart{IssueNumber: number, RunID: run, BaseSHA: strings.Repeat("a", 40), StartedAt: now})
				if !reject && actual != (state.ExecutionIdentity{RunID: "run_model", Generation: model.generation}) {
					t.Fatalf("identity=%+v model=%+v", actual, model)
				}
			case "reload":
				store = state.Store{Dir: store.Dir, RepoID: store.RepoID, RepoPath: store.RepoPath}
				result, err = store.Load()
			case "answer", "different-answer", "unknown-answer":
				id, answer := "req_model", "yes"
				if op == "different-answer" {
					answer = "no"
				}
				if op == "unknown-answer" {
					id = "req_unknown"
				}
				result, _, err = store.RecordAnswer(id, answer, now.Add(time.Duration(step)*time.Second))
			case "prepare":
				result, err = store.PrepareAnsweredRequests(now)
			default:
				result, err = store.Update(op, 1, owner.RunID, nil, func(s *state.Snapshot) error {
					item := s.Issues["1"]
					var transition issuedomain.Transition
					var decisionErr error
					switch op {
					case "stale-run", "stale-generation":
						if op == "stale-run" {
							owner.RunID = "run_old"
						} else {
							owner.Generation--
						}
						return state.CaptureContinuation(s, 1, owner, "checkpoint_stale", now)
					case "stale-decision":
						from := issuedomain.StatusClaiming
						if item.Status == from {
							from = issuedomain.StatusClaimed
						}
						if from == issuedomain.StatusClaiming {
							transition, decisionErr = issuedomain.ConfirmClaim(from)
						} else {
							transition, decisionErr = issuedomain.StartClaimedWorker(from)
						}
					case "out-of-order":
						transition, decisionErr = issuedomain.ResumeAfterAnswer(item.Status, issuedomain.StatusRunning)
					case "claim":
						transition, decisionErr = issuedomain.ConfirmClaim(item.Status)
						item.Worktree, item.Branch = "/tmp/conformance-worktree", "codex/conformance"
						item.Workspace = &state.WorkerWorkspace{Path: item.Worktree, Branch: item.Branch, RepoID: store.RepoID, Repository: "owner/repo", RepositoryID: 1, GitCommonDir: "/tmp/conformance/.git", MainCheckout: store.RepoPath, CapturedAt: now}
					case "launch":
						transition, decisionErr = issuedomain.StartClaimedWorker(item.Status)
					case "running":
						transition, decisionErr = issuedomain.ConfirmWorkerStarted(item.Status)
						item.WorkerPID, item.WorkerPGID = 4242, 4242
					case "retry":
						d, e := issuedomain.ScheduleRetry(item.Status, "retry", now.Add(time.Minute), "worker")
						transition, decisionErr = d.Transition, e
					case "reconcile":
						transition, decisionErr = issuedomain.InterruptExecution(item.Status)
					case "input":
						d, e := issuedomain.RequestInput(item.Status)
						transition, decisionErr = d.Transition, e
						if e == nil {
							if err := state.CaptureContinuation(s, 1, owner, "checkpoint_model", now); err != nil {
								return err
							}
							c := item.Continuation
							c.Kind, c.RequestID, c.Stage, c.HeadSHA, c.WorktreeSHA256 = state.ContinuationKindNeedsInput, "req_model", issuedomain.ContinuationStageResume, strings.Repeat("b", 40), strings.Repeat("c", 64)
							s.PendingRequests["req_model"] = &state.Request{ID: "req_model", IssueNumber: 1, RunID: owner.RunID, CheckpointID: c.ID, ReleasedExecution: &owner, Question: "Continue?", AllowFreeText: true, Status: issuedomain.RequestStatusPending, CreatedAt: now}
							if err := state.SetEffect(s, 1, owner.RunID, d.Effect, now); err != nil {
								return err
							}
						}
					case "block":
						d, e := issuedomain.SuspendWorker(item.Status, "environment", "environment")
						item.Continuation = &state.ContinuationCheckpoint{ID: "checkpoint_environment", Stage: issuedomain.ContinuationStageResume, WorktreeSHA256: strings.Repeat("c", 64)}
						item.LastError, item.FailureKind = d.LastError, d.FailureKind
						transition, decisionErr = d.Transition, e
						if e == nil {
							if err := state.SetEffect(s, 1, owner.RunID, d.Effect, now); err != nil {
								return err
							}
						}
					case "resolve":
						transition, decisionErr = issuedomain.ResolveSuspension(item.Status, issuedomain.ResolutionResume, item.Continuation.Stage)
						item.Suspension.Status, item.Suspension.Resolution, item.Suspension.ResolvedAt = issuedomain.SuspensionResolved, issuedomain.ResolutionResume, now
						if err := state.ClearEffect(s, 1, state.PendingEffect(s, 1).ID); err != nil {
							return err
						}
					case "retry-launch", "resume-launch", "conflict-launch":
						switch op {
						case "retry-launch":
							transition, decisionErr = issuedomain.StartRetry(item.Status)
						case "resume-launch":
							transition, decisionErr = issuedomain.StartAnsweredResume(item.Status)
						case "conflict-launch":
							transition, decisionErr = issuedomain.StartConflictAttempt(item.Status)
						}
						if decisionErr == nil {
							if _, err := state.ResumeContinuation(s, 1, item.Continuation.ID, now); err != nil {
								return err
							}
						}
					case "checks":
						d, e := issuedomain.AwaitChecks(item.Status)
						transition, decisionErr = d.Transition, e
						item.PullRequestURL, item.PullRequestNumber, item.HeadSHA = "https://example.test/pull/1", 1, strings.Repeat("b", 40)
					case "merge":
						transition, decisionErr = issuedomain.AwaitMerge(item.Status)
					case "conflict":
						transition, decisionErr = issuedomain.ResolveConflict(item.Status)
					case "complete":
						d, e := issuedomain.Complete(item.Status, item.PullRequestURL)
						transition, decisionErr = d.Transition, e
						if e == nil {
							if err := state.SetEffect(s, 1, owner.RunID, d.Effect, now); err != nil {
								return err
							}
						}
					case "repeat-effect":
						return state.SetEffect(s, 1, owner.RunID, issuedomain.EffectMarkDone, now)
					case "stale-effect":
						return state.ClearEffect(s, 1, "effect_stale")
					case "clear-effect":
						return state.ClearEffect(s, 1, effectID)
					default:
						return fmt.Errorf("unknown operation %q", op)
					}
					if decisionErr != nil {
						return decisionErr
					}
					if transition.To == issuedomain.StatusLaunching {
						item.LaunchSource = item.Status
					}
					return state.ApplyIssueTransition(item, transition)
				})
			}
			if (err != nil) != reject {
				t.Fatalf("error=%v want rejection=%v", err, reject)
			}
			after, loadErr := store.Load()
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			if unchanged {
				afterEvents, e := os.ReadFile(store.EventsPath())
				if e != nil {
					t.Fatal(e)
				}
				if !reflect.DeepEqual(before, after) || !bytes.Equal(eventBytes, afterEvents) {
					t.Fatal("rejected/idempotent operation changed durable state or events")
				}
			} else if after.StateRevision != before.StateRevision+1 {
				t.Fatalf("revision=%d want=%d", after.StateRevision, before.StateRevision+1)
			}
			if !reject && !reflect.DeepEqual(result, after) {
				t.Fatal("commit result differs from reloaded snapshot")
			}
			item := after.Issues["1"]
			if item == nil || len(after.Issues) != 1 || len(after.QuarantinedIssues) != 0 {
				t.Fatalf("unexpected aggregates: %+v", after)
			}
			if item.Status != model.status || item.Generation != model.generation || item.RunID != "run_model" || (after.ActiveExecution != nil) != model.active || (item.Continuation != nil) != model.continuation || len(item.Answers) != model.answers {
				t.Fatalf("actual=%+v active=%+v model=%+v", item, after.ActiveExecution, model)
			}
			if model.continuation {
				c := item.Continuation
				if c.RunID != "run_model" || c.Generation == 0 || c.Generation > model.generation || c.BaseSHA != strings.Repeat("a", 40) || !reflect.DeepEqual(c.Workspace, item.Workspace) {
					t.Fatalf("continuation evidence=%+v", c)
				}
			}
			if model.active {
				if a := after.ActiveExecution; a.IssueNumber != 1 || a.RunID != "run_model" || a.Generation != model.generation {
					t.Fatalf("active=%+v", a)
				}
			} else if item.WorkerPID != 0 || item.WorkerPGID != 0 {
				t.Fatal("released execution retains worker process")
			}
			if (item.Suspension != nil) != (model.suspension != noSuspension) || item.Suspension != nil && item.Suspension.Status != model.suspension {
				t.Fatalf("suspension=%+v want=%s", item.Suspension, model.suspension)
			}
			if model.request != noRequest {
				r := after.PendingRequests["req_model"]
				if r == nil || r.Status != model.request {
					t.Fatalf("request=%+v want=%s", r, model.request)
				}
				if model.request == issuedomain.RequestStatusAnswered && (r.Answer != "yes" || r.AnsweredAt == nil) {
					t.Fatalf("answer=%+v", r)
				}
				if model.answers == 1 && (item.Answers[0].RequestID != r.ID || item.Answers[0].Answer != "yes" || !item.Answers[0].AnsweredAt.Equal(*r.AnsweredAt)) {
					t.Fatal("answer delivery changed")
				}
			}
			effect := state.PendingEffect(&after, 1)
			if (effect != nil) != (model.effect != issuedomain.EffectNone) || effect != nil && effect.Kind != model.effect {
				t.Fatalf("effect=%+v want=%s", effect, model.effect)
			}
			if effect != nil {
				if len(after.PendingEffects) != 1 || effect.IssueNumber != 1 || effect.RunID != "run_model" || !effect.CreatedAt.Equal(now) {
					t.Fatalf("effect identity=%+v", effect)
				}
				if op == "repeat-effect" && effect.ID != effectID {
					t.Fatal("duplicate publication created a new effect")
				}
				effectID = effect.ID
			}
		})
		if t.Failed() {
			return
		}
	}
	snapshot, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, event := range assertContiguousEvents(t, store.EventsPath(), snapshot.StateRevision) {
		counts[event.Type]++
	}
	if counts["complete"] != 1 || counts["answer_recorded"] != model.answers || counts["answer_ready"] != model.answers {
		t.Fatalf("operations=%q durable applications=%v", ops, counts)
	}
}
