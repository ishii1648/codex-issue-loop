package deliverymeta

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ishii1648/codex-issue-loop/internal/platform/layout"
	"github.com/ishii1648/codex-issue-loop/internal/platform/registry"
)

const RepositoryCommandProtocol = 1

func ResolveAssignment(l layout.Layout, repo string) (registry.Entry, RepositoryAssignment, error) {
	cwd, _ := os.Getwd()
	entry, err := (registry.Store{Path: l.RegistryPath}).Resolve(repo, cwd)
	if err != nil {
		return entry, RepositoryAssignment{}, err
	}
	path, err := DefaultConfigPath()
	if err != nil {
		return entry, RepositoryAssignment{}, err
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		return entry, RepositoryAssignment{}, err
	}
	ref, ok := cfg.Assignments[entry.RepoID]
	if !ok {
		return entry, ref, errors.New("repository has no assigned runtime")
	}
	if ref.AssignmentRef != SlotRef(l, ref.Version, ref.Commit, ref.ArtifactSHA256) {
		return entry, ref, errors.New("assignment is outside its canonical trusted slot")
	}
	if err := VerifySlot(ref.AssignmentRef); err != nil {
		return entry, ref, err
	}
	return entry, ref, nil
}

func CheckAssignmentIdle(l layout.Layout, id string) error {
	tx, err := LoadAssignmentTransaction(l.DeliveryAssignmentTransactionPath(id))
	if err != nil {
		return err
	}
	if tx.RepositoryID != "" && tx.Phase != AssignmentSucceeded && tx.Phase != AssignmentRolledBack {
		return fmt.Errorf("repository has an unfinished assignment transaction: %s", tx.Phase)
	}
	for _, path := range []string{l.DeliveryAssignmentFencePath(id), RuntimePaths(l.Root).Maintenance} {
		if _, err := os.Lstat(path); err == nil {
			return errors.New("repository is under delivery maintenance")
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func VerifyExecutable(ref AssignmentRef) error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		return err
	}
	expected, err := filepath.EvalSymlinks(ref.Slot)
	if err != nil {
		return err
	}
	if filepath.Clean(executable) != filepath.Clean(expected) {
		return errors.New("command must run from its assigned immutable slot")
	}
	return VerifySlot(ref)
}

func (l *Lock) Borrow() *Lock { return &Lock{file: l.file, borrowed: true} }

func RepositoryArgument(args []string) (string, error) {
	_, value, err := SelectRepository(args, "")
	return value, err
}

func SelectRepository(args []string, canonical string) ([]string, string, error) {
	return selectOption(args, "repo", canonical)
}

func CommandOption(args []string, name string) (string, error) {
	_, value, err := selectOption(args, name, "")
	return value, err
}

func selectOption(args []string, option, canonical string) ([]string, string, error) {
	if len(args) == 0 {
		return nil, "", errors.New("command is required")
	}
	start := 1
	switch args[0] {
	case "issue", "incident":
		start = 2
	case "delivery":
		start = 3
	}
	if len(args) < start {
		return nil, "", errors.New("subcommand is required")
	}
	terminator := len(args)
	out := append([]string(nil), args...)
	value := ""
	found := false
	booleans := map[string]bool{"apply": true, "assignment-health": true, "check": true, "confirm-exact-backup": true, "confirm-legacy-merged-identities": true, "confirm-retained-fence": true, "confirm-synthetic-evidence": true, "dry-run": true, "force": true, "json": true, "rollback": true, "stderr": true, "until-attention": true, "until-idle": true}
	for i := start; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			if i+1 < len(args) {
				return nil, "", errors.New("unexpected positional arguments")
			}
			terminator = i
			break
		}
		if !strings.HasPrefix(arg, "-") {
			return nil, "", fmt.Errorf("unexpected positional argument %q", arg)
		}
		name := strings.TrimLeft(arg, "-")
		parts := strings.SplitN(name, "=", 2)
		name = parts[0]
		if name == option {
			if found {
				return nil, "", fmt.Errorf("--%s must occur exactly once", option)
			}
			found = true
			if len(parts) == 2 {
				value = parts[1]
				if canonical != "" {
					out[i] = "--" + option + "=" + canonical
				}
			} else {
				if i+1 == len(args) {
					return nil, "", fmt.Errorf("--%s requires a value", option)
				}
				i++
				value = args[i]
				if canonical != "" {
					out[i] = canonical
				}
			}
		} else if len(parts) == 1 && !booleans[name] {
			i++
		}
	}
	if canonical != "" && !found {
		tail := append([]string(nil), out[terminator:]...)
		out = append(out[:terminator], "--"+option, canonical)
		out = append(out, tail...)
	}
	return out, value, nil
}
