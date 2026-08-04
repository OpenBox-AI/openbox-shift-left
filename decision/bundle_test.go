package decision

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestParseBundle_Valid(t *testing.T) {
	raw := []byte(`{
		"version": "v1",
		"default_decision": "allow",
		"rules": [
			{"id": "r1", "match": {"tool_name": "Bash", "attribute_contains": {"command": "rm -rf /"}}, "decision": "block", "reason": "destructive"}
		]
	}`)
	b, err := ParseBundle(raw)
	if err != nil {
		t.Fatalf("ParseBundle: %v", err)
	}
	if b.Version != "v1" || len(b.Rules) != 1 {
		t.Fatalf("unexpected bundle: %+v", b)
	}
}

func TestParseBundle_RejectsDenyByDefault(t *testing.T) {
	// A block-by-default bundle is a fail-CLOSED posture (E6-S3), never a bundle
	// default — must be rejected so the daemon never silently denies-by-default.
	for _, d := range []string{"block", "halt", "stop"} {
		raw := []byte(`{"version":"v","default_decision":"` + d + `","rules":[]}`)
		if _, err := ParseBundle(raw); err == nil {
			t.Errorf("default_decision=%q: expected rejection, got nil", d)
		}
	}
}

func TestParseBundle_RejectsUnknownDecision(t *testing.T) {
	raw := []byte(`{"version":"v","rules":[{"match":{},"decision":"nuke"}]}`)
	if _, err := ParseBundle(raw); err == nil {
		t.Error("unknown decision: expected rejection, got nil")
	}
}

func TestParseBundle_RejectsUnknownFields(t *testing.T) {
	raw := []byte(`{"version":"v","rules":[],"surprise":true}`)
	if _, err := ParseBundle(raw); err == nil {
		t.Error("unknown top-level field: expected rejection, got nil")
	}
}

func TestParseBundle_EmptyDefaultIsAllowClass(t *testing.T) {
	// Empty default_decision → ALLOW (fail-open safe), accepted.
	raw := []byte(`{"version":"v","rules":[]}`)
	if _, err := ParseBundle(raw); err != nil {
		t.Errorf("empty default should be accepted (allow-class): %v", err)
	}
}

func TestLoadBundleFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "b.json")
	if err := os.WriteFile(path, []byte(`{"version":"vX","rules":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	b, err := LoadBundleFile(path)
	if err != nil {
		t.Fatalf("LoadBundleFile: %v", err)
	}
	if b.Version != "vX" {
		t.Errorf("version = %q, want vX", b.Version)
	}
}

func TestExampleBundleParses(t *testing.T) {
	// The shipped example must always be a valid, load-safe bundle — it is the
	// reference operators and E6-S1 copy from.
	b, err := LoadBundleFile("examples/policy-bundle.example.json")
	if err != nil {
		t.Fatalf("example bundle does not parse: %v", err)
	}
	if len(b.Rules) == 0 {
		t.Error("example bundle has no rules")
	}
}

func TestLoadBundleFile_MissingIsNotExist(t *testing.T) {
	_, err := LoadBundleFile(filepath.Join(t.TempDir(), "nope.json"))
	if err == nil {
		t.Fatal("expected error for missing file")
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("expected LoadBundleFile to wrap fs.ErrNotExist, got %v", err)
	}
}
