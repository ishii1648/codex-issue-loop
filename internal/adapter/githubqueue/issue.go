package githubqueue

import (
	"github.com/ishii1648/codex-issue-loop/internal/platform/config"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

type Issue struct {
	Number      int
	Title       string
	Body        string
	URL         string
	CreatedAt   time.Time
	Labels      []string
	Assignees   []string
	Milestone   string
	Comments    []string
	State       string
	StateReason string `json:"StateReason,omitempty"`
	AuthorLogin string `json:"author_login,omitempty"`
	AuthorType  string `json:"author_type,omitempty"`
}

const (
	MaxIssueTitleBytes = 512
	MaxIssueBodyBytes  = 64 * 1024
	MaxIssueComments   = 20
	MaxCommentBytes    = 8 * 1024
)

func Eligible(labels []string, cfg config.GitHub) bool {
	set := map[string]bool{}
	for _, label := range labels {
		set[strings.ToLower(label)] = true
	}
	for _, label := range cfg.ReadyLabels {
		if !set[strings.ToLower(label)] {
			return false
		}
	}
	for _, label := range cfg.ExcludeLabels {
		if set[strings.ToLower(label)] {
			return false
		}
	}
	for _, label := range []string{cfg.RunningLabel, cfg.NeedsInputLabel, cfg.FailedLabel, cfg.DoneLabel} {
		if label != "" && set[strings.ToLower(label)] {
			return false
		}
	}
	return true
}

func EligibleIssue(issue Issue, cfg config.GitHub) bool {
	if !strings.EqualFold(issue.State, "open") {
		return false
	}
	if !Eligible(issue.Labels, cfg) {
		return false
	}
	if cfg.Assignee != "" {
		matched := false
		for _, assignee := range issue.Assignees {
			matched = matched || strings.EqualFold(assignee, cfg.Assignee)
		}
		if !matched {
			return false
		}
	}
	return cfg.Milestone == "" || issue.Milestone == cfg.Milestone
}

func NormalizeIssue(issue Issue) Issue {
	issue.State = strings.ToUpper(strings.TrimSpace(issue.State))
	issue.StateReason = strings.ToUpper(strings.TrimSpace(issue.StateReason))
	issue.Title = SafeText(issue.Title, MaxIssueTitleBytes)
	issue.Body = SafeText(issue.Body, MaxIssueBodyBytes)
	issue.URL = SafeText(issue.URL, 2048)
	comments := make([]string, len(issue.Comments))
	for index, comment := range issue.Comments {
		comments[index] = SafeText(comment, len(comment))
	}
	if issue.Comments != nil {
		issue.Comments = comments
	}
	return issue
}

func SafeText(value string, limit int) string {
	value = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' || r >= ' ' && r != 0x7f {
			return r
		}
		return -1
	}, value)
	if len(value) <= limit {
		return value
	}
	value = value[:limit]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value + "\n[TRUNCATED]"
}

func OrderIssues(issues []Issue, queue config.Queue) {
	priorityRank := make(map[string]int, len(queue.PriorityLabels))
	for index, label := range queue.PriorityLabels {
		priorityRank[strings.ToLower(label)] = index
	}
	rank := func(issue Issue) int {
		result := len(priorityRank)
		for _, label := range issue.Labels {
			if candidate, ok := priorityRank[strings.ToLower(label)]; ok && candidate < result {
				result = candidate
			}
		}
		return result
	}
	createdBefore := func(left, right Issue) bool {
		if left.CreatedAt.IsZero() != right.CreatedAt.IsZero() {
			return !left.CreatedAt.IsZero()
		}
		if !left.CreatedAt.Equal(right.CreatedAt) {
			return left.CreatedAt.Before(right.CreatedAt)
		}
		return left.Number < right.Number
	}
	sort.SliceStable(issues, func(i, j int) bool {
		switch queue.Order {
		case "created_at_asc":
			return createdBefore(issues[i], issues[j])
		case "priority_then_created_at":
			left, right := rank(issues[i]), rank(issues[j])
			if left != right {
				return left < right
			}
			return createdBefore(issues[i], issues[j])
		default:
			return issues[i].Number < issues[j].Number
		}
	})
}
