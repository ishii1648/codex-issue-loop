package statusapi

import (
	"encoding/json"
	"log"
	"net/http"
	"sort"
	"sync/atomic"
	"time"

	"github.com/ishii1648/codex-issue-loop/internal/adapter/state"
)

type Execution struct {
	IssueNumber int    `json:"issue_number"`
	RunID       string `json:"run_id"`
	Generation  uint64 `json:"generation"`
}
type Issue struct {
	Execution
	Status      string     `json:"status"`
	HumanReason string     `json:"human_reason"`
	ReasonCode  string     `json:"reason_code"`
	RetryAfter  *time.Time `json:"retry_after"`
	UpdatedAt   time.Time  `json:"updated_at"`
}
type Supervisor struct {
	State       string     `json:"state"`
	PID         int        `json:"pid"`
	StartedAt   time.Time  `json:"started_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	FailureKind string     `json:"failure_kind"`
	RetryAfter  *time.Time `json:"retry_after"`
}
type Response struct {
	APIVersion       int        `json:"api_version"`
	Repository       string     `json:"repository"`
	RepoID           string     `json:"repo_id"`
	RuntimeID        string     `json:"runtime_id"`
	RuntimeStartedAt time.Time  `json:"runtime_started_at"`
	RuntimePhase     string     `json:"runtime_phase"`
	Revision         uint64     `json:"snapshot_revision"`
	ObservedAt       time.Time  `json:"observed_at"`
	Supervisor       Supervisor `json:"saved_supervisor"`
	ActiveExecution  *Execution `json:"saved_active_execution"`
	Issues           []Issue    `json:"issues"`
}
type Handler struct {
	Store      state.Store
	Repository string
	RuntimeID  string
	StartedAt  time.Time
	AutoMerge  bool
	Ready      atomic.Bool
	logger     *log.Logger
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	code, status := "", 0
	if r.URL.Path != "/v1/status" {
		code, status = "unsupported_endpoint", http.StatusNotFound
	} else if r.Method != http.MethodGet {
		code, status = "method_not_allowed", http.StatusMethodNotAllowed
		w.Header().Set("Allow", "GET")
	}
	if code != "" {
		h.writeError(w, status, code)
		return
	}
	s, err := h.Store.ReadStatusSnapshot()
	if err != nil {
		h.writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	phase := "starting"
	if h.Ready.Load() {
		phase = "serving"
	}
	response := project(s, h.AutoMerge)
	response.Repository, response.RuntimeID, response.RuntimeStartedAt, response.RuntimePhase = h.Repository, h.RuntimeID, h.StartedAt, phase
	response.ObservedAt = time.Now().UTC()
	h.writeJSON(w, response)
}
func (h *Handler) writeError(w http.ResponseWriter, status int, code string) {
	w.WriteHeader(status)
	h.writeJSON(w, struct {
		APIVersion int    `json:"api_version"`
		Code       string `json:"error_code"`
	}{1, code})
}
func project(s state.Snapshot, autoMerge bool) Response {
	response := Response{APIVersion: 1, RepoID: s.RepoID, Revision: s.StateRevision, Issues: []Issue{}}
	v := s.Supervisor
	response.Supervisor = Supervisor{string(v.State), v.PID, v.StartedAt, v.UpdatedAt, v.FailureKind, v.RetryAfter}
	if a := s.ActiveExecution; a != nil {
		response.ActiveExecution = &Execution{a.IssueNumber, a.RunID, a.Generation}
	}
	human := map[int]string{}
	for _, need := range s.HumanNeeds(autoMerge) {
		human[need.IssueNumber] = need.Reason
	}
	for _, item := range s.Issues {
		reason := item.FailureKind
		if item.Suspension != nil {
			reason = item.Suspension.ReasonCode
		}
		if item.Cancellation != nil {
			reason = item.Cancellation.Source
		}
		response.Issues = append(response.Issues, Issue{Execution{item.Number, item.RunID, item.Generation}, string(item.Status), human[item.Number], reason, item.RetryAfter, item.UpdatedAt})
	}
	for _, q := range s.QuarantinedIssues {
		response.Issues = append(response.Issues, Issue{Execution{q.IssueNumber, q.RunID, q.Generation}, "quarantined", human[q.IssueNumber], q.ReasonCode, nil, q.QuarantinedAt})
	}
	sort.Slice(response.Issues, func(i, j int) bool { return response.Issues[i].IssueNumber < response.Issues[j].IssueNumber })
	return response
}

func (h *Handler) writeJSON(w http.ResponseWriter, value any) {
	if err := json.NewEncoder(w).Encode(value); err != nil && h.logger != nil {
		h.logger.Printf("status API response: %v", err)
	}
}
