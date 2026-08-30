package claudecode

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A hook entry left behind at a DIFFERENT engine path is OURS and it is WRONG —
// not a foreign hook to preserve. Matching by exact command string could not tell
// those apart, so an install run once with another HOME left both engines
// registered: every hook fired twice and every governed tool call was stored
// twice, silently, for the life of the project. Re-init has to repair that, and
// it is the only command that can.
func TestReInitReplacesAnOpenBoxEntryAtAStaleEnginePath(t *testing.T) {
	dir := t.TempDir()
	const stale = "/opt/openbox-from-another-home/bin/openbox"
	engine := filepath.Join(dir, "bin", "openbox")

	settingsPath := filepath.Join(dir, ".claude", "settings.local.json")
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o755); err != nil {
		t.Fatal(err)
	}
	old := map[string]any{"hooks": map[string]any{}}
	for _, ev := range localHookEvents {
		handlers := []any{map[string]any{
			"type":    "command",
			"command": localHookCommand(stale, "hook claude-code "+ev.Event),
			"timeout": ev.Timeout,
		}}
		if ev.Event == "PreToolUse" {
			handlers = append(handlers, map[string]any{
				"type":        "command",
				"command":     localHookCommand(stale, "rewake claude-code"),
				"asyncRewake": true,
				"timeout":     rewakeHookTimeoutSec,
			})
		}
		old["hooks"].(map[string]any)[ev.Event] = []any{map[string]any{
			"matcher": ev.Matcher,
			"hooks":   handlers,
		}}
	}
	// Two things that must survive: a hook the developer wrote, and a compound
	// command that merely EMBEDS our invocation.
	old["hooks"].(map[string]any)["PostToolUse"] = append(
		old["hooks"].(map[string]any)["PostToolUse"].([]any),
		map[string]any{"matcher": "*", "hooks": []any{
			map[string]any{"type": "command", "command": "my-own-linter"},
			map[string]any{"type": "command", "command": `my-linter && "` + stale + `" hook claude-code PostToolUse`},
		}})
	raw, _ := json.MarshalIndent(old, "", "  ")
	if err := os.WriteFile(settingsPath, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	notice := captureStderr(t, func() {
		if err := writeLocalHooks(dir, engine); err != nil {
			t.Fatalf("re-init: %v", err)
		}
	})

	got, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), stale+`" hook claude-code`) {
		t.Error("a stale-engine OpenBox registration survived re-init — both engines still fire")
	}
	if strings.Contains(string(got), stale+`" rewake`) {
		t.Error("the stale-engine rewake handler survived re-init")
	}

	var settings struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Type    string `json:"type"`
				Command string `json:"command"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(got, &settings); err != nil {
		t.Fatal(err)
	}

	for _, ev := range localHookEvents {
		want := 1
		if ev.Event == "PreToolUse" {
			want = 2 // the gate plus the approval watcher
		}
		owned := 0
		for _, entry := range settings.Hooks[ev.Event] {
			if len(entry.Hooks) == 0 {
				t.Errorf("%s has an entry with an empty hooks array", ev.Event)
			}
			for _, h := range entry.Hooks {
				path, ok := ownedLocalHook(h.Type, h.Command, ev.Event)
				if !ok {
					continue
				}
				owned++
				if path != engine {
					t.Errorf("%s owned handler points at %q, want the engine being installed %q", ev.Event, path, engine)
				}
			}
		}
		if owned != want {
			t.Errorf("%s has %d OpenBox handlers, want %d", ev.Event, owned, want)
		}
	}

	var foreign, compound bool
	for _, entry := range settings.Hooks["PostToolUse"] {
		for _, h := range entry.Hooks {
			switch {
			case h.Command == "my-own-linter":
				foreign = true
			case strings.HasPrefix(h.Command, "my-linter &&"):
				compound = true
			}
		}
	}
	if !foreign {
		t.Error("re-init dropped a foreign PostToolUse hook the developer had added")
	}
	if !compound {
		t.Error("re-init dropped a compound foreign command that merely embeds our invocation")
	}

	// Swapping a governing binary without saying so is the same class of problem
	// as the silent duplicate it repairs.
	for _, want := range []string{stale, engine, settingsPath, "PreToolUse"} {
		if !strings.Contains(notice, want) {
			t.Errorf("replacement notice does not mention %q; got:\n%s", want, notice)
		}
	}
}

// The notice must stay silent when nothing was stale, or every ordinary re-init
// trains the reader to ignore it.
func TestReInitAtTheSameEnginePathPrintsNothing(t *testing.T) {
	dir := t.TempDir()
	engine := filepath.Join(dir, "bin", "openbox")
	if err := writeLocalHooks(dir, engine); err != nil {
		t.Fatalf("first init: %v", err)
	}
	notice := captureStderr(t, func() {
		if err := writeLocalHooks(dir, engine); err != nil {
			t.Fatalf("re-init: %v", err)
		}
	})
	if notice != "" {
		t.Errorf("re-init at the same engine printed a replacement notice:\n%s", notice)
	}
}

// The check and the fix have to agree, or `openbox doctor` sends a developer to
// run a command that does not clear the warning. They share one classifier for
// that reason, and this is what holds the pairing: audit sees two engines, the
// merge repairs it, audit sees one.
func TestTheAuditAgreesWithWhatReInitRepairs(t *testing.T) {
	dir := t.TempDir()
	const stale = "/opt/openbox-from-another-home/bin/openbox"
	engine := filepath.Join(dir, "bin", "openbox")

	settingsPath := filepath.Join(dir, ".claude", "settings.local.json")
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o755); err != nil {
		t.Fatal(err)
	}
	// The live shape: an unquoted registration from an install run with another
	// HOME, beside the current quoted one.
	seed := `{"hooks":{"PreToolUse":[` +
		`{"matcher":"*","hooks":[{"type":"command","command":"` + stale + ` hook claude-code PreToolUse"}]},` +
		`{"matcher":"*","hooks":[{"type":"command","command":"\"` + engine + `\" hook claude-code PreToolUse"}]}]}}`
	if err := os.WriteFile(settingsPath, []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}

	before, err := AuditLocalHooks(dir)
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	if len(before.Engines) != 2 {
		t.Fatalf("audit found %d engines before the repair, want 2: %v", len(before.Engines), before.Engines)
	}
	if len(before.DuplicateEvents) == 0 {
		t.Error("audit did not flag PreToolUse as registered more than once")
	}

	captureStderr(t, func() {
		if err := writeLocalHooks(dir, engine); err != nil {
			t.Fatalf("re-init: %v", err)
		}
	})

	after, err := AuditLocalHooks(dir)
	if err != nil {
		t.Fatalf("audit after repair: %v", err)
	}
	if len(after.Engines) != 1 || after.Engines[0] != engine {
		t.Errorf("after one re-init the audit still reports %v, want exactly [%s] — "+
			"doctor would keep warning after the command it recommends", after.Engines, engine)
	}
	if len(after.DuplicateEvents) != 0 {
		t.Errorf("after one re-init the audit still reports duplicates: %v", after.DuplicateEvents)
	}
}

// The OTHER shape doctor warns about: our own invocation registered twice at the
// SAME engine. It double-counts exactly as a second engine does, and doctor names
// this command as the remedy for both — so a re-init that could not clear it
// would leave a developer re-running a command that never removes the warning.
//
// The gate and the approval watcher must survive that collapse: they are two
// registrations under one event key and only their INVOCATIONS distinguish them.
func TestReInitCollapsesADuplicateRegistrationAtTheSameEngine(t *testing.T) {
	dir := t.TempDir()
	engine := filepath.Join(dir, "bin", "openbox")
	settingsPath := filepath.Join(dir, ".claude", "settings.local.json")
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o755); err != nil {
		t.Fatal(err)
	}
	// One copy in the same entry, one in a separate entry — a hand-edited file or
	// a merge resolution produces either.
	seed := `{"hooks":{` +
		`"Stop":[{"hooks":[` +
		`{"type":"command","command":"\"` + engine + `\" hook claude-code Stop"},` +
		`{"type":"command","command":"\"` + engine + `\" hook claude-code Stop"}]}],` +
		`"PreToolUse":[` +
		`{"matcher":"*","hooks":[{"type":"command","command":"\"` + engine + `\" hook claude-code PreToolUse"},` +
		`{"type":"command","command":"\"` + engine + `\" rewake claude-code"}]},` +
		`{"matcher":"*","hooks":[{"type":"command","command":"\"` + engine + `\" hook claude-code PreToolUse"}]}]}}`
	if err := os.WriteFile(settingsPath, []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}

	before, err := AuditLocalHooks(dir)
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	if len(before.DuplicateEvents) != 2 {
		t.Fatalf("audit flagged %v, want both Stop and PreToolUse", before.DuplicateEvents)
	}

	notice := captureStderr(t, func() {
		if err := writeLocalHooks(dir, engine); err != nil {
			t.Fatalf("re-init: %v", err)
		}
	})

	after, err := AuditLocalHooks(dir)
	if err != nil {
		t.Fatalf("audit after repair: %v", err)
	}
	if len(after.DuplicateEvents) != 0 {
		t.Errorf("after the remedy doctor recommends, the audit still reports %v — "+
			"the warning would never clear", after.DuplicateEvents)
	}
	if len(after.Engines) != 1 || after.Engines[0] != engine {
		t.Errorf("engines after repair = %v, want exactly [%s]", after.Engines, engine)
	}
	for _, want := range []string{"Stop", "PreToolUse", settingsPath} {
		if !strings.Contains(notice, want) {
			t.Errorf("de-duplication notice does not mention %q; got:\n%s", want, notice)
		}
	}

	// The gate and the watcher are two DIFFERENT invocations under one event key.
	// Collapsing by event rather than by invocation would delete the watcher and
	// the approval hold would never wake.
	raw, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(raw), "rewake claude-code"); n != 1 {
		t.Errorf("rewake watcher appears %d times, want exactly 1", n)
	}
	if n := strings.Count(string(raw), "hook claude-code PreToolUse"); n != 1 {
		t.Errorf("PreToolUse gate appears %d times, want exactly 1", n)
	}
}

// An absent settings file is the normal state for a global-scope install, and an
// unreadable one is what a developer already knows is broken. Neither may be an
// error the caller has to special-case beyond rendering it.
func TestAuditLocalHooksOnAbsentAndUnparsableFiles(t *testing.T) {
	dir := t.TempDir()
	audit, err := AuditLocalHooks(dir)
	if err != nil {
		t.Errorf("absent settings file returned an error: %v", err)
	}
	if audit.Present || len(audit.Engines) != 0 {
		t.Errorf("absent file reported as present: %+v", audit)
	}
	if audit.SettingsPath == "" {
		t.Error("audit must name the path it inspected even when absent")
	}

	if err := os.MkdirAll(filepath.Join(dir, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(audit.SettingsPath, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := AuditLocalHooks(dir); err == nil {
		t.Error("invalid JSON must surface as an error, not as zero engines — silently reporting none reads as governed-and-clean")
	}
}

// The classifier is the one thing standing between this merge and deleting a
// developer's own hook, so it is tested directly rather than only through the
// merge. Over-keep is the safe direction: every uncertain shape must read as
// foreign.
func TestOwnedLocalHookRecognizesOurArgvShapeOnly(t *testing.T) {
	const engine = "/opt/openbox/bin/openbox"
	cases := []struct {
		name       string
		hookType   string
		command    string
		event      string
		wantEngine string
		wantOK     bool
	}{
		{"quoted, as we write it", "command",
			`"` + engine + `" hook claude-code PreToolUse`, "PreToolUse", engine, true},
		{"unquoted, as a pre-quoting install wrote it", "command",
			engine + " hook claude-code SessionStart", "SessionStart", engine, true},
		{"the rewake watcher", "command",
			`"` + engine + `" rewake claude-code`, "PreToolUse", engine, true},
		{"our invocation filed under the wrong event", "command",
			`"` + engine + `" hook claude-code Stop`, "PreToolUse", "", false},
		{"a hook the developer wrote", "command", "my-own-linter", "PostToolUse", "", false},
		{"a compound command that embeds ours", "command",
			`my-linter && "` + engine + `" hook claude-code Stop`, "Stop", "", false},
		{"not a command handler", "prompt",
			`"` + engine + `" hook claude-code Stop`, "Stop", "", false},
		{"unterminated quote", "command",
			`"` + engine + ` hook claude-code Stop`, "Stop", "", false},
		{"empty command", "command", "", "Stop", "", false},
		{"engine token only", "command", engine, "Stop", "", false},
		{"trailing argument beyond our shape", "command",
			`"` + engine + `" hook claude-code Stop --debug`, "Stop", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ownedLocalHook(tc.hookType, tc.command, tc.event)
			if ok != tc.wantOK || got != tc.wantEngine {
				t.Errorf("ownedLocalHook(%q, %q, %q) = (%q, %v), want (%q, %v)",
					tc.hookType, tc.command, tc.event, got, ok, tc.wantEngine, tc.wantOK)
			}
		})
	}
}

