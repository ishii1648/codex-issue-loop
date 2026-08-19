package github

import (
	"encoding/json"
	"testing"
)

func TestPullRequestHeadRepositoryFullName(t *testing.T) {
	for _, test := range []struct {
		name    string
		payload string
		want    string
	}{
		{
			name:    "current gh shape",
			payload: `{"headRepository":{"id":"R_kgDOTdsezg","name":"repo"},"headRepositoryOwner":{"login":"owner"}}`,
			want:    "owner/repo",
		},
		{
			name:    "legacy name with owner",
			payload: `{"headRepository":{"nameWithOwner":"owner/repo"}}`,
			want:    "owner/repo",
		},
		{name: "missing owner", payload: `{"headRepository":{"name":"repo"}}`},
		{name: "missing repository name", payload: `{"headRepository":{},"headRepositoryOwner":{"login":"owner"}}`},
		{name: "missing repository", payload: `{"headRepositoryOwner":{"login":"owner"}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			var identity PullRequestHeadRepository
			if err := json.Unmarshal([]byte(test.payload), &identity); err != nil {
				t.Fatal(err)
			}
			if got := identity.FullName(); got != test.want {
				t.Fatalf("FullName()=%q, want %q", got, test.want)
			}
		})
	}
}
