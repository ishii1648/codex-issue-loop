package state

import (
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"time"

	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
)

// Requests includes quarantined requests so execution recovery cannot hide a conversation.
func (s Snapshot) Requests() []*Request {
	requests := make([]*Request, 0, len(s.PendingRequests))
	for _, request := range s.PendingRequests {
		if request != nil {
			requests = append(requests, request)
		}
	}
	for _, record := range s.QuarantinedIssues {
		if record != nil {
			for _, request := range record.Requests {
				if request != nil {
					requests = append(requests, request)
				}
			}
		}
	}
	sort.Slice(requests, func(i, j int) bool { return requests[i].ID < requests[j].ID })
	return requests
}

func (s Snapshot) Request(id string) (*Request, error) {
	var found *Request
	for _, request := range s.Requests() {
		if request.ID != id {
			continue
		}
		if found != nil {
			return nil, fmt.Errorf("ambiguous request ID %s", id)
		}
		found = request
	}
	if found == nil {
		return nil, fmt.Errorf("unknown request ID %s", id)
	}
	return found, nil
}

// RecordAnswer commits only the response. Dispatch and recovery cannot roll back its receipt.
func (s Store) RecordAnswer(id, answer string, now time.Time, observations ...*AnswerProvenance) (Snapshot, *Request, error) {
	snapshot, err := s.Load()
	if err != nil {
		return snapshot, nil, err
	}
	if !ValidID(id, "req_") || now.IsZero() || len(observations) > 1 {
		return snapshot, nil, fmt.Errorf("answer identity and timestamp are required")
	}
	var provenance *AnswerProvenance
	if len(observations) == 1 && observations[0] != nil {
		copy := *observations[0]
		provenance = &copy
	}
	request, err := snapshot.Request(id)
	if err != nil {
		return snapshot, nil, ConflictError{Message: "unknown or ambiguous request"}
	}
	if err := ValidateAnswer(request, answer, s.Secrets); err != nil {
		return snapshot, nil, err
	}
	if err := validateAnswerObservation(snapshot, request, provenance); err != nil {
		return snapshot, nil, err
	}
	if request.Status == issuedomain.RequestStatusAnswered {
		if request.Answer != answer {
			return snapshot, nil, ConflictError{Message: "request already has a different answer"}
		}
		return snapshot, request, nil
	}
	payload := map[string]any{"request_id": id, "source": "cli"}
	if provenance != nil {
		payload["source"], payload["provenance"] = provenance.Source, provenance
	}
	updated, err := s.Update("answer_recorded", request.IssueNumber, request.RunID, payload, func(current *Snapshot) error {
		request, err := current.Request(id)
		if err != nil {
			return ConflictError{Message: "unknown or ambiguous request"}
		}
		if err := ValidateAnswer(request, answer, s.Secrets); err != nil {
			return err
		}
		if err := validateAnswerObservation(*current, request, provenance); err != nil {
			return err
		}
		if request.Status == issuedomain.RequestStatusAnswered {
			if request.Answer == answer {
				return errAnswerRecorded
			}
			return ConflictError{Message: "request already has a different answer"}
		}
		if err := ApplyRequestAnswer(request, answer, now); err != nil {
			return err
		}
		request.AnswerProvenance = provenance
		return nil
	})
	if errors.Is(err, errAnswerRecorded) {
		updated, err = s.Load()
	}
	if err != nil {
		return snapshot, nil, err
	}
	request, err = updated.Request(id)
	return updated, request, err
}

var errAnswerRecorded = errors.New("answer already recorded")

func validateAnswerObservation(snapshot Snapshot, request *Request, provenance *AnswerProvenance) error {
	if request.Status != issuedomain.RequestStatusPending && request.Status != issuedomain.RequestStatusAnswered {
		return ConflictError{Message: "request is canceled"}
	}
	if provenance == nil {
		return nil
	}
	if provenance.Source != "github_issue_comment" || provenance.CommentID <= 0 || provenance.Actor == "" ||
		(provenance.Permission != "write" && provenance.Permission != "maintain" && provenance.Permission != "admin") ||
		provenance.RequestID != request.ID || provenance.IssueNumber != request.IssueNumber || provenance.RunID != request.RunID ||
		!validSHA256(provenance.BodySHA256) || provenance.CommentedAt.IsZero() || provenance.CommentedAt.Before(request.CreatedAt) ||
		provenance.CommentEdited.Before(provenance.CommentedAt) {
		return ConflictError{Message: "comment observation does not match request"}
	}
	if request.Status == issuedomain.RequestStatusAnswered {
		old := request.AnswerProvenance
		if old == nil || old.CommentID != provenance.CommentID || old.BodySHA256 != provenance.BodySHA256 ||
			old.Actor != provenance.Actor || !old.CommentEdited.Equal(provenance.CommentEdited) {
			return ConflictError{Message: "request was answered by a different observation"}
		}
		return nil
	}
	key := strconv.Itoa(request.IssueNumber)
	run := ""
	if item := snapshot.Issues[key]; item != nil {
		run = item.RunID
	} else if item := snapshot.QuarantinedIssues[key]; item != nil {
		run = item.RunID
	} else {
		return ConflictError{Message: "request has no Issue"}
	}
	if run != request.RunID {
		return ConflictError{Message: "request belongs to a stale run"}
	}
	return nil
}

