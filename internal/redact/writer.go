package redact

import (
	"bytes"
	"encoding/json"
	"io"
	"regexp"
	"sort"
	"strings"
	"sync"
)

var patterns = []*regexp.Regexp{
	regexp.MustCompile(`(?s)-----BEGIN (?:RSA |EC |OPENSSH )?PRIVATE KEY-----.*?-----END (?:RSA |EC |OPENSSH )?PRIVATE KEY-----`),
	regexp.MustCompile(`\bgh[opusr]_[A-Za-z0-9_]{20,}\b`),
	regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{20,}\b`),
	regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{20,}\b`),
	regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._~+/-]{20,}=*`),
	regexp.MustCompile(`\bAKIA[A-Z0-9]{16}\b`),
	regexp.MustCompile(`(?i)-----BEGIN (?:RSA |EC |OPENSSH )?PRIVATE KEY-----`),
}

func String(value string) string {
	return StringWithSecrets(value, nil)
}

func StringWithSecrets(value string, secrets []string) string {
	for _, pattern := range patterns {
		value = pattern.ReplaceAllString(value, "[REDACTED]")
	}
	secrets = append([]string(nil), secrets...)
	sort.Slice(secrets, func(i, j int) bool { return len(secrets[i]) > len(secrets[j]) })
	for _, secret := range secrets {
		if len(secret) >= 4 {
			value = strings.ReplaceAll(value, secret, "[REDACTED]")
		}
	}
	return value
}

// Marshal sanitizes every JSON string before it can cross a durable or
// externally visible boundary. UseNumber avoids changing large integer IDs.
func Marshal(value any, secrets []string) ([]byte, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var generic any
	if err := decoder.Decode(&generic); err != nil {
		return nil, err
	}
	return json.Marshal(sanitize(generic, secrets))
}

func sanitize(value any, secrets []string) any {
	switch current := value.(type) {
	case string:
		return StringWithSecrets(current, secrets)
	case []any:
		for index := range current {
			current[index] = sanitize(current[index], secrets)
		}
	case map[string]any:
		for key := range current {
			current[key] = sanitize(current[key], secrets)
		}
	}
	return value
}

// LineWriter buffers partial lines so credentials split across subprocess
// writes are still redacted before reaching disk.
type LineWriter struct {
	mu      sync.Mutex
	dst     io.Writer
	buf     []byte
	secrets []string
	pem     bool
}

func NewLineWriter(dst io.Writer) *LineWriter { return &LineWriter{dst: dst} }

func NewLineWriterWithSecrets(dst io.Writer, secrets []string) *LineWriter {
	return &LineWriter{dst: dst, secrets: append([]string(nil), secrets...)}
}

func (w *LineWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	for {
		index := bytes.IndexByte(w.buf, '\n')
		if index < 0 {
			break
		}
		line := string(w.buf[:index+1])
		if strings.Contains(line, "-----BEGIN PRIVATE KEY-----") || strings.Contains(line, "-----BEGIN RSA PRIVATE KEY-----") || strings.Contains(line, "-----BEGIN EC PRIVATE KEY-----") || strings.Contains(line, "-----BEGIN OPENSSH PRIVATE KEY-----") {
			w.pem = true
			if _, err := io.WriteString(w.dst, "[REDACTED PRIVATE KEY]\n"); err != nil {
				return 0, err
			}
			w.buf = w.buf[index+1:]
			continue
		}
		if w.pem {
			if strings.Contains(line, "-----END ") && strings.Contains(line, "PRIVATE KEY-----") {
				w.pem = false
			}
			w.buf = w.buf[index+1:]
			continue
		}
		if _, err := io.WriteString(w.dst, StringWithSecrets(line, w.secrets)); err != nil {
			return 0, err
		}
		w.buf = w.buf[index+1:]
	}
	return len(p), nil
}

func (w *LineWriter) Flush() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.buf) == 0 {
		return nil
	}
	buffered := string(w.buf)
	if w.pem {
		w.buf = nil
		w.pem = false
		return nil
	}
	if strings.Contains(buffered, "-----BEGIN PRIVATE KEY-----") || strings.Contains(buffered, "-----BEGIN RSA PRIVATE KEY-----") || strings.Contains(buffered, "-----BEGIN EC PRIVATE KEY-----") || strings.Contains(buffered, "-----BEGIN OPENSSH PRIVATE KEY-----") {
		w.buf = nil
		_, err := io.WriteString(w.dst, "[REDACTED PRIVATE KEY]")
		return err
	}
	_, err := io.WriteString(w.dst, StringWithSecrets(buffered, w.secrets))
	w.buf = nil
	return err
}
