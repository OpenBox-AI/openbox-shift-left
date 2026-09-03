package managed

import (
	"runtime"
	"testing"
)

// TestMandates_CodexRejectsNestedKeys a mandate must be recognized from a TOP-
// level key only.
func TestMandates_CodexRejectsNestedKeys(t *testing.T) {
	nested := []byte("[hooks]\nallow_managed_hooks_only = true\nallowed_sandbox_modes = [\"read-only\"]\n")
	if mandates(ProviderCodex, nested) {
		t.Error("keys nested under [hooks] are ignored by Codex and must not count as a mandate")
	}
	top := []byte("allowed_sandbox_modes = [\"read-only\"]\n\n[experimental_network]\nenabled = true\n")
	if !mandates(ProviderCodex, top) {
		t.Error("a top-level mandate key must be recognized")
	}
	if mandates(ProviderCodex, []byte("[hooks]\nPreToolUse = \"openbox hook codex PreToolUse\"\n")) {
		t.Error("naming our hook in requirements.toml is not a mandate")
	}
}

// TestProviderStateReportsWithoutInstalling. The write half is gone -- an
// administrator deploys these files with whatever the fleet already uses -- but
// doctor still has to say whether this machine carries a mandate, and after the
// hook-blocking reader that answer is a safety statement rather than a note.
func TestProviderStateReportsWithoutInstalling(t *testing.T) {
	for _, p := range []Provider{ProviderClaudeCode, ProviderCodex} {
		state := ProviderState(p)
		if state == "" {
			t.Errorf("ProviderState(%s) said nothing; doctor would print a blank line", p)
		}
	}
}

// TestTheManagedDirsStillResolve. doctor reads through these, so their
// signatures are load-bearing even though nothing writes to them any more.
func TestTheManagedDirsStillResolve(t *testing.T) {
	if dir := ClaudeCodeManagedDir(); dir == "" && runtime.GOOS != "windows" {
		t.Error("ClaudeCodeManagedDir() is empty on a platform that has one")
	}
	if dir, warning := codexDir(); dir == "" && warning == "" {
		t.Error("codexDir() returned neither a directory nor a reason")
	}
}
