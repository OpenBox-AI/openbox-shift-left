// Package conformance validates OpenBox normalized developer-runtime events
// against the versioned JSON Schema in api/.
//
// Structural validation is performed by santhosh-tekuri/jsonschema/v6, which
// covers the whole draft. The package shipped a hand-rolled subset covering only
// the keywords the contract happened to use, so a constraint written with any
// other keyword read as a tightened contract and enforced nothing; D-OSS-5 traded
// that — and this module's former zero-dependency property — for the reference
// implementation. The allowlist in internal/depguard keeps the trade bounded.
//
// DEPENDENCY BOUNDARY. This subtree's imports are held to an allowlist in
// internal/depguard, both external and repo-local (ADR-0023 as amended by
// ADR-0024). Adding an import outside it fails there first, which is the
// point — widening the list to make an import pass inverts the ADR's
// reasoning. This comment is the signpost; depguard is the enforcement.
package conformance

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// schemaRelPath is the contract schema location relative to this source file.
const schemaRelPath = "../../api/dev-event.schema.json"

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
