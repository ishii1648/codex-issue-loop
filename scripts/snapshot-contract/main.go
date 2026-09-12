package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const repository = "ishii1648/codex-issue-loop"
const versionPath = "internal/domain/statecontract/contract.go"

var contractPaths = []string{"internal/domain/snapshot", "internal/domain/statecontract", "internal/domain/issue", "internal/domain/publication", "internal/domain/queue"}
var stableTag = regexp.MustCompile(`^v(\d+)\.(\d+)\.(\d+)$`)

type release struct {
	Tag        string    `json:"tag_name"`
	Draft      bool      `json:"draft"`
	Prerelease bool      `json:"prerelease"`
	Published  time.Time `json:"published_at"`
}

func command(dir, name string, args ...string) ([]byte, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%s %v: %w: %s", name, args, err, stderr.String())
	}
	return out, nil
}

func applicable(r release) bool {
	m := stableTag.FindStringSubmatch(r.Tag)
	if r.Draft || r.Prerelease || m == nil {
		return false
	}
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	patch, _ := strconv.Atoi(m[3])
	return major > 0 || minor > 12 || minor == 12 && patch >= 35
}

func releases(data []byte) ([]release, error) {
	var pages [][]release
	if err := json.Unmarshal(data, &pages); err != nil {
		return nil, err
	}
	var result []release
	seen := map[string]bool{}
	for _, page := range pages {
		for _, r := range page {
			if !applicable(r) {
				continue
			}
			if r.Published.IsZero() || seen[r.Tag] {
				return nil, fmt.Errorf("ambiguous release %s", r.Tag)
			}
			seen[r.Tag] = true
			result = append(result, r)
		}
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("no applicable public release (floor v0.12.35)")
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Published.After(result[j].Published) })
	if len(result) > 1 && result[0].Published.Equal(result[1].Published) {
		return nil, fmt.Errorf("ambiguous latest publication")
	}
	return result, nil
}

func version(source []byte) (int, error) {
	value := func(name string) string {
		m := regexp.MustCompile(`(?m)^\s*` + name + `\s*=\s*([A-Za-z0-9]+)\s*$`).FindSubmatch(source)
		if m == nil {
			return ""
		}
		return string(m[1])
	}
	v := value("CurrentSchemaVersion")
	unified := v == "CurrentVersion"
	if v == "CurrentVersion" {
		v = value("CurrentVersion")
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 5 || n >= 6 && !unified {
		return 0, fmt.Errorf("unrecognized snapshot version definition")
	}
	return n, nil
}

func contractTree(root, ref string) ([]byte, error) {
	args := append([]string{"ls-tree", "-r", ref, "--"}, contractPaths...)
	out, err := command(root, "git", args...)
	if err != nil {
		return nil, err
	}
	var lines []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line != "" && !strings.HasSuffix(line, "_test.go") {
			lines = append(lines, line)
		}
	}
	if len(lines) == 0 {
		return nil, fmt.Errorf("contract tree missing at %s", ref)
	}
	return []byte(strings.Join(lines, "\n")), nil
}

func checkVersion(base, head, maximum int, changed bool) error {
	if head < 6 {
		return fmt.Errorf("HEAD must use the unified snapshot contract version (6 or later)")
	}
	if head < base || head < maximum {
		return fmt.Errorf("snapshot version regressed: base=%d maximum=%d head=%d", base, maximum, head)
	}
	if changed && head <= maximum {
		return fmt.Errorf("contract changed without a fresh version: public maximum=%d head=%d", maximum, head)
	}
	return nil
}

func archive(root, ref, destination string) error {
	if err := os.MkdirAll(destination, 0700); err != nil {
		return err
	}
	data, err := command(root, "git", "archive", ref)
	if err != nil {
		return err
	}
	cmd := exec.Command("tar", "-xf", "-", "-C", destination)
	cmd.Stdin = bytes.NewReader(data)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("extract: %w: %s", err, out)
	}
	return nil
}

func run(root string) error {
	data, err := command(root, "gh", "api", "--paginate", "--slurp", "repos/"+repository+"/releases?per_page=100")
	if err != nil {
		return err
	}
	public, err := releases(data)
	if err != nil {
		return err
	}
	maximum := 0
	var baseVersion int
	for i, r := range public {
		remote, err := command(root, "gh", "api", "repos/"+repository+"/git/ref/tags/"+r.Tag)
		if err != nil {
			return err
		}
		var ref struct {
			Ref    string `json:"ref"`
			Object struct {
				SHA string `json:"sha"`
			} `json:"object"`
		}
		if err := json.Unmarshal(remote, &ref); err != nil {
			return err
		}
		local, err := command(root, "git", "rev-parse", "refs/tags/"+r.Tag)
		if err != nil {
			return err
		}
		if ref.Ref != "refs/tags/"+r.Tag || len(ref.Object.SHA) != 40 || strings.TrimSpace(string(local)) != ref.Object.SHA {
			return fmt.Errorf("public tag identity mismatch: %s", r.Tag)
		}
		source, err := command(root, "git", "show", "refs/tags/"+r.Tag+":"+versionPath)
		if err != nil {
			return err
		}
		v, err := version(source)
		if err != nil {
			return err
		}
		if i == 0 {
			baseVersion = v
		}
		if v > maximum {
			maximum = v
		}
	}
	base := "refs/tags/" + public[0].Tag
	sha, err := command(root, "git", "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	head := strings.TrimSpace(string(sha))
	source, err := command(root, "git", "show", head+":"+versionPath)
	if err != nil {
		return err
	}
	headVersion, err := version(source)
	if err != nil {
		return err
	}
	before, err := contractTree(root, base)
	if err != nil {
		return err
	}
	after, err := contractTree(root, head)
	if err != nil {
		return err
	}
	if err := checkVersion(baseVersion, headVersion, maximum, !bytes.Equal(before, after)); err != nil {
		return err
	}
	fmt.Printf("snapshot-contract: baseline=%s version=%d HEAD=%s version=%d\n", public[0].Tag, baseVersion, head, headVersion)
	tmp, err := os.MkdirTemp("", "snapshot-contract-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	oldDir, newDir := filepath.Join(tmp, "old"), filepath.Join(tmp, "new")
	if err := archive(root, base, oldDir); err != nil {
		return err
	}
	if err := archive(root, head, newDir); err != nil {
		return err
	}
	if err := compatibility(root, oldDir, newDir, tmp, baseVersion == headVersion); err != nil {
		return err
	}
	out, err := command(newDir, "go", "test", "./internal/domain/...", "./internal/adapter/state", "./internal/application/supervisor", "-run", "Architecture|Boundar|Assignments|SnapshotContract|DomainPackageImports|InternalPackagesFollowLayerDependencies|IssueStatusLogic|VerticalLifecycle", "-count=1")
	fmt.Print(string(out))
	if err != nil {
		return err
	}
	fmt.Printf("snapshot-contract: PASS HEAD=%s baseline=%s (version, bidirectional fixtures, architecture)\n", head, public[0].Tag)
	return nil
}

func main() {
	root, err := os.Getwd()
	if err == nil {
		err = run(root)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "snapshot-contract: FAIL (violation or verification unavailable):", err)
		os.Exit(1)
	}
}
