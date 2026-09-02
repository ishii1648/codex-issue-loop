package incidentloop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"time"

	gh "github.com/ishii1648/codex-issue-loop/internal/adapter/github"
	"github.com/ishii1648/codex-issue-loop/internal/platform/config"
	"github.com/ishii1648/codex-issue-loop/internal/platform/redact"
)

type GitHubIssues struct {
	Path    string
	Secrets []string
	Config  config.Config
}

func (g GitHubIssues) FindByFingerprint(ctx context.Context, fingerprint string) (*IssueRef, error) {
	if len(fingerprint) != 64 {
		return nil, errors.New("incident fingerprint must be a SHA-256 hex value")
	}
	path := g.Path
	if path == "" {
		path = "gh"
	}
	marker := "incident-fingerprint:" + fingerprint
	out, err := exec.CommandContext(ctx, path, "issue", "list", "--repo", g.Config.GitHub.Repo, "--state", "all", "--limit", "100", "--search", marker+" in:body", "--json", "number,url,body,labels,createdAt").CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("search incident Issues: %w: %s", err, redact.StringWithSecrets(string(out), g.Secrets))
	}
	var records []struct {
		Number    int       `json:"number"`
		URL       string    `json:"url"`
		Body      string    `json:"body"`
		CreatedAt time.Time `json:"createdAt"`
		Labels    []struct {
			Name string `json:"name"`
		} `json:"labels"`
	}
	if err := json.Unmarshal(out, &records); err != nil {
		return nil, fmt.Errorf("decode incident Issue search: %w", err)
	}
	var matches []IssueRef
	for _, record := range records {
		if !strings.Contains(record.Body, "<!-- "+marker+" -->") {
			continue
		}
		labels := make([]string, 0, len(record.Labels))
		for _, label := range record.Labels {
			labels = append(labels, label.Name)
		}
		matches = append(matches, IssueRef{Number: record.Number, URL: record.URL, Labels: labels, Fingerprint: fingerprint, Status: "existing", CreatedAt: record.CreatedAt})
	}
	if len(matches) > 1 {
		return nil, fmt.Errorf("multiple Issues contain incident fingerprint %s", fingerprint)
	}
	if len(matches) == 0 {
		return nil, nil
	}
	return &matches[0], nil
}

func (g GitHubIssues) Create(ctx context.Context, draft IssueDraft) (IssueRef, error) {
	path := g.Path
	if path == "" {
		path = "gh"
	}
	args := []string{"issue", "create", "--repo", g.Config.GitHub.Repo, "--title", draft.Title, "--body", draft.Body}
	for _, label := range draft.Labels {
		args = append(args, "--label", label)
	}
	out, err := exec.CommandContext(ctx, path, args...).CombinedOutput()
	if err != nil {
		return IssueRef{}, fmt.Errorf("create incident Issue: %w: %s", err, redact.StringWithSecrets(string(out), g.Secrets))
	}
	issueURL := strings.TrimSpace(string(out))
	number, err := issueNumberFromURL(issueURL)
	if err != nil {
		return IssueRef{}, err
	}
	return IssueRef{Number: number, URL: issueURL, Labels: append([]string(nil), draft.Labels...), Fingerprint: draft.Fingerprint, Status: "created", CreatedAt: time.Now().UTC()}, nil
}

func (g GitHubIssues) ReadBack(ctx context.Context, number int) (IssueRef, error) {
	issue, err := (gh.CLI{Path: g.Path, Secrets: g.Secrets}).Get(ctx, g.Config, number)
	if err != nil {
		return IssueRef{}, err
	}
	prefix := "<!-- incident-fingerprint:"
	start := strings.Index(issue.Body, prefix)
	if start < 0 {
		return IssueRef{}, errors.New("created Issue does not contain incident fingerprint")
	}
	start += len(prefix)
	end := strings.Index(issue.Body[start:], " -->")
	if end < 0 {
		return IssueRef{}, errors.New("created Issue fingerprint marker is malformed")
	}
	return IssueRef{Number: issue.Number, URL: issue.URL, Labels: append([]string(nil), issue.Labels...), Fingerprint: issue.Body[start : start+end], Status: "read_back", CreatedAt: issue.CreatedAt}, nil
}

func issueNumberFromURL(value string) (int, error) {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Host != "github.com" {
		return 0, fmt.Errorf("gh issue create returned unexpected URL %q", value)
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(parts) != 4 || parts[2] != "issues" {
		return 0, fmt.Errorf("gh issue create returned unexpected URL %q", value)
	}
	number, err := strconv.Atoi(parts[3])
	if err != nil || number < 1 {
		return 0, fmt.Errorf("gh issue create returned invalid Issue number in %q", value)
	}
	return number, nil
}
