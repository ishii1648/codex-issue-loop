package model

import "time"

type Report struct {
	SchemaVersion       int                `json:"schema_version"`
	Repository          string             `json:"repository"`
	From                time.Time          `json:"from"`
	To                  time.Time          `json:"to"`
	DurationsSeconds    map[Status]float64 `json:"durations_seconds"`
	DemandAvailability  *float64           `json:"demand_availability"`
	ObservationCoverage float64            `json:"observation_coverage"`
}

func BuildReport(repository string, intervals []Interval, from, to time.Time) Report {
	durations := map[Status]float64{Idle: 0, Healthy: 0, Down: 0, Unknown: 0}
	for _, interval := range intervals {
		start := interval.StartedAt
		end := interval.EndedAt
		if end.IsZero() || end.After(to) {
			end = to
		}
		if start.Before(from) {
			start = from
		}
		if end.After(start) {
			durations[interval.Status] += end.Sub(start).Seconds()
		}
	}
	total := to.Sub(from).Seconds()
	known := durations[Idle] + durations[Healthy] + durations[Down]
	coverage := 0.0
	if total > 0 {
		coverage = known / total
		if coverage > 1 {
			coverage = 1
		}
	}
	demand := durations[Healthy] + durations[Down]
	var availability *float64
	if demand > 0 {
		value := durations[Healthy] / demand
		availability = &value
	}
	return Report{SchemaVersion: SchemaVersion, Repository: repository, From: from.UTC(), To: to.UTC(), DurationsSeconds: durations, DemandAvailability: availability, ObservationCoverage: coverage}
}
