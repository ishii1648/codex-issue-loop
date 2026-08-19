package app

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ishii1648/codex-issue-loop/internal/config"
	gh "github.com/ishii1648/codex-issue-loop/internal/github"
	"github.com/ishii1648/codex-issue-loop/internal/producer"
)

func (a App) prepareIssue(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("prepare-issue", flag.ContinueOnError)
	fs.SetOutput(a.Err)
	repo := fs.String("repo", "", "repository path")
	issueNumber := fs.Int("issue", 0, "Issue number")
	proposalPath := fs.String("proposal", "", "proposal JSON file, or - for stdin")
	audit := fs.Bool("audit", false, "audit already-persisted metadata")
	apply := fs.Bool("apply", false, "persist validated metadata and add the ready label")
	jsonOut := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return exitError{2, err}
	}
	if fs.NArg() != 0 {
		return exitError{2, fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))}
	}
	if *repo == "" || *issueNumber < 1 {
		return exitError{2, fmt.Errorf("--repo and a positive --issue are required")}
	}
	if *audit == (*proposalPath != "") {
		return exitError{2, fmt.Errorf("specify exactly one of --audit or --proposal")}
	}
	if *audit && *apply {
		return exitError{2, fmt.Errorf("--audit cannot be combined with --apply")}
	}
	cfg, err := config.Load(*repo)
	if err != nil {
		return exitError{2, err}
	}
	configBytes, err := os.ReadFile(filepath.Join(cfg.RepoPath, config.FileName))
	if err != nil {
		return err
	}
	client := gh.CLI{Secrets: cfg.RedactionValues()}
	issue, err := client.GetIssueMetadata(ctx, cfg, *issueNumber)
	if err != nil {
		return err
	}
	if *audit {
		return a.outputAudit(ctx, client, cfg, issue, configBytes, *jsonOut)
	}
	proposal, err := readProposal(a.In, *proposalPath)
	if err != nil {
		return exitError{2, err}
	}
	if proposal.IssueNumber != *issueNumber {
		return exitError{2, fmt.Errorf("proposal issue_number %d does not match --issue %d", proposal.IssueNumber, *issueNumber)}
	}
	proposalReport, err := producer.ValidateProposal(cfg, proposal)
	if err != nil {
		return err
	}
	if *apply {
		proposalReport.Mode = "apply"
	}
	if proposalReport.Valid && *apply {
		// The structurally valid apply attempt is now a metadata revision. Make
		// the Issue ineligible before remote dependency checks or body changes,
		// and leave it that way on every subsequent failure.
		ready := presentLabels(issue.Labels, cfg.GitHub.ReadyLabels)
		if err := client.RemoveIssueLabels(ctx, cfg, issue.Number, ready); err != nil {
			return err
		}
		issue.Labels = withoutLabels(issue.Labels, ready)
	}
	dependencySnapshots := readDependencySnapshots(ctx, client, cfg, proposalReport.Dependencies)
	for _, dependency := range dependencySnapshots {
		if dependency.State == "" {
			proposalReport.Valid = false
			proposalReport.Errors = append(proposalReport.Errors, fmt.Sprintf("dependency Issue #%d does not exist or is inaccessible", dependency.IssueNumber))
		}
	}
	if !proposalReport.Valid {
		proposalReport.Errors = uniqueSorted(proposalReport.Errors)
		if outputErr := a.output(*jsonOut, proposalReport); outputErr != nil {
			return outputErr
		}
		return exitError{1, fmt.Errorf("Issue metadata proposal validation failed")}
	}

	desiredBody, err := producer.MetadataBody(issue.Body, proposalReport.Dependencies)
	if err != nil {
		return err
	}
	desired := issue
	desired.Body = desiredBody
	desired.Labels = desiredLabels(issue.Labels, cfg, proposalReport.Resources, false)
	preview, err := producer.Audit(cfg, desired, dependencySnapshots, configBytes)
	if err != nil {
		return err
	}
	preview.Mode = "preview"
	if *apply {
		preview.Mode = "apply"
	}
	preview.FallbackReasons = uniqueSorted(append(preview.FallbackReasons, proposalReport.FallbackReasons...))
	if !sameIntSet(preview.Dependencies, proposalReport.Dependencies) || preview.Exclusive != proposalReport.Exclusive {
		preview.Valid = false
		preview.Errors = append(preview.Errors, "persisted metadata would not match the validated proposal")
	}
	if !preview.Valid || !*apply {
		preview.Errors = uniqueSorted(preview.Errors)
		if outputErr := a.output(*jsonOut, preview); outputErr != nil {
			return outputErr
		}
		if !preview.Valid {
			return exitError{1, fmt.Errorf("Issue metadata preview validation failed")}
		}
		return nil
	}

	// GitHub cannot update the body and labels transactionally. Remove ready
	// first, write metadata, re-read and validate, then add ready last.
	baseline, err := client.GetIssueMetadata(ctx, cfg, issue.Number)
	if err != nil {
		return err
	}
	if baseline.Body != issue.Body || !sameStringSet(baseline.Labels, issue.Labels) {
		return exitError{1, fmt.Errorf("Issue changed after intake validation; ready remains absent and a new preview is required")}
	}
	if err := client.SetIssueBody(ctx, cfg, issue.Number, desiredBody); err != nil {
		return err
	}
	areaLabels := labelsWithPrefix(issue.Labels, "area:")
	if err := client.RemoveIssueLabels(ctx, cfg, issue.Number, areaLabels); err != nil {
		return err
	}
	if err := client.AddIssueLabels(ctx, cfg, issue.Number, areaLabelsForResources(proposalReport.Resources)); err != nil {
		return err
	}
	persisted, err := client.GetIssueMetadata(ctx, cfg, issue.Number)
	if err != nil {
		return err
	}
	persistedDependencies := readDependencySnapshots(ctx, client, cfg, proposalReport.Dependencies)
	validated, err := producer.Audit(cfg, persisted, persistedDependencies, configBytes)
	if err != nil {
		return err
	}
	if !validated.Valid || validated.Ready || persisted.Body != desiredBody || !sameStringSet(persisted.Labels, desired.Labels) || validated.Exclusive != proposalReport.Exclusive ||
		!sameIntSet(validated.Dependencies, proposalReport.Dependencies) || !sameStringSet(validated.Resources, preview.Resources) {
		if ready := presentLabels(persisted.Labels, cfg.GitHub.ReadyLabels); len(ready) > 0 {
			if cleanupErr := client.RemoveIssueLabels(ctx, cfg, issue.Number, ready); cleanupErr == nil {
				if cleanedIssue, getErr := client.GetIssueMetadata(ctx, cfg, issue.Number); getErr == nil {
					if cleaned, auditErr := producer.Audit(cfg, cleanedIssue, persistedDependencies, configBytes); auditErr == nil {
						validated = cleaned
					}
				}
			}
		}
		validated.Mode = "apply"
		validated.Valid = false
		validated.Errors = uniqueSorted(append(validated.Errors, "persisted metadata did not match the validated proposal; ready was not added"))
		validated.FallbackReasons = uniqueSorted(append(validated.FallbackReasons, proposalReport.FallbackReasons...))
		if outputErr := a.output(*jsonOut, validated); outputErr != nil {
			return outputErr
		}
		return exitError{1, fmt.Errorf("persisted Issue metadata validation failed")}
	}
	if err := client.AddIssueLabels(ctx, cfg, issue.Number, cfg.GitHub.ReadyLabels); err != nil {
		return err
	}
	finalIssue, err := client.GetIssueMetadata(ctx, cfg, issue.Number)
	if err != nil {
		return err
	}
	finalDependencies := readDependencySnapshots(ctx, client, cfg, proposalReport.Dependencies)
	finalReport, err := producer.Audit(cfg, finalIssue, finalDependencies, configBytes)
	if err != nil {
		return err
	}
	finalReport.Mode = "apply"
	finalReport.Applied = true
	finalReport.FallbackReasons = uniqueSorted(append(finalReport.FallbackReasons, proposalReport.FallbackReasons...))
	expectedFinalLabels := desiredLabels(issue.Labels, cfg, proposalReport.Resources, true)
	if !finalReport.Valid || !finalReport.Ready || finalIssue.Body != desiredBody || !sameStringSet(finalIssue.Labels, expectedFinalLabels) || finalReport.Exclusive != proposalReport.Exclusive ||
		!sameIntSet(finalReport.Dependencies, proposalReport.Dependencies) || !sameStringSet(finalReport.Resources, preview.Resources) {
		// A concurrent edit between validation and the final read must not remain
		// queue-ready. Remove ready and re-read rather than reporting an assumed
		// cleanup state.
		failure := "final ready snapshot did not match the validated proposal"
		if cleanupErr := client.RemoveIssueLabels(ctx, cfg, issue.Number, presentLabels(finalIssue.Labels, cfg.GitHub.ReadyLabels)); cleanupErr != nil {
			failure += "; ready removal failed"
		} else if cleanedIssue, getErr := client.GetIssueMetadata(ctx, cfg, issue.Number); getErr != nil {
			failure += "; ready removal could not be verified"
		} else if cleaned, auditErr := producer.Audit(cfg, cleanedIssue, finalDependencies, configBytes); auditErr != nil {
			failure += "; cleaned snapshot could not be audited"
		} else {
			finalReport = cleaned
			finalReport.Mode = "apply"
			finalReport.Applied = true
			failure += "; ready was removed"
		}
		finalReport.FallbackReasons = uniqueSorted(append(finalReport.FallbackReasons, proposalReport.FallbackReasons...))
		finalReport.Valid = false
		finalReport.Errors = uniqueSorted(append(finalReport.Errors, failure))
		if outputErr := a.output(*jsonOut, finalReport); outputErr != nil {
			return outputErr
		}
		return exitError{1, fmt.Errorf("final Issue metadata validation failed")}
	}
	return a.output(*jsonOut, finalReport)
}

