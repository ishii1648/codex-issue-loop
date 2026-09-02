package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/ishii1648/codex-issue-loop/internal/application/incidentanalysis"
)

func main() {
	dataDir := flag.String("data-dir", "analysis/incident-taxonomy", "directory containing corpus.json and rules.json")
	goldenPath := flag.String("golden", "", "evaluation golden path; defaults to <data-dir>/evaluation.golden.json")
	check := flag.Bool("check", true, "compare the result with the committed golden file")
	flag.Parse()

	corpus, rules, err := incidentanalysis.Load(filepath.Join(*dataDir, "corpus.json"), filepath.Join(*dataDir, "rules.json"))
	if err != nil {
		fatal(err)
	}
	if err := incidentanalysis.ValidateBundle(*dataDir, corpus); err != nil {
		fatal(err)
	}
	evaluation, err := incidentanalysis.Evaluate(corpus, rules)
	if err != nil {
		fatal(err)
	}
	raw, err := incidentanalysis.MarshalCanonical(evaluation)
	if err != nil {
		fatal(err)
	}
	if *check {
		path := *goldenPath
		if path == "" {
			path = filepath.Join(*dataDir, "evaluation.golden.json")
		}
		golden, err := os.ReadFile(path)
		if err != nil {
			fatal(err)
		}
		if !bytes.Equal(raw, golden) {
			fatal(fmt.Errorf("evaluation differs from %s", path))
		}
	}
	if _, err := os.Stdout.Write(raw); err != nil {
		fatal(err)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "incident-eval:", err)
	os.Exit(1)
}
