package app

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"reflect"
	"strconv"
	"time"

	"github.com/ishii1648/codex-issue-loop/monitor/internal/config"
	"github.com/ishii1648/codex-issue-loop/monitor/internal/model"
	"github.com/ishii1648/codex-issue-loop/monitor/internal/store"
)

//go:embed dashboard.html
var dashboardPage []byte

func (a App) serve(ctx context.Context, args []string) error {
	flags, path, _ := commonFlags("serve", a.Err)
	address := flags.String("listen", "127.0.0.1:19110", "loopback listen address")
	if err := flags.Parse(args); err != nil {
		return err
	}
	host, _, err := net.SplitHostPort(*address)
	if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
		return errors.New("--listen must use a loopback IP address")
	}
	cfg, err := config.Load(*path)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", *address)
	if err != nil {
		return err
	}
	server := &http.Server{Handler: a.monitorHandler(cfg), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: time.Minute}
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = server.Shutdown(shutdown)
		case <-done:
		}
	}()
	err = server.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (a App) monitorHandler(cfg config.Config) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		host, _, err := net.SplitHostPort(r.Host)
		if err != nil {
			host = r.Host
		}
		ip := net.ParseIP(host)
		if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			http.Error(w, "loopback Host required", http.StatusForbidden)
			return
		}
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			http.Error(w, "read-only endpoint", http.StatusMethodNotAllowed)
			return
		}
		now := a.now()
		var out bytes.Buffer
		reader := a
		reader.Out = &out
		reader.Now = func() time.Time { return now }
		args := []string{"--config", cfg.Path, "--json"}
		query := r.URL.Query()
		switch r.URL.Path {
		case "/api/status", "/api/details", "/api/history", "/api/report", "/api/timeline":
			allowed := map[string]bool{"repo": true}
			if r.URL.Path == "/api/status" || r.URL.Path == "/api/details" {
				allowed["at"] = true
			} else {
				allowed["from"] = true
				allowed["to"] = true
			}
			for key, values := range query {
				if !allowed[key] || len(values) != 1 {
					http.Error(w, "invalid query parameter", 400)
					return
				}
				args = append(args, "--"+key, values[0])
			}
			switch r.URL.Path {
			case "/api/status", "/api/details":
				err = reader.status(args)
				if err == nil && r.URL.Path == "/api/details" {
					err = flattenDetails(&out)
				}
			case "/api/history", "/api/timeline":
				err = reader.history(args)
				if err == nil && r.URL.Path == "/api/timeline" {
					var repos []config.Repository
					repos, err = selectRepos(cfg, query.Get("repo"))
					if err == nil {
						err = normalizeTimeline(&out, repos)
					}
				}
			case "/api/report":
				err = reader.report(args)
			}
			w.Header().Set("Content-Type", "application/json")
		case "/":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write(dashboardPage)
			return
		case "/api/freshness":
			client := &http.Client{Timeout: 5 * time.Second}
			req, requestErr := http.NewRequestWithContext(r.Context(), http.MethodGet, "http://127.0.0.1:13000/api/datasources/proxy/uid/monitor-prometheus/api/v1/query?query=agent_loop_monitor_reference_time_seconds%20and%20on()%20(up%7Bjob%3D%22agent-loop-monitor%22%7D%20%3D%3D%201)", nil)
			if requestErr != nil {
				err = requestErr
				break
			}
			response, requestErr := client.Do(req)
			if requestErr != nil {
				err = requestErr
				break
			}
			defer response.Body.Close()
			if response.StatusCode != http.StatusOK {
				err = fmt.Errorf("Grafana/Prometheus freshness query: HTTP %d", response.StatusCode)
				break
			}
			_, err = io.Copy(&out, io.LimitReader(response.Body, 1<<20))
			w.Header().Set("Content-Type", "application/json")
		case "/metrics":
			err = reader.dashboardMetrics(cfg, now.Truncate(time.Second))
			w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		default:
			http.NotFound(w, r)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write(out.Bytes())
	})
}

