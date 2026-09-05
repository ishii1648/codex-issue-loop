package app

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/ishii1648/codex-issue-loop/monitor/internal/config"
	gh "github.com/ishii1648/codex-issue-loop/monitor/internal/github"
	"github.com/ishii1648/codex-issue-loop/monitor/internal/model"
	monitorruntime "github.com/ishii1648/codex-issue-loop/monitor/internal/monitor"
	"github.com/ishii1648/codex-issue-loop/monitor/internal/store"
)

var (
	Version = "0.1.0-dev"
	Commit  = "unknown"
)

type App struct {
	In       io.Reader
	Out      io.Writer
	Err      io.Writer
	Observer gh.Observer
	Now      func() time.Time
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
	if len(args) == 0 {
		a.usage()
		return 2
	}
	if args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		a.usage()
		return 0
	}
	if args[0] == "version" || args[0] == "--version" {
		if len(args) > 1 && args[1] == "--json" {
			_ = json.NewEncoder(a.Out).Encode(map[string]any{"version": Version, "commit": Commit, "target": runtime.GOOS + "/" + runtime.GOARCH, "monitor_schema_version": model.SchemaVersion})
		} else {
			fmt.Fprintf(a.Out, "agent-loop-monitor %s (%s)\n", Version, Commit)
		}
		return 0
	}
	var err error
	switch args[0] {
	case "run":
		err = a.runMonitor(ctx, args[1:])
	case "status":
		err = a.status(args[1:])
	case "history":
		err = a.history(args[1:])
	case "report":
		err = a.report(args[1:])
	case "install":
		err = a.install(args[1:])
	case "service":
		err = a.service(ctx, args[1:])
	default:
		fmt.Fprintf(a.Err, "unknown command %q\n", args[0])
		return 2
	}
	if err != nil {
		fmt.Fprintln(a.Err, err)
		return 1
	}
	return 0
}

func (a App) usage() {
	fmt.Fprintln(a.Out, `Usage: agent-loop-monitor <command> [options]

Commands:
  run       Observe every configured repository
  status    Show each repository's current availability state
  history   Show finalized and current state intervals
  report    Calculate demand availability and observation coverage
  install   Install this monitor binary into its independent state root
  service   Register, start, stop, restart, or inspect the monitor LaunchAgent
  version   Show build metadata`)
}

func commonFlags(name string, stderr io.Writer) (*flag.FlagSet, *string, *bool) {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(stderr)
	path := flags.String("config", "", "monitor configuration path")
	jsonOut := flags.Bool("json", false, "emit JSON")
	return flags, path, jsonOut
}

func (a App) runMonitor(ctx context.Context, args []string) error {
	flags, path, jsonOut := commonFlags("run", a.Err)
	once := flags.Bool("once", false, "poll each repository once")
	if err := flags.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(*path)
	if err != nil {
		return err
	}
	observer := a.Observer
	if observer == nil {
		observer = gh.CLI{Path: cfg.GitHubCLI}
	}
	runner := monitorruntime.Runner{Observer: observer, Store: store.Store{Root: cfg.StateDir}, ObservationTimeout: cfg.ObservationTimeout.Duration, Now: a.Now}
	poll := func() bool {
		failed := false
		for _, repo := range cfg.Repositories {
			snapshot, pollErr := runner.Poll(ctx, repo)
			if *jsonOut {
				_ = json.NewEncoder(a.Out).Encode(map[string]any{"repository": repo.Name, "state": snapshot.Current.Status, "observed_at": snapshot.LastObservationAt, "error": errorText(pollErr)})
			} else if pollErr != nil {
				fmt.Fprintf(a.Err, "%s: %v\n", repo.Name, pollErr)
			} else {
				fmt.Fprintf(a.Out, "%s %s\n", repo.Name, snapshot.Current.Status)
			}
			failed = failed || pollErr != nil
		}
		return failed
	}
	failed := poll()
	if *once {
		if failed {
			return errors.New("one or more repositories could not be observed")
		}
		return nil
	}
	ticker := time.NewTicker(cfg.PollInterval.Duration)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			poll()
		}
	}
}

