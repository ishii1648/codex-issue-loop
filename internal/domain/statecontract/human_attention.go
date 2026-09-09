package statecontract

import (
	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
	"sort"
	"strconv"
)

type HumanNeed struct {
	IssueNumber int    `json:"issue_number"`
	Reason      string `json:"reason"`
}

func (s Snapshot) HumanNeeds(autoMerge bool) []HumanNeed {
	unanswered := map[int]bool{}
	for _, r := range s.Requests() {
		if r.Status == issuedomain.RequestStatusPending {
			unanswered[r.IssueNumber] = true
		}
	}
	needs := []HumanNeed{}
	for _, item := range s.Issues {
		if item == nil {
			continue
		}
		wait := issuedomain.HumanWait{Status: item.Status, Unanswered: unanswered[item.Number], ReviewDecision: item.ReviewDecision, AutoMerge: autoMerge}
		if item.Suspension != nil {
			wait.Recoverability, wait.SuspensionStatus = item.Suspension.Recoverability, item.Suspension.Status
		}
		if reason := wait.Reason(); reason != "" {
			needs = append(needs, HumanNeed{IssueNumber: item.Number, Reason: reason})
		}
	}
	for key, q := range s.QuarantinedIssues {
		if q == nil {
			continue
		}
		number, _ := strconv.Atoi(key)
		wait := issuedomain.HumanWait{Quarantined: true, Unanswered: unanswered[number]}
		needs = append(needs, HumanNeed{IssueNumber: number, Reason: wait.Reason()})
	}
	sort.Slice(needs, func(i, j int) bool { return needs[i].IssueNumber < needs[j].IssueNumber })
	return needs
}

func (s Snapshot) NeedsHuman(number int, autoMerge bool) bool {
	for _, need := range s.HumanNeeds(autoMerge) {
		if need.IssueNumber == number {
			return true
		}
	}
	return false
}
