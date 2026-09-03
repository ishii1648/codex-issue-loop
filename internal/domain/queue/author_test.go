package queue

import (
	"testing"
	"time"
)

func TestVerifyAuthorPolicy(t *testing.T) {
	now := time.Date(2026, 9, 4, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		facts  AuthorFacts
		allow  []string
		trust  bool
		reason string
	}{
		{name: "owner", facts: AuthorFacts{Login: "owner", RepositoryOwner: true}, trust: true, reason: "repository_owner"},
		{name: "writer", facts: AuthorFacts{Login: "dev", Permission: "write"}, trust: true, reason: "repository_permission"},
		{name: "reader", facts: AuthorFacts{Login: "reader", Permission: "read"}, reason: "permission_below_write"},
		{name: "bot denied", facts: AuthorFacts{Login: "bot", AccountType: "Bot", Permission: "admin"}, reason: "automation_not_allowlisted"},
		{name: "bot allowlisted", facts: AuthorFacts{Login: "bot", AccountType: "Bot"}, allow: []string{"bot"}, trust: true, reason: "exact_allowlist"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := VerifyAuthor(AuthorPolicy{AllowLogins: test.allow}, test.facts, now)
			if got.Trusted != test.trust || got.Reason != test.reason || !got.VerifiedAt.Equal(now) {
				t.Fatalf("verification=%+v", got)
			}
		})
	}
}
