package claudecode

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// seedSettings writes body to a settings file in a fresh directory.
func seedSettings(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestRemoveLocalHooksTakesOursAtAnyEnginePath. Uninstall differs from install
// here: sweepStale keeps the handler registered at the engine being installed,
// because that one is the live registration. Removal has no live engine to
// keep — a hook left pointing at a deleted binary keeps firing immediately,
// since Claude Code's file watcher picks up settings edits at once, and with
// the credentials gone it fails open on every tool call.
func TestRemoveLocalHooksTakesOursAtAnyEnginePath(t *testing.T) {
	path := seedSettings(t, `{
	  "hooks": {
	    "PreToolUse": [
	      {"matcher": "*", "hooks": [
	        {"type": "command", "command": "\"/old/openbox\" hook claude-code PreToolUse", "timeout": 60},
	        {"type": "command", "command": "\"/new/openbox\" rewake claude-code", "asyncRewake": true}
	      ]}
	    ],
	    "SessionStart": [
	      {"hooks": [{"type": "command", "command": "\"/new/openbox\" hook claude-code SessionStart", "timeout": 5}]}
	    ]
	  }
	}`)

	removed, err := RemoveLocalHooks(path)
	if err != nil {
		t.Fatalf("RemoveLocalHooks: %v", err)
	}
	if len(removed) != 3 {
		t.Errorf("removed %d handlers, want 3 (two engines on PreToolUse plus SessionStart): %v", len(removed), removed)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{"/old/openbox", "/new/openbox", "hook claude-code", "rewake claude-code"} {
		if strings.Contains(string(raw), gone) {
			t.Errorf("%q survived removal:\n%s", gone, raw)
		}
	}
	// An event whose only handlers were ours must not be left as an empty
	// matcher group: Claude Code reads a group with no hooks as a group, and the
	// residue reads as a registration that failed to clean up.
	for _, event := range []string{"PreToolUse", "SessionStart"} {
		if r := gjson.GetBytes(raw, "hooks."+event); r.Exists() && len(r.Array()) != 0 {
			t.Errorf("hooks.%s kept %d entries after removal: %s", event, len(r.Array()), r.Raw)
		}
	}
}

// TestRemoveLocalHooksLeavesForeignEntriesAlone. These files belong to the
// developer and their org. A hook we did not write is not ours to remove, and
// a settings key we do not know about must survive byte-identical.
func TestRemoveLocalHooksLeavesForeignEntriesAlone(t *testing.T) {
	path := seedSettings(t, `{
	  "permissions": {"allow": ["Bash(ls:*)"]},
	  "hooks": {
	    "PreToolUse": [
	      {"matcher": "Bash", "hooks": [{"type": "command", "command": "/usr/local/bin/lint-guard --strict", "timeout": 9}]},
	      {"matcher": "*", "hooks": [{"type": "command", "command": "\"/opt/openbox\" hook claude-code PreToolUse", "timeout": 60}]}
	    ],
	    "Notification": [
	      {"hooks": [{"type": "command", "command": "notify-send hi"}]}
	    ]
	  }
	}`)

	if _, err := RemoveLocalHooks(path); err != nil {
		t.Fatalf("RemoveLocalHooks: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, kept := range []string{"lint-guard --strict", "notify-send hi", "Bash(ls:*)"} {
		if !strings.Contains(string(raw), kept) {
			t.Errorf("removal took a foreign entry with it (%q is gone):\n%s", kept, raw)
		}
	}
	if r := gjson.GetBytes(raw, "hooks.PreToolUse"); len(r.Array()) != 1 {
		t.Errorf("hooks.PreToolUse should keep exactly the foreign group, got: %s", r.Raw)
	}
	// The foreign group keeps its own timeout, matcher and command verbatim.
	if got := gjson.GetBytes(raw, "hooks.PreToolUse.0.hooks.0.timeout").Int(); got != 9 {
		t.Errorf("the foreign handler's timeout changed to %d", got)
	}
	if got := gjson.GetBytes(raw, "hooks.PreToolUse.0.matcher").String(); got != "Bash" {
		t.Errorf("the foreign group's matcher changed to %q", got)
	}
}

// TestRemoveLocalHooksOnAnAbsentFileIsSuccess. Uninstall runs across four
// surfaces unconditionally rather than branching on a detected provider, so
// "this machine never had one" is the common case, not an error.
func TestRemoveLocalHooksOnAnAbsentFileIsSuccess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nothing", "settings.json")
	removed, err := RemoveLocalHooks(path)
	if err != nil || removed != nil {
		t.Fatalf("RemoveLocalHooks on an absent file = %v, %v; want nil, nil", removed, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("removal created the file it was asked to clean")
	}
}

// TestRemoveLocalHooksRefusesAnUnparsableFile. sjson edits a malformed
// document without complaint, and a truncated settings file stops every hook
// in it applying — including the foreign ones — while reporting nothing. So
// refuse with the path, the way the install side refuses, and let the operator
// fix it by hand.
func TestRemoveLocalHooksRefusesAnUnparsableFile(t *testing.T) {
	path := seedSettings(t, `{"hooks": {"PreToolUse": [`)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := RemoveLocalHooks(path); err == nil {
		t.Fatal("RemoveLocalHooks accepted a malformed settings file")
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

// TestRemoveLocalHooksIsIdempotent. A second uninstall on an already-clean
// machine must exit quietly rather than reporting work: one-shot coverage
// passes on state a re-run corrupts.
func TestRemoveLocalHooksIsIdempotent(t *testing.T) {
	path := seedSettings(t, `{
	  "hooks": {"Stop": [{"hooks": [{"type": "command", "command": "\"/opt/openbox\" hook claude-code Stop", "timeout": 5}]}]}
	}`)
	if _, err := RemoveLocalHooks(path); err != nil {
		t.Fatalf("first RemoveLocalHooks: %v", err)
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	removed, err := RemoveLocalHooks(path)
	if err != nil {
		t.Fatalf("second RemoveLocalHooks: %v", err)
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

// TestRemoveLocalHooksDoesNotRewriteAFileItOwnsNothingIn. Touching a file with
// no OpenBox registration in it would reformat a document inside the
// developer's own repository for no change at all.
func TestRemoveLocalHooksDoesNotRewriteAFileItOwnsNothingIn(t *testing.T) {
	const body = `{
  "hooks": {
    "PreToolUse": [
      { "matcher": "Bash", "hooks": [ { "type": "command", "command": "own-guard" } ] }
    ]
  }
}
`
	path := seedSettings(t, body)
	removed, err := RemoveLocalHooks(path)
	if err != nil {
		t.Fatalf("RemoveLocalHooks: %v", err)
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

// TestRemoveLocalHooksNamesTheEventAndEngine. The report is the product: an
// operator has to be able to see which surface was cleaned and which engine
// path it had been pointing at.
func TestRemoveLocalHooksNamesTheEventAndEngine(t *testing.T) {
	path := seedSettings(t, `{
	  "hooks": {"SessionEnd": [{"hooks": [{"type": "command", "command": "\"/opt/ob/openbox\" hook claude-code SessionEnd", "timeout": 15}]}]}
	}`)
	removed, err := RemoveLocalHooks(path)
	if err != nil {
		t.Fatalf("RemoveLocalHooks: %v", err)
	}
	if len(removed) != 1 {
		t.Fatalf("removed = %v; want one entry", removed)
	}
	for _, want := range []string{"SessionEnd", "/opt/ob/openbox"} {
		if !strings.Contains(removed[0], want) {
			t.Errorf("the report %q does not name %q", removed[0], want)
		}
	}
}

// TestRemoveLocalHooksKeepsTheDocumentValid guards the failure mode that
// blocks its own repair: a file this command truncated cannot be rewritten by
// the install side either, because that refuses what it cannot parse.
func TestRemoveLocalHooksKeepsTheDocumentValid(t *testing.T) {
	path := seedSettings(t, `{
	  "model": "opus",
	  "hooks": {
	    "PreToolUse": [{"matcher": "*", "hooks": [{"type": "command", "command": "\"/o/openbox\" hook claude-code PreToolUse"}]}],
	    "SessionStart": [{"hooks": [{"type": "command", "command": "\"/o/openbox\" hook claude-code SessionStart"}]}]
	  }
	}`)
	if _, err := RemoveLocalHooks(path); err != nil {
		t.Fatalf("RemoveLocalHooks: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !gjson.ValidBytes(raw) {
		t.Fatalf("removal left an invalid document:\n%s", raw)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("removal left a document Claude Code cannot read: %v\n%s", err, raw)
	}
	if doc["model"] != "opus" {
		t.Errorf("an unrelated setting was lost: %v", doc)
	}
}
