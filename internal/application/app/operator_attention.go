package app

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ishii1648/codex-issue-loop/internal/adapter/state"
	"github.com/ishii1648/codex-issue-loop/internal/adapter/webhook"
	"github.com/ishii1648/codex-issue-loop/internal/application/observe"
	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
	"github.com/ishii1648/codex-issue-loop/internal/platform/config"
	"github.com/ishii1648/codex-issue-loop/internal/platform/launchd"
	"github.com/ishii1648/codex-issue-loop/internal/platform/layout"
	"github.com/ishii1648/codex-issue-loop/internal/platform/ratelimit"
	"github.com/ishii1648/codex-issue-loop/internal/platform/redact"
)

func (a App) status(ctx context.Context, l layout.Layout, args []string) error {
	entry, jsonOut, err := a.resolve(l, "status", args)
	if err != nil {
		return err
	}
	cfg, err := config.Load(entry.RepoPath)
	if err != nil {
		return err
	}
	store := state.Store{Dir: l.RepoDir(entry.RepoID), RepoID: entry.RepoID, RepoPath: entry.RepoPath, Secrets: cfg.RedactionValues()}
	snapshot, err := store.Load()
	if err != nil {
		return err
	}
	if cooldown, active, cooldownErr := (ratelimit.Store{Path: l.RateLimitPath()}).Current(time.Now().UTC()); cooldownErr != nil {
		return cooldownErr
	} else if active {
		snapshot.Supervisor.RateLimit = &state.RateLimit{
			Resource: cooldown.Resource, ObservedResetAt: cooldown.ResetAt,
			CooldownSource: cooldown.Source, SuppressedRetryCount: cooldown.SuppressedRetryCount,
		}
		if snapshot.Supervisor.RetryAfter == nil || cooldown.ResetAt.After(*snapshot.Supervisor.RetryAfter) {
			snapshot.Supervisor.RetryAfter = &cooldown.ResetAt
		}
	}
	launchStatus, err := (launchd.Manager{Layout: l, Launchctl: entry.Commands["launchctl"]}).Status(ctx, entry)
	if err != nil {
		return err
	}
	result := buildStatus(launchStatus, snapshot, cfg.Queue.Concurrency)
	if cfg.Webhook.Enabled() {
		manager := launchd.Manager{Layout: l, Launchctl: entry.Commands["launchctl"]}
		brokerLaunchd, statusErr := manager.BrokerStatus(ctx)
		if statusErr != nil {
			return statusErr
		}
		var brokerState webhook.Status
		data, readErr := os.ReadFile(filepath.Join(l.BrokerDir(), "status.json"))
		if readErr == nil {
			if err := json.Unmarshal(data, &brokerState); err != nil {
				return fmt.Errorf("decode broker status: %w", err)
			}
		} else if !errors.Is(readErr, os.ErrNotExist) {
			return readErr
		}
		sweep, sweepErr := webhook.LoadSweepState(l.RepoDir(entry.RepoID))
		if sweepErr != nil {
			return sweepErr
		}
		brokerState.LastSuccessfulSafetySweep = sweep.LastSuccessful
		brokerState.NotModified304 = sweep.NotModified304
		deliveries, mailboxErr := webhook.ReadMailbox(l.RepoDir(entry.RepoID))
		if mailboxErr != nil {
			return mailboxErr
		}
		brokerState.QueueDepth = len(deliveries)
		result.Broker = &brokerStatus{Launchd: brokerLaunchd, State: brokerState, Sweep: sweep,
			Queue: assessQueueHealth(time.Now().UTC(), cfg.Watch.ReconcileInterval.Duration, snapshot, sweep, deliveries)}
	}
	return a.output(jsonOut, result)
}

func (a App) watch(ctx context.Context, l layout.Layout, args []string) error {
	fs := flag.NewFlagSet("watch", flag.ContinueOnError)
	fs.SetOutput(a.Err)
	repo := fs.String("repo", "", "repository path")
	jsonOut := fs.Bool("json", false, "emit JSON")
	untilAttention := fs.Bool("until-attention", false, "wait for attention")
	untilIdle := fs.Bool("until-idle", false, "also return when idle")
	if err := fs.Parse(args); err != nil {
		return exitError{2, err}
	}
	if !*untilAttention && !*untilIdle {
		return exitError{2, fmt.Errorf("watch requires --until-attention or --until-idle")}
	}
	entry, err := a.resolvePath(l, *repo)
	if err != nil {
		return err
	}
	cfg, err := config.Load(entry.RepoPath)
	if err != nil {
		return err
	}
	store := state.Store{Dir: l.RepoDir(entry.RepoID), RepoID: entry.RepoID, RepoPath: entry.RepoPath, Secrets: cfg.RedactionValues()}
	result, err := observe.Wait(ctx, store, cfg.Watch.ReconcileInterval.Duration, cfg.Watch.ReconcileJitter, *untilIdle)
	if err != nil {
		return err
	}
	return a.output(*jsonOut, result)
}

