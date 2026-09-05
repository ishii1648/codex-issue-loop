package inputanswer

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/ishii1648/codex-issue-loop/internal/adapter/state"
	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
	"github.com/ishii1648/codex-issue-loop/internal/platform/redact"
)

const MaxAnswerBytes = 16 * 1024

type ConflictError struct{ Message string }

func (e ConflictError) Error() string { return e.Message }

var errAlreadyRecorded = errors.New("answer already recorded")

func Validate(request *state.Request, answer string, secrets []string) error {
	if answer == "" {
		return fmt.Errorf("answer must not be empty")
	}
	if len(answer) > MaxAnswerBytes {
		return fmt.Errorf("answer must not exceed %d bytes", MaxAnswerBytes)
	}
	if !utf8.ValidString(answer) {
		return fmt.Errorf("answer must be valid UTF-8")
	}
	for _, r := range answer {
		if r == '\n' || r == '\t' {
			continue
		}
		if unicode.IsControl(r) {
			return fmt.Errorf("answer must not contain control characters")
		}
	}
	if redact.StringWithSecrets(answer, secrets) != answer {
		return fmt.Errorf("answer must not contain a credential or configured secret")
	}
	if request != nil && len(request.Options) > 0 {
		for _, option := range request.Options {
			if answer == option.ID {
				return nil
			}
		}
		if !request.AllowFreeText {
			return fmt.Errorf("answer must be one of the advertised option IDs")
		}
	}
	return nil
}

func BodyDigest(body string) string {
	digest := sha256.Sum256([]byte(body))
	return hex.EncodeToString(digest[:])
}

