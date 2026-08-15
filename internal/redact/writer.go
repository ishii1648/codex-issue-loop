package redact

import (
	"bytes"
	"io"
	"regexp"
	"sync"
)

var patterns = []*regexp.Regexp{
	regexp.MustCompile(`\bgh[opusr]_[A-Za-z0-9_]{20,}\b`),
	regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{20,}\b`),
	regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{20,}\b`),
	regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._~+/-]{20,}=*`),
}

func String(value string) string {
	for _, pattern := range patterns {
		value = pattern.ReplaceAllString(value, "[REDACTED]")
	}
	return value
}

// LineWriter buffers partial lines so credentials split across subprocess
// writes are still redacted before reaching disk.
type LineWriter struct {
	mu  sync.Mutex
	dst io.Writer
	buf []byte
}

func NewLineWriter(dst io.Writer) *LineWriter { return &LineWriter{dst: dst} }

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
		if _, err := io.WriteString(w.dst, String(line)); err != nil {
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
	_, err := io.WriteString(w.dst, String(string(w.buf)))
	w.buf = nil
	return err
}
