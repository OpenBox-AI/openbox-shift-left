package main

import (
	"os"
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/cli/providers"
)

// claudeHooksWithThinkingSummaries is newInstalledMachine's claudeHooks
// fixture plus a showThinkingSummaries value at the document's top level, so
// these tests exercise the restore against a real hook-bearing settings
// document rather than a bare `{"showThinkingSummaries": ...}` file.
func claudeHooksWithThinkingSummaries(value string) string {
	return `{"showThinkingSummaries":` + value + `,"hooks":{"PreToolUse":[{"matcher":"*","hooks":[` +
		`{"type":"command","command":"\"/o/openbox\" hook claude-code PreToolUse","timeout":60}]}],` +
		`"SessionStart":[{"hooks":[{"type":"command","command":"\"/o/openbox\" hook claude-code SessionStart"}]}]}}`
}

const priorSettingsAbsentRecord = `{"schema":"openbox.claude-code.prior-settings/v1",` +
	`"keys":{"showThinkingSummaries":{"present":false}}}`

// TestUninstallRestoresThinkingSummariesAndPurgesTheRecord is acceptance
// criterion 7: the restore happens (removeHookSurfaces runs before
// removeArtifacts, uninstall.go:73-77), and the record itself is gone
// afterward -- `~/.openbox` is never removed recursively, so nothing purges
// this file except the by-name sweep in inv.posture.
func TestUninstallRestoresThinkingSummariesAndPurgesTheRecord(t *testing.T) {
	skipUnlessSupervised(t)
	m := newInstalledMachine(t)
	writeFile(t, m.userHooks, claudeHooksWithThinkingSummaries("true"))
	recPath := providers.ClaudePriorSettingsPath(m.home)
	writeFile(t, recPath, priorSettingsAbsentRecord)

	a, out := m.app(t)
	if code := a.runUninstall(nil); code != exitOK {
		t.Fatalf("exit = %d:\n%s", code, out.String())
	}
	s := out.String()
	if !strings.Contains(s, "showThinkingSummaries") {
		t.Errorf("no report of the settings restore:\n%s", s)
	}

	raw, err := os.ReadFile(m.userHooks)
	if err != nil {
		t.Fatalf("user settings deleted entirely: %v", err)
	}
	if strings.Contains(string(raw), "showThinkingSummaries") {
		t.Errorf("showThinkingSummaries survived uninstall (recorded prior state was absent): %s", raw)
	}
	if exists(recPath) {
		t.Errorf("the prior-settings record survived uninstall: %s", recPath)
	}
}

// TestUninstallRestoresThinkingSummariesToANonBooleanPriorValue is the
// four-row matrix's last row, exercised through the full uninstall command:
// whatever a developer had typed there before `init` comes back verbatim.
func TestUninstallRestoresThinkingSummariesToANonBooleanPriorValue(t *testing.T) {
	skipUnlessSupervised(t)
	m := newInstalledMachine(t)
	writeFile(t, m.userHooks, claudeHooksWithThinkingSummaries("true"))
	writeFile(t, providers.ClaudePriorSettingsPath(m.home),
		`{"schema":"openbox.claude-code.prior-settings/v1",`+
			`"keys":{"showThinkingSummaries":{"present":true,"raw":"\"yes\""}}}`)

	a, out := m.app(t)
	if code := a.runUninstall(nil); code != exitOK {
		t.Fatalf("exit = %d:\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "restored") {
		t.Errorf("no restore was reported:\n%s", out.String())
	}
	raw, err := os.ReadFile(m.userHooks)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"showThinkingSummaries":"yes"`) {
		t.Errorf("the non-boolean prior value was not restored verbatim: %s", raw)
	}
}

// TestUninstallReportsThinkingSummariesDriftWithoutFailing is acceptance
// criterion 1: a developer who changed the value after `init` keeps that
// change, and the command reports the drift and still exits clean.
func TestUninstallReportsThinkingSummariesDriftWithoutFailing(t *testing.T) {
	skipUnlessSupervised(t)
	m := newInstalledMachine(t)
	writeFile(t, m.userHooks, claudeHooksWithThinkingSummaries("false"))
	writeFile(t, providers.ClaudePriorSettingsPath(m.home), priorSettingsAbsentRecord)

	a, out := m.app(t)
	code := a.runUninstall(nil)
	s := out.String()
	if code != exitOK {
		t.Fatalf("drift alone must not fail the command: exit=%d\n%s", code, s)
	}
	if !strings.Contains(s, "left alone") || !strings.Contains(s, "showThinkingSummaries") {
		t.Errorf("the drift was not reported:\n%s", s)
	}
	raw, err := os.ReadFile(m.userHooks)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"showThinkingSummaries":false`) {
		t.Errorf("a drifted value must be left untouched: %s", raw)
	}
}

