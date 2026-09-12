package hostcli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"syscall"

	meta "github.com/ishii1648/codex-issue-loop/internal/platform/deliverymeta"
	"github.com/ishii1648/codex-issue-loop/internal/platform/layout"
)

var Version = "0.1.0-dev"
var Commit = "unknown"

type Interrupted struct{ Signal os.Signal }

func (e Interrupted) Error() string { return e.Signal.String() }

func interruptionSignal(ctx context.Context) syscall.Signal {
	var interrupted Interrupted
	if errors.As(context.Cause(ctx), &interrupted) {
		if sig, ok := interrupted.Signal.(syscall.Signal); ok {
			return sig
		}
	}
	return syscall.SIGTERM
}

type App struct {
	In       io.Reader
	Out, Err io.Writer
}

func (a App) Run(ctx context.Context, args []string) int {
	if a.In == nil {
		a.In = os.Stdin
	}
	if a.Out == nil {
		a.Out = os.Stdout
	}
	if a.Err == nil {
		a.Err = os.Stderr
	}
	fail := func(err error) int { fmt.Fprintln(a.Err, err); return 1 }
	if len(args) == 0 {
		fmt.Fprintln(a.Err, "usage: agent-loopctl <command> [--repo path] [options]")
		return 2
	}
	switch args[0] {
	case "version", "--version":
		if len(args) == 2 && args[1] == "--json" {
			_ = json.NewEncoder(a.Out).Encode(map[string]any{"version": Version, "commit": Commit, "target": runtime.GOOS + "/" + runtime.GOARCH, "repository_command_protocol": meta.RepositoryCommandProtocol})
		} else {
			fmt.Fprintf(a.Out, "agent-loopctl %s (%s)\n", Version, Commit)
		}
		return 0
	case "help", "--help", "-h":
		fmt.Fprintln(a.Out, "Usage: agent-loopctl <command> [options]\nHost: install update rollback uninstall init delivery\nRepository: register unregister start stop restart status doctor watch answer issue logs cleanup purge migrate bootstrap-labels incident recover-quarantined-snapshot recover-semantic-quarantine recover-lifecycle-quarantine export-recovery-fixture verify-recovery-fixture\nRepository operations run only through a verified assigned agent-loop runtime.")
		return 0
	}
	l, err := layout.New()
	if err != nil {
		return fail(err)
	}
	switch args[0] {
	case "install", "update", "rollback", "uninstall":
		if err := a.installation(ctx, l, args); err != nil {
			return fail(err)
		}
		return 0
	case "broker":
		if err := a.Broker(ctx, l, args[1:]); err != nil {
			return fail(err)
		}
		return 0
	case "delivery":
		return a.delivery(ctx, l, args)
	case "init", "register", "bootstrap-labels", "verify-recovery-fixture":
		return a.bootstrap(ctx, l, args)
	case "status", "doctor", "watch", "answer", "issue", "logs", "cleanup", "purge", "start", "stop", "restart", "unregister", "migrate", "recover-quarantined-snapshot", "recover-semantic-quarantine", "recover-lifecycle-quarantine", "export-recovery-fixture", "incident":
		return a.repository(ctx, l, args)
	default:
		fmt.Fprintf(a.Err, "unknown command %q\n", args[0])
		return 2
	}
}

func (a App) repository(ctx context.Context, l layout.Layout, args []string) int {
	fail := func(err error) int { fmt.Fprintln(a.Err, err); return 1 }
	if args[0] == "answer" {
		via, err := meta.CommandOption(args, "via")
		if err != nil {
			return fail(err)
		}
		if via == "github" {
			return a.bootstrap(ctx, l, args)
		}
	}
	repo, err := meta.RepositoryArgument(args)
	if err != nil {
		return fail(err)
	}
	entry, ref, err := meta.ResolveAssignment(l, repo)
	if err != nil {
		return fail(err)
	}
	if args[0] != "delivery" {
		if err := meta.CheckAssignmentIdle(l, entry.RepoID); err != nil {
			return fail(err)
		}
	}
	if err := a.probe(ctx, ref.AssignmentRef); err != nil {
		return fail(err)
	}
	forwarded, _, err := meta.SelectRepository(args, entry.RepoPath)
	if err != nil {
		return fail(err)
	}
	if args[0] == "doctor" {
		forwarded = append(forwarded, "--assignment-health")
	}

	command := []string{"dispatch", "--repo", entry.RepoPath, "--repository-id", entry.RepoID, "--generation", strconv.FormatUint(ref.Generation, 10), "--"}
	return a.execute(ctx, ref.Slot, append(command, forwarded...))
}

func (a App) probe(ctx context.Context, ref meta.AssignmentRef) error {
	if err := meta.VerifySlot(ref); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, ref.Slot, "version", "--json")
	data, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("runtime capability probe failed: %w", err)
	}
	var info struct {
		Version  string `json:"version"`
		Commit   string `json:"commit"`
		Protocol int    `json:"repository_command_protocol"`
	}
	if err := json.Unmarshal(data, &info); err != nil {
		return fmt.Errorf("invalid runtime capability response: %w", err)
	}
	if info.Version != ref.Version || info.Commit != ref.Commit || info.Protocol != meta.RepositoryCommandProtocol {
		return errors.New("runtime identity or repository command protocol is incompatible; no repository command executed")
	}
	return nil
}

func (a App) execute(ctx context.Context, binary string, args []string) int {
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Stdin = a.In
	cmd.Stdout = a.Out
	cmd.Stderr = a.Err
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, interruptionSignal(ctx)) }
	err := cmd.Run()
	if err == nil {
		return 0
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		if status, ok := exit.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			return 128 + int(status.Signal())
		}
		return exit.ExitCode()
	}
	fmt.Fprintln(a.Err, err)
	if ctx.Err() != nil {
		return 128 + int(interruptionSignal(ctx))
	}
	return 1
}
