// Package conformance validates OpenBox normalized developer-runtime events
// against the versioned JSON Schema in api/.
package conformance

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

const schemaRelPath = "../../api/dev-event.schema.json"

// LoadSchema reads and parses the dev-event JSON Schema as a generic tree.
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
