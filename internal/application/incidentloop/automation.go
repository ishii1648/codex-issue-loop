package incidentloop

import (
	"context"
	"errors"
)

type Automation struct {
	Collector StateEventCollector
	Pipeline  Pipeline
}

type AutomationReport struct {
	Version          int       `json:"version"`
	CollectedSignals int       `json:"collected_signals"`
	Run              RunReport `json:"run"`
}

func (a Automation) RunOnce(ctx context.Context) (AutomationReport, error) {
	collected, err := a.Collector.Collect()
	if err != nil {
		return AutomationReport{Version: SchemaVersion}, err
	}
	report, err := a.Pipeline.RunOnce(ctx)
	return AutomationReport{Version: SchemaVersion, CollectedSignals: collected, Run: report}, err
}

func (a Automation) RunIncidentOnce(ctx context.Context) error {
	_, err := a.RunOnce(ctx)
	if errors.Is(err, ErrAlreadyRunning) {
		return nil
	}
	return err
}
