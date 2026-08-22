// Package admission implements the deterministic, side-effect-free part of
// Issue admission. GitHub and durable-state reads must be completed before an
// Input is assembled; Select never reads the clock, filesystem, or network.
package admission

import (
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ishii1648/codex-issue-loop/internal/domain/capability"
	"gopkg.in/yaml.v3"
)

const RepositoryResource = "repo:*"

const (
	ReasonAlreadyActive        = "already_active"
	ReasonIneligible           = "ineligible"
	ReasonDependencyCycle      = "dependency_cycle"
	ReasonDependencyIncomplete = "dependency_incomplete"
	ReasonResourceConflict     = "resource_conflict"
	ReasonCapabilityMismatch   = "capability_mismatch"
	ReasonNoCapacity           = "no_capacity"
)

const (
	FallbackMetadataMissing = "metadata_missing"
	FallbackMetadataInvalid = "metadata_invalid"
	FallbackResourceMissing = "resource_claim_missing"
	FallbackResourceInvalid = "resource_claim_invalid"
	FallbackResourceUnknown = "resource_claim_unknown"
)

type ResourceDefinition struct {
	Name  string
	Paths []string
}

type Settings struct {
	Concurrency        int
	MetadataVersion    int
	Definitions        []ResourceDefinition
	CapabilityProfiles map[string]capability.Provider

	// Legacy keeps schema-v2 queues on the same selector without activating
	// metadata semantics before the schema-v3 migration. Every candidate is an
	// exclusive repository claim and dependencies are ignored.
	Legacy bool
}

type Queue struct {
	Order          string
	PriorityLabels []string
}

type Candidate struct {
	Number    int
	CreatedAt time.Time
	Labels    []string
	Body      string
}

type Lease struct {
	IssueNumber int
	Resources   []string
}

type DependencyState struct {
	Exists                   bool
	Accessible               bool
	Closed                   bool
	LocalCompleted           bool
	PullRequestMergeRecorded bool
	KnownOpenOrUnmergedPR    bool
}

func (s DependencyState) Complete() bool {
	if s.LocalCompleted && s.PullRequestMergeRecorded {
		return true
	}
	return s.Exists && s.Accessible && s.Closed && !s.KnownOpenOrUnmergedPR
}

type Input struct {
	Settings   Settings
	Queue      Queue
	Candidates []Candidate
	Active     []Lease
	// OccupiedSlots is independent from Active: needs-input and open-PR leases
	// continue to conflict after their worker slot has been released.
	OccupiedSlots int
	Dependencies  map[int]DependencyState
	// Ineligible contains already-known local exclusions such as completed or
	// blocked Issues. Values are stable details, not reason codes.
	Ineligible map[int]string
}

type Evaluation struct {
	Candidate Candidate
	// DeclaredResources is the normalized area: label set observed at claim
	// time. Resources is the effective, safety-fallback claim used for leases.
	DeclaredResources []string
	Resources         []string
	Dependencies      []int
	FallbackReason    string
	Errors            []string
	Capability        capability.Evaluation
	metadataValid     bool
}

type Skip struct {
	Evaluation     Evaluation
	Reason         string
	Detail         string
	BlockingIssues []int
	Dependencies   []int
}

type Result struct {
	Selected []Evaluation
	Skipped  []Skip
}

func (s Settings) Validate() error {
	if s.Concurrency < 1 {
		return fmt.Errorf("admission concurrency must be at least 1")
	}
	if s.MetadataVersion != 1 {
		return fmt.Errorf("admission metadata_version must be 1")
	}
	if s.Legacy {
		return nil
	}
	if len(s.Definitions) == 0 {
		return fmt.Errorf("admission resource definitions must not be empty")
	}
	seenNames := map[string]bool{}
	for index, definition := range s.Definitions {
		name, ok := normalizeResourceName(strings.Trim(definition.Name, " \t"))
		if !ok || name == "repo" {
			return fmt.Errorf("admission resource definition %d has invalid name %q", index, definition.Name)
		}
		if seenNames[name] {
			return fmt.Errorf("admission resource definitions contain duplicate name %q", name)
		}
		seenNames[name] = true
		if len(definition.Paths) == 0 {
			return fmt.Errorf("admission resource %q paths must not be empty", name)
		}
		seenPaths := map[string]bool{}
		for _, path := range definition.Paths {
			if err := validateResourcePath(path); err != nil {
				return fmt.Errorf("admission resource %q path %q: %w", name, path, err)
			}
			if seenPaths[path] {
				return fmt.Errorf("admission resource %q contains duplicate path %q", name, path)
			}
			seenPaths[path] = true
		}
	}
	return nil
}