func (a App) status(args []string) error {
	flags, path, jsonOut := commonFlags("status", a.Err)
	repository := flags.String("repo", "", "repository owner/name")
	if err := flags.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(*path)
	if err != nil {
		return err
	}
	repositories, err := selectRepos(cfg, *repository)
	if err != nil {
		return err
	}
	var snapshots []model.Snapshot
	for _, repo := range repositories {
		snapshot, loadErr := (store.Store{Root: cfg.StateDir}).Load(repo.Name)
		if loadErr != nil {
			return loadErr
		}
		if snapshot == nil {
			snapshots = append(snapshots, model.Snapshot{SchemaVersion: model.SchemaVersion, Repository: repo.Name, Current: model.Interval{Repository: repo.Name, Status: model.Unknown, Reason: "no observations recorded"}})
		} else {
			snapshots = append(snapshots, effectiveSnapshot(*snapshot, cfg.ObservationTimeout.Duration, a.now()))
		}
	}
	if *jsonOut {
		return json.NewEncoder(a.Out).Encode(map[string]any{"schema_version": model.SchemaVersion, "repositories": snapshots})
	}
	for _, snapshot := range snapshots {
		fmt.Fprintf(a.Out, "%s %s since %s\n", snapshot.Repository, snapshot.Current.Status, formatTime(snapshot.Current.StartedAt))
	}
	return nil
}

func (a App) history(args []string) error {
	flags, path, jsonOut := commonFlags("history", a.Err)
	repository := flags.String("repo", "", "repository owner/name")
	fromValue := flags.String("from", "", "RFC3339 interval start")
	toValue := flags.String("to", "", "RFC3339 interval end")
	if err := flags.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(*path)
	if err != nil {
		return err
	}
	from, to, err := parseRange(*fromValue, *toValue, false, a.now())
	if err != nil {
		return err
	}
	result := map[string][]model.Interval{}
	repositories, err := selectRepos(cfg, *repository)
	if err != nil {
		return err
	}
	for _, repo := range repositories {
		storage := store.Store{Root: cfg.StateDir}
		intervals, loadErr := effectiveIntervals(storage, repo.Name, cfg.ObservationTimeout.Duration, to)
		if loadErr != nil {
			return loadErr
		}
		for _, interval := range intervals {
			end := interval.EndedAt
			if end.IsZero() {
				end = to
			}
			if end.After(from) && interval.StartedAt.Before(to) {
				result[repo.Name] = append(result[repo.Name], interval)
			}
		}
	}
	if *jsonOut {
		return json.NewEncoder(a.Out).Encode(map[string]any{"schema_version": model.SchemaVersion, "from": from, "to": to, "repositories": result})
	}
	for repo, intervals := range result {
		for _, interval := range intervals {
			fmt.Fprintf(a.Out, "%s %s %s %s\n", repo, interval.Status, formatTime(interval.StartedAt), formatTime(interval.EndedAt))
		}
	}
	return nil
}

func (a App) report(args []string) error {
	flags, path, jsonOut := commonFlags("report", a.Err)
	repository := flags.String("repo", "", "repository owner/name")
	fromValue := flags.String("from", "", "required RFC3339 report start")
	toValue := flags.String("to", "", "RFC3339 report end")
	if err := flags.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(*path)
	if err != nil {
		return err
	}
	from, to, err := parseRange(*fromValue, *toValue, true, a.now())
	if err != nil {
		return err
	}
	repositories, err := selectRepos(cfg, *repository)
	if err != nil {
		return err
	}
	var reports []model.Report
	for _, repo := range repositories {
		storage := store.Store{Root: cfg.StateDir}
		intervals, loadErr := effectiveIntervals(storage, repo.Name, cfg.ObservationTimeout.Duration, to)
		if loadErr != nil {
			return loadErr
		}
		reports = append(reports, model.BuildReport(repo.Name, intervals, from, to))
	}
	if *jsonOut {
		return json.NewEncoder(a.Out).Encode(map[string]any{"schema_version": model.SchemaVersion, "reports": reports})
	}
	for _, report := range reports {
		availability := "n/a"
		if report.DemandAvailability != nil {
			availability = fmt.Sprintf("%.6f", *report.DemandAvailability)
		}
		fmt.Fprintf(a.Out, "%s availability=%s coverage=%.6f\n", report.Repository, availability, report.ObservationCoverage)
	}
	return nil
}