func Record(store state.Store, requestID, answer string, secrets []string, provenance *state.AnswerProvenance, now time.Time) (state.Snapshot, *state.Request, error) {
	currentSnapshot, err := store.Load()
	if err != nil {
		return state.Snapshot{}, nil, err
	}
	currentRequest := currentSnapshot.PendingRequests[requestID]
	if currentRequest == nil {
		return state.Snapshot{}, nil, ConflictError{Message: fmt.Sprintf("unknown request ID %s", requestID)}
	}
	if err := Validate(currentRequest, answer, secrets); err != nil {
		return state.Snapshot{}, nil, err
	}
	if currentRequest.Status == issuedomain.RequestStatusAnswered {
		if currentRequest.Answer != answer {
			return state.Snapshot{}, nil, ConflictError{Message: fmt.Sprintf("request %s already has a different answer", requestID)}
		}
		if !sameObservation(currentRequest.AnswerProvenance, provenance) {
			return state.Snapshot{}, nil, ConflictError{Message: fmt.Sprintf("request %s was answered by a different observation", requestID)}
		}
		return currentSnapshot, currentRequest, nil
	}
	if currentRequest.Status != issuedomain.RequestStatusPending {
		return state.Snapshot{}, nil, ConflictError{Message: fmt.Sprintf("request %s is not pending", requestID)}
	}
	currentIssue := currentSnapshot.Issues[strconv.Itoa(currentRequest.IssueNumber)]
	if currentIssue == nil {
		return state.Snapshot{}, nil, ConflictError{Message: fmt.Sprintf("Issue #%d is missing from state", currentRequest.IssueNumber)}
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
			return state.Snapshot{}, nil, ConflictError{Message: fmt.Sprintf("Issue #%d has ambiguous pending requests", currentIssue.Number)}
		}
		if currentIssue.Status != issuedomain.StatusNeedsInput || currentIssue.WorkerPID != 0 || currentIssue.WorkerPGID != 0 ||
			(currentSnapshot.ActiveExecution != nil && currentSnapshot.ActiveExecution.IssueNumber == currentIssue.Number) {
			return state.Snapshot{}, nil, ConflictError{Message: fmt.Sprintf("Issue #%d is not a stopped needs-input continuation", currentIssue.Number)}
		}
		if err := state.ValidateNeedsInputContinuation(currentIssue, currentRequest); err != nil {
			return state.Snapshot{}, nil, ConflictError{Message: err.Error()}
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
			return state.Snapshot{}, nil, ConflictError{Message: transitionErr.Error()}
		}
		answerTransitions[target] = transition
	}
	payload := map[string]any{"request_id": requestID, "source": "cli"}
	if provenance != nil {
		payload = map[string]any{
			"request_id": requestID, "source": provenance.Source, "comment_id": provenance.CommentID,
			"actor": provenance.Actor, "body_sha256": provenance.BodySHA256,
		}
	}
	updated, err := store.Update("answer_recorded", currentRequest.IssueNumber, currentIssue.RunID, payload, func(s *state.Snapshot) error {
		request := s.PendingRequests[requestID]
		if request == nil {
			return ConflictError{Message: fmt.Sprintf("unknown request ID %s", requestID)}
		}
		if request.Status == issuedomain.RequestStatusAnswered {
			if request.Answer == answer {
				if sameObservation(request.AnswerProvenance, provenance) {
					return errAlreadyRecorded
				}
				return ConflictError{Message: fmt.Sprintf("request %s was answered by a different observation", requestID)}
			}
			return ConflictError{Message: fmt.Sprintf("request %s already has a different answer", requestID)}
		}
		if request.Status != issuedomain.RequestStatusPending {
			return ConflictError{Message: fmt.Sprintf("request %s is not pending", requestID)}
		}
		issue := s.Issues[strconv.Itoa(request.IssueNumber)]
		if issue == nil {
			return fmt.Errorf("Issue #%d is missing from state", request.IssueNumber)
		}
		resumeStatus := request.ResumeStatus
		if resumeStatus == issuedomain.StatusUnset {
			pendingForIssue := 0
			for _, candidate := range s.PendingRequests {
				if candidate != nil && candidate.IssueNumber == issue.Number && candidate.Status == issuedomain.RequestStatusPending && candidate.ID != request.ID {
					pendingForIssue++
				}
			}
			if pendingForIssue != 0 {
				return ConflictError{Message: fmt.Sprintf("Issue #%d has ambiguous pending requests", issue.Number)}
			}
			if issue.Status != issuedomain.StatusNeedsInput || issue.WorkerPID != 0 || issue.WorkerPGID != 0 ||
				(s.ActiveExecution != nil && s.ActiveExecution.IssueNumber == issue.Number) {
				return ConflictError{Message: fmt.Sprintf("Issue #%d changed before its answer was recorded", issue.Number)}
			}
			if err := state.ValidateNeedsInputContinuation(issue, request); err != nil {
				return ConflictError{Message: err.Error()}
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
		answeredAt := now.UTC()
		request.Status, request.Answer, request.AnsweredAt = issuedomain.RequestStatusAnswered, answer, &answeredAt
		request.AnswerProvenance = cloneProvenance(provenance)
		issue.RetryAfter, issue.UpdatedAt = nil, answeredAt
		if resumeStatus == issuedomain.StatusResolvingConflict {
			if err := state.SetEffect(s, issue.Number, issue.RunID, issuedomain.EffectRetryConflict, answeredAt); err != nil {
				return err
			}
		}
		issue.Answers = append(issue.Answers, state.AnswerRecord{
			RequestID: request.ID, Question: request.Question, Answer: answer, AnsweredAt: answeredAt,
		})
		return nil
	})
	if errors.Is(err, errAlreadyRecorded) {
		current, loadErr := store.Load()
		if loadErr != nil {
			return state.Snapshot{}, nil, loadErr
		}
		return current, current.PendingRequests[requestID], nil
	}
	if err != nil {
		return state.Snapshot{}, nil, err
	}
	return updated, updated.PendingRequests[requestID], nil
}

func sameObservation(existing, proposed *state.AnswerProvenance) bool {
	if proposed == nil {
		return true
	}
	return existing != nil && existing.Source == proposed.Source && existing.CommentID == proposed.CommentID &&
		existing.RequestID == proposed.RequestID && existing.IssueNumber == proposed.IssueNumber && existing.RunID == proposed.RunID &&
		existing.BodySHA256 == proposed.BodySHA256
}

func cloneProvenance(value *state.AnswerProvenance) *state.AnswerProvenance {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func NormalizeCommandAnswer(value string) string {
	return strings.TrimSpace(value)
}
