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
	// A dotted key binds as nesting, so neither the leaf nor the parent is a
	// top-level mandate.
	//
	// The safety property is the first assertion and it is the E8-S8 one: asking
	// for `allow_managed_hooks_only` must NOT match `hooks.allow_managed_hooks_only`,
	// because Codex reads the former and the file defines the latter.
	//
	// The second assertion changed with the go-toml swap. The retired scanner also
	// reported the literal string "hooks.allow_managed_hooks_only", which it could
	// do only because it never parsed anything; a real TOML parse binds the dotted
	// key as a table named `hooks`, and `hooks = {…}` is indistinguishable from a
	// `[hooks]` header once decoded. Nothing ever consumed the verbatim form —
	// codexRequirementKeys are all bare names — and reconstructing it would mean
	// re-deriving what the parser deliberately normalises. So the assertion now
	// states the parse-based truth: the dotted key contributes NO top-level key.
	got := TopLevelTOMLKeys([]byte("hooks.allow_managed_hooks_only = true\n"))
	if got["allow_managed_hooks_only"] {
		t.Error("a dotted key must not match its leaf name — it is not a top-level mandate")
	}
	if got["hooks.allow_managed_hooks_only"] {
		t.Error("a dotted key is bound as nesting, so its verbatim spelling is not a top-level key")
	}
	if got["hooks"] {
		t.Error("`hooks` holds a table, so it is not a top-level key either")
	}
}

// A multi-line value whose continuation line BEGINS with `[` must not hide the
// top-level keys that follow it.
//
// The retired scanner set `inTable` on any line whose first character was `[`
// and skipped everything after. TOML allows a line inside a multi-line basic
// string, or an element of a wrapped array-of-arrays, to begin with `[` — so a
// perfectly valid mandate file could silence every later top-level key. The
// consumer is codexMandated, which decides whether Codex hooks are mandated by
// requirements.toml, and the failure direction was a mandated machine reading as
// UNMANDATED: enforcement reported absent while it was in force.
//
// Note the shape that matters: `key = [` followed by indented elements does NOT
// trip the scanner, because those lines start with a quote or a digit. It takes a
// continuation line whose own first character is `[`.
func TestTopLevelTOMLKeys_BracketLeadingContinuationDoesNotHideLaterKeys(t *testing.T) {
	// A multi-line basic string documenting a TOML snippet — entirely plausible
	// in a managed requirements file.
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

	// An array of arrays, wrapped.
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