func (a App) install(args []string) error {
	flags, path, jsonOut := commonFlags("install", a.Err)
	if err := flags.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(*path)
	if err != nil {
		return err
	}
	binary, err := installBinary(cfg.StateDir)
	if err != nil {
		return err
	}
	return a.output(*jsonOut, map[string]any{"binary": binary, "version": Version, "commit": Commit})
}

func (a App) service(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("service action is required: register, start, stop, restart, or status")
	}
	action := args[0]
	flags, path, jsonOut := commonFlags("service "+action, a.Err)
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	cfg, err := config.Load(*path)
	if err != nil {
		return err
	}
	if action == "register" {
		plist, registerErr := registerService(cfg)
		if registerErr != nil {
			return registerErr
		}
		return a.output(*jsonOut, map[string]any{"label": serviceLabel, "plist": plist})
	}
	status, err := controlService(ctx, action)
	if err != nil {
		return err
	}
	return a.output(*jsonOut, status)
}

func (a App) output(jsonOut bool, value any) error {
	if jsonOut {
		return json.NewEncoder(a.Out).Encode(value)
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err == nil {
		fmt.Fprintln(a.Out, string(data))
	}
	return err
}

func (a App) now() time.Time {
	if a.Now != nil {
		return a.Now().UTC()
	}
	return time.Now().UTC()
}

func selectRepos(cfg config.Config, name string) ([]config.Repository, error) {
	if name == "" {
		return cfg.Repositories, nil
	}
	for _, repo := range cfg.Repositories {
		if strings.EqualFold(repo.Name, name) {
			return []config.Repository{repo}, nil
		}
	}
	return nil, fmt.Errorf("repository %q is not configured", name)
}

func parseRange(fromValue, toValue string, requireFrom bool, now time.Time) (time.Time, time.Time, error) {
	to := now.UTC()
	var err error
	if toValue != "" {
		to, err = time.Parse(time.RFC3339, toValue)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("invalid --to: %w", err)
		}
	}
	from := time.Unix(0, 0).UTC()
	if fromValue != "" {
		from, err = time.Parse(time.RFC3339, fromValue)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("invalid --from: %w", err)
		}
	} else if requireFrom {
		return time.Time{}, time.Time{}, errors.New("--from is required")
	}
	if !to.After(from) {
		return time.Time{}, time.Time{}, errors.New("--to must be after --from")
	}
	return from.UTC(), to.UTC(), nil
}

func formatTime(value time.Time) string {
	if value.IsZero() {
		return "unknown"
	}
	return value.UTC().Format(time.RFC3339)
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func effectiveSnapshot(snapshot model.Snapshot, timeout time.Duration, now time.Time) model.Snapshot {
	expiredAt := snapshot.LastObservationAt.Add(timeout)
	if timeout > 0 && now.After(expiredAt) && snapshot.Current.Status != model.Unknown {
		snapshot.Current = model.Interval{ID: fmt.Sprintf("%s:%s:%d", snapshot.Repository, model.Unknown, expiredAt.UnixNano()), Repository: snapshot.Repository, Status: model.Unknown, StartedAt: expiredAt, Reason: "monitor observation history has a gap"}
		snapshot.LastError = "monitor observation history has a gap"
	}
	return snapshot
}

func effectiveIntervals(storage store.Store, repository string, timeout time.Duration, to time.Time) ([]model.Interval, error) {
	intervals, err := storage.AllIntervals(repository)
	if err != nil {
		return nil, err
	}
	snapshot, err := storage.Load(repository)
	if err != nil || snapshot == nil || timeout <= 0 {
		return intervals, err
	}
	expiredAt := snapshot.LastObservationAt.Add(timeout)
	if !to.After(expiredAt) || snapshot.Current.Status == model.Unknown || len(intervals) == 0 {
		return intervals, nil
	}
	last := &intervals[len(intervals)-1]
	if last.EndedAt.IsZero() && expiredAt.After(last.StartedAt) {
		last.EndedAt = expiredAt
		intervals = append(intervals, model.Interval{ID: fmt.Sprintf("%s:%s:%d", repository, model.Unknown, expiredAt.UnixNano()), Repository: repository, Status: model.Unknown, StartedAt: expiredAt, Reason: "monitor observation history has a gap"})
	}
	return intervals, nil
}
