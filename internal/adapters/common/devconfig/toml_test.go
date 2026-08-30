package devconfig

import "testing"

func TestTopLevelTOMLKeys(t *testing.T) {
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
	if TopLevelTOMLKeys([]byte("[[servers]]\nname = \"a\"\n"))["name"] {
		t.Error("a key under [[servers]] is not top-level")
	}
	if !TopLevelTOMLKeys([]byte(`"allowed_sandbox_modes" = ["read-only"]`))["allowed_sandbox_modes"] {
		t.Error("a quoted top-level key should be reported by its bare name")
	}
	// The safety property is the first assertion and it is the E8-S8 one: asking
	// for `allow_managed_hooks_only` must NOT match
	// `hooks.allow_managed_hooks_only`, because Codex reads the former and the
	// file defines the latter.
	got := TopLevelTOMLKeys([]byte("hooks.allow_managed_hooks_only = true\n"))
	if got["allow_managed_hooks_only"] {
		t.Error("a dotted key must not match its leaf name; it is not a top-level mandate")
	}
	if got["hooks.allow_managed_hooks_only"] {
		t.Error("a dotted key is bound as nesting, so its verbatim spelling is not a top-level key")
	}
	if got["hooks"] {
		t.Error("`hooks` holds a table, so it is not a top-level key either")
	}
}

// TestTopLevelTOMLKeys_BracketLeadingContinuationDoesNotHideLaterKeys a multi-
// line value whose continuation line begins with `[` must not hide the top-
// level keys that follow it.
func TestTopLevelTOMLKeys_BracketLeadingContinuationDoesNotHideLaterKeys(t *testing.T) {
	multilineString := []byte(`
notice = """
Place hook overrides under:
[hooks]
"""
allow_managed_hooks_only = true
`)
	got := TopLevelTOMLKeys(multilineString)
	if !got["allow_managed_hooks_only"] {
		t.Errorf("a mandate key AFTER a multi-line string containing a bracketed line is "+
			"still top-level. This is the posture bug: the machine IS mandated and reads as "+
			"unmandated. got %v", got)
	}

	nestedArray := []byte(`
pairs = [
[1, 2],
[3, 4],
]
allowed_sandbox_modes = ["read-only"]
`)
	got = TopLevelTOMLKeys(nestedArray)
	if !got["allowed_sandbox_modes"] {
		t.Errorf("a key after a wrapped array-of-arrays is still top-level, got %v", got)
	}
}
