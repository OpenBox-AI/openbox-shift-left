// Package conformance validates OpenBox normalized developer-runtime events
// against the versioned JSON Schema at ../schema/dev-event.schema.json.
//
// It is intentionally dependency-free: it ships a minimal JSON Schema
// validator covering exactly the keywords the contract uses, so `go build
// ./... && go test ./...` runs offline with no module downloads.
package conformance

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// schemaRelPath is the contract schema location relative to this source file.
const schemaRelPath = "../schema/dev-event.schema.json"

// LoadSchema reads and parses the dev-event JSON Schema as a generic tree.
// The path is resolved relative to this source file so it works regardless of
// the caller's working directory.
func LoadSchema() (map[string]any, error) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		return nil, fmt.Errorf("cannot resolve conformance source path")
	}
	p := filepath.Join(filepath.Dir(thisFile), schemaRelPath)
	raw, err := os.ReadFile(p)
	if err != nil {
		return nil, fmt.Errorf("read schema %s: %w", p, err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse schema %s: %w", p, err)
	}
	return doc, nil
}
