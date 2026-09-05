package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type issue struct {
	Number    int      `json:"number"`
	Title     string   `json:"title"`
	Body      string   `json:"body"`
	URL       string   `json:"url"`
	CreatedAt string   `json:"createdAt"`
	State     string   `json:"state"`
	Labels    []string `json:"labels"`
	Comments  []string `json:"comments"`
}

type pullRequest struct {
	Number           int     `json:"number"`
	URL              string  `json:"url"`
	State            string  `json:"state"`
	IsDraft          bool    `json:"isDraft"`
	MergedAt         *string `json:"mergedAt"`
	HeadRefName      string  `json:"headRefName"`
	BaseRefName      string  `json:"baseRefName"`
	BaseRefOID       string  `json:"baseRefOid"`
	HeadRefOID       string  `json:"headRefOid"`
	MergeCommitOID   string  `json:"mergeCommitOid,omitempty"`
	MergeStateStatus string  `json:"mergeStateStatus"`
}

type contractState struct {
	SchemaVersion int                     `json:"schema_version"`
	Remote        string                  `json:"remote"`
	Issues        map[string]*issue       `json:"issues"`
	PullRequests  map[string]*pullRequest `json:"pull_requests"`
	Calls         []string                `json:"calls"`
}

type label struct {
	Name        string `json:"name"`
	Color       string `json:"color,omitempty"`
	Description string `json:"description,omitempty"`
}

var issueNumberPattern = regexp.MustCompile(`issue-([0-9]+)(?:-|$)`)

