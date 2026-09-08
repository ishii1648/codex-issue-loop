package fsutil

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadPrivateRegular(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		name    string
		mode    os.FileMode
		wantErr bool
	}{
		{"private", 0o600, false},
		{"read_only", 0o400, false},
		{"executable", 0o700, false},
		{"group_read", 0o640, true},
		{"other_write", 0o602, true},
		{"group_execute", 0o610, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, tc.name)
			if err := os.WriteFile(path, []byte("payload"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, tc.mode); err != nil {
				t.Fatal(err)
			}
			data, err := ReadPrivateRegular(path, "fixture")
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), "fixture is not owner-only:") || data != nil {
					t.Fatalf("data=%q err=%v", data, err)
				}
			} else if err != nil || string(data) != "payload" {
				t.Fatalf("data=%q err=%v", data, err)
			}
		})
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(filepath.Join(dir, "private"), link); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{dir, link} {
		if data, err := ReadPrivateRegular(path, "fixture"); err == nil || !strings.Contains(err.Error(), "not a regular file") || data != nil {
			t.Fatalf("path=%s data=%q err=%v", path, data, err)
		}
	}
	if _, err := ReadPrivateRegular(filepath.Join(dir, "missing"), "fixture"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing file error=%v", err)
	}
}

func TestDecodeStrictJSON(t *testing.T) {
	for _, tc := range []struct {
		name    string
		data    string
		wantErr string
	}{
		{"valid", `{"value":1}`, ""},
		{"whitespace", "{\"value\":1}\n\t ", ""},
		{"unknown_field", `{"value":1,"unknown":2}`, "unknown field"},
		{"second_object", `{"value":1} {}`, "trailing JSON data"},
		{"trailing_null", `{"value":1} null`, "trailing JSON data"},
		{"trailing_garbage", `{"value":1} invalid`, "trailing JSON data"},
		{"empty", "", "EOF"},
		{"incomplete", `{"value":`, "unexpected EOF"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var value struct {
				Value int `json:"value"`
			}
			err := DecodeStrictJSON([]byte(tc.data), &value)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error=%v want=%s", err, tc.wantErr)
				}
			} else if err != nil || value.Value != 1 {
				t.Fatalf("value=%+v err=%v", value, err)
			}
		})
	}
}
