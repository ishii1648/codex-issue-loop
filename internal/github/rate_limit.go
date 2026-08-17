package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var resetHeaderPattern = regexp.MustCompile(`(?i)x-ratelimit-reset\s*[:=]\s*([0-9]+)`)

// RateLimitError identifies a GitHub primary quota exhaustion. ResetAt is
// populated from a response header when available, or by the REST rate_limit
// endpoint. Callers must not probe GraphQL while waiting for this deadline.
type RateLimitError struct {
	Resource string
	ResetAt  time.Time
	Source   string
	Err      error
}

func (e *RateLimitError) Error() string { return e.Err.Error() }
func (e *RateLimitError) Unwrap() error { return e.Err }

func AsRateLimit(err error) (*RateLimitError, bool) {
	var limited *RateLimitError
	ok := errors.As(err, &limited)
	return limited, ok
}

func (c CLI) commandError(ctx context.Context, path, operation string, commandErr error, output []byte) error {
	base := fmt.Errorf("%s: %w: %s", operation, commandErr, c.safe(output))
	resource, resetAt, source, limited := primaryRateLimit(output)
	if !limited {
		return base
	}
	if resetAt.IsZero() && ctx.Err() == nil {
		if observed, ok := c.observeRateLimitReset(ctx, path, resource); ok {
			resetAt = observed
			source = "rest-rate-limit"
		}
	}
	return &RateLimitError{Resource: resource, ResetAt: resetAt, Source: source, Err: base}
}

func primaryRateLimit(output []byte) (string, time.Time, string, bool) {
	text := strings.ToLower(string(output))
	if strings.Contains(text, "secondary rate limit") || strings.Contains(text, "abuse detection") {
		return "", time.Time{}, "", false
	}
	if !strings.Contains(text, "rate limit exceeded") && !strings.Contains(text, "rate limit already exceeded") && !strings.Contains(text, "rate limit exhaustion") {
		return "", time.Time{}, "", false
	}
	resource := "core"
	if strings.Contains(text, "graphql") {
		resource = "graphql"
	}
	if match := resetHeaderPattern.FindSubmatch(output); len(match) == 2 {
		seconds, err := strconv.ParseInt(string(match[1]), 10, 64)
		if err == nil && seconds > 0 {
			return resource, time.Unix(seconds, 0).UTC(), "x-ratelimit-reset", true
		}
	}
	return resource, time.Time{}, "", true
}

func (c CLI) observeRateLimitReset(ctx context.Context, path, resource string) (time.Time, bool) {
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(probeCtx, path, "api", "/rate_limit").Output()
	if err != nil {
		return time.Time{}, false
	}
	var response struct {
		Resources map[string]struct {
			Reset int64 `json:"reset"`
		} `json:"resources"`
	}
	if err := json.Unmarshal(out, &response); err != nil {
		return time.Time{}, false
	}
	value, ok := response.Resources[resource]
	if !ok || value.Reset <= 0 {
		return time.Time{}, false
	}
	return time.Unix(value.Reset, 0).UTC(), true
}
