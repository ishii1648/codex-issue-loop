package admission

import (
	"reflect"
	"testing"
	"time"
)

func testSettings(concurrency int) Settings {
	return Settings{
		Concurrency: concurrency, MetadataVersion: 1,
		Definitions: []ResourceDefinition{
			{Name: "config", Paths: []string{"internal/config/**"}},
			{Name: "docs", Paths: []string{"docs/**"}},
			{Name: "worker", Paths: []string{"internal/worker/**"}},
		},
	}
}

func metadata(dependencies string) string {
	return "<!-- agent-loop:metadata\nversion: 1\ndepends_on: " + dependencies + "\n-->"
}

func candidate(number int, resource, dependencies string) Candidate {
	labels := []string{}
	if resource != "" {
		labels = append(labels, resource)
	}
	return Candidate{Number: number, Labels: labels, Body: metadata(dependencies)}
}

func selectedNumbers(result Result) []int {
	numbers := make([]int, len(result.Selected))
	for index, selected := range result.Selected {
		numbers[index] = selected.Candidate.Number
	}
	return numbers
}

func skipReasons(result Result) map[int]string {
	reasons := map[int]string{}
	for _, skipped := range result.Skipped {
		reasons[skipped.Evaluation.Candidate.Number] = skipped.Reason
	}
	return reasons
}

func TestSelectTable(t *testing.T) {
	complete := DependencyState{Exists: true, Accessible: true, Closed: true}
	tests := []struct {
		name         string
		input        Input
		wantSelected []int
		wantReasons  map[int]string
	}{
		{
			name: "resource conflict is skipped and lower candidate backfills",
			input: Input{Settings: testSettings(3), Candidates: []Candidate{
				candidate(3, "area:worker", "[]"), candidate(2, "area:config", "[]"), candidate(1, "area:config", "[]"),
			}},
			wantSelected: []int{1, 3},
			wantReasons:  map[int]string{2: ReasonResourceConflict},
		},
		{
			name: "active lease permits safe backfill",
			input: Input{Settings: testSettings(1), Active: []Lease{{IssueNumber: 90, Resources: []string{"config"}}}, Candidates: []Candidate{
				candidate(1, "area:config", "[]"), candidate(2, "area:docs", "[]"),
			}},
			wantSelected: []int{2},
			wantReasons:  map[int]string{1: ReasonResourceConflict},
		},
		{
			name: "incomplete dependency waits without taking a lease",
			input: Input{Settings: testSettings(2), Candidates: []Candidate{
				candidate(1, "area:config", "[20]"), candidate(2, "area:config", "[]"),
			}},
			wantSelected: []int{2},
			wantReasons:  map[int]string{1: ReasonDependencyIncomplete},
		},
		{
			name: "completed dependency admits candidate",
			input: Input{Settings: testSettings(1), Candidates: []Candidate{
				candidate(1, "area:config", "[20]"),
			}, Dependencies: map[int]DependencyState{20: complete}},
			wantSelected: []int{1},
			wantReasons:  map[int]string{},
		},
		{
			name: "dependency cycle waits but unrelated candidate proceeds",
			input: Input{Settings: testSettings(2), Candidates: []Candidate{
				candidate(1, "area:config", "[2]"), candidate(2, "area:docs", "[1]"), candidate(3, "area:worker", "[]"),
			}},
			wantSelected: []int{3},
			wantReasons:  map[int]string{1: ReasonDependencyCycle, 2: ReasonDependencyCycle},
		},
		{
			name: "missing metadata is exclusive",
			input: Input{Settings: testSettings(3), Candidates: []Candidate{
				{Number: 1, Labels: []string{"area:config"}}, candidate(2, "area:docs", "[]"),
			}},
			wantSelected: []int{1},
			wantReasons:  map[int]string{2: ReasonResourceConflict},
		},
		{
			name: "unknown resource is exclusive and preserves valid dependency",
			input: Input{Settings: testSettings(2), Candidates: []Candidate{
				candidate(1, "area:unknown", "[20]"), candidate(2, "area:docs", "[]"),
			}, Dependencies: map[int]DependencyState{20: complete}},
			wantSelected: []int{1},
			wantReasons:  map[int]string{2: ReasonResourceConflict},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result, err := Select(test.input)
			if err != nil {
				t.Fatal(err)
			}
			if got := selectedNumbers(result); !reflect.DeepEqual(got, test.wantSelected) {
				t.Fatalf("selected=%v want=%v result=%+v", got, test.wantSelected, result)
			}
			if got := skipReasons(result); !reflect.DeepEqual(got, test.wantReasons) {
				t.Fatalf("reasons=%v want=%v result=%+v", got, test.wantReasons, result)
			}
		})
	}
}

