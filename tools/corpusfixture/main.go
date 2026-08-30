// Command corpusfixture turns a recorded openbox-logger run into the sanitized
// test fixtures this repository commits.
//
// It is a MAINTENANCE command, run by hand, and it is committed rather than kept
// as a one-off script so that the fixtures are reproducible: the next person to
// refresh them can see exactly which records were selected and on what basis,
// instead of inheriting a directory of JSON with no provenance.
//
// It is not built into the release binary — .goreleaser.yaml names ./cmd/openbox
// explicitly — and it reads a corpus that is not part of this repository.
//
// Usage:
//
//	go run ./cmd/corpusfixture -corpus <run-dir> -out <repo-root>
//
// The write is GATED on corpusfixture.Scan reporting nothing, so an unsanitized
// fixture cannot reach the working tree, let alone git history. That gate is
// belt-and-braces with the permanent scan test — this one protects the person
// running the command, that one protects the repository forever.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/openbox-ai/openbox-shift-left/internal/cli/corpusfixture"
)

func main() {
	corpus := flag.String("corpus", "", "path to an openbox-logger run directory (required)")
	out := flag.String("out", ".", "repository root to write fixtures into")
	flag.Parse()

	if *corpus == "" {
		fmt.Fprintln(os.Stderr, "corpusfixture: -corpus is required")
		os.Exit(2)
	}
	if err := run(*corpus, *out); err != nil {
		fmt.Fprintln(os.Stderr, "corpusfixture:", err)
		os.Exit(1)
	}
}

func run(corpus, out string) error {
	if err := extractOTel(corpus, out); err != nil {
		return err
	}
	return extractProxy(corpus, out)
}

// write sanitizes, scans, and only then writes. The scan is the gate.
func write(path string, doc any) error {
	raw, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	clean, err := corpusfixture.Sanitize(raw)
	if err != nil {
		return err
	}
	if v := corpusfixture.Scan(clean); len(v) != 0 {
		return fmt.Errorf("refusing to write %s: %d sentinel violation(s) survived sanitization, first: %s",
			path, len(v), v[0])
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, clean, 0o644); err != nil {
		return err
	}
	fmt.Printf("wrote %s (%d bytes)\n", path, len(clean))
	return nil
}
