package producer

import (
	"reflect"
	"strings"
	"testing"

	"github.com/ishii1648/codex-issue-loop/internal/admission"
	"github.com/ishii1648/codex-issue-loop/internal/config"
	gh "github.com/ishii1648/codex-issue-loop/internal/github"
)

func producerConfig() config.Config {
	cfg := config.Defaults()
	cfg.GitHub.Repo = "owner/repo"
	cfg.Queue.Concurrency = 2
	cfg.Resources.Definitions = []config.ResourceDefinition{
		{Name: "config", Paths: []string{"internal/config/**", "docs/resource-admission.md"}},
		{Name: "docs", Paths: []string{"docs/**"}},
	}
	return cfg
}

func validProposal() Proposal {
	return Proposal{
		Version: ProposalVersion, IssueNumber: 69, Confidence: "high", AmbiguityReasons: []string{},
		Resources: []ResourceCandidate{
			{Name: "config", Paths: []string{"docs/resource-admission.md"}, Reason: "contract changes"},
			{Name: "docs", Paths: []string{"docs/resource-admission.md"}, Reason: "documentation changes"},
		},
		Dependencies: []DependencyCandidate{{IssueNumber: 61, Reason: "contract prerequisite"}},
	}
}

func TestValidateProposalReturnsParallelCandidates(t *testing.T) {
	report, err := ValidateProposal(producerConfig(), validProposal())
	if err != nil {
		t.Fatal(err)
	}
	if !report.Valid || report.Exclusive || !reflect.DeepEqual(report.Resources, []string{"config", "docs"}) || !reflect.DeepEqual(report.Dependencies, []int{61}) {
		t.Fatalf("report=%+v", report)
	}
}

func TestValidateProposalFallsBackInsteadOfAssumingParallelSafety(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Proposal)
		reason string
	}{
		{name: "low confidence", mutate: func(p *Proposal) { p.Confidence = "low" }, reason: "confidence_not_high"},
		{name: "ambiguity", mutate: func(p *Proposal) { p.AmbiguityReasons = []string{"scope is unclear"} }, reason: "issue_ambiguous"},
		{name: "overlap missing", mutate: func(p *Proposal) { p.Resources = p.Resources[:1] }, reason: "overlapping_resource_missing"},
		{name: "unknown path", mutate: func(p *Proposal) { p.Resources[0].Paths = []string{"future/file.go"}; p.Resources[1].Paths = []string{"future/file.go"} }, reason: "path_unmapped"},
		{name: "unknown resource", mutate: func(p *Proposal) { p.Resources[0].Name = "future" }, reason: "resource_unknown"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			proposal := validProposal()
			test.mutate(&proposal)
			report, err := ValidateProposal(producerConfig(), proposal)
			if err != nil {
				t.Fatal(err)
			}
			if !report.Valid || !report.Exclusive || len(report.Resources) != 0 || !contains(report.FallbackReasons, test.reason) {
				t.Fatalf("report=%+v", report)
			}
		})
	}
}

func TestValidateProposalRejectsInvalidDependenciesAndExclusiveReason(t *testing.T) {
	proposal := validProposal()
	proposal.Dependencies = append(proposal.Dependencies,
		DependencyCandidate{IssueNumber: 61, Reason: "duplicate"},
		DependencyCandidate{IssueNumber: 69, Reason: "self"})
	proposal.Exclusive = true
	report, err := ValidateProposal(producerConfig(), proposal)
	if err != nil {
		t.Fatal(err)
	}
	if report.Valid || len(report.Errors) != 3 {
		t.Fatalf("report=%+v", report)
	}
}

func TestAuditAcceptsDeliberateExclusiveAndRejectsBrokenMetadata(t *testing.T) {
	cfg := producerConfig()
	body, err := MetadataBody("scope", []int{61})
	if err != nil {
		t.Fatal(err)
	}
	report, err := Audit(cfg, gh.Issue{Number: 69, Body: body}, []DependencySnapshot{{IssueNumber: 61, State: "open"}}, []byte("config"))
	if err != nil {
		t.Fatal(err)
	}
	if !report.Valid || !report.Exclusive || !reflect.DeepEqual(report.Resources, []string{admission.RepositoryResource}) || len(report.Errors) != 0 {
		t.Fatalf("report=%+v", report)
	}
	report, err = Audit(cfg, gh.Issue{Number: 69, Body: "missing"}, nil, []byte("config"))
	if err != nil {
		t.Fatal(err)
	}
	if report.Valid || report.FallbackReasons[0] != admission.FallbackMetadataMissing {
		t.Fatalf("report=%+v", report)
	}
}

func TestMetadataBodyIsCanonicalAndRejectsUnsafeRepair(t *testing.T) {
	body, err := MetadataBody("scope\n", []int{62, 61})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, "depends_on:\n  - 61\n  - 62") {
		t.Fatalf("body=%q", body)
	}
	replaced, err := MetadataBody(body, nil)
	if err != nil || strings.Count(replaced, "<!-- agent-loop:metadata") != 1 || !strings.Contains(replaced, "depends_on: []") {
		t.Fatalf("replaced=%q err=%v", replaced, err)
	}
	if _, err := MetadataBody("scope\n<!-- agent-loop:metadata\nversion: 1", nil); err == nil {
		t.Fatal("unterminated block was repaired destructively")
	}
}

func TestAuditSnapshotIsStableForLabelOrder(t *testing.T) {
	cfg := producerConfig()
	body, err := MetadataBody("scope", nil)
	if err != nil {
		t.Fatal(err)
	}
	left, err := Audit(cfg, gh.Issue{Number: 69, Body: body, Labels: []string{"area:docs", "area:config"}}, nil, []byte("config"))
	if err != nil {
		t.Fatal(err)
	}
	right, err := Audit(cfg, gh.Issue{Number: 69, Body: body, Labels: []string{"area:config", "area:docs", "area:docs"}}, nil, []byte("config"))
	if err != nil {
		t.Fatal(err)
	}
	if left.SnapshotSHA256 != right.SnapshotSHA256 || left.SnapshotSHA256 == "" {
		t.Fatalf("left=%s right=%s", left.SnapshotSHA256, right.SnapshotSHA256)
	}
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
