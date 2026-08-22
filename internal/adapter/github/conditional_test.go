package github

import (
	"net/http"
	"testing"
)

func TestParseIncludedConditionalResponse(t *testing.T) {
	response := "HTTP/2 304 Not Modified\r\nETag: W/\"fixture\"\r\nLast-Modified: Sun, 16 Aug 2026 00:00:00 GMT\r\nX-RateLimit-Remaining: 4999\r\n\r\n"
	result, body, err := parseIncludedResponse([]byte(response))
	if err != nil {
		t.Fatal(err)
	}
	if result.StatusCode != http.StatusNotModified || result.ETag != `W/"fixture"` || result.RateRemaining != "4999" || len(body) != 0 {
		t.Fatalf("result=%+v body=%q", result, body)
	}
}