func main() {
	var err error
	switch filepath.Base(os.Args[0]) {
	case "gh":
		err = runGH(os.Args[1:])
	case "codex":
		err = runCodex(os.Args[1:])
	default:
		err = runHarness(os.Args[1:])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func runHarness(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: offline-stub seed|summary")
	}
	switch args[0] {
	case "seed":
		body := func(dependencies string) string {
			return "Offline release contract.\n<!-- agent-loop:metadata\nversion: 1\ndepends_on: " + dependencies + "\n-->"
		}
		state := contractState{
			SchemaVersion: 1,
			Remote:        requiredEnvironment("OFFLINE_CONTRACT_REMOTE"),
			Issues: map[string]*issue{
				"1": {Number: 1, Title: "Offline completion", Body: body("[]"), URL: "https://offline.invalid/offline/repository/issues/1", CreatedAt: "2026-01-01T00:00:00Z", State: "OPEN", Labels: []string{"codex-loop:ready", "area:contract"}, Comments: []string{}},
				"2": {Number: 2, Title: "Offline answer resume", Body: body("[1]"), URL: "https://offline.invalid/offline/repository/issues/2", CreatedAt: "2026-01-01T00:01:00Z", State: "OPEN", Labels: []string{"codex-loop:ready", "area:contract"}, Comments: []string{}},
			},
			PullRequests: map[string]*pullRequest{},
			Calls:        []string{},
		}
		return saveState(state)
	case "summary":
		state, unlock, err := lockedState(false)
		if err != nil {
			return err
		}
		defer unlock()
		return json.NewEncoder(os.Stdout).Encode(state)
	default:
		return fmt.Errorf("unknown offline-stub command %q", args[0])
	}
}

func runGH(args []string) error {
	if len(args) == 1 && args[0] == "--version" {
		fmt.Println("gh version 2.69.0 (offline contract)")
		return nil
	}
	if contains(args, "--help") {
		fmt.Println("--json --limit --label --assignee --milestone --add-label --remove-label --body --repo --state --head --base --title --draft --squash --color --description")
		return nil
	}
	state, unlock, err := lockedState(true)
	if err != nil {
		return err
	}
	defer unlock()
	state.Calls = append(state.Calls, "gh "+strings.Join(args, " "))
	defer func() { _ = saveStateUnlocked(state) }()
	if len(args) < 2 {
		return errors.New("offline gh requires a command")
	}
	switch args[0] + " " + args[1] {
	case "issue list":
		items := make([]*issue, 0, len(state.Issues))
		wantedLabel := option(args, "--label")
		for _, item := range state.Issues {
			if item.State == "OPEN" && (wantedLabel == "" || contains(item.Labels, wantedLabel)) {
				items = append(items, item)
			}
		}
		sort.Slice(items, func(i, j int) bool { return items[i].CreatedAt < items[j].CreatedAt })
		return writeIssues(items)
	case "issue view":
		item, err := findIssue(state, args[2])
		if err != nil {
			return err
		}
		if option(args, "--jq") == ".comments[].body" {
			for _, comment := range item.Comments {
				fmt.Println(comment)
			}
			return nil
		}
		return writeIssue(item)
	case "issue edit":
		item, err := findIssue(state, args[2])
		if err != nil {
			return err
		}
		for index := 3; index < len(args); index++ {
			switch args[index] {
			case "--add-label":
				index++
				if !contains(item.Labels, args[index]) {
					item.Labels = append(item.Labels, args[index])
				}
			case "--remove-label":
				index++
				item.Labels = remove(item.Labels, args[index])
			}
		}
		return nil
	case "issue comment":
		item, err := findIssue(state, args[2])
		if err != nil {
			return err
		}
		item.Comments = append(item.Comments, option(args, "--body"))
		return nil
	case "issue close":
		item, err := findIssue(state, args[2])
		if err != nil {
			return err
		}
		item.State = "CLOSED"
		return nil
	case "pr list":
		return listPullRequests(state, args)
	case "pr create":
		return createPullRequest(state, args)
	case "pr ready":
		pr, err := findPullRequestByURL(state, args[2])
		if err != nil {
			return err
		}
		pr.IsDraft = false
		return nil
	case "pr update-branch":
		return nil
	case "pr merge":
		pr, err := findPullRequestByURL(state, args[2])
		if err != nil {
			return err
		}
		mergeCommit, err := squashMerge(state.Remote, pr.BaseRefName, pr.HeadRefOID, pr.Number)
		if err != nil {
			return err
		}
		now := time.Now().UTC().Format(time.RFC3339)
		pr.State, pr.MergedAt, pr.MergeCommitOID = "MERGED", &now, mergeCommit
		return nil
	case "label list":
		return json.NewEncoder(os.Stdout).Encode([]label{{Name: "codex-loop:ready"}, {Name: "area:contract"}})
	case "label create":
		return nil
	case "api /rate_limit":
		fmt.Println(`{"resources":{"graphql":{"remaining":1000,"reset":4102444800}}}`)
		return nil
	default:
		return fmt.Errorf("unsupported offline gh call: %s", strings.Join(args, " "))
	}
}

func runCodex(args []string) error {
	if len(args) == 1 && args[0] == "--version" {
		fmt.Println("codex-cli 0.136.0")
		return nil
	}
	if len(args) >= 2 && args[0] == "features" && args[1] == "list" {
		fmt.Println("network_proxy apps browser_use computer_use plugins remote_plugin skill_search tool_suggest")
		return nil
	}
	if contains(args, "--help") {
		fmt.Println("--json --output-schema --output-last-message --sandbox --cd --ignore-user-config --strict-config --disable")
		return nil
	}
	prompt, err := io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
	if err != nil {
		return err
	}
	workspace := option(args, "--cd")
	resultPath := option(args, "--output-last-message")
	if workspace == "" || resultPath == "" {
		return fmt.Errorf("offline codex missing workspace or result path: %s", strings.Join(args, " "))
	}
	number := issueNumber(workspace, string(prompt))
	resumed := contains(args, "resume")
	if number == 0 {
		return errors.New("offline codex could not identify Issue")
	}
	if err := appendCodexCall(number, resumed, args); err != nil {
		return err
	}
	fmt.Printf("{\"type\":\"thread.started\",\"thread_id\":\"offline-session-%d\"}\n", number)
	if number == 2 && !resumed {
		return writeJSON(resultPath, map[string]any{
			"version": 1, "status": "needs_input", "execution_profile": "standard", "summary": "input required",
			"question": map[string]any{"text": "Continue the offline contract?", "reason": "exercise durable answer and resume", "recommended_option": "yes", "options": []map[string]string{{"id": "yes", "label": "Continue"}}, "allow_free_text": true},
			"tests":    []any{}, "git": nil, "retry": nil,
		})
	}
	path := filepath.Join(workspace, "offline-contract", fmt.Sprintf("sequence-%d.txt", number))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	content := "completed\n"
	if resumed {
		content = "resumed\n"
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return err
	}
	return writeJSON(resultPath, map[string]any{
		"version": 1, "status": "completed", "execution_profile": "standard", "summary": "offline contract completed",
		"question": nil, "tests": []map[string]string{{"command": "offline-contract", "result": "passed"}}, "git": nil, "retry": nil,
	})
}

func createPullRequest(state contractState, args []string) error {
	branch, base := option(args, "--head"), option(args, "--base")
	if branch == "" || base == "" {
		return errors.New("offline pull request is missing refs")
	}
	for _, existing := range state.PullRequests {
		if existing.HeadRefName == branch && existing.State == "OPEN" {
			fmt.Println(existing.URL)
			return nil
		}
	}
	head, err := gitOutput(state.Remote, "rev-parse", "refs/heads/"+branch)
	if err != nil {
		return err
	}
	baseOID, err := gitOutput(state.Remote, "rev-parse", "refs/heads/"+base)
	if err != nil {
		return err
	}
	number := len(state.PullRequests) + 1
	url := fmt.Sprintf("https://offline.invalid/offline/repository/pull/%d", number)
	state.PullRequests[strconv.Itoa(number)] = &pullRequest{
		Number: number, URL: url, State: "OPEN", IsDraft: contains(args, "--draft"), HeadRefName: branch,
		BaseRefName: base, BaseRefOID: baseOID, HeadRefOID: head, MergeStateStatus: "CLEAN",
	}
	fmt.Println(url)
	return nil
}

func listPullRequests(state contractState, args []string) error {
	head, wantedState := option(args, "--head"), strings.ToUpper(option(args, "--state"))
	items := []*pullRequest{}
	for _, pr := range state.PullRequests {
		if head != "" && pr.HeadRefName != head {
			continue
		}
		if wantedState != "" && wantedState != "ALL" && pr.State != wantedState {
			continue
		}
		items = append(items, pr)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Number > items[j].Number })
	encoded := make([]map[string]any, 0, len(items))
	for _, pr := range items {
		encoded = append(encoded, map[string]any{
			"number": pr.Number, "url": pr.URL, "state": pr.State, "isDraft": pr.IsDraft, "mergedAt": pr.MergedAt,
			"headRefName": pr.HeadRefName, "baseRefName": pr.BaseRefName, "baseRefOid": pr.BaseRefOID, "headRefOid": pr.HeadRefOID,
			"mergeCommit": map[string]any{"oid": pr.MergeCommitOID}, "headRepository": map[string]any{"name": "repository"},
			"headRepositoryOwner": map[string]any{"login": "offline"}, "mergeStateStatus": pr.MergeStateStatus,
			"statusCheckRollup": []map[string]string{{"status": "COMPLETED", "conclusion": "SUCCESS"}},
		})
	}
	return json.NewEncoder(os.Stdout).Encode(encoded)
}