// captureStderr collects what fn writes to os.Stderr. The notice is written from
// inside writeLocalHooks so its signature — and therefore the three pre-existing
// call sites — stay unchanged.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stderr
	os.Stderr = w
	done := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		io.Copy(&buf, r)
		done <- buf.String()
	}()
	fn()
	os.Stderr = saved
	w.Close()
	out := <-done
	r.Close()
	return out
}

// A re-init at the SAME engine path must reconcile the registration SHAPE, not
// just skip on the command match. Found live: that decision UserPromptSubmit
// ceiling raise (5s → the gating ceiling) never arrived on a re-init, so the
// prompt gate was killed by Claude Code's old 5s timeout mid-evaluation and
// failed open — through the very `openbox init` the docs name as the upgrade
// step. Foreign hooks keep their own shape.
func TestReInitReconcilesRegistrationShape(t *testing.T) {
	dir := t.TempDir()
	engine := filepath.Join(dir, "bin", "openbox")

	settingsPath := filepath.Join(dir, ".claude", "settings.local.json")
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o755); err != nil {
		t.Fatal(err)
	}
	// Our entries at the CURRENT engine path, but in the pre-that decision
	// shape: UserPromptSubmit still at 5s with no statusMessage, plus a stale
	// watcher timeout — and one foreign hook whose shape must survive
	// untouched.
	old := map[string]any{"hooks": map[string]any{
		"UserPromptSubmit": []any{map[string]any{
			"matcher": "",
			"hooks": []any{
				map[string]any{"type": "command", "command": localHookCommand(engine, "hook claude-code UserPromptSubmit"), "timeout": 5},
				map[string]any{"type": "command", "command": "my-prompt-linter", "timeout": 7},
			},
		}},
		"PreToolUse": []any{map[string]any{
			"matcher": "*",
			"hooks": []any{
				map[string]any{"type": "command", "command": localHookCommand(engine, "hook claude-code PreToolUse"), "timeout": 5},
				map[string]any{"type": "command", "command": localHookCommand(engine, "rewake claude-code"), "asyncRewake": true, "timeout": 60},
			},
		}},
	}}
	raw, _ := json.MarshalIndent(old, "", "  ")
	if err := os.WriteFile(settingsPath, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := writeLocalHooks(dir, engine); err != nil {
		t.Fatal(err)
	}

	read := func() map[string]any {
		t.Helper()
		raw, err := os.ReadFile(settingsPath)
		if err != nil {
			t.Fatal(err)
		}
		var got map[string]any
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	hookFields := func(got map[string]any, event, command string) map[string]any {
		t.Helper()
		entries, _ := got["hooks"].(map[string]any)[event].([]any)
		want := unquoteHookCommand(command)
		for _, e := range entries {
			entry, _ := e.(map[string]any)
			inner, _ := entry["hooks"].([]any)
			for _, h := range inner {
				hook, _ := h.(map[string]any)
				cmd, _ := hook["command"].(string)
				if unquoteHookCommand(cmd) == want {
					return hook
				}
			}
		}
		t.Fatalf("no %s hook with command %q", event, command)
		return nil
	}

	got := read()
	ups := hookFields(got, "UserPromptSubmit", localHookCommand(engine, "hook claude-code UserPromptSubmit"))
	if ts, _ := ups["timeout"].(float64); int(ts) != preToolUseHookTimeoutSec {
		t.Errorf("UserPromptSubmit timeout = %v, want the gating ceiling %d", ups["timeout"], preToolUseHookTimeoutSec)
	}
	if msg, _ := ups["statusMessage"].(string); msg == "" {
		t.Error("UserPromptSubmit gate must carry a statusMessage after reconcile")
	}
	watcher := hookFields(got, "PreToolUse", localHookCommand(engine, "rewake claude-code"))
	if ts, _ := watcher["timeout"].(float64); int(ts) != rewakeHookTimeoutSec {
		t.Errorf("rewake watcher timeout = %v, want %d", watcher["timeout"], rewakeHookTimeoutSec)
	}
	foreign := hookFields(got, "UserPromptSubmit", "my-prompt-linter")
	if ts, _ := foreign["timeout"].(float64); int(ts) != 7 {
		t.Errorf("foreign hook timeout = %v, want its own 7 — reconcile must not touch foreign hooks", foreign["timeout"])
	}

	// No duplicate entries were appended alongside the reconciled ones, and a
	// second run is byte-stable (idempotent).
	first, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeLocalHooks(dir, engine); err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Error("second re-init changed the file — reconcile is not idempotent")
	}
}
