package app

import (
	"context"
	"flag"
	"fmt"

	"github.com/ishii1648/codex-issue-loop/internal/application/delivery"
	"github.com/ishii1648/codex-issue-loop/internal/platform/config"
	meta "github.com/ishii1648/codex-issue-loop/internal/platform/deliverymeta"
	"github.com/ishii1648/codex-issue-loop/internal/platform/layout"
	"github.com/ishii1648/codex-issue-loop/internal/platform/registry"
)

func (a App) acquireDeliveryLock(l layout.Layout) (*delivery.Lock, error) {
	if a.assignmentLock != nil {
		return a.assignmentLock.Borrow(), nil
	}
	return delivery.AcquireLock(delivery.RuntimePaths(l.Root).Lock)
}

func (a App) dispatch(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("dispatch", flag.ContinueOnError)
	fs.SetOutput(a.Err)
	repo := fs.String("repo", "", "registered repository path")
	id := fs.String("repository-id", "", "repository identity")
	generation := fs.Uint64("generation", 0, "assignment generation")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	command := fs.Args()
	fail := func(err error) int { fmt.Fprintln(a.Err, err); return 1 }
	if len(command) == 0 || *repo == "" || *id == "" || *generation == 0 {
		return fail(fmt.Errorf("incomplete repository dispatch"))
	}
	switch command[0] {
	case "register", "status", "doctor", "watch", "answer", "issue", "logs", "cleanup", "purge", "start", "stop", "restart", "unregister", "migrate", "recover-quarantined-snapshot", "recover-semantic-quarantine", "recover-lifecycle-quarantine", "export-recovery-fixture", "incident", "delivery":
	default:
		return fail(fmt.Errorf("unsupported repository command %q", command[0]))
	}
	l, err := layout.New()
	if err != nil {
		return fail(err)
	}
	lock, err := a.acquireDeliveryLock(l)
	if err != nil {
		return fail(err)
	}
	defer lock.Close()
	entry, ref, err := meta.ResolveAssignment(l, *repo)
	if err != nil {
		return fail(err)
	}
	if entry.RepoID != *id || ref.Generation != *generation {
		return fail(fmt.Errorf("assignment changed before command execution; command was not run"))
	}
	if err := meta.VerifyExecutable(ref.AssignmentRef); err != nil {
		return fail(err)
	}
	if ref.Version != Version || ref.Commit != Commit {
		return fail(fmt.Errorf("runtime identity differs from assignment"))
	}
	if command[0] == "delivery" {
		alternate, err := meta.CommandOption(command, "config")
		if err != nil {
			return fail(err)
		}
		if alternate != "" {
			return fail(fmt.Errorf("dispatch requires the authoritative default delivery config"))
		}
	}
	if command[0] != "delivery" {
		if err := meta.CheckAssignmentIdle(l, entry.RepoID); err != nil {
			return fail(err)
		}
	}
	selected, err := meta.RepositoryArgument(command)
	if err != nil {
		return fail(err)
	}
	if selected != entry.RepoPath {
		return fail(fmt.Errorf("forwarded repository differs from dispatch authority"))
	}
	a.assignmentLock = lock
	return a.Run(ctx, command)
}

func (a App) bootstrapDispatch(ctx context.Context, args []string) int {
	fail := func(err error) int { fmt.Fprintln(a.Err, err); return 1 }
	if len(args) == 0 || args[0] != "register" {
		return fail(fmt.Errorf("bootstrap only accepts register"))
	}
	l, err := layout.New()
	if err != nil {
		return fail(err)
	}
	lock, err := a.acquireDeliveryLock(l)
	if err != nil {
		return fail(err)
	}
	defer lock.Close()
	installed, err := meta.ReadHostInstallation(l)
	if err != nil {
		return fail(err)
	}
	if installed.Bootstrap.Version != Version || installed.Bootstrap.Commit != Commit {
		return fail(fmt.Errorf("bootstrap runtime identity differs from installation"))
	}
	if err := meta.VerifyExecutable(installed.Bootstrap); err != nil {
		return fail(err)
	}
	repo, err := meta.RepositoryArgument(args)
	if err != nil {
		return fail(err)
	}
	registered, err := (registry.Store{Path: l.RegistryPath}).Load()
	if err != nil {
		return fail(err)
	}
	canonical, err := config.CanonicalRepoPath(repo)
	if err != nil {
		return fail(err)
	}
	for _, entry := range registered.Repos {
		if entry.RepoPath == canonical {
			return fail(fmt.Errorf("repository was registered before bootstrap; select its assignment"))
		}
	}
	a.assignmentLock = lock
	return a.Run(ctx, args)
}
