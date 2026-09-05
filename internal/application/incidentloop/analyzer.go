package incidentloop

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"time"

	"github.com/ishii1648/codex-issue-loop/internal/platform/redact"
)

const maxAnalyzerOutput = 256 * 1024

// CommandAnalyzer executes an argv-only adapter contract: the process receives
// one sanitized EvidenceBundle on stdin and must emit one AIAnalysis JSON value
// on stdout. No shell expansion or inherited prompt content is involved.
type CommandAnalyzer struct {
	Path    string
	Args    []string
	Timeout time.Duration
	Secrets []string
}

func (a CommandAnalyzer) Analyze(parent context.Context, bundle EvidenceBundle) (AIAnalysis, error) {
	if a.Path == "" || a.Timeout <= 0 {
		return AIAnalysis{}, errors.New("analyzer command and positive timeout are required")
	}
	input, err := redact.Marshal(bundle, a.Secrets)
	if err != nil {
		return AIAnalysis{}, err
	}
	ctx, cancel := context.WithTimeout(parent, a.Timeout)
	defer cancel()
	command := exec.CommandContext(ctx, a.Path, a.Args...)
	command.Stdin = bytes.NewReader(append(input, '\n'))
	stdout := &boundedBuffer{limit: maxAnalyzerOutput}
	stderr := &boundedBuffer{limit: 32 * 1024}
	command.Stdout = stdout
	command.Stderr = stderr
	if err := command.Run(); err != nil {
		if ctx.Err() != nil {
			return AIAnalysis{}, fmt.Errorf("AI analyzer timeout: %w", ctx.Err())
		}
		return AIAnalysis{}, fmt.Errorf("AI analyzer failed: %w: %s", err, redact.StringWithSecrets(stderr.String(), a.Secrets))
	}
	if stdout.overflow {
		return AIAnalysis{}, errors.New("AI analyzer output exceeded 256 KiB")
	}
	var analysis AIAnalysis
	decoder := json.NewDecoder(bytes.NewReader(stdout.Bytes()))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&analysis); err != nil {
		return AIAnalysis{}, fmt.Errorf("decode AI analyzer output: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return AIAnalysis{}, errors.New("AI analyzer emitted trailing JSON content")
	}
	return analysis, nil
}

type boundedBuffer struct {
	bytes.Buffer
	limit    int
	overflow bool
}

func (b *boundedBuffer) Write(data []byte) (int, error) {
	written := len(data)
	remaining := b.limit - b.Len()
	if remaining <= 0 {
		b.overflow = true
		return written, nil
	}
	if len(data) > remaining {
		b.overflow = true
		data = data[:remaining]
	}
	_, _ = b.Buffer.Write(data)
	return written, nil
}