func TestSelectIsIndependentOfCandidateLabelAndLeaseOrder(t *testing.T) {
	base := time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC)
	first := Input{
		Settings: testSettings(3),
		Queue:    Queue{Order: "priority_then_created_at", PriorityLabels: []string{"priority:high", "priority:low"}},
		Active:   []Lease{{IssueNumber: 91, Resources: []string{"worker"}}, {IssueNumber: 90, Resources: []string{"config"}}},
		Candidates: []Candidate{
			{Number: 3, CreatedAt: base, Labels: []string{"area:worker", "priority:low"}, Body: metadata("[]")},
			{Number: 2, CreatedAt: base.Add(time.Hour), Labels: []string{"area:docs"}, Body: metadata("[]")},
			{Number: 1, CreatedAt: base, Labels: []string{"area:config", "PRIORITY:HIGH"}, Body: metadata("[]")},
		},
	}
	second := first
	second.Active = []Lease{first.Active[1], first.Active[0]}
	second.Candidates = []Candidate{first.Candidates[1], first.Candidates[0], first.Candidates[2]}
	second.Candidates[2].Labels = []string{"PRIORITY:HIGH", "area:config"}
	left, err := Select(first)
	if err != nil {
		t.Fatal(err)
	}
	right, err := Select(second)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(left, right) {
		t.Fatalf("nondeterministic results: left=%+v right=%+v", left, right)
	}
}

func TestDependencyCompletionContract(t *testing.T) {
	tests := []struct {
		name  string
		state DependencyState
		want  bool
	}{
		{name: "local completion with recorded merge", state: DependencyState{LocalCompleted: true, PullRequestMergeRecorded: true}, want: true},
		{name: "local completion without recorded merge", state: DependencyState{LocalCompleted: true}, want: false},
		{name: "closed remote Issue", state: DependencyState{Exists: true, Accessible: true, Closed: true}, want: true},
		{name: "closed Issue with unmerged PR", state: DependencyState{Exists: true, Accessible: true, Closed: true, KnownOpenOrUnmergedPR: true}, want: false},
		{name: "inaccessible Issue", state: DependencyState{Exists: true, Closed: true}, want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := test.state.Complete(); got != test.want {
				t.Fatalf("Complete()=%v want=%v", got, test.want)
			}
		})
	}
}