// TestUninstallOnAMachineWithNoThinkingSummariesRecordDoesNotMentionIt is
// criterion 6 at the full command level: most machines in this test file's
// own fixture never ran writeThinkingSummaries (newInstalledMachine's
// claudeHooks fixture carries no such key or record), and uninstall must not
// invent a report line for a restore that never happened.
func TestUninstallOnAMachineWithNoThinkingSummariesRecordDoesNotMentionIt(t *testing.T) {
	skipUnlessSupervised(t)
	m := newInstalledMachine(t)
	a, out := m.app(t)
	if code := a.runUninstall(nil); code != exitOK {
		t.Fatalf("exit = %d:\n%s", code, out.String())
	}
	if strings.Contains(out.String(), "showThinkingSummaries") {
		t.Errorf("a restore was reported on a machine with no record:\n%s", out.String())
	}
}

// TestUninstallKeepsThinkingSummariesRecordWhenTheRestoreDidNotComplete is the
// strand the tests above do not cover: removeArtifacts purges the
// prior-settings record unconditionally (uninstall.go's inv.posture sweep),
// even when removeHookSurfaces never got to restore from it because the
// settings file itself would not parse (RemoveLocalHooks refuses unparsable
// JSON, and the loop `continue`s past the restore call for that surface). A
// second uninstall against a now-record-less machine reads Recorded=false and
// is a no-op, so the developer's original showThinkingSummaries value is
// unrecoverable. Mirrors TestUninstallKeepsWhatARetryNeedsWhenALaneRefuses,
// which pins the same shape for the activation record.
func TestUninstallKeepsThinkingSummariesRecordWhenTheRestoreDidNotComplete(t *testing.T) {
	skipUnlessSupervised(t)
	m := newInstalledMachine(t)
	valid := claudeHooksWithThinkingSummaries("true")
	// A trailing comma before the final close-brace: gjson.ValidBytes refuses
	// it (verified against this repo's own gjson dependency), so
	// RemoveLocalHooks errors and removeHookSurfaces never reaches the
	// restore call for this surface.
	writeFile(t, m.userHooks, valid[:len(valid)-1]+",}")
	recPath := providers.ClaudePriorSettingsPath(m.home)
	const priorValue = `{"schema":"openbox.claude-code.prior-settings/v1",` +
		`"keys":{"showThinkingSummaries":{"present":true,"raw":"\"custom\""}}}`
	writeFile(t, recPath, priorValue)

	a, out := m.app(t)
	code := a.runUninstall(nil)
	if code == exitOK {
		t.Fatalf("an unparsable settings file must not report success:\n%s", out.String())
	}
	if !exists(recPath) {
		t.Fatalf("the prior-settings record was purged even though the restore never ran, so the "+
			"developer's original showThinkingSummaries value is now unrecoverable:\n%s", out.String())
	}

	// The developer fixes the JSON by hand, exactly as the report tells them
	// to: the trailing comma is gone, and the forced key and hook
	// registrations are both still there, untouched by the failed first run.
	writeFile(t, m.userHooks, valid)

	b, second := m.app(t)
	if code := b.runUninstall(nil); code != exitOK {
		t.Fatalf("second uninstall, after the fix, exit = %d:\n%s", code, second.String())
	}
	raw, err := os.ReadFile(m.userHooks)
	if err != nil {
		t.Fatalf("user settings deleted entirely: %v", err)
	}
	if !strings.Contains(string(raw), `"showThinkingSummaries":"custom"`) {
		t.Errorf("the original value was not restored once the restore could finally run: %s", raw)
	}
	if exists(recPath) {
		t.Errorf("the prior-settings record survived the completing uninstall: %s", recPath)
	}
}

