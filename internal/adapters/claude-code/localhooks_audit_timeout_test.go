package claudecode

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// writeSettingsHooks writes a minimal settings.local.json under dir/.claude
// whose "hooks" block is exactly the given events -> entries value, and
// returns the settings path AuditHooks should be called with.
func writeSettingsHooks(t *testing.T, dir string, hooks map[string]any) string {
	t.Helper()
	settingsPath := ProjectSettingsPath(dir)
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o755); err != nil {
		t.Fatal(err)
	}
	settings := map[string]any{"hooks": hooks}
	raw, err := json.Marshal(settings)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(settingsPath, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	return settingsPath
}

// ownedPreToolUseEntry builds the "hooks.PreToolUse" array shape for one
// OpenBox-owned primary handler at engine, with the given installed timeout.
// omitTimeout writes no "timeout" key at all, the shape Claude Code treats as
// its own 60s default.
func ownedEventEntry(engine, event string, timeout int, omitTimeout bool) []any {
	hook := map[string]any{"type": "command", "command": localHookCommand(engine, "hook claude-code "+event)}
	if !omitTimeout {
		hook["timeout"] = timeout
	}
	return []any{map[string]any{"matcher": "*", "hooks": []any{hook}}}
}

// TestAuditFlagsAShortTimeoutOnAGatedEvent a PreToolUse entry installed at 5s
// predates the gate's timeout moving up to preToolUseHookTimeoutSec (30s); a
// hook Claude Code can still kill mid-evaluation is exactly the ungoverned-
// tool-call gap doctor must surface.
func TestAuditFlagsAShortTimeoutOnAGatedEvent(t *testing.T) {
	dir := t.TempDir()
	const engine = "/opt/openbox/bin/openbox"
	settingsPath := writeSettingsHooks(t, dir, map[string]any{
		"PreToolUse": ownedEventEntry(engine, "PreToolUse", 5, false),
	})

	audit, err := AuditHooks(settingsPath)
	if err != nil {
		t.Fatalf("AuditHooks: %v", err)
	}
	if len(audit.ShortTimeoutEvents) != 1 || audit.ShortTimeoutEvents[0] != "PreToolUse" {
		t.Errorf("ShortTimeoutEvents = %v, want [PreToolUse]", audit.ShortTimeoutEvents)
	}
}

// TestAuditIgnoresAnAbsentTimeout an absent "timeout" key is Claude Code's own
// default (60s), longer than every spec in localHookEvents, so it must never
// be reported as short.
func TestAuditIgnoresAnAbsentTimeout(t *testing.T) {
	dir := t.TempDir()
	const engine = "/opt/openbox/bin/openbox"
	settingsPath := writeSettingsHooks(t, dir, map[string]any{
		"PreToolUse": ownedEventEntry(engine, "PreToolUse", 0, true),
	})

	audit, err := AuditHooks(settingsPath)
	if err != nil {
		t.Fatalf("AuditHooks: %v", err)
	}
	if len(audit.ShortTimeoutEvents) != 0 {
		t.Errorf("an absent timeout must not be flagged: %v", audit.ShortTimeoutEvents)
	}
}

// TestAuditIgnoresANonGatedEventAtItsOwnSpec a 5s non-gated hook (Stop, using
// otherHookTimeoutSec) installed at its own 5s spec is correctly configured;
// comparing every event to the same fixed constant instead of its own
// localHookEvents entry would misflag this as short.
func TestAuditIgnoresANonGatedEventAtItsOwnSpec(t *testing.T) {
	dir := t.TempDir()
	const engine = "/opt/openbox/bin/openbox"
	settingsPath := writeSettingsHooks(t, dir, map[string]any{
		"Stop": ownedEventEntry(engine, "Stop", otherHookTimeoutSec, false),
	})

	audit, err := AuditHooks(settingsPath)
	if err != nil {
		t.Fatalf("AuditHooks: %v", err)
	}
	if len(audit.ShortTimeoutEvents) != 0 {
		t.Errorf("a non-gated event at its own spec must not be flagged: %v", audit.ShortTimeoutEvents)
	}
}

// TestAuditIgnoresAForeignShortTimeoutEntry a developer's own hook registered
// under the same event, with a tiny timeout, is not OpenBox's to warn about:
// ownedLocalHook must gate the timeout check the same way it gates engines
// and DuplicateEvents.
func TestAuditIgnoresAForeignShortTimeoutEntry(t *testing.T) {
	dir := t.TempDir()
	settingsPath := writeSettingsHooks(t, dir, map[string]any{
		"PreToolUse": []any{map[string]any{"matcher": "*", "hooks": []any{
			map[string]any{"type": "command", "command": "my-own-linter", "timeout": 1},
		}}},
	})

	audit, err := AuditHooks(settingsPath)
	if err != nil {
		t.Fatalf("AuditHooks: %v", err)
	}
	if len(audit.ShortTimeoutEvents) != 0 {
		t.Errorf("a foreign entry must not be flagged: %v", audit.ShortTimeoutEvents)
	}
}
