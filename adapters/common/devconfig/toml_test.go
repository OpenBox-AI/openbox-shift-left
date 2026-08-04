package devconfig

import "testing"

func TestTopLevelTOMLKeys(t *testing.T) {
	// The shape that shipped the E8-S8 hole: mandate keys written after a table
	// header belong to that table, so none of them is top-level.
	nested := []byte(`
# OpenBox managed requirements
[hooks]
PreToolUse = "openbox hook codex PreToolUse"
allow_managed_hooks_only = true
allowed_approval_policies = ["untrusted"]
`)
	got := TopLevelTOMLKeys(nested)
	for _, k := range []string{"allow_managed_hooks_only", "allowed_approval_policies", "PreToolUse"} {
		if got[k] {
			t.Errorf("%q is nested under [hooks] and must not be reported top-level (got %v)", k, got)
		}
	}

	// The corrected shape: bare keys first, tables last.
	fixed := []byte(`
# comment
allowed_approval_policies = ["untrusted", "on-request"]
allowed_sandbox_modes = ["read-only"]
# allow_managed_hooks_only = true

[experimental_network]
enabled = true
`)
	got = TopLevelTOMLKeys(fixed)
	for _, k := range []string{"allowed_approval_policies", "allowed_sandbox_modes"} {
		if !got[k] {
			t.Errorf("%q should be top-level, got %v", k, got)
		}
	}
	if got["allow_managed_hooks_only"] {
		t.Error("a commented-out key is not defined")
	}
	if got["enabled"] {
		t.Error("a key under [experimental_network] is not top-level")
	}
}

func TestTopLevelTOMLKeys_Edges(t *testing.T) {
	if len(TopLevelTOMLKeys(nil)) != 0 {
		t.Error("empty input has no keys")
	}
	// An array-of-tables header opens a scope just as a table header does.
	if TopLevelTOMLKeys([]byte("[[servers]]\nname = \"a\"\n"))["name"] {
		t.Error("a key under [[servers]] is not top-level")
	}
	// Quoted keys are unwrapped so a caller can match the bare name.
	if !TopLevelTOMLKeys([]byte(`"allowed_sandbox_modes" = ["read-only"]`))["allowed_sandbox_modes"] {
		t.Error("a quoted top-level key should be reported by its bare name")
	}
	// A dotted key is recorded verbatim, so asking for the parent does not match.
	got := TopLevelTOMLKeys([]byte("hooks.allow_managed_hooks_only = true\n"))
	if got["allow_managed_hooks_only"] {
		t.Error("a dotted key must not match its leaf name — it is not a top-level mandate")
	}
	if !got["hooks.allow_managed_hooks_only"] {
		t.Error("a dotted key should be recorded verbatim")
	}
}
