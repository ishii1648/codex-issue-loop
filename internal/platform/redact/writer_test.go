package redact

import (
	"bytes"
	"encoding/json"
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

func TestConfiguredSecretIsRedactedFromTextAndJSON(t *testing.T) {
	secret := "custom-secret-value"
	if got := StringWithSecrets("before "+secret+" after", []string{secret}); strings.Contains(got, secret) {
		t.Fatalf("secret remained in text: %q", got)
	}
	data, err := Marshal(map[string]any{"nested": []any{"ghp_abcdefghijklmnopqrstuvwxyz123456", secret}, "id": json.Number("9007199254740993")}, []string{secret})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), secret) || strings.Contains(string(data), "ghp_") || !strings.Contains(string(data), "9007199254740993") {
		t.Fatalf("unsafe sanitized JSON: %s", data)
	}
}

func TestLineWriterRedactsPrivateKeyBlock(t *testing.T) {
	var output bytes.Buffer
	w := NewLineWriter(&output)
	_, _ = w.Write([]byte("before\n-----BEGIN PRIVATE KEY-----\nsecret-key-material\n-----END PRIVATE KEY-----\nafter\n"))
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), "secret-key-material") || !strings.Contains(output.String(), "[REDACTED PRIVATE KEY]") || !strings.Contains(output.String(), "after") {
		t.Fatalf("output=%q", output.String())
	}
}
