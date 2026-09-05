package model

import "time"

const SchemaVersion = 1

type Status string

const (
	Idle    Status = "IDLE"
	Healthy Status = "HEALTHY"
	Down    Status = "DOWN"
	Unknown Status = "UNKNOWN"
)

type Phase string

const (
	Ready   Phase = "ready"
	Running Phase = "running"
)

type EventKind string

const (
	ReadyLabeled     EventKind = "ready_labeled"
	RunningLabeled   EventKind = "running_labeled"
	ReadyUnlabeled   EventKind = "ready_unlabeled"
	RunningUnlabeled EventKind = "running_unlabeled"
	QueueExited      EventKind = "queue_exited"
)

type QueueEvent struct {
	ID          int64
	IssueNumber int
	Kind        EventKind
	At          time.Time
}

type QueueItem struct {
	Number     int       `json:"number"`
	Phase      Phase     `json:"phase"`
	PhaseSince time.Time `json:"phase_since"`
	Deadline   time.Time `json:"deadline"`
}

type Observation struct {
	Repository        string        `json:"repository"`
	ObservedAt        time.Time     `json:"observed_at"`
	Items             []QueueItem   `json:"queue"`
	Events            []QueueEvent  `json:"-"`
	Cursor            int64         `json:"event_cursor,omitempty"`
	CursorInitialized bool          `json:"-"`
	AcceptanceTimeout time.Duration `json:"-"`
	ProcessingTimeout time.Duration `json:"-"`
	Error             string        `json:"error,omitempty"`
}

type Interval struct {
	ID         string    `json:"id"`
	Repository string    `json:"repository"`
	Status     Status    `json:"status"`
	StartedAt  time.Time `json:"started_at"`
	EndedAt    time.Time `json:"ended_at,omitempty"`
	Reason     string    `json:"reason,omitempty"`
}

type Snapshot struct {
	SchemaVersion          int         `json:"schema_version"`
	Repository             string      `json:"repository"`
	Current                Interval    `json:"current"`
	Queue                  []QueueItem `json:"queue,omitempty"`
	QueuePhase             Phase       `json:"queue_phase,omitempty"`
	QueuePhaseSince        time.Time   `json:"queue_phase_since,omitempty"`
	QueueDeadline          time.Time   `json:"queue_deadline,omitempty"`
	LastObservationAt      time.Time   `json:"last_observation_at"`
	LastSuccessAt          time.Time   `json:"last_success_at,omitempty"`
	EventCursor            int64       `json:"event_cursor,omitempty"`
	EventCursorInitialized bool        `json:"event_cursor_initialized,omitempty"`
	LastError              string      `json:"last_error,omitempty"`
}
