package queue

import (
	"strings"
	"time"
)

type AuthorFacts struct {
	Login           string
	AccountType     string
	Permission      string
	RepositoryOwner bool
}

type AuthorPolicy struct {
	AllowLogins []string
}

type AuthorVerification struct {
	Trusted     bool      `json:"trusted"`
	Login       string    `json:"login,omitempty"`
	AccountType string    `json:"account_type,omitempty"`
	Permission  string    `json:"permission,omitempty"`
	Reason      string    `json:"reason"`
	VerifiedAt  time.Time `json:"verified_at,omitempty"`
}

func VerifyAuthor(policy AuthorPolicy, facts AuthorFacts, observedAt time.Time) AuthorVerification {
	login := strings.ToLower(strings.TrimSpace(facts.Login))
	result := AuthorVerification{
		Login: login, AccountType: strings.TrimSpace(facts.AccountType),
		Permission: strings.ToLower(strings.TrimSpace(facts.Permission)), VerifiedAt: observedAt.UTC(),
	}
	if login == "" {
		result.Reason = "author_missing"
		return result
	}
	for _, allowed := range policy.AllowLogins {
		if login == allowed {
			result.Trusted, result.Reason = true, "exact_allowlist"
			return result
		}
	}
	if facts.RepositoryOwner && !automationAccount(facts.AccountType) {
		result.Trusted, result.Permission, result.Reason = true, "admin", "repository_owner"
		return result
	}
	if automationAccount(facts.AccountType) {
		result.Reason = "automation_not_allowlisted"
		return result
	}
	switch result.Permission {
	case "write", "maintain", "admin":
		result.Trusted, result.Reason = true, "repository_permission"
	default:
		result.Reason = "permission_below_write"
	}
	return result
}

func automationAccount(accountType string) bool {
	return strings.EqualFold(accountType, "Bot") || strings.EqualFold(accountType, "App")
}