func writeIssue(item *issue) error {
	value := map[string]any{
		"number": item.Number, "title": item.Title, "body": item.Body, "url": item.URL, "createdAt": item.CreatedAt,
		"state": item.State, "labels": labels(item.Labels), "assignees": []any{}, "milestone": nil, "comments": comments(item.Comments),
		"author": map[string]any{"login": "offline", "is_bot": false},
	}
	return json.NewEncoder(os.Stdout).Encode(value)
}

func writeIssues(items []*issue) error {
	values := make([]map[string]any, 0, len(items))
	for _, item := range items {
		values = append(values, map[string]any{
			"number": item.Number, "title": item.Title, "body": item.Body, "url": item.URL, "createdAt": item.CreatedAt,
			"state": item.State, "labels": labels(item.Labels), "assignees": []any{}, "milestone": nil,
			"author": map[string]any{"login": "offline", "is_bot": false},
		})
	}
	return json.NewEncoder(os.Stdout).Encode(values)
}

func labels(values []string) []map[string]string {
	result := make([]map[string]string, 0, len(values))
	for _, value := range values {
		result = append(result, map[string]string{"name": value})
	}
	return result
}

func comments(values []string) []map[string]string {
	result := make([]map[string]string, 0, len(values))
	for _, value := range values {
		result = append(result, map[string]string{"body": value})
	}
	return result
}

func findIssue(state contractState, value string) (*issue, error) {
	item := state.Issues[value]
	if item == nil {
		return nil, fmt.Errorf("offline Issue #%s not found", value)
	}
	return item, nil
}