func TestRepositoryLeaseConflictsWithEveryResource(t *testing.T) {
	result, err := Select(Input{
		Settings:   testSettings(2),
		Active:     []Lease{{IssueNumber: 99, Resources: []string{RepositoryResource}}},
		Candidates: []Candidate{candidate(1, "area:config", "[]"), candidate(2, "area:docs", "[]")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Selected) != 0 || !reflect.DeepEqual(skipReasons(result), map[int]string{1: ReasonResourceConflict, 2: ReasonResourceConflict}) {
		t.Fatalf("result=%+v", result)
	}
}

func TestMetadataValidationRejectsAmbiguousForms(t *testing.T) {
	bodies := []string{
		"",
		" <!-- agent-loop:metadata\nversion: 1\ndepends_on: []\n-->",
		"<!-- agent-loop:metadata\nversion: \"1\"\ndepends_on: []\n-->",
		"<!-- agent-loop:metadata\nversion: 1\ndepends_on: [\"2\"]\n-->",
		"<!-- agent-loop:metadata\nversion: 1\ndepends_on: [2, 2]\n-->",
		"<!-- agent-loop:metadata\nversion: 1\ndepends_on: []\nextra: true\n-->",
		"<!-- agent-loop:metadata\nversion: 1\nversion: 1\ndepends_on: []\n-->",
		"<!-- agent-loop:metadata\nversion: 1\ndepends_on: []",
		metadata("[]") + "\n" + metadata("[]"),
	}
	for index, body := range bodies {
		result, err := Select(Input{Settings: testSettings(1), Candidates: []Candidate{{Number: 1, Labels: []string{"area:config"}, Body: body}}})
		if err != nil {
			t.Fatalf("case %d: %v", index, err)
		}
		if len(result.Selected) != 1 || (result.Selected[0].FallbackReason != FallbackMetadataMissing && result.Selected[0].FallbackReason != FallbackMetadataInvalid) {
			t.Fatalf("case %d accepted ambiguously: %+v", index, result)
		}
	}
}

func TestMetadataAndClaimFallbackPriority(t *testing.T) {
	tests := []struct {
		name       string
		candidate  Candidate
		wantReason string
		wantDeps   []int
	}{
		{name: "missing metadata wins", candidate: Candidate{Number: 1, Labels: []string{"area:unknown"}}, wantReason: FallbackMetadataMissing, wantDeps: []int{}},
		{name: "invalid metadata wins", candidate: Candidate{Number: 1, Labels: []string{"area:config"}, Body: metadata("[1]")}, wantReason: FallbackMetadataInvalid, wantDeps: []int{}},
		{name: "missing claim", candidate: Candidate{Number: 1, Body: metadata("[2]")}, wantReason: FallbackResourceMissing, wantDeps: []int{2}},
		{name: "invalid claim", candidate: Candidate{Number: 1, Labels: []string{"area: config"}, Body: metadata("[2]")}, wantReason: FallbackResourceInvalid, wantDeps: []int{2}},
		{name: "unknown claim", candidate: Candidate{Number: 1, Labels: []string{"area:future"}, Body: metadata("[2]")}, wantReason: FallbackResourceUnknown, wantDeps: []int{2}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result, err := Select(Input{
				Settings: testSettings(1), Candidates: []Candidate{test.candidate},
				Dependencies: map[int]DependencyState{2: {Exists: true, Accessible: true, Closed: true}},
			})
			if err != nil {
				t.Fatal(err)
			}
			got := result.Selected
			if len(got) != 1 {
				t.Fatalf("result=%+v", result)
			}
			if got[0].FallbackReason != test.wantReason || !reflect.DeepEqual(got[0].Dependencies, test.wantDeps) || !reflect.DeepEqual(got[0].Resources, []string{RepositoryResource}) {
				t.Fatalf("evaluation=%+v", got[0])
			}
		})
	}
}

func TestSettingsValidation(t *testing.T) {
	tests := []Settings{
		{Concurrency: 0, MetadataVersion: 1, Definitions: []ResourceDefinition{{Name: "config", Paths: []string{"a"}}}},
		{Concurrency: 1, MetadataVersion: 2, Definitions: []ResourceDefinition{{Name: "config", Paths: []string{"a"}}}},
		{Concurrency: 1, MetadataVersion: 1},
		{Concurrency: 1, MetadataVersion: 1, Definitions: []ResourceDefinition{{Name: "repo", Paths: []string{"a"}}}},
		{Concurrency: 1, MetadataVersion: 1, Definitions: []ResourceDefinition{{Name: "Config", Paths: []string{"a"}}, {Name: "config", Paths: []string{"b"}}}},
		{Concurrency: 1, MetadataVersion: 1, Definitions: []ResourceDefinition{{Name: "config", Paths: []string{}}}},
		{Concurrency: 1, MetadataVersion: 1, Definitions: []ResourceDefinition{{Name: "config", Paths: []string{"/absolute"}}}},
		{Concurrency: 1, MetadataVersion: 1, Definitions: []ResourceDefinition{{Name: "config", Paths: []string{"a/**b"}}}},
	}
	for index, settings := range tests {
		if err := settings.Validate(); err == nil {
			t.Fatalf("case %d accepted: %+v", index, settings)
		}
	}
	if err := testSettings(4).Validate(); err != nil {
		t.Fatalf("valid settings rejected: %v", err)
	}
}

func TestLegacySelectorPreservesConcurrencyOneOrdering(t *testing.T) {
	result, err := Select(Input{
		Settings:   Settings{Concurrency: 1, MetadataVersion: 1, Legacy: true},
		Candidates: []Candidate{{Number: 9}, {Number: 2}, {Number: 5}},
		Ineligible: map[int]string{2: "completed"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := selectedNumbers(result); !reflect.DeepEqual(got, []int{5}) {
		t.Fatalf("selected=%v result=%+v", got, result)
	}
}
