package hostcli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	meta "github.com/ishii1648/codex-issue-loop/internal/platform/deliverymeta"
	"github.com/ishii1648/codex-issue-loop/internal/platform/fsutil"
	"github.com/ishii1648/codex-issue-loop/internal/platform/layout"
	"github.com/ishii1648/codex-issue-loop/internal/platform/registry"
)

const fixtureCommit = "0123456789abcdef0123456789abcdef01234567"

func testLayout(t *testing.T) layout.Layout {
	t.Helper()
	root := t.TempDir()
	root, canonicalErr := filepath.EvalSymlinks(root)
	if canonicalErr != nil {
		t.Fatal(canonicalErr)
	}
	cache, err := exec.Command("go", "env", "GOMODCACHE").Output()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOMODCACHE", strings.TrimSpace(string(cache)))
	t.Setenv("HOME", root)
	t.Setenv("AGENT_LOOP_HOME", filepath.Join(root, "managed"))
	t.Setenv("AGENT_LOOP_SKILLS_DIR", filepath.Join(root, "skills"))
	t.Setenv("AGENT_LOOP_LAUNCH_AGENTS_DIR", filepath.Join(root, "launchagents"))
	l, err := layout.New()
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	return l
}

func installFixture(t *testing.T, l layout.Layout, version, body string, protocol int) meta.AssignmentRef {
	t.Helper()
	script := fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = version ]; then\n printf '%%s\\n' '{\"version\":\"%s\",\"commit\":\"%s\",\"repository_command_protocol\":%d}'\n exit 0\nfi\n%s\n", version, fixtureCommit, protocol, body)
	source := filepath.Join(t.TempDir(), "agent-loop")
	if err := os.WriteFile(source, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	ref := meta.SlotRef(l, version, fixtureCommit, fmt.Sprintf("%x", sha256.Sum256([]byte(script))))
	if err := meta.StageSlot(l, ref, source); err != nil {
		t.Fatal(err)
	}
	return ref
}

func assign(t *testing.T, l layout.Layout, id string, ref meta.AssignmentRef) string {
	t.Helper()
	repo := filepath.Join(l.Root, id)
	if err := os.MkdirAll(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	repo, canonicalErr := filepath.EvalSymlinks(repo)
	if canonicalErr != nil {
		t.Fatal(canonicalErr)
	}
	r, err := (registry.Store{Path: l.RegistryPath}).Load()
	if err != nil {
		t.Fatal(err)
	}
	r.Repos[id] = registry.Entry{RepoID: id, RepoPath: repo, GitHubRepo: "owner/" + id}
	if err := fsutil.WriteJSON(l.RegistryPath, r, 0o600); err != nil {
		t.Fatal(err)
	}
	path, err := meta.DefaultConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := meta.LoadConfig(path)
	if errors.Is(err, os.ErrNotExist) {
		cfg = meta.DefaultConfig("owner/releases")
	} else if err != nil {
		t.Fatal(err)
	}
	cfg.Assignments[id] = meta.RepositoryAssignment{RepositoryID: id, AssignmentRef: ref, Generation: 1, UpdatedAt: time.Now()}
	if err := meta.WriteConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	return repo
}

func TestHostHasNoSnapshotDependency(t *testing.T) {
	cmd := exec.Command("go", "list", "-deps", "./cmd/agent-loopctl")
	cmd.Dir = "../../.."
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("dependency graph: %v: %s", err, out)
	}
	for _, name := range []string{"/adapter/state", "/domain/snapshot", "/domain/issue", "/domain/statecontract", "/application/app", "/application/migration", "/application/supervisor", "/application/delivery"} {
		for _, line := range strings.Split(string(out), "\n") {
			if strings.HasSuffix(line, name) {
				t.Fatalf("host CLI depends on %s", line)
			}
		}
	}
}