func answeredTransition(snapshot Snapshot, request *Request) (issuedomain.Transition, error) {
	item := snapshot.Issues[strconv.Itoa(request.IssueNumber)]
	if item == nil || request.Status != issuedomain.RequestStatusAnswered || request.AnsweredAt == nil {
		return issuedomain.Transition{}, fmt.Errorf("answer has no managed continuation")
	}
	for _, recorded := range item.Answers {
		if recorded.RequestID == request.ID {
			return issuedomain.Transition{}, fmt.Errorf("answer already delivered")
		}
	}
	target := request.ResumeStatus
	if target == issuedomain.StatusUnset {
		target = issuedomain.StatusResumePending
	}
	transition, err := issuedomain.ResumeAfterAnswer(item.Status, target)
	if err != nil {
		return transition, err
	}
	if item.WorkerPID != 0 || item.WorkerPGID != 0 || snapshot.ActiveExecution != nil && snapshot.ActiveExecution.IssueNumber == item.Number {
		return transition, fmt.Errorf("answer continuation still owns execution")
	}
	for _, other := range snapshot.PendingRequests {
		if other != nil && other.IssueNumber == item.Number && other.Status == issuedomain.RequestStatusPending {
			return transition, fmt.Errorf("another question remains unanswered")
		}
	}
	if request.ResumeStatus == issuedomain.StatusUnset {
		if err := ValidateNeedsInputContinuation(item, request); err != nil {
			return transition, err
		}
	}
	return transition, nil
}

// PrepareAnsweredRequests is retried by the supervisor after receipt, including after restart.
func (s Store) PrepareAnsweredRequests(now time.Time) (Snapshot, error) {
	snapshot, err := s.Load()
	if err != nil {
		return snapshot, err
	}
	for _, request := range snapshot.Requests() {
		if _, err := answeredTransition(snapshot, request); err != nil {
			continue
		}
		snapshot, err = s.Update("answer_ready", request.IssueNumber, request.RunID, map[string]string{"request_id": request.ID}, func(current *Snapshot) error {
			latest, err := current.Request(request.ID)
			if err != nil {
				return err
			}
			transition, err := answeredTransition(*current, latest)
			if err != nil {
				return err
			}
			item := current.Issues[strconv.Itoa(latest.IssueNumber)]
			if err := ApplyIssueTransition(item, transition); err != nil {
				return err
			}
			item.Answers = append(item.Answers, AnswerRecord{RequestID: latest.ID, Question: latest.Question, Answer: latest.Answer, AnsweredAt: *latest.AnsweredAt})
			item.RetryAfter, item.UpdatedAt = nil, now.UTC()
			if effect := PendingEffect(current, item.Number); effect != nil && effect.Kind == issuedomain.EffectMarkNeedsInput {
				if err := ClearEffect(current, item.Number, effect.ID); err != nil {
					return err
				}
			}
			if transition.To == issuedomain.StatusResolvingConflict {
				return SetEffect(current, item.Number, item.RunID, issuedomain.EffectRetryConflict, now)
			}
			return nil
		})
		if err != nil {
			return snapshot, err
		}
	}
	return snapshot, nil
}

func (s Store) AskIntake(number int, title, question, reason, recommended string, options []Option, freeText bool, now time.Time) (Snapshot, *Request, error) {
	id := NewID("req")
	updated, err := s.Update("input_requested", number, "", nil, func(current *Snapshot) error {
		item := current.Issues[strconv.Itoa(number)]
		if current.QuarantinedIssues[strconv.Itoa(number)] != nil || item != nil && (item.Status != issuedomain.StatusUnset || item.RunID != "") {
			return fmt.Errorf("intake question requires an Issue without an execution")
		}
		for _, r := range current.Requests() {
			if r.IssueNumber == number && r.Status == issuedomain.RequestStatusPending {
				if r.Question != question || r.Reason != reason || r.Recommended != recommended || !reflect.DeepEqual(r.Options, options) || r.AllowFreeText != freeText {
					return fmt.Errorf("Issue already has a different unanswered question")
				}
				id = r.ID
				return nil
			}
		}
		if item == nil {
			current.Issues[strconv.Itoa(number)] = &Issue{Number: number, Title: title, UpdatedAt: now.UTC()}
		}
		current.PendingRequests[id] = &Request{ID: id, IssueNumber: number, Question: question, Reason: reason, Recommended: recommended, Options: options, AllowFreeText: freeText, Status: issuedomain.RequestStatusPending, CreatedAt: now.UTC()}
		return nil
	})
	if err != nil {
		return updated, nil, err
	}
	request, err := updated.Request(id)
	return updated, request, err
}