func (a App) answer(ctx context.Context, l layout.Layout, args []string) error {
	const maxAnswerBytes = 16 * 1024
	fs := flag.NewFlagSet("answer", flag.ContinueOnError)
	fs.SetOutput(a.Err)
	repo := fs.String("repo", "", "repository path")
	requestID := fs.String("request-id", "", "request ID")
	message := fs.String("message", "", "answer text")
	messageFile := fs.String("message-file", "", "answer file or - for stdin")
	jsonOut := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return exitError{2, err}
	}
	if *requestID == "" {
		return exitError{2, fmt.Errorf("--request-id is required")}
	}
	if (*message == "") == (*messageFile == "") {
		return exitError{2, fmt.Errorf("provide exactly one of --message or --message-file")}
	}
	answer := *message
	if *messageFile != "" {
		var data []byte
		var err error
		if *messageFile == "-" {
			data, err = io.ReadAll(io.LimitReader(a.In, maxAnswerBytes+1))
		} else {
			file, openErr := os.Open(*messageFile)
			if openErr != nil {
				return openErr
			}
			data, err = io.ReadAll(io.LimitReader(file, maxAnswerBytes+1))
			_ = file.Close()
		}
		if err != nil {
			return err
		}
		answer = strings.TrimSpace(string(data))
	}
	if answer == "" {
		return exitError{2, fmt.Errorf("answer must not be empty")}
	}
	if len(answer) > maxAnswerBytes {
		return exitError{2, fmt.Errorf("answer must not exceed %d bytes", maxAnswerBytes)}
	}
	entry, err := a.resolvePath(l, *repo)
	if err != nil {
		return err
	}
	cfg, err := config.Load(entry.RepoPath)
	if err != nil {
		return err
	}
	secrets := cfg.RedactionValues()
	if redact.StringWithSecrets(answer, secrets) != answer {
		return exitError{2, fmt.Errorf("answer must not contain a credential or configured secret")}
	}
	store := state.Store{Dir: l.RepoDir(entry.RepoID), RepoID: entry.RepoID, RepoPath: entry.RepoPath, Secrets: secrets}
	currentSnapshot, err := store.Load()
	if err != nil {
		return err
	}
	currentRequest := currentSnapshot.PendingRequests[*requestID]
	if currentRequest == nil {
		return exitError{4, fmt.Errorf("unknown request ID %s", *requestID)}
	}
	if currentRequest.Status == issuedomain.RequestStatusAnswered {
		if currentRequest.Answer != answer {
			return exitError{4, fmt.Errorf("request %s already has a different answer", *requestID)}
		}
		return a.answerOutput(*jsonOut, currentSnapshot, cfg.Queue.Concurrency, currentRequest)
	}
	if currentRequest.Status != issuedomain.RequestStatusPending {
		return exitError{4, fmt.Errorf("request %s is not pending", *requestID)}
	}
	currentIssue := currentSnapshot.Issues[fmt.Sprint(currentRequest.IssueNumber)]
	if currentIssue == nil {
		return exitError{4, fmt.Errorf("Issue #%d is missing from state", currentRequest.IssueNumber)}
	}
	parkedNeedsInput := currentRequest.ResumeStatus == issuedomain.StatusUnset
	if parkedNeedsInput {
		pendingForIssue := 0
		for _, request := range currentSnapshot.PendingRequests {
			if request != nil && request.IssueNumber == currentIssue.Number && request.Status == issuedomain.RequestStatusPending {
				pendingForIssue++
			}
		}
		if pendingForIssue != 1 {
			return exitError{4, fmt.Errorf("Issue #%d has ambiguous pending requests", currentIssue.Number)}
		}
		if currentIssue.Status != issuedomain.StatusNeedsInput || currentIssue.WorkerPID != 0 || currentIssue.WorkerPGID != 0 ||
			(currentSnapshot.ActiveExecution != nil && currentSnapshot.ActiveExecution.IssueNumber == currentIssue.Number) {
			return exitError{4, fmt.Errorf("Issue #%d is not a stopped needs-input continuation", currentIssue.Number)}
		}
		if err := state.ValidateNeedsInputContinuation(currentIssue, currentRequest); err != nil {
			return exitError{4, err}
		}
	}
	answerTransitions := map[issuedomain.Status]issuedomain.Transition{}
	answerTargets := []issuedomain.Status{currentRequest.ResumeStatus}
	if parkedNeedsInput {
		answerTargets = []issuedomain.Status{issuedomain.StatusResumePending}
	}
	for _, target := range answerTargets {
		transition, transitionErr := issuedomain.ResumeAfterAnswer(currentIssue.Status, target)
		if transitionErr != nil {
			return exitError{4, transitionErr}
		}
		answerTransitions[target] = transition
	}
	payload := map[string]any{"request_id": *requestID}
	updated, err := store.Update("answer_recorded", currentRequest.IssueNumber, currentIssue.RunID, payload, func(s *state.Snapshot) error {
		request := s.PendingRequests[*requestID]
		if request == nil {
			return exitError{4, fmt.Errorf("unknown request ID %s", *requestID)}
		}
		if request.Status == issuedomain.RequestStatusAnswered {
			if request.Answer == answer {
				return nil
			}
			return exitError{4, fmt.Errorf("request %s already has a different answer", *requestID)}
		}
		if request.Status != issuedomain.RequestStatusPending {
			return exitError{4, fmt.Errorf("request %s is not pending", *requestID)}
		}
		now := time.Now().UTC()
		request.Status, request.Answer, request.AnsweredAt = issuedomain.RequestStatusAnswered, answer, &now
		issue := s.Issues[fmt.Sprint(request.IssueNumber)]
		if issue == nil {
			return fmt.Errorf("Issue #%d is missing from state", request.IssueNumber)
		}
		resumeStatus := request.ResumeStatus
		if resumeStatus == issuedomain.StatusUnset {
			pendingForIssue := 0
			for _, candidate := range s.PendingRequests {
				if candidate != nil && candidate.IssueNumber == issue.Number && candidate.Status == issuedomain.RequestStatusPending {
					pendingForIssue++
				}
			}
			if pendingForIssue != 0 {
				return exitError{4, fmt.Errorf("Issue #%d has ambiguous pending requests", issue.Number)}
			}
			if issue.Status != issuedomain.StatusNeedsInput || issue.WorkerPID != 0 || issue.WorkerPGID != 0 ||
				(s.ActiveExecution != nil && s.ActiveExecution.IssueNumber == issue.Number) {
				return exitError{4, fmt.Errorf("Issue #%d changed before its answer was recorded", issue.Number)}
			}
			if err := state.ValidateNeedsInputContinuation(issue, request); err != nil {
				return exitError{4, err}
			}
			resumeStatus = issuedomain.StatusResumePending
			payload["execution_waiting"] = s.ActiveExecution != nil
		}
		transition, ok := answerTransitions[resumeStatus]
		if !ok {
			return fmt.Errorf("Issue #%d answer selected unsupported resume status %q", issue.Number, resumeStatus)
		}
		if err := state.ApplyIssueTransition(issue, transition); err != nil {
			return err
		}
		issue.RetryAfter, issue.UpdatedAt = nil, now
		if resumeStatus == issuedomain.StatusResolvingConflict {
			if err := state.SetEffect(s, issue.Number, issue.RunID, issuedomain.EffectRetryConflict, now); err != nil {
				return err
			}
		}
		issue.Answers = append(issue.Answers, state.AnswerRecord{RequestID: request.ID, Question: request.Question, Answer: answer, AnsweredAt: now})
		return nil
	})
	if err != nil {
		return err
	}
	_ = ctx // answer is a durable local transaction; the supervisor owns remote reconciliation.
	return a.answerOutput(*jsonOut, updated, cfg.Queue.Concurrency, updated.PendingRequests[*requestID])
}

func (a App) answerOutput(jsonOut bool, snapshot state.Snapshot, _ int, request *state.Request) error {
	output := map[string]any{"request_id": request.ID, "recorded": true}
	issue := snapshot.Issues[strconv.Itoa(request.IssueNumber)]
	if issue != nil {
		output["status"] = issue.Status
		if issue.Continuation != nil && issue.Continuation.Kind == state.ContinuationKindNeedsInput {
			output["checkpoint_id"] = issue.Continuation.ID
			output["execution_waiting"] = snapshot.ActiveExecution != nil && snapshot.ActiveExecution.IssueNumber != issue.Number
		}
	}
	return a.output(jsonOut, output)
}
