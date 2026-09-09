package statecontract

import (
	"fmt"
	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
	"sort"
	"strconv"
)

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

func ValidateAnswerObservation(snapshot Snapshot, request *Request, provenance *AnswerProvenance) error {
	if request.Status != issuedomain.RequestStatusPending && request.Status != issuedomain.RequestStatusAnswered {
		return ConflictError{Message: "request is canceled"}
	}
	if provenance == nil {
		return nil
	}
	if provenance.Source != "github_issue_comment" || provenance.CommentID <= 0 || provenance.Actor == "" ||
		provenance.RequestID != request.ID || provenance.IssueNumber != request.IssueNumber || provenance.RunID != request.RunID ||
		!ValidSHA256(provenance.BodySHA256) || provenance.CommentedAt.IsZero() || provenance.CommentedAt.Before(request.CreatedAt) ||
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

type ConflictError struct{ Message string }

func (e ConflictError) Error() string { return e.Message }
