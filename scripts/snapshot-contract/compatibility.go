package main

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
)

//go:embed testdata/probe.go.txt
var probe []byte

func buildProbe(root, output string) error {
	dir := filepath.Join(root, "snapshot-contract-probe")
	if err := os.Mkdir(dir, 0700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), probe, 0600); err != nil {
		return err
	}
	_, err := command(root, "go", "build", "-o", output, "./snapshot-contract-probe")
	return err
}

func fileHashes(dir string) (map[string][32]byte, error) {
	hashes := map[string][32]byte{}
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		hashes[path] = sha256.Sum256(data)
		return nil
	})
	return hashes, err
}

func readFixture(binary, dir string, want bool, versionRejection bool) (json.RawMessage, error) {
	before, err := fileHashes(dir)
	if err != nil {
		return nil, err
	}
	data, err := command("", binary, "read", dir)
	if err != nil {
		return nil, err
	}
	after, err := fileHashes(dir)
	if err != nil {
		return nil, err
	}
	if !reflect.DeepEqual(before, after) {
		return nil, fmt.Errorf("reader changed durable files: %s", binary)
	}
	var result struct {
		Accepted bool            `json:"accepted"`
		Error    string          `json:"error"`
		Snapshot json.RawMessage `json:"snapshot"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, err
	}
	if result.Accepted != want || (!want && result.Error == "") || (versionRejection && !strings.Contains(result.Error, "version")) {
		return nil, fmt.Errorf("reader %s: accepted=%v want=%v error=%s", binary, result.Accepted, want, result.Error)
	}
	return result.Snapshot, nil
}

func compatibility(root, oldDir, newDir, tmp string, sameVersion bool) error {
	binaries := []string{filepath.Join(tmp, "old-probe"), filepath.Join(tmp, "new-probe")}
	for i, dir := range []string{oldDir, newDir} {
		if err := buildProbe(dir, binaries[i]); err != nil {
			return err
		}
	}
	fixture := filepath.Join(root, "internal/adapter/state/testdata/issue-439-answered-next-checkpoint.json")
	for writer, binary := range binaries {
		dir := filepath.Join(tmp, fmt.Sprintf("writer-%d", writer))
		if _, err := command("", binary, "write", dir, fixture); err != nil {
			return err
		}
		own, err := readFixture(binary, dir, true, false)
		if err != nil {
			return err
		}
		other, err := readFixture(binaries[1-writer], dir, sameVersion, !sameVersion)
		if err != nil {
			return err
		}
		if sameVersion && !bytes.Equal(own, other) {
			return fmt.Errorf("same-version readers changed snapshot meaning for writer %d", writer)
		}
		data, err := os.ReadFile(filepath.Join(dir, "state.json"))
		if err != nil {
			return err
		}
		var snapshot map[string]any
		if err := json.Unmarshal(data, &snapshot); err != nil {
			return err
		}
		issues := snapshot["issues"].(map[string]any)
		issue := issues["439"].(map[string]any)
		answer := issue["answers"].([]any)[0].(map[string]any)
		if answer["answer"] != "yes" {
			return fmt.Errorf("writer lost historical answer")
		}
		answer["answer"] = "inconsistent"
		data, err = json.Marshal(snapshot)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, "state.json"), data, 0600); err != nil {
			return err
		}
		for reader, binary := range binaries {
			if _, err := readFixture(binary, dir, false, reader != writer && !sameVersion); err != nil {
				return err
			}
		}
		fmt.Printf("snapshot-contract: writer=%d own reader accepts; peer accepts=%v; corrupt answer rejected; files unchanged\n", writer, sameVersion)
	}
	return nil
}