// TestUninstallLeavesSettingsUntouchedWhenThePriorValueIsCorrupted is the
// splice-safety gap at the full command: RestoreThinkingSummaries refuses a
// corrupted "raw" field rather than splicing it into the developer's real
// settings file (see thinkingsummaries_test.go's unit-level pin), so this
// surface's settings.json comes back byte-for-byte unchanged, the record
// survives for a later retry -- mirroring
// TestUninstallKeepsThinkingSummariesRecordWhenTheRestoreDidNotComplete's
// shape for the other cause of an incomplete restore -- and the command still
// completes rather than aborting: it reports the problem and lets every other
// step run, the same "reported failure, never fatal" contract that test uses.
func TestUninstallLeavesSettingsUntouchedWhenThePriorValueIsCorrupted(t *testing.T) {
	skipUnlessSupervised(t)
	m := newInstalledMachine(t)
	// No hook entries here on purpose: RemoveLocalHooks is a true no-op on a
	// file with nothing of ours in it (it skips the write when its own
	// candidate output is byte-equal to what it read), so this settings file
	// is untouched for a reason independent of the fix under test, and the
	// byte-identical assertion below isolates the restore path alone.
	writeFile(t, m.userHooks, `{"showThinkingSummaries": true}`)
	before, err := os.ReadFile(m.userHooks)
	if err != nil {
		t.Fatal(err)
	}
	recPath := providers.ClaudePriorSettingsPath(m.home)
	writeFile(t, recPath,
		`{"schema":"openbox.claude-code.prior-settings/v1",`+
			`"keys":{"showThinkingSummaries":{"present":true,"raw":"tru"}}}`)

	a, out := m.app(t)
	code := a.runUninstall(nil)
	if code == exitOK {
		t.Fatalf("a corrupted prior value must not report success:\n%s", out.String())
	}
	s := out.String()
	if !strings.Contains(s, "showThinkingSummaries") {
		t.Errorf("no report naming the setting that could not be restored:\n%s", s)
	}
	after, err := os.ReadFile(m.userHooks)
	if err != nil {
		t.Fatalf("user settings deleted entirely: %v", err)
	}
	if string(after) != string(before) {
		t.Errorf("settings.json changed despite the refusal:\nbefore: %s\nafter:  %s", before, after)
	}
	if !exists(recPath) {
		t.Fatalf("the prior-settings record was purged even though the restore never completed, so the "+
			"developer's original showThinkingSummaries value is now unrecoverable:\n%s", s)
	}
}

// TestUninstallNamesTheRecordWhenItBlocksTheRestore is the attribution fix: a
// restore failure caused by the internal prior-settings record must name THAT
// file in its output, not the developer's settings.json, which in this
// scenario parses fine and was never at fault. The record's OWN envelope is
// unparsable here (as opposed to the sibling test above, where the envelope
// parses but "raw" does not) -- loadPriorSettings already refused this shape
// before this fix, so this test isolates the attribution bug from the
// splice-safety one.
func TestUninstallNamesTheRecordWhenItBlocksTheRestore(t *testing.T) {
	skipUnlessSupervised(t)
	m := newInstalledMachine(t)
	writeFile(t, m.userHooks, claudeHooksWithThinkingSummaries("true"))
	recPath := providers.ClaudePriorSettingsPath(m.home)
	writeFile(t, recPath, `{not json`)

	a, out := m.app(t)
	code := a.runUninstall(nil)
	if code == exitOK {
		t.Fatalf("a record this command cannot read must not report success:\n%s", out.String())
	}
	s := out.String()
	if !strings.Contains(s, recPath+" could not be read") {
		t.Errorf("the report does not name the prior-settings record as the blocker:\n%s", s)
	}
	if strings.Contains(s, m.userHooks+" could not be parsed") {
		t.Errorf("the report blames settings.json, which parses fine, instead of the record:\n%s", s)
	}
	if !exists(recPath) {
		t.Fatalf("the unreadable record was purged anyway, losing the only trace of the original value:\n%s", s)
	}
}
