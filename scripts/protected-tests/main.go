package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path"
	"regexp"
	"strings"
)

const definition = ".github/protected-tests.tsv"

type rule struct{ kind, path, reason string }
type change struct {
	Status  string `json:"status"`
	OldPath string `json:"old_path,omitempty"`
	NewPath string `json:"new_path,omitempty"`
	Reason  string `json:"reason"`
}
type report struct {
	Base                string   `json:"base"`
	Head                string   `json:"head"`
	MergeBase           string   `json:"merge_base"`
	DedicatedPRRequired bool     `json:"dedicated_pr_required"`
	Changes             []change `json:"changes"`
	Error               string   `json:"error,omitempty"`
}

func git(repo string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s failed: %w", args[0], err)
	}
	return out, nil
}

func readRules(data []byte) ([]rule, error) {
	var rules []rule
	seen := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		fields := strings.Split(line, "\t")
		if len(fields) != 3 {
			return nil, fmt.Errorf("invalid protection definition")
		}
		kind, p, reason := fields[0], fields[1], fields[2]
		clean := strings.TrimSuffix(p, "/")
		if (kind != "test" && kind != "control") || clean == "." || clean == ".." || path.Clean(clean) != clean || strings.HasPrefix(clean, "/") || strings.HasPrefix(clean, "../") || strings.ContainsAny(p, "\x00\r\n") || reason == "" || seen[p] {
			return nil, fmt.Errorf("invalid protection entry: %q", p)
		}
		seen[p] = true
		rules = append(rules, rule{kind, p, reason})
	}
	for _, p := range []string{definition, "scripts/protected-tests/", ".github/workflows/"} {
		found := false
		for _, r := range rules {
			if r.path == p && r.kind == "control" {
				found = true
			}
		}
		if !found {
			return nil, fmt.Errorf("missing control protection: %s", p)
		}
	}
	return rules, nil
}

func inspect(repo, base, head string) (r report) {
	r = report{Base: base, Head: head, Changes: []change{}}
	err := classify(repo, &r)
	if err != nil {
		r.Error = err.Error()
	}
	return r
}

func classify(repo string, r *report) error {
	sha := regexp.MustCompile(`^[0-9a-f]{40}$|^[0-9a-f]{64}$`)
	for _, ref := range []string{r.Base, r.Head} {
		if !sha.MatchString(ref) {
			return fmt.Errorf("full commit SHA required")
		}
		actual, err := git(repo, "rev-parse", "--verify", ref+"^{commit}")
		if err != nil {
			return err
		}
		if strings.TrimSpace(string(actual)) != ref {
			return fmt.Errorf("commit identity mismatch")
		}
	}
	checkedOut, err := git(repo, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(checkedOut)) != r.Base {
		return fmt.Errorf("trusted checkout does not match base")
	}
	data, err := git(repo, "show", r.Base+":"+definition)
	if err != nil {
		return err
	}
	rules, err := readRules(data)
	if err != nil {
		return err
	}
	merge, err := git(repo, "merge-base", "--all", r.Base, r.Head)
	if err != nil {
		return err
	}
	r.MergeBase = strings.TrimSpace(string(merge))
	if !sha.MatchString(r.MergeBase) {
		return fmt.Errorf("unique merge base required")
	}
	// Disabling rename detection makes every move include the original deletion.
	diff, err := git(repo, "diff", "--no-ext-diff", "--no-textconv", "--no-renames", "--name-status", "-z", r.MergeBase, r.Head, "--")
	if err != nil {
		return err
	}
	parts := bytes.Split(diff, []byte{0})
	if len(parts)%2 != 1 || len(parts[len(parts)-1]) != 0 {
		return fmt.Errorf("invalid NUL-delimited diff")
	}
	for i := 0; i < len(parts)-1; i += 2 {
		status, p := string(parts[i]), string(parts[i+1])
		if len(status) != 1 || !strings.Contains("AMDT", status) || p == "" {
			return fmt.Errorf("unsupported diff record")
		}
		for _, rule := range rules {
			if p != rule.path && !(strings.HasSuffix(rule.path, "/") && strings.HasPrefix(p, rule.path)) {
				continue
			}
			if status == "A" && rule.kind == "test" && strings.HasSuffix(p, "_test.go") {
				existing, err := git(repo, "ls-tree", "-z", r.Base, "--", ":(literal)"+p)
				if err != nil {
					return err
				}
				if len(existing) == 0 {
					continue
				}
			}
			c := change{Status: status, Reason: rule.reason}
			if status != "A" {
				c.OldPath = p
			}
			if status != "D" {
				c.NewPath = p
			}
			r.Changes = append(r.Changes, c)
			break
		}
	}
	return nil
}

func main() {
	r := inspect(".", os.Getenv("BASE_SHA"), os.Getenv("HEAD_SHA"))
	if err := json.NewEncoder(os.Stdout).Encode(r); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if r.Error != "" {
		os.Exit(1)
	}
}