func (a App) dashboardMetrics(cfg config.Config, at time.Time) error {
	var data bytes.Buffer
	reader := a
	reader.Out = &data
	if err := reader.status([]string{"--config", cfg.Path, "--json", "--at", at.Format(time.RFC3339Nano)}); err != nil {
		return err
	}
	var result struct {
		Repositories []model.Snapshot `json:"repositories"`
	}
	if err := json.Unmarshal(data.Bytes(), &result); err != nil {
		return err
	}
	fmt.Fprintln(a.Out, "# TYPE agent_loop_monitor_reference_time_seconds gauge")
	fmt.Fprintf(a.Out, "agent_loop_monitor_reference_time_seconds %d\n", at.Unix())
	for _, metric := range []string{"state", "state_since_seconds", "state_duration_seconds", "last_observation_seconds", "demand_availability", "observation_coverage"} {
		fmt.Fprintf(a.Out, "# TYPE agent_loop_monitor_%s gauge\n", metric)
	}
	for _, snapshot := range result.Repositories {
		repo := strconv.Quote(snapshot.Repository)
		code := map[model.Status]int{model.Unknown: 0, model.Healthy: 1, model.Down: 2, model.Idle: 3}[snapshot.Current.Status]
		fmt.Fprintf(a.Out, "agent_loop_monitor_state{repository=%s} %d\n", repo, code)
		since, duration, observed := "NaN", "NaN", "NaN"
		if !snapshot.Current.StartedAt.IsZero() {
			since = strconv.FormatInt(snapshot.Current.StartedAt.Unix(), 10)
			duration = strconv.FormatFloat(at.Sub(snapshot.Current.StartedAt).Seconds(), 'f', -1, 64)
		}
		if !snapshot.LastObservationAt.IsZero() {
			observed = strconv.FormatInt(snapshot.LastObservationAt.Unix(), 10)
		}
		fmt.Fprintf(a.Out, "agent_loop_monitor_state_since_seconds{repository=%s} %s\nagent_loop_monitor_state_duration_seconds{repository=%s} %s\nagent_loop_monitor_last_observation_seconds{repository=%s} %s\n", repo, since, repo, duration, repo, observed)
		intervals, err := effectiveIntervals(store.Store{Root: cfg.StateDir}, snapshot.Repository, cfg.ObservationTimeout.Duration, at)
		if err != nil {
			return err
		}
		latest, err := (store.Store{Root: cfg.StateDir}).Load(snapshot.Repository)
		if err != nil {
			return err
		}
		if latest != nil {
			latestEffective := effectiveSnapshot(*latest, cfg.ObservationTimeout.Duration, at)
			if !reflect.DeepEqual(latestEffective, snapshot) {
				return errors.New("monitor state changed during read")
			}
		} else if !snapshot.LastObservationAt.IsZero() {
			return errors.New("monitor state disappeared during read")
		}
		for _, days := range []int{1, 7, 30} {
			report := model.BuildReport(snapshot.Repository, intervals, at.Add(-time.Duration(days)*24*time.Hour), at)
			availability := "-1"
			if report.DemandAvailability != nil {
				availability = strconv.FormatFloat(*report.DemandAvailability, 'f', -1, 64)
			}
			fmt.Fprintf(a.Out, "agent_loop_monitor_demand_availability{repository=%s,window=%q} %s\nagent_loop_monitor_observation_coverage{repository=%s,window=%q} %g\n", repo, fmt.Sprintf("%dd", days), availability, repo, fmt.Sprintf("%dd", days), report.ObservationCoverage)
		}
	}
	return nil
}

func normalizeTimeline(data *bytes.Buffer, repositories []config.Repository) error {
	var history struct {
		From         time.Time                   `json:"from"`
		To           time.Time                   `json:"to"`
		Repositories map[string][]model.Interval `json:"repositories"`
	}
	if err := json.Unmarshal(data.Bytes(), &history); err != nil {
		return err
	}
	for _, repo := range repositories {
		intervals := history.Repositories[repo.Name]
		rows := []model.Interval{}
		cursor := history.From
		for _, interval := range intervals {
			if interval.DecisionVersion != model.DecisionVersion {
				interval.Reason = fmt.Sprintf("legacy decision contract (status=%s): %s", interval.Status, interval.Reason)
				interval.Status = model.Unknown
			}
			if interval.StartedAt.Before(cursor) {
				interval.StartedAt = cursor
			}
			if interval.EndedAt.IsZero() || interval.EndedAt.After(history.To) {
				interval.EndedAt = history.To
			}
			if interval.StartedAt.After(cursor) {
				rows = append(rows, model.Interval{DecisionVersion: model.DecisionVersion, Repository: repo.Name, Status: model.Unknown, StartedAt: cursor, EndedAt: interval.StartedAt, Reason: "no observations recorded"})
			}
			if interval.EndedAt.After(interval.StartedAt) {
				rows = append(rows, interval)
				cursor = interval.EndedAt
			}
		}
		if cursor.Before(history.To) {
			rows = append(rows, model.Interval{DecisionVersion: model.DecisionVersion, Repository: repo.Name, Status: model.Unknown, StartedAt: cursor, EndedAt: history.To, Reason: "no observations recorded"})
		}
		history.Repositories[repo.Name] = rows
	}
	data.Reset()
	return json.NewEncoder(data).Encode(history)
}

func flattenDetails(data *bytes.Buffer) error {
	var result struct {
		Repositories []model.Snapshot `json:"repositories"`
	}
	if err := json.Unmarshal(data.Bytes(), &result); err != nil {
		return err
	}
	type row struct {
		Status       model.Status `json:"status"`
		Detail       string       `json:"detail"`
		Issue        *int         `json:"issue"`
		Deadline     *time.Time   `json:"deadline"`
		ItemDeadline *time.Time   `json:"item_deadline"`
	}
	rows := []row{}
	for _, snapshot := range result.Repositories {
		base := row{Status: snapshot.Current.Status, Detail: snapshot.Current.Reason}
		if snapshot.LastError != "" {
			if base.Detail != "" {
				base.Detail += "\n"
			}
			base.Detail += "観測エラー: " + snapshot.LastError
		}
		if !snapshot.QueueDeadline.IsZero() {
			base.Deadline = &snapshot.QueueDeadline
		}
		if len(snapshot.Queue) == 0 {
			rows = append(rows, base)
		}
		for _, item := range snapshot.Queue {
			detail := base
			detail.Issue = &item.Number
			if !item.Deadline.IsZero() {
				detail.ItemDeadline = &item.Deadline
			}
			rows = append(rows, detail)
		}
	}
	data.Reset()
	return json.NewEncoder(data).Encode(map[string]any{"rows": rows})
}
