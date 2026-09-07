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

func TestPrivateKeyFormats(t *testing.T) {
	for _, format := range []string{"PRIVATE KEY", "RSA PRIVATE KEY", "EC PRIVATE KEY", "OPENSSH PRIVATE KEY", "ENCRYPTED PRIVATE KEY", "DSA PRIVATE KEY", "PGP PRIVATE KEY BLOCK"} {
		t.Run(format, func(t *testing.T) {
			header := "-----BEGIN " + format + "-----"
			block := header + "\nsynthetic-key-material\n-----END " + format + "-----"
			t.Run("String", func(t *testing.T) {
				if got := String("before\n" + block + "\nafter\n"); got != "before\n[REDACTED]\nafter\n" {
					t.Fatal("private key block was not fully redacted or surrounding text changed")
				}
				if got := String(header); got != "[REDACTED]" {
					t.Fatal("private key header was not redacted")
				}
			})
			t.Run("Marshal", func(t *testing.T) {
				data, err := Marshal(map[string]string{"key": block}, nil)
				if err != nil {
					t.Fatal(err)
				}
				if string(data) != `{"key":"[REDACTED]"}` {
					t.Fatal("private key block was not fully redacted in JSON")
				}
			})
			t.Run("LineWriter", func(t *testing.T) {
				var output bytes.Buffer
				w := NewLineWriter(&output)
				for _, b := range []byte("before\n" + block + "\nafter\n") {
					if _, err := w.Write([]byte{b}); err != nil {
						t.Fatal(err)
					}
				}
				if err := w.Flush(); err != nil {
					t.Fatal(err)
				}
				if output.String() != "before\n[REDACTED PRIVATE KEY]\nafter\n" {
					t.Fatal("private key block was not fully redacted or surrounding text changed")
				}
			})
			t.Run("Flush", func(t *testing.T) {
				var output bytes.Buffer
				w := NewLineWriter(&output)
				if _, err := w.Write([]byte(header + " synthetic-key-material")); err != nil {
					t.Fatal(err)
				}
				if err := w.Flush(); err != nil {
					t.Fatal(err)
				}
				if output.String() != "[REDACTED PRIVATE KEY]" {
					t.Fatal("buffered private key was not fully redacted")
				}
			})
		})
	}
}
