package codex

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func seedHooks(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hooks.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestRemoveHooksTakesOursAtAnyEnginePath. Removal has no live engine to keep,
// unlike the merge on the install side: a handler left pointing at a deleted
// binary keeps firing and, with the credentials gone, fails open on every tool
// call.
func TestRemoveHooksTakesOursAtAnyEnginePath(t *testing.T) {
	path := seedHooks(t, `{
	  "description": "x",
	  "hooks": {
	    "PreToolUse": [
	      {"matcher": "*", "hooks": [{"type": "command", "command": "\"/old/openbox\" hook codex PreToolUse", "timeout": 60}]}
	    ],
	    "SessionStart": [
	      {"hooks": [{"type": "command", "command": "\"/new/openbox\" hook codex SessionStart", "timeout": 5}]}
	    ]
	  }
	}`)

	removed, err := RemoveHooks(path)
	if err != nil {
		t.Fatalf("RemoveHooks: %v", err)
	}
	if len(removed) != 2 {
		t.Errorf("removed %d handlers, want 2: %v", len(removed), removed)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{"/old/openbox", "/new/openbox", "hook codex"} {
		if strings.Contains(string(raw), gone) {
			t.Errorf("%q survived removal:\n%s", gone, raw)
		}
	}
	for _, event := range []string{"PreToolUse", "SessionStart"} {
		if r := gjson.GetBytes(raw, "hooks."+event); r.Exists() && len(r.Array()) != 0 {
			t.Errorf("hooks.%s kept %d entries after removal: %s", event, len(r.Array()), r.Raw)
		}
	}
}

// TestRemoveHooksLeavesForeignEntriesAlone. Every other key in this file, at
// any depth, is somebody else's — the install side says so, and removal has to
// hold the same line.
func TestRemoveHooksLeavesForeignEntriesAlone(t *testing.T) {
	path := seedHooks(t, `{
	  "description": "team hooks",
	  "hooks": {
	    "PreToolUse": [
	      {"matcher": "shell", "hooks": [{"type": "command", "command": "/opt/team/guard --deny-rm", "timeout": 4}]},
	      {"matcher": "*", "hooks": [{"type": "command", "command": "\"/opt/openbox\" hook codex PreToolUse", "timeout": 60}]}
	    ],
	    "TurnEnd": [{"hooks": [{"type": "command", "command": "/opt/team/report"}]}]
	  }
	}`)

	if _, err := RemoveHooks(path); err != nil {
		t.Fatalf("RemoveHooks: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, kept := range []string{"guard --deny-rm", "/opt/team/report", "team hooks"} {
		if !strings.Contains(string(raw), kept) {
			t.Errorf("removal took a foreign entry with it (%q is gone):\n%s", kept, raw)
		}
	}
	if r := gjson.GetBytes(raw, "hooks.PreToolUse"); len(r.Array()) != 1 {
		t.Errorf("hooks.PreToolUse should keep exactly the foreign group, got: %s", r.Raw)
	}
	if got := gjson.GetBytes(raw, "hooks.PreToolUse.0.matcher").String(); got != "shell" {
		t.Errorf("the foreign group's matcher changed to %q", got)
	}
	if got := gjson.GetBytes(raw, "hooks.PreToolUse.0.hooks.0.timeout").Int(); got != 4 {
		t.Errorf("the foreign handler's timeout changed to %d", got)
	}
}

// TestRemoveHooksOnAnAbsentFileIsSuccess. Uninstall walks every surface
// unconditionally, so a machine that never ran Codex is the ordinary case.
func TestRemoveHooksOnAnAbsentFileIsSuccess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nothing", "hooks.json")
	removed, err := RemoveHooks(path)
	if err != nil || removed != nil {
		t.Fatalf("RemoveHooks on an absent file = %v, %v; want nil, nil", removed, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("removal created the file it was asked to clean")
	}
}

// TestRemoveHooksRefusesAnUnparsableFile. sjson edits a malformed document
// without complaint, and Codex reads none of the hooks in a file it cannot
// parse — so refuse with the path rather than write over it.
func TestRemoveHooksRefusesAnUnparsableFile(t *testing.T) {
	path := seedHooks(t, `{"hooks": {"PreToolUse": [`)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := RemoveHooks(path); err == nil {
		t.Fatal("RemoveHooks accepted a malformed hooks file")
	} else if !strings.Contains(err.Error(), path) {
		t.Errorf("the refusal does not name the file to fix: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Errorf("a file it refused to parse was rewritten anyway:\n%s", after)
	}
}

// TestRemoveHooksIsIdempotent. The second uninstall on a clean machine must be
// quiet: one-shot coverage passes on state a re-run corrupts.
func TestRemoveHooksIsIdempotent(t *testing.T) {
	path := seedHooks(t, `{
	  "hooks": {"SessionEnd": [{"hooks": [{"type": "command", "command": "\"/opt/openbox\" hook codex SessionEnd", "timeout": 15}]}]}
	}`)
	if _, err := RemoveHooks(path); err != nil {
		t.Fatalf("first RemoveHooks: %v", err)
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	removed, err := RemoveHooks(path)
	if err != nil {
		t.Fatalf("second RemoveHooks: %v", err)
	}
	if len(removed) != 0 {
		t.Errorf("the second run reported removing %v", removed)
	}
	second, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Errorf("the second run rewrote the file:\n%s\n---\n%s", first, second)
	}
}

// TestRemoveHooksDoesNotRewriteAFileItOwnsNothingIn.
func TestRemoveHooksDoesNotRewriteAFileItOwnsNothingIn(t *testing.T) {
	const body = `{
  "hooks": {
    "PreToolUse": [ { "matcher": "shell", "hooks": [ { "type": "command", "command": "own-guard" } ] } ]
  }
}`
	path := seedHooks(t, body)
	removed, err := RemoveHooks(path)
	if err != nil {
		t.Fatalf("RemoveHooks: %v", err)
	}
	if len(removed) != 0 {
		t.Errorf("reported removing %v from a file it owns nothing in", removed)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != body {
		t.Errorf("the file was reformatted:\n%s", after)
	}
}

// TestRemoveHooksNamesTheEventAndEngine keeps the report usable: an operator
// has to see which surface was cleaned and which engine it pointed at.
func TestRemoveHooksNamesTheEventAndEngine(t *testing.T) {
	path := seedHooks(t, `{
	  "hooks": {"UserPromptSubmit": [{"hooks": [{"type": "command", "command": "\"/opt/ob/openbox\" hook codex UserPromptSubmit"}]}]}
	}`)
	removed, err := RemoveHooks(path)
	if err != nil {
		t.Fatalf("RemoveHooks: %v", err)
	}
	if len(removed) != 1 {
		t.Fatalf("removed = %v; want one entry", removed)
	}
	for _, want := range []string{"UserPromptSubmit", "/opt/ob/openbox"} {
		if !strings.Contains(removed[0], want) {
			t.Errorf("the report %q does not name %q", removed[0], want)
		}
	}
}

// TestDefaultHooksPathHonoursCodexHome. Uninstall has to look where the
// install wrote, and CODEX_HOME moves the whole directory.
func TestDefaultHooksPathHonoursCodexHome(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CODEX_HOME", dir)
	if got, want := DefaultHooksPath(), filepath.Join(dir, "hooks.json"); got != want {
		t.Errorf("DefaultHooksPath() = %q; want %q", got, want)
	}
	t.Setenv("CODEX_HOME", "")
	if got := DefaultHooksPath(); !strings.HasSuffix(got, filepath.Join(".codex", "hooks.json")) {
		t.Errorf("DefaultHooksPath() = %q; want a path under ~/.codex", got)
	}
}
