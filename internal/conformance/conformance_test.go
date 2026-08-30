package conformance

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func read(t *testing.T, rel string) []byte {
	t.Helper()
	raw, err := os.ReadFile(rel)
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return raw
}

// AC: a well-formed sample of EACH lifecycle event type validates.
// (tool_call_mcp.json additionally exercises the kind=mcp -> mcp_server rule.)
// The count is derived from contractEventTypes rather than written as a literal,
// so adding a type to the vocabulary without a sample fails here instead of
// silently leaving the type unexercised.
func TestValidSamples(t *testing.T) {
	dir := "testdata/valid"
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) < len(contractEventTypes) {
		t.Fatalf("expected >=%d valid samples (one per lifecycle type), got %d", len(contractEventTypes), len(entries))
	}

	seen := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		path := filepath.Join(dir, e.Name())
		raw := read(t, path)
		// content-capture disabled (the default posture) — none of these carry content.
		if err := ValidateDevEvent(raw, false); err != nil {
			t.Errorf("%s: expected valid, got: %v", e.Name(), err)
			continue
		}
		if et := eventType(t, raw); et != "" {
			seen[et] = true
		}
	}

	for _, et := range contractEventTypes {
		if !seen[et] {
			t.Errorf("no valid sample covered lifecycle type %q", et)
		}
	}
}

// AC (a): malformed / unknown-type events are rejected.
func TestInvalidSamplesRejected(t *testing.T) {
	dir := "testdata/invalid"
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		raw := read(t, filepath.Join(dir, e.Name()))
		if err := ValidateDevEvent(raw, false); err == nil {
			t.Errorf("%s: expected rejection, got nil", e.Name())
		}
	}
}

// AC (b): any event carrying content is rejected when content-capture is
// DISABLED, and accepted when it is ENABLED (INV-2 / OD4).
// The fixture set is read from the directory rather than listed here, like the
// two tests above: a hardcoded list means a fixture added for a NEW gated field
// is never validated, and "no test ran it" is indistinguishable from "it
// passed".
func TestContentGate(t *testing.T) {
	dir := "testdata/content"
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("no content fixtures — the gate would be asserted against nothing")
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		raw := read(t, filepath.Join(dir, name))

		err := ValidateDevEvent(raw, false)
		if !errors.Is(err, ErrContentDisabled) {
			t.Errorf("%s: content-disabled: want ErrContentDisabled, got %v", name, err)
		}

		if err := ValidateDevEvent(raw, true); err != nil {
			t.Errorf("%s: content-enabled: want valid, got %v", name, err)
		}
	}
}

// The malformed-type sample specifically must fail on the event_type enum,
// independent of content posture.
func TestUnknownTypeRejected(t *testing.T) {
	raw := read(t, "testdata/invalid/unknown_type.json")
	if err := ValidateDevEvent(raw, true); err == nil {
		t.Fatal("unknown event_type must be rejected even with content enabled")
	}
}

// eventType extracts the event_type field for coverage bookkeeping.
func eventType(t *testing.T, raw []byte) string {
	t.Helper()
	m := map[string]any{}
	if err := json.Unmarshal(raw, &m); err != nil {
		return ""
	}
	s, _ := m["event_type"].(string)
	return s
}
