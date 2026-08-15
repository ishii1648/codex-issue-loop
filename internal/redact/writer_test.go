package redact

import (
	"bytes"
	"strings"
	"testing"
)

func TestLineWriterRedactsCredentialAcrossWrites(t *testing.T) {
	var output bytes.Buffer
	w := NewLineWriter(&output)
	_, _ = w.Write([]byte("token ghp_abcdefghijkl"))
	_, _ = w.Write([]byte("mnopqrstuvwxyz123456\nnext\n"))
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), "ghp_") || !strings.Contains(output.String(), "[REDACTED]") {
		t.Fatalf("output=%q", output.String())
	}
}