func (a App) outputAudit(ctx context.Context, client gh.CLI, cfg config.Config, issue gh.Issue, configBytes []byte, jsonOut bool) error {
	initial, err := producer.Audit(cfg, issue, nil, configBytes)
	if err != nil {
		return err
	}
	dependencies := readDependencySnapshots(ctx, client, cfg, initial.Dependencies)
	report, err := producer.Audit(cfg, issue, dependencies, configBytes)
	if err != nil {
		return err
	}
	if outputErr := a.output(jsonOut, report); outputErr != nil {
		return outputErr
	}
	if !report.Valid {
		return exitError{1, fmt.Errorf("persisted Issue metadata audit failed")}
	}
	return nil
}

func readProposal(stdin io.Reader, path string) (producer.Proposal, error) {
	var reader io.Reader
	if path == "-" {
		reader = stdin
	} else {
		file, err := os.Open(path)
		if err != nil {
			return producer.Proposal{}, fmt.Errorf("open proposal: %w", err)
		}
		defer file.Close()
		reader = file
	}
	data, err := io.ReadAll(io.LimitReader(reader, 1024*1024+1))
	if err != nil {
		return producer.Proposal{}, fmt.Errorf("read proposal JSON: %w", err)
	}
	if len(data) > 1024*1024 {
		return producer.Proposal{}, fmt.Errorf("proposal JSON exceeds 1 MiB")
	}
	if err := rejectDuplicateJSONKeys(data); err != nil {
		return producer.Proposal{}, fmt.Errorf("decode proposal JSON: %w", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return producer.Proposal{}, fmt.Errorf("decode proposal JSON: %w", err)
	}
	for _, field := range []string{"version", "issue_number", "resources", "dependencies", "exclusive", "exclusive_reason", "confidence", "ambiguity_reasons"} {
		if _, exists := fields[field]; !exists {
			return producer.Proposal{}, fmt.Errorf("proposal field %q is required", field)
		}
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	var proposal producer.Proposal
	if err := decoder.Decode(&proposal); err != nil {
		return producer.Proposal{}, fmt.Errorf("decode proposal JSON: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return producer.Proposal{}, fmt.Errorf("proposal JSON must contain exactly one object")
	}
	return proposal, nil
}

func rejectDuplicateJSONKeys(data []byte) error {
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	if err := walkJSONValue(decoder); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("multiple JSON values")
		}
		return err
	}
	return nil
}

func walkJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, composite := token.(json.Delim)
	if !composite {
		return nil
	}
	switch delimiter {
	case '{':
		seen := map[string]bool{}
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return fmt.Errorf("object key must be a string")
			}
			if seen[key] {
				return fmt.Errorf("duplicate object key %q", key)
			}
			seen[key] = true
			if err := walkJSONValue(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim('}') {
			return fmt.Errorf("unterminated JSON object")
		}
	case '[':
		for decoder.More() {
			if err := walkJSONValue(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim(']') {
			return fmt.Errorf("unterminated JSON array")
		}
	default:
		return fmt.Errorf("unexpected JSON delimiter %q", delimiter)
	}
	return nil
}

func readDependencySnapshots(ctx context.Context, client gh.CLI, cfg config.Config, numbers []int) []producer.DependencySnapshot {
	result := make([]producer.DependencySnapshot, 0, len(numbers))
	for _, number := range numbers {
		dependency, err := client.GetIssueMetadata(ctx, cfg, number)
		state := ""
		if err == nil {
			state = strings.ToLower(dependency.State)
		}
		result = append(result, producer.DependencySnapshot{IssueNumber: number, State: state})
	}
	return result
}

func desiredLabels(current []string, cfg config.Config, resources []string, ready bool) []string {
	result := []string{}
	for _, label := range current {
		if strings.HasPrefix(strings.ToLower(label), "area:") || containsString(cfg.GitHub.ReadyLabels, label) {
			continue
		}
		result = append(result, label)
	}
	result = append(result, areaLabelsForResources(resources)...)
	if ready {
		result = append(result, cfg.GitHub.ReadyLabels...)
	}
	return uniqueSorted(result)
}

func areaLabelsForResources(resources []string) []string {
	result := make([]string, 0, len(resources))
	for _, resource := range resources {
		result = append(result, "area:"+resource)
	}
	return result
}

func labelsWithPrefix(labels []string, prefix string) []string {
	result := []string{}
	for _, label := range labels {
		if strings.HasPrefix(strings.ToLower(label), strings.ToLower(prefix)) {
			result = append(result, label)
		}
	}
	return uniqueSorted(result)
}

func presentLabels(current, requested []string) []string {
	result := []string{}
	for _, label := range requested {
		if containsString(current, label) {
			result = append(result, label)
		}
	}
	return result
}

func withoutLabels(current, removed []string) []string {
	result := []string{}
	for _, label := range current {
		if !containsString(removed, label) {
			result = append(result, label)
		}
	}
	return result
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func uniqueSorted(values []string) []string {
	set := map[string]bool{}
	for _, value := range values {
		if value != "" {
			set[value] = true
		}
	}
	result := make([]string, 0, len(set))
	for value := range set {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func sameIntSet(left, right []int) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func sameStringSet(left, right []string) bool {
	left = uniqueSorted(left)
	right = uniqueSorted(right)
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