func TestDifferentAssignmentsForwardStreamsAndExitWithoutResending(t *testing.T) {
	l := testLayout(t)
	for i, version := range []string{"v1.2.3", "v1.3.0"} {
		id := fmt.Sprintf("repo%d", i)
		ref := installFixture(t, l, version, "printf '%s\\n' '"+version+"' >&2\ncat\nexit 7", 1)
		repo := assign(t, l, id, ref)
		input := "{\"text\":\"answer\\n--repo other\",\"unknown\":true}\n"
		var out, stderr bytes.Buffer
		code := (App{In: strings.NewReader(input), Out: &out, Err: &stderr}).Run(context.Background(), []string{"answer", "--repo", repo, "--message-file", "-", "--request-id", "req_1", "--json"})
		if code != 7 || out.String() != input || stderr.String() != version+"\n" {
			t.Fatalf("code=%d stdout=%q stderr=%q", code, out.String(), stderr.String())
		}
	}
}

func TestMissingUnknownAndTamperedRuntimesFailWithoutSnapshotReads(t *testing.T) {
	for _, kind := range []string{"missing", "unknown", "tampered", "foreign-slot", "fenced"} {
		t.Run(kind, func(t *testing.T) {
			l := testLayout(t)
			protocol := 1
			if kind == "unknown" {
				protocol = 99
			}
			ref := installFixture(t, l, "v1.2.3", "echo must-not-run", protocol)
			repo := assign(t, l, "repo", ref)
			stateDir := l.RepoDir("repo")
			if err := os.MkdirAll(stateDir, 0o700); err != nil {
				t.Fatal(err)
			}
			snapshot := filepath.Join(stateDir, "state.json")
			original := []byte("future snapshot: deliberately invalid JSON")
			if err := os.WriteFile(snapshot, original, 0o600); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "missing":
				if err := os.Remove(ref.Slot); err != nil {
					t.Fatal(err)
				}
			case "tampered":
				if err := os.WriteFile(ref.Slot, []byte("#!/bin/sh\necho must-not-run\n"), 0o700); err != nil {
					t.Fatal(err)
				}
			case "foreign-slot":
				path, _ := meta.DefaultConfigPath()
				cfg, _ := meta.LoadConfig(path)
				a := cfg.Assignments["repo"]
				a.Slot = filepath.Join(t.TempDir(), "agent-loop")
				cfg.Assignments["repo"] = a
				if err := meta.WriteConfig(path, cfg); err != nil {
					t.Fatal(err)
				}
			case "fenced":
				if err := fsutil.WriteFile(l.DeliveryAssignmentFencePath("repo"), []byte("maintenance"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			var out, stderr bytes.Buffer
			code := (App{Out: &out, Err: &stderr}).Run(context.Background(), []string{"status", "--repo", repo, "--json"})
			if code == 0 || strings.Contains(out.String(), "must-not-run") {
				t.Fatalf("code=%d out=%s err=%s", code, out.String(), stderr.String())
			}
			after, err := os.ReadFile(snapshot)
			if err != nil || !bytes.Equal(original, after) {
				t.Fatalf("snapshot changed: %v", err)
			}
		})
	}
}

func TestCancellationReachesRuntimeWithoutResend(t *testing.T) {
	l := testLayout(t)
	marker := filepath.Join(t.TempDir(), "started")
	ref := installFixture(t, l, "v1.2.3", "echo started >> '"+marker+"'\ntrap 'exit 42' TERM\nwhile :; do sleep 1; done", 1)
	repo := assign(t, l, "repo", ref)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var out, stderr bytes.Buffer
	done := make(chan int, 1)
	go func() { done <- (App{Out: &out, Err: &stderr}).Run(ctx, []string{"watch", "--repo", repo}) }()
	deadline := time.After(5 * time.Second)
	for {
		if data, err := os.ReadFile(marker); err == nil && string(data) == "started\n" {
			break
		}
		select {
		case <-deadline:
			t.Fatal("runtime never started")
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	select {
	case code := <-done:
		if code == 0 {
			t.Fatal("cancel returned success")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancellation was not forwarded")
	}
	data, err := os.ReadFile(marker)
	if err != nil || string(data) != "started\n" {
		t.Fatalf("unexpected attempts: %q %v", data, err)
	}
}

func TestHostUpdateAndRollbackPreserveAssignmentsAndLaunchAgents(t *testing.T) {
	l := testLayout(t)
	ref := installFixture(t, l, "v1.2.3", "exit 0", 1)
	assign(t, l, "A", ref)
	other := installFixture(t, l, "v2.0.0", "exit 0", 1)
	assign(t, l, "B", other)
	previous := []byte("previous host executable")
	skill := []byte("previous skill")
	manifest := meta.HostInstallation{Format: 1, Version: "v1.0.0", Commit: fixtureCommit, Digest: fmt.Sprintf("%x", sha256.Sum256(previous)), SkillDigest: fmt.Sprintf("%x", sha256.Sum256(skill)), Bootstrap: ref}
	if err := writeHost(l, previous, skill, manifest); err != nil {
		t.Fatal(err)
	}
	paths := []string{l.RegistryPath, l.PlistPath("A"), l.PlistPath("B"), ref.Slot, other.Slot}
	for _, id := range []string{"A", "B"} {
		if err := fsutil.WriteFile(l.PlistPath(id), []byte("immutable "+id), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	configPath, _ := meta.DefaultConfigPath()
	paths = append(paths, configPath)
	before := map[string][]byte{}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		before[path] = data
	}
	var out, stderr bytes.Buffer
	a := App{Out: &out, Err: &stderr}
	if code := a.Run(context.Background(), []string{"update", "--json"}); code != 0 {
		t.Fatalf("update %d: %s", code, stderr.String())
	}
	if code := a.Run(context.Background(), []string{"rollback", "--json"}); code != 0 {
		t.Fatalf("rollback %d: %s", code, stderr.String())
	}
	for path, data := range before {
		after, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(after, data) {
			t.Fatalf("changed %s: %v", path, err)
		}
	}
	restored, err := os.ReadFile(filepath.Join(l.BinDir, "agent-loopctl"))
	if err != nil || !bytes.Equal(restored, previous) {
		t.Fatalf("rollback failed: %q %v", restored, err)
	}
	legacy, err := os.ReadFile(filepath.Join(l.BinDir, "agent-loop"))
	if err != nil || string(legacy) != retiredCLI {
		t.Fatal("legacy CLI was reactivated")
	}
}

func TestRepositorySelectorDoesNotInterpretAnswerText(t *testing.T) {
	args := []string{"answer", "--message", "--repo", "--repo", "/selected", "--json"}
	forwarded, repo, err := meta.SelectRepository(args, "/canonical")
	if err != nil || repo != "/selected" || forwarded[2] != "--repo" || forwarded[4] != "/canonical" {
		t.Fatalf("forwarded=%q repo=%s err=%v", forwarded, repo, err)
	}
	_, _, err = meta.SelectRepository([]string{"status", "--repo", "A", "--repo=B"}, "")
	if err == nil {
		t.Fatal("duplicate selector accepted")
	}
	if _, _, err := meta.SelectRepository([]string{"status", "ignored", "--repo", "A"}, ""); err == nil {
		t.Fatal("positional argument can hide the repository selector")
	}

}

func TestRuntimeDispatchRejectsAssignmentChangeBeforeStateAccess(t *testing.T) {
	l := testLayout(t)
	aliasRoot := filepath.Join(t.TempDir(), "managed-alias")
	if err := os.Symlink(l.Root, aliasRoot); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENT_LOOP_HOME", aliasRoot)
	var layoutErr error
	l, layoutErr = layout.New()
	if layoutErr != nil {
		t.Fatal(layoutErr)
	}

	source := filepath.Join(t.TempDir(), "agent-loop")
	cmd := exec.Command("go", "build", "-o", source, "-ldflags", "-X github.com/ishii1648/codex-issue-loop/internal/application/app.Version=v1.2.3 -X github.com/ishii1648/codex-issue-loop/internal/application/app.Commit="+fixtureCommit, "./cmd/agent-loop")
	cmd.Dir = "../../.."
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build runtime: %v: %s", err, out)
	}
	digest, err := meta.FileDigest(source)
	if err != nil {
		t.Fatal(err)
	}
	ref := meta.SlotRef(l, "v1.2.3", fixtureCommit, digest)
	if err := meta.StageSlot(l, ref, source); err != nil {
		t.Fatal(err)
	}
	repo := assign(t, l, "repo", ref)
	path, _ := meta.DefaultConfigPath()
	cfg, _ := meta.LoadConfig(path)
	changed := cfg.Assignments["repo"]
	changed.Generation = 2
	cfg.Assignments["repo"] = changed
	if err := meta.WriteConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(ref.Slot, "dispatch", "--repo", repo, "--repository-id", "repo", "--generation", "1", "--", "status", "--repo", repo, "--json")
	out, err := command.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "assignment changed") {
		t.Fatalf("err=%v out=%s", err, out)
	}
	if _, err := os.Stat(l.RepoDir("repo")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("runtime touched state directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".agent-loop.yaml"), []byte("version: 5\ngithub:\n  repo: owner/repo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	command = exec.Command(ref.Slot, "dispatch", "--repo", repo, "--repository-id", "repo", "--generation", "2", "--", "status", "--repo", repo, "--json")
	out, _ = command.Output()
	var result map[string]json.RawMessage
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatalf("status did not return runtime JSON: %s %v", out, err)
	}
}

func TestFirstHostInstallVerifiesRuntimeAndRetiresCommonCLI(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(fmt.Sprintf("existing=%t", existing), func(t *testing.T) {
			l := testLayout(t)
			oldVersion, oldCommit := Version, Commit
			Version, Commit = "v1.2.3", fixtureCommit
			t.Cleanup(func() { Version, Commit = oldVersion, oldCommit })
			assetDir := t.TempDir()
			marker := filepath.Join(assetDir, "executed")
			info := meta.BinaryInfo{Version: Version, Commit: Commit, Target: "darwin/arm64", DeliveryProtocol: 1, AssignmentProtocol: 1, RepositoryCommandProtocol: 1, StateSchemaCurrent: 5, StateSchemaMigrationFrom: 4, SemanticContractCurrent: 4, SemanticContractMinimum: 1}
			infoJSON, err := json.Marshal(info)
			if err != nil {
				t.Fatal(err)
			}
			binary := []byte("#!/bin/sh\necho executed >> '" + marker + "'\nprintf '%s\\n' '" + string(infoJSON) + "'\n")
			digest := fmt.Sprintf("%x", sha256.Sum256(binary))
			manifest := meta.ReleaseManifest{ManifestVersion: 1, Version: Version, Commit: Commit, Target: "darwin/arm64", Artifact: meta.BinaryAsset, ArtifactSHA256: digest, DeliveryProtocol: 1, AssignmentProtocol: 1, RepositoryCommandProtocol: 1, StateSchemaCurrent: 5, StateSchemaMigrationFrom: 4, SemanticContractCurrent: 4, SemanticContractMinimum: 1}
			data, err := json.Marshal(manifest)
			if err != nil {
				t.Fatal(err)
			}
			checksums := digest + "  " + meta.BinaryAsset + "\n" + fmt.Sprintf("%x", sha256.Sum256(data)) + "  " + meta.ManifestAsset + "\n"
			for name, content := range map[string][]byte{meta.BinaryAsset: binary, meta.ManifestAsset: data, meta.ChecksumAsset: []byte(checksums)} {
				if err := os.WriteFile(filepath.Join(assetDir, name), content, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			fakeDir := t.TempDir()
			gh := `#!/bin/sh
case "$1 $2" in
 'release view') printf '%s\n' '{"tagName":"v1.2.3","isDraft":false,"isPrerelease":false}' ;;
 'api repos/ishii1648/codex-issue-loop/git/ref/tags/v1.2.3') printf '%s\n' '{"object":{"type":"tag","sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}' ;;
 'api repos/ishii1648/codex-issue-loop/git/tags/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa') printf '%s\n' '{"tag":"v1.2.3","object":{"type":"commit","sha":"0123456789abcdef0123456789abcdef01234567"}}' ;;
 'release download') while [ "$1" != --dir ]; do shift; done; shift; cp "$FIXTURE_ASSETS/agent-loop_Darwin_arm64" "$FIXTURE_ASSETS/release-manifest.json" "$FIXTURE_ASSETS/checksums.txt" "$1/" ;;
 'attestation verify') test ! -f "$FIXTURE_ASSETS/executed" ;;
 *) exit 9 ;;
esac
`
			if err := os.WriteFile(filepath.Join(fakeDir, "gh"), []byte(gh), 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", fakeDir+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("FIXTURE_ASSETS", assetDir)
			if err := fsutil.WriteFile(filepath.Join(l.BinDir, "agent-loop"), []byte("legacy executable"), 0o700); err != nil {
				t.Fatal(err)
			}
			var preservedPath string
			var preservedPlist []byte
			var existingRef meta.AssignmentRef
			if existing {
				existingRef = installFixture(t, l, "v1.1.0", "exit 0", 1)
				preservedPath = assign(t, l, "existing", existingRef)
				configPath, _ := meta.DefaultConfigPath()
				cfg, err := meta.LoadConfig(configPath)
				if err != nil {
					t.Fatal(err)
				}
				cfg.ReleaseRepository = "ishii1648/codex-issue-loop"
				if err := meta.WriteConfig(configPath, cfg); err != nil {
					t.Fatal(err)
				}
				registered, err := (registry.Store{Path: l.RegistryPath}).Load()
				if err != nil {
					t.Fatal(err)
				}
				if err := hostManager(l).WritePlist(registered.Repos["existing"], existingRef.Slot); err != nil {
					t.Fatal(err)
				}
				preservedPlist, err = os.ReadFile(l.PlistPath("existing"))
				if err != nil {
					t.Fatal(err)
				}
			}
			var out, stderr bytes.Buffer
			if code := (App{Out: &out, Err: &stderr}).Run(context.Background(), []string{"install", "--json"}); code != 0 {
				t.Fatalf("install=%d: %s", code, stderr.String())
			}
			installed, err := meta.ReadHostInstallation(l)
			if err != nil {
				t.Fatal(err)
			}
			if installed.Bootstrap.ArtifactSHA256 != digest {
				t.Fatal("wrong bootstrap runtime")
			}
			legacy, err := os.ReadFile(filepath.Join(l.BinDir, "agent-loop"))
			if err != nil || string(legacy) != retiredCLI {
				t.Fatal("common CLI was not retired")
			}
			if existing {
				_, after, err := meta.ResolveAssignment(l, preservedPath)
				if err != nil || after.AssignmentRef != existingRef {
					t.Fatalf("install changed assignment: %v", err)
				}
				plist, err := os.ReadFile(l.PlistPath("existing"))
				if err != nil || !bytes.Equal(plist, preservedPlist) {
					t.Fatal("install changed repository LaunchAgent")
				}
			} else if _, err := os.Stat(l.LaunchAgents); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("fresh install created LaunchAgents: %v", err)
			}
		})
	}
}

func TestInterruptSignalIsPreserved(t *testing.T) {
	l := testLayout(t)
	marker := filepath.Join(t.TempDir(), "ready")
	ref := installFixture(t, l, "v1.2.3", "trap 'exit 42' INT\ntrap 'exit 43' TERM\necho ready > '"+marker+"'\nwhile :; do sleep 1; done", 1)
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	var out, stderr bytes.Buffer
	done := make(chan int, 1)
	go func() { done <- (App{Out: &out, Err: &stderr}).execute(ctx, ref.Slot, []string{"watch"}) }()
	deadline := time.After(5 * time.Second)
	for {
		if data, err := os.ReadFile(marker); err == nil && string(data) == "ready\n" {
			break
		}
		select {
		case <-deadline:
			t.Fatal("runtime did not become ready")
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel(Interrupted{Signal: os.Interrupt})
	select {
	case code := <-done:
		if code != 42 {
			t.Fatalf("interrupt code=%d stderr=%s", code, stderr.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("interrupt not forwarded")
	}
}

func TestBootstrapRegistrationUsesVerifiedRuntime(t *testing.T) {
	l := testLayout(t)
	source := filepath.Join(t.TempDir(), "agent-loop")
	cmd := exec.Command("go", "build", "-o", source, "-ldflags", "-X github.com/ishii1648/codex-issue-loop/internal/application/app.Version=v1.2.3 -X github.com/ishii1648/codex-issue-loop/internal/application/app.Commit="+fixtureCommit, "./cmd/agent-loop")
	cmd.Dir = "../../.."
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build: %v: %s", err, out)
	}
	digest, err := meta.FileDigest(source)
	if err != nil {
		t.Fatal(err)
	}
	ref := meta.SlotRef(l, "v1.2.3", fixtureCommit, digest)
	if err := meta.StageSlot(l, ref, source); err != nil {
		t.Fatal(err)
	}
	host := []byte("host executable")
	skill := []byte("host skill")
	installed := meta.HostInstallation{Format: 1, Version: "v1.2.3", Commit: fixtureCommit, Digest: fmt.Sprintf("%x", sha256.Sum256(host)), SkillDigest: fmt.Sprintf("%x", sha256.Sum256(skill)), Bootstrap: ref}
	if err := writeHost(l, host, skill, installed); err != nil {
		t.Fatal(err)
	}
	path, _ := meta.DefaultConfigPath()
	if err := meta.WriteConfig(path, meta.DefaultConfig("owner/releases")); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	for name, body := range map[string]string{"gh": "echo 'gh version 2.86.0'", "codex": "exit 0", "launchctl": "if [ \"$1\" = print ]; then echo 'Could not find service' >&2; exit 113; fi; exit 0"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	repo := filepath.Join(l.Root, "new-repo")
	if err := os.MkdirAll(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "init", "-q", repo).CombinedOutput(); err != nil {
		t.Fatalf("git: %v %s", err, out)
	}
	if err := os.WriteFile(filepath.Join(repo, ".agent-loop.yaml"), []byte("version: 5\ngithub:\n  repo: owner/bootstrap\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	a := App{Out: &out, Err: &stderr}
	if code := a.Run(context.Background(), []string{"register", "--repo", repo, "--json"}); code != 0 {
		t.Fatalf("register=%d: %s", code, stderr.String())
	}
	entry, assignment, err := meta.ResolveAssignment(l, repo)
	if err != nil {
		t.Fatal(err)
	}
	if assignment.AssignmentRef != ref {
		t.Fatal("bootstrap assigned another runtime")
	}
	program, err := hostManager(l).Program(entry)
	if err != nil || program != ref.Slot {
		t.Fatalf("program=%s err=%v", program, err)
	}
	out.Reset()
	stderr.Reset()
	if code := a.Run(context.Background(), []string{"status", "--repo", repo, "--json"}); code != 0 {
		t.Fatalf("stopped status=%d: %s stdout=%s", code, stderr.String(), out.String())
	}
	if !json.Valid(out.Bytes()) {
		t.Fatal("status did not preserve JSON")
	}
	var status struct {
		Launchd struct {
			Loaded bool `json:"loaded"`
		} `json:"launchd"`
	}
	if err := json.Unmarshal(out.Bytes(), &status); err != nil || status.Launchd.Loaded {
		t.Fatalf("supervisor was not stopped: %s %v", out.String(), err)
	}
	snapshotPath := filepath.Join(l.RepoDir(entry.RepoID), "state.json")
	before, err := os.ReadFile(snapshotPath)
	if err != nil {
		t.Fatal(err)
	}
	out.Reset()
	stderr.Reset()
	a.Run(context.Background(), []string{"doctor", "--repo", repo, "--json"})
	if !json.Valid(out.Bytes()) || !strings.Contains(out.String(), "ASSIGNMENT_RUNTIME_ISOLATED") {
		t.Fatalf("doctor did not use the assigned runtime: %s %s", out.String(), stderr.String())
	}
	after, err := os.ReadFile(snapshotPath)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("stopped doctor changed the snapshot")
	}

}

func TestHostDeliveryPausePreservesRetainedMaintenance(t *testing.T) {
	l := testLayout(t)
	ref := installFixture(t, l, "v1.2.3", "exit 0", 1)
	assign(t, l, "repo", ref)
	path, _ := meta.DefaultConfigPath()
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := fsutil.WriteFile(meta.RuntimePaths(l.Root).Maintenance, []byte("retained fence"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	if code := (App{Out: &out, Err: &stderr}).Run(context.Background(), []string{"delivery", "pause", "--json"}); code == 0 {
		t.Fatal("paused through retained maintenance")
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("pause rewrote assignments under maintenance")
	}
}