func findPullRequestByURL(state contractState, url string) (*pullRequest, error) {
	for _, pr := range state.PullRequests {
		if pr.URL == url {
			return pr, nil
		}
	}
	return nil, fmt.Errorf("offline Pull Request not found: %s", url)
}

func appendCodexCall(number int, resumed bool, args []string) error {
	state, unlock, err := lockedState(true)
	if err != nil {
		return err
	}
	defer unlock()
	state.Calls = append(state.Calls, fmt.Sprintf("codex issue=%d resumed=%t", number, resumed))
	state.Calls = append(state.Calls, "codex argv="+strings.Join(args, " "))
	return saveStateUnlocked(state)
}

func issueNumber(workspace, prompt string) int {
	if match := issueNumberPattern.FindStringSubmatch(filepath.Base(workspace)); len(match) == 2 {
		number, _ := strconv.Atoi(match[1])
		return number
	}
	for _, number := range []int{1, 2} {
		if strings.Contains(prompt, fmt.Sprintf(`"number":%d`, number)) {
			return number
		}
	}
	return 0
}

func option(args []string, name string) string {
	for index := 0; index+1 < len(args); index++ {
		if args[index] == name {
			return args[index+1]
		}
	}
	return ""
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func remove(values []string, unwanted string) []string {
	result := values[:0]
	for _, value := range values {
		if value != unwanted {
			result = append(result, value)
		}
	}
	return result
}

func statePath() string {
	return filepath.Join(requiredEnvironment("OFFLINE_CONTRACT_STATE"), "state.json")
}

func requiredEnvironment(name string) string {
	value := os.Getenv(name)
	if value == "" {
		fmt.Fprintf(os.Stderr, "%s is required\n", name)
		os.Exit(2)
	}
	return value
}

func lockedState(write bool) (contractState, func(), error) {
	dir := requiredEnvironment("OFFLINE_CONTRACT_STATE")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return contractState{}, func() {}, err
	}
	lock, err := os.OpenFile(filepath.Join(dir, "state.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return contractState{}, func() {}, err
	}
	operation := syscall.LOCK_SH
	if write {
		operation = syscall.LOCK_EX
	}
	if err := syscall.Flock(int(lock.Fd()), operation); err != nil {
		_ = lock.Close()
		return contractState{}, func() {}, err
	}
	data, err := os.ReadFile(statePath())
	if err != nil {
		_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
		_ = lock.Close()
		return contractState{}, func() {}, err
	}
	var state contractState
	if err := json.Unmarshal(data, &state); err != nil {
		_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
		_ = lock.Close()
		return contractState{}, func() {}, err
	}
	return state, func() {
		_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
		_ = lock.Close()
	}, nil
}

func saveState(state contractState) error {
	dir := requiredEnvironment("OFFLINE_CONTRACT_STATE")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(dir, "state.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	return saveStateUnlocked(state)
}

func saveStateUnlocked(state contractState) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	path := statePath()
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}

func writeJSON(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o600)
}

func git(remote string, args ...string) error {
	command := exec.Command("git", append([]string{"--git-dir", remote}, args...)...)
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return nil
}

func gitOutput(remote string, args ...string) (string, error) {
	command := exec.Command("git", append([]string{"--git-dir", remote}, args...)...)
	output, err := command.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return strings.TrimSpace(string(output)), nil
}

func squashMerge(remote, baseRef, head string, number int) (string, error) {
	base, err := gitOutput(remote, "rev-parse", "refs/heads/"+baseRef)
	if err != nil {
		return "", err
	}
	tree, err := gitOutput(remote, "merge-tree", "--write-tree", base, head)
	if err != nil {
		return "", err
	}
	command := exec.Command("git", "--git-dir", remote, "commit-tree", tree, "-p", base, "-m", fmt.Sprintf("Offline squash merge PR #%d", number))
	command.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=offline-contract", "GIT_AUTHOR_EMAIL=offline-contract@example.invalid",
		"GIT_COMMITTER_NAME=offline-contract", "GIT_COMMITTER_EMAIL=offline-contract@example.invalid",
	)
	output, err := command.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git commit-tree: %w: %s", err, strings.TrimSpace(string(output)))
	}
	commit := strings.TrimSpace(string(output))
	if err := git(remote, "update-ref", "refs/heads/"+baseRef, commit, base); err != nil {
		return "", err
	}
	return commit, nil
}
