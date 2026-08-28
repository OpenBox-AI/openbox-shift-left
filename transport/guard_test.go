package transport

import (
	"os"
	"strings"
	"testing"
)

// allowedDirectRequires is this module's entire reviewed direct-dependency set.
//
// The boundary is ADR-0023's: DIRECT requires are bounded here, and an
// allowlisted module's own tree is bounded at that module's guard. goproxy's
// tree is small — 203 transitive packages against telemetry's 492 — but the
// reason this module exists separately is not size. It is that goproxy sits on
// the CREDENTIAL PATH: every model call, with the developer's provider key in
// its headers, transits it. Putting that inside `gateway/` would have breached
// gateway's two-entry allowlist and, worse, moved credential-path code outside
// the scan that allowlist protects (plan 260827-2301, validation round 2).
var allowedDirectRequires = map[string]bool{
	"github.com/elazarl/goproxy": true,
}

// TestOnlyReviewedDirectRequires bounds what this module can execute.
func TestOnlyReviewedDirectRequires(t *testing.T) {
	raw, err := os.ReadFile("go.mod")
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}
	for _, path := range directRequires(string(raw)) {
		if !allowedDirectRequires[path] {
			t.Errorf("go.mod takes a direct dependency on %q, which is outside this module's "+
				"reviewed set. Adding one to the credential path is a decision: record it.", path)
		}
	}
}

// TestAllowlistHasNoDeadEntries keeps the list honest in the other direction: an
// allowlist naming modules the go.mod no longer requires reads as broader review
// than actually happened.
func TestAllowlistHasNoDeadEntries(t *testing.T) {
	raw, err := os.ReadFile("go.mod")
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}
	present := map[string]bool{}
	for _, p := range directRequires(string(raw)) {
		present[p] = true
	}
	for allowed := range allowedDirectRequires {
		if !present[allowed] {
			t.Errorf("allowlist names %q but go.mod no longer requires it directly; "+
				"drop it rather than leaving a claim of review standing", allowed)
		}
	}
}

// directRequires returns the module paths of every non-indirect require.
func directRequires(mod string) []string {
	var out []string
	inBlock := false
	for _, line := range strings.Split(mod, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "//"), trimmed == "":
			continue
		case trimmed == "require (":
			inBlock = true
			continue
		case inBlock && trimmed == ")":
			inBlock = false
			continue
		}
		if strings.Contains(trimmed, "// indirect") {
			continue
		}
		fields := strings.Fields(trimmed)
		switch {
		case inBlock && len(fields) >= 2:
			out = append(out, fields[0])
		case !inBlock && len(fields) >= 3 && fields[0] == "require":
			// The single-line form: `require path v1.2.3`.
			out = append(out, fields[1])
		}
	}
	return out
}

// forbiddenCalls are the ways this module could start reading a credential or the
// developer's files, keyed by IMPORT PATH rather than by the identifier at the
// call site — an alias defeats identifier matching (`import os2 "os"`).
//
// This module runs as a daemon with content flowing through it. It has no reason
// to read the environment or the filesystem: its configuration arrives as a
// struct and its output leaves through the Emitter. Anything else is the CLI's
// job, where the credential handling already lives and is already scanned.
var forbiddenCalls = map[string][]string{
	"os":        {"Getenv", "LookupEnv", "Environ", "ReadFile", "Open", "OpenFile", "UserHomeDir", "UserConfigDir"},
	"syscall":   {"Getenv", "Environ", "Open", "Read"},
	"io/ioutil": {"ReadFile", "ReadDir"},
}
