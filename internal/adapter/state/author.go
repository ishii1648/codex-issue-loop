package state

import (
	"fmt"
	"strconv"
	"time"

	queuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/queue"
)

func (s Store) RecordAuthorVerification(issueNumber int, verification queuedomain.AuthorVerification) error {
	if verification.VerifiedAt.IsZero() {
		verification.VerifiedAt = time.Now().UTC()
	}
	if issueNumber < 1 || verification.Reason == "" {
		return fmt.Errorf("author verification evidence is incomplete")
	}
	_, err := s.Update("issue_author_verification_recorded", issueNumber, "", verification, func(snapshot *Snapshot) error {
		copy := verification
		snapshot.IntakeVerifications[strconv.Itoa(issueNumber)] = &copy
		return nil
	})
	return err
}

func SameAuthorDecision(left *queuedomain.AuthorVerification, right queuedomain.AuthorVerification) bool {
	return left != nil && left.Trusted == right.Trusted && left.Login == right.Login &&
		left.AccountType == right.AccountType && left.Permission == right.Permission && left.Reason == right.Reason
}