func validateResourcePath(path string) error {
	if path == "" || !utf8.ValidString(path) || strings.ContainsRune(path, 0) {
		return fmt.Errorf("must be non-empty UTF-8 without NUL")
	}
	if strings.Contains(path, `\`) || strings.HasPrefix(path, "/") || strings.HasSuffix(path, "/") {
		return fmt.Errorf("must be repository-relative and use '/' separators")
	}
	for _, segment := range strings.Split(path, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return fmt.Errorf("contains an invalid path segment")
		}
		if strings.ContainsAny(segment, "[]{}!") {
			return fmt.Errorf("contains an unsupported glob token")
		}
		if strings.Contains(segment, "**") && segment != "**" {
			return fmt.Errorf("'**' must occupy an entire segment")
		}
	}
	return nil
}

// Select returns candidates and skip reasons in queue order. It does not
// mutate Input slices, including the candidate order supplied by the caller.
func Select(input Input) (Result, error) {
	if err := input.Settings.Validate(); err != nil {
		return Result{}, err
	}
	if input.OccupiedSlots < 0 {
		return Result{}, fmt.Errorf("occupied admission slots must not be negative")
	}
	if err := validateQueue(input.Queue); err != nil {
		return Result{}, err
	}
	known := make(map[string]bool, len(input.Settings.Definitions))
	for _, definition := range input.Settings.Definitions {
		name, _ := normalizeResourceName(strings.Trim(definition.Name, " \t"))
		known[name] = true
	}
	evaluations := make([]Evaluation, 0, len(input.Candidates))
	seenCandidates := map[int]bool{}
	for _, candidate := range input.Candidates {
		if candidate.Number < 1 {
			return Result{}, fmt.Errorf("candidate Issue number must be at least 1")
		}
		if seenCandidates[candidate.Number] {
			return Result{}, fmt.Errorf("duplicate candidate Issue #%d", candidate.Number)
		}
		seenCandidates[candidate.Number] = true
		evaluations = append(evaluations, evaluate(candidate, input.Settings, known))
	}
	orderEvaluations(evaluations, input.Queue)

	active, activeNumbers, err := normalizeLeases(input.Active)
	if err != nil {
		return Result{}, err
	}
	cycle := dependencyCycles(evaluations)
	capacity := input.Settings.Concurrency - input.OccupiedSlots
	if capacity < 0 {
		capacity = 0
	}
	result := Result{Selected: []Evaluation{}, Skipped: []Skip{}}
	selectedLeases := []Lease{}
	for _, evaluation := range evaluations {
		number := evaluation.Candidate.Number
		if activeNumbers[number] {
			result.Skipped = append(result.Skipped, Skip{Evaluation: evaluation, Reason: ReasonAlreadyActive})
			continue
		}
		if detail, excluded := input.Ineligible[number]; excluded {
			result.Skipped = append(result.Skipped, Skip{Evaluation: evaluation, Reason: ReasonIneligible, Detail: detail})
			continue
		}
		if !evaluation.Capability.Compatible {
			result.Skipped = append(result.Skipped, Skip{Evaluation: evaluation, Reason: ReasonCapabilityMismatch, Detail: capabilityDetail(evaluation.Capability)})
			continue
		}
		if cycle[number] {
			result.Skipped = append(result.Skipped, Skip{Evaluation: evaluation, Reason: ReasonDependencyCycle})
			continue
		}
		incomplete := incompleteDependencies(evaluation.Dependencies, input.Dependencies)
		if len(incomplete) > 0 {
			result.Skipped = append(result.Skipped, Skip{Evaluation: evaluation, Reason: ReasonDependencyIncomplete, Dependencies: incomplete})
			continue
		}
		if len(result.Selected) >= capacity {
			result.Skipped = append(result.Skipped, Skip{Evaluation: evaluation, Reason: ReasonNoCapacity})
			continue
		}
		blockers := conflictingIssues(evaluation.Resources, active, selectedLeases)
		if len(blockers) > 0 {
			result.Skipped = append(result.Skipped, Skip{Evaluation: evaluation, Reason: ReasonResourceConflict, BlockingIssues: blockers})
			continue
		}
		result.Selected = append(result.Selected, evaluation)
		selectedLeases = append(selectedLeases, Lease{IssueNumber: number, Resources: append([]string(nil), evaluation.Resources...)})
	}
	return result, nil
}

// EvaluateCandidate applies the same deterministic normalization used by
// Select without considering capacity, dependencies, or active leases.
func EvaluateCandidate(settings Settings, candidate Candidate) (Evaluation, error) {
	if err := settings.Validate(); err != nil {
		return Evaluation{}, err
	}
	known := make(map[string]bool, len(settings.Definitions))
	for _, definition := range settings.Definitions {
		name, _ := normalizeResourceName(strings.Trim(definition.Name, " \t"))
		known[name] = true
	}
	return evaluate(candidate, settings, known), nil
}

func evaluate(candidate Candidate, settings Settings, known map[string]bool) Evaluation {
	candidate.Labels = normalizedSet(append([]string(nil), candidate.Labels...))
	result := Evaluation{Candidate: candidate, DeclaredResources: []string{}, Resources: []string{RepositoryResource}, Dependencies: []int{}, Capability: capability.Evaluate(candidate.Body, settings.CapabilityProfiles)}
	if settings.Legacy {
		result.DeclaredResources = []string{RepositoryResource}
		return result
	}
	metadata, metadataReason, metadataErrors := parseMetadata(candidate.Number, candidate.Body, settings.MetadataVersion)
	claims, claimReason, claimErrors := parseClaims(candidate.Labels, known)
	result.DeclaredResources = append([]string(nil), claims...)
	result.Errors = append(result.Errors, metadataErrors...)
	result.Errors = append(result.Errors, claimErrors...)
	sort.Strings(result.Errors)
	if metadataReason != "" {
		result.FallbackReason = metadataReason
		return result
	}
	result.metadataValid = true
	result.Dependencies = metadata
	if claimReason != "" {
		result.FallbackReason = claimReason
		return result
	}
	result.Resources = claims
	return result
}

func capabilityDetail(evaluation capability.Evaluation) string {
	codes := make([]string, 0, len(evaluation.Mismatches))
	for _, mismatch := range evaluation.Mismatches {
		codes = append(codes, mismatch.Code)
	}
	sort.Strings(codes)
	return strings.Join(codes, ",")
}

func normalizeLeases(leases []Lease) ([]Lease, map[int]bool, error) {
	result := make([]Lease, 0, len(leases))
	numbers := map[int]bool{}
	for _, lease := range leases {
		if lease.IssueNumber < 1 || numbers[lease.IssueNumber] {
			return nil, nil, fmt.Errorf("active leases require unique positive Issue numbers")
		}
		numbers[lease.IssueNumber] = true
		resources := normalizedSet(lease.Resources)
		if len(resources) == 0 {
			resources = []string{RepositoryResource}
		}
		result = append(result, Lease{IssueNumber: lease.IssueNumber, Resources: resources})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].IssueNumber < result[j].IssueNumber })
	return result, numbers, nil
}

func incompleteDependencies(dependencies []int, states map[int]DependencyState) []int {
	result := []int{}
	for _, number := range dependencies {
		if !states[number].Complete() {
			result = append(result, number)
		}
	}
	return result
}

func dependencyCycles(evaluations []Evaluation) map[int]bool {
	graph := map[int][]int{}
	for _, evaluation := range evaluations {
		if evaluation.metadataValid {
			graph[evaluation.Candidate.Number] = append([]int(nil), evaluation.Dependencies...)
		}
	}
	result := map[int]bool{}
	for root := range graph {
		seen := map[int]bool{}
		var reachesRoot func(int) bool
		reachesRoot = func(current int) bool {
			for _, next := range graph[current] {
				if next == root {
					return true
				}
				if _, exists := graph[next]; !exists || seen[next] {
					continue
				}
				seen[next] = true
				if reachesRoot(next) {
					return true
				}
			}
			return false
		}
		seen[root] = true
		result[root] = reachesRoot(root)
	}
	return result
}

func conflictingIssues(resources []string, groups ...[]Lease) []int {
	set := map[int]bool{}
	for _, leases := range groups {
		for _, lease := range leases {
			if conflicts(resources, lease.Resources) {
				set[lease.IssueNumber] = true
			}
		}
	}
	result := make([]int, 0, len(set))
	for number := range set {
		result = append(result, number)
	}
	sort.Ints(result)
	return result
}

func conflicts(left, right []string) bool {
	leftSet := map[string]bool{}
	for _, resource := range left {
		if resource == RepositoryResource {
			return true
		}
		leftSet[resource] = true
	}
	for _, resource := range right {
		if resource == RepositoryResource || leftSet[resource] {
			return true
		}
	}
	return false
}

func normalizedSet(values []string) []string {
	set := map[string]bool{}
	for _, value := range values {
		set[value] = true
	}
	result := make([]string, 0, len(set))
	for value := range set {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func validateQueue(queue Queue) error {
	switch queue.Order {
	case "", "issue_number_asc", "created_at_asc":
	case "priority_then_created_at":
		if len(queue.PriorityLabels) == 0 {
			return fmt.Errorf("admission priority labels must not be empty for priority ordering")
		}
	default:
		return fmt.Errorf("unsupported admission queue order %q", queue.Order)
	}
	seen := map[string]bool{}
	for _, label := range queue.PriorityLabels {
		canonical := strings.ToLower(label)
		if label == "" || strings.TrimSpace(label) != label || seen[canonical] {
			return fmt.Errorf("invalid or duplicate admission priority label %q", label)
		}
		seen[canonical] = true
	}
	return nil
}

func orderEvaluations(values []Evaluation, queue Queue) {
	ranks := map[string]int{}
	for index, label := range queue.PriorityLabels {
		ranks[strings.ToLower(label)] = index
	}
	rank := func(candidate Candidate) int {
		result := len(ranks)
		for _, label := range candidate.Labels {
			if value, exists := ranks[strings.ToLower(label)]; exists && value < result {
				result = value
			}
		}
		return result
	}
	createdBefore := func(left, right Candidate) bool {
		if left.CreatedAt.IsZero() != right.CreatedAt.IsZero() {
			return !left.CreatedAt.IsZero()
		}
		if !left.CreatedAt.Equal(right.CreatedAt) {
			return left.CreatedAt.Before(right.CreatedAt)
		}
		return left.Number < right.Number
	}
	sort.Slice(values, func(i, j int) bool {
		left, right := values[i].Candidate, values[j].Candidate
		switch queue.Order {
		case "created_at_asc":
			return createdBefore(left, right)
		case "priority_then_created_at":
			leftRank, rightRank := rank(left), rank(right)
			if leftRank != rightRank {
				return leftRank < rightRank
			}
			return createdBefore(left, right)
		default:
			return left.Number < right.Number
		}
	})
}

func parseMetadata(issueNumber int, body string, supportedVersion int) ([]int, string, []string) {
	body = strings.ReplaceAll(body, "\r\n", "\n")
	lines := strings.Split(body, "\n")
	starts := []int{}
	for index, line := range lines {
		if line == "<!-- agent-loop:metadata" {
			starts = append(starts, index)
		}
	}
	if len(starts) == 0 {
		return nil, FallbackMetadataMissing, []string{"metadata block is missing"}
	}
	if len(starts) != 1 {
		return nil, FallbackMetadataInvalid, []string{"multiple metadata blocks"}
	}
	end := -1
	for index := starts[0] + 1; index < len(lines); index++ {
		if lines[index] == "-->" {
			end = index
			break
		}
	}
	if end < 0 {
		return nil, FallbackMetadataInvalid, []string{"metadata block is not terminated"}
	}
	var document yaml.Node
	if err := yaml.Unmarshal([]byte(strings.Join(lines[starts[0]+1:end], "\n")), &document); err != nil {
		return nil, FallbackMetadataInvalid, []string{"metadata YAML: " + err.Error()}
	}
	if len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return nil, FallbackMetadataInvalid, []string{"metadata must be a mapping"}
	}
	mapping := document.Content[0]
	values := map[string]*yaml.Node{}
	errors := []string{}
	for index := 0; index+1 < len(mapping.Content); index += 2 {
		key, value := mapping.Content[index], mapping.Content[index+1]
		if key.Kind != yaml.ScalarNode || key.Tag != "!!str" {
			errors = append(errors, "metadata key must be a string")
			continue
		}
		if key.Value != "version" && key.Value != "depends_on" {
			errors = append(errors, "unknown metadata key "+key.Value)
			continue
		}
		if values[key.Value] != nil {
			errors = append(errors, "duplicate metadata key "+key.Value)
			continue
		}
		values[key.Value] = value
	}
	version := values["version"]
	dependencies := values["depends_on"]
	if version == nil {
		errors = append(errors, "metadata version is required")
	} else if version.Kind != yaml.ScalarNode || version.Tag != "!!int" || version.Style != 0 || version.Value != fmt.Sprint(supportedVersion) {
		errors = append(errors, fmt.Sprintf("metadata version must be unquoted integer %d", supportedVersion))
	}
	result := []int{}
	seen := map[int]bool{}
	if dependencies == nil {
		errors = append(errors, "metadata depends_on is required")
	} else if dependencies.Kind != yaml.SequenceNode || dependencies.Tag != "!!seq" {
		errors = append(errors, "metadata depends_on must be an integer array")
	} else {
		for _, node := range dependencies.Content {
			var number int
			if node.Kind != yaml.ScalarNode || node.Tag != "!!int" || node.Style != 0 || node.Decode(&number) != nil || number < 1 {
				errors = append(errors, "metadata dependency must be a positive unquoted integer")
				continue
			}
			if number == issueNumber {
				errors = append(errors, fmt.Sprintf("metadata dependency #%d refers to itself", number))
				continue
			}
			if seen[number] {
				errors = append(errors, fmt.Sprintf("duplicate metadata dependency #%d", number))
				continue
			}
			seen[number] = true
			result = append(result, number)
		}
	}
	if containsAliasOrCustomTag(mapping) {
		errors = append(errors, "metadata aliases and custom tags are not allowed")
	}
	sort.Ints(result)
	sort.Strings(errors)
	if len(errors) > 0 {
		return nil, FallbackMetadataInvalid, errors
	}
	return result, "", nil
}

func containsAliasOrCustomTag(node *yaml.Node) bool {
	if node.Kind == yaml.AliasNode || strings.HasPrefix(node.Tag, "!") && !strings.HasPrefix(node.Tag, "!!") {
		return true
	}
	for _, child := range node.Content {
		if containsAliasOrCustomTag(child) {
			return true
		}
	}
	return false
}

func parseClaims(labels []string, known map[string]bool) ([]string, string, []string) {
	claims := []string{}
	invalid := []string{}
	unknown := []string{}
	for _, original := range labels {
		label := strings.Trim(original, " \t")
		if len(label) < len("area:") || !strings.EqualFold(label[:len("area:")], "area:") {
			continue
		}
		name, ok := normalizeResourceName(label[len("area:"):])
		if !ok {
			invalid = append(invalid, original)
			continue
		}
		claims = append(claims, name)
		if !known[name] {
			unknown = append(unknown, name)
		}
	}
	claims = normalizedSet(claims)
	sort.Strings(invalid)
	unknown = normalizedSet(unknown)
	if len(claims) == 0 && len(invalid) == 0 {
		return nil, FallbackResourceMissing, []string{"resource claim is missing"}
	}
	if len(invalid) > 0 {
		return claims, FallbackResourceInvalid, prefixed("invalid resource label ", invalid)
	}
	if len(unknown) > 0 {
		return claims, FallbackResourceUnknown, prefixed("unknown resource ", unknown)
	}
	return claims, "", nil
}

// ResourcesForPaths maps changed repository-relative paths to every matching
// resource. One path may intentionally require multiple resources. Any path
// that cannot be represented safely or matches no definition falls back to
// repo:*, which conflicts with every claim.
func ResourcesForPaths(settings Settings, paths []string) ([]string, error) {
	if err := settings.Validate(); err != nil {
		return nil, err
	}
	if len(paths) == 0 {
		return []string{}, nil
	}
	if settings.Legacy {
		return []string{RepositoryResource}, nil
	}
	resources := map[string]bool{}
	for _, path := range paths {
		if validateChangedPath(path) != nil {
			return []string{RepositoryResource}, nil
		}
		matched := false
		for _, definition := range settings.Definitions {
			for _, pattern := range definition.Paths {
				if matchPathPattern(pattern, path) {
					name, _ := normalizeResourceName(strings.Trim(definition.Name, " \t"))
					resources[name] = true
					matched = true
					break
				}
			}
		}
		if !matched {
			return []string{RepositoryResource}, nil
		}
	}
	result := make([]string, 0, len(resources))
	for resource := range resources {
		result = append(result, resource)
	}
	sort.Strings(result)
	return result, nil
}

// Covers reports whether every actual resource was present in the effective
// declaration. repo:* is only covered by an explicit repository-wide claim.
func Covers(declared, actual []string) bool {
	set := map[string]bool{}
	for _, resource := range declared {
		set[resource] = true
	}
	for _, resource := range actual {
		if !set[resource] {
			return false
		}
	}
	return true
}

func validateChangedPath(path string) error {
	if err := validateResourcePath(path); err != nil {
		return err
	}
	if strings.ContainsAny(path, "*?") {
		return fmt.Errorf("changed path contains glob tokens")
	}
	return nil
}

func matchPathPattern(pattern, path string) bool {
	patterns := strings.Split(pattern, "/")
	segments := strings.Split(path, "/")
	type position struct{ pattern, path int }
	memo := map[position]bool{}
	seen := map[position]bool{}
	var match func(int, int) bool
	match = func(patternIndex, pathIndex int) bool {
		key := position{patternIndex, pathIndex}
		if seen[key] {
			return memo[key]
		}
		seen[key] = true
		if patternIndex == len(patterns) {
			memo[key] = pathIndex == len(segments)
			return memo[key]
		}
		if patterns[patternIndex] == "**" {
			memo[key] = match(patternIndex+1, pathIndex) || pathIndex < len(segments) && match(patternIndex, pathIndex+1)
			return memo[key]
		}
		memo[key] = pathIndex < len(segments) && matchSegment(patterns[patternIndex], segments[pathIndex]) && match(patternIndex+1, pathIndex+1)
		return memo[key]
	}
	return match(0, 0)
}

func matchSegment(pattern, value string) bool {
	patterns, values := []rune(pattern), []rune(value)
	type position struct{ pattern, value int }
	memo := map[position]bool{}
	seen := map[position]bool{}
	var match func(int, int) bool
	match = func(patternIndex, valueIndex int) bool {
		key := position{patternIndex, valueIndex}
		if seen[key] {
			return memo[key]
		}
		seen[key] = true
		if patternIndex == len(patterns) {
			memo[key] = valueIndex == len(values)
			return memo[key]
		}
		switch patterns[patternIndex] {
		case '*':
			memo[key] = match(patternIndex+1, valueIndex) || valueIndex < len(values) && match(patternIndex, valueIndex+1)
		case '?':
			memo[key] = valueIndex < len(values) && match(patternIndex+1, valueIndex+1)
		default:
			memo[key] = valueIndex < len(values) && patterns[patternIndex] == values[valueIndex] && match(patternIndex+1, valueIndex+1)
		}
		return memo[key]
	}
	return match(0, 0)
}

func prefixed(prefix string, values []string) []string {
	result := make([]string, len(values))
	for index, value := range values {
		result[index] = prefix + value
	}
	return result
}

func normalizeResourceName(value string) (string, bool) {
	if len(value) < 1 || len(value) > 63 {
		return "", false
	}
	bytes := []byte(value)
	for index, char := range bytes {
		if char >= 'A' && char <= 'Z' {
			bytes[index] = char + ('a' - 'A')
			char = bytes[index]
		}
		if index == 0 {
			if char < 'a' || char > 'z' {
				if char < '0' || char > '9' {
					return "", false
				}
			}
			continue
		}
		if (char < 'a' || char > 'z') && (char < '0' || char > '9') && char != '-' {
			return "", false
		}
	}
	return string(bytes), true
}
