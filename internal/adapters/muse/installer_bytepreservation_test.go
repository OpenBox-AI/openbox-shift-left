package muse

import (
	"os"
	"strings"
	"testing"
)

// Muse's settings file is the developer's own. An install followed by an
// uninstall must give back every byte that was not an OpenBox handler:
// formatting, key order, foreign top-level keys and foreign handlers alike.
var preservedDocuments = map[string]string{
	"compact, foreign key and handler":   `{"schema_version":1,"theme":"dark","hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"/usr/local/bin/audit"}]}]}}`,
	"tab indented, no trailing newline":  "{\n\t\"schema_version\": 1,\n\t\"model\": \"x\",\n\t\"hooks\": {\n\t\t\"Stop\": [\n\t\t\t{\n\t\t\t\t\"hooks\": [{\"type\": \"command\", \"command\": \"notify-send done\"}]\n\t\t\t}\n\t\t]\n\t}\n}",
	"hooks block absent":                 "{\n  \"schema_version\": 1,\n  \"zeta\": [1, 2],\n  \"alpha\": {\"b\": 1, \"a\": 2}\n}\n",
	"empty object":                       "{}\n",
	"foreign handler in an event we own": "{\n  \"schema_version\": 1,\n  \"hooks\": {\n    \"UserPromptSubmit\": [\n      {\"hooks\": [{\"type\": \"command\", \"command\": \"echo hi\", \"timeout\": 3, \"async\": true}]}\n    ],\n    \"PreCompact\": [{\"hooks\": [{\"type\": \"command\", \"command\": \"echo compacting\"}]}]\n  }\n}\n",
	"odd key order and unicode":          "{\"z\":\"é\",\"schema_version\":1,\"a\":null,\"hooks\":{}}\n",
}

func TestInstallThenUninstallRestoresTheOriginalBytes(t *testing.T) {
	for name, original := range preservedDocuments {
		t.Run(name, func(t *testing.T) {
			i, path := newTestInstaller(t)
			writeFile(t, path, original)
			if err := i.Install(testRef); err != nil {
				t.Fatal(err)
			}
			if readFile(t, path) == original {
				t.Fatal("install changed nothing")
			}
			removed, err := RemoveHooks(path)
			if err != nil {
				t.Fatal(err)
			}
			if len(removed) != ExpectedHandlers() {
				t.Errorf("removed %d handlers, want %d: %v", len(removed), ExpectedHandlers(), removed)
			}
			got := readFile(t, path)
			// An empty hooks object the install found is one the removal may
			// drop with the events it emptied; everything else must be exact.
			if name == "odd key order and unicode" {
				got = strings.Replace(got, `,"hooks":{}`, "", 1)
				original = strings.Replace(original, `,"hooks":{}`, "", 1)
			}
			if got != original {
				t.Errorf("round trip changed the file:\n--- original\n%q\n--- after\n%q", original, got)
			}
		})
	}
}

func TestInstallKeepsForeignBytesVerbatim(t *testing.T) {
	for name, original := range preservedDocuments {
		t.Run(name, func(t *testing.T) {
			i, path := newTestInstaller(t)
			writeFile(t, path, original)
			if err := i.Install(testRef); err != nil {
				t.Fatal(err)
			}
			after := readFile(t, path)
			// Every foreign handler and key, byte for byte, is still in the file.
			for _, frag := range []string{`"/usr/local/bin/audit"`, `notify-send done`, `"echo hi"`, `"async": true`,
				`"echo compacting"`, `"zeta": [1, 2]`, `"alpha": {"b": 1, "a": 2}`, `"z":"é"`} {
				if strings.Contains(original, frag) && !strings.Contains(after, frag) {
					t.Errorf("foreign fragment %s changed:\n%s", frag, after)
				}
			}
		})
	}
}

func TestUninstallKeepsAForeignHandlerSharingAGroupWithOurs(t *testing.T) {
	i, path := newTestInstaller(t)
	cmd, err := formatInvocation(testEngine, "", false, HookPostToolUse)
	if err != nil {
		t.Fatal(err)
	}
	original := `{"schema_version":1,"hooks":{"PostToolUse":[{"matcher":"Bash","hooks":[` +
		`{"type":"command","command":"first"},{"type":"command","command":"` + strings.ReplaceAll(cmd, `"`, `\"`) + `"},{"type":"command","command":"last"}]}]}}`
	writeFile(t, path, original)
	removed, err := RemoveHooks(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || removed[0] != "PostToolUse "+testEngine {
		t.Fatalf("removed = %v", removed)
	}
	want := `{"schema_version":1,"hooks":{"PostToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"first"},{"type":"command","command":"last"}]}]}}`
	if got := readFile(t, path); got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
	_ = i
}

func TestUninstallIgnoresALookalike(t *testing.T) {
	_, path := newTestInstaller(t)
	original := `{"schema_version":1,"hooks":{"PreToolUse":[{"hooks":[` +
		`{"type":"command","command":"my-audit && \"/x/openbox\" hook muse PreToolUse"},` +
		`{"type":"command","command":"\"/x/openbox\" hook codex PreToolUse"},` +
		`{"type":"command","command":"\"/x/openbox\" hook muse PreToolUse extra"},` +
		`{"type":"command","command":"\"/x/openbox\" hook muse NotAnEvent"},` +
		`{"type":"http","command":"\"/x/openbox\" hook muse PreToolUse"}]}]}}`
	writeFile(t, path, original)
	removed, err := RemoveHooks(path)
	if err != nil || len(removed) != 0 {
		t.Fatalf("removed %v, %v", removed, err)
	}
	if got := readFile(t, path); got != original {
		t.Errorf("a lookalike was touched:\n%s", got)
	}
}

func TestUninstallLeavesSchemaVersionAndIsIdempotent(t *testing.T) {
	i, path := newTestInstaller(t)
	if err := i.Install(testRef); err != nil {
		t.Fatal(err)
	}
	if _, err := RemoveHooks(path); err != nil {
		t.Fatal(err)
	}
	after := readFile(t, path)
	if !strings.Contains(after, `"schema_version": 1`) || strings.Contains(after, "hook muse") {
		t.Errorf("after uninstall:\n%s", after)
	}
	removed, err := RemoveHooks(path)
	if err != nil || len(removed) != 0 {
		t.Errorf("second removal = %v, %v", removed, err)
	}
	if readFile(t, path) != after {
		t.Error("a no-op removal rewrote the file")
	}
}

func TestUninstallOfAnAbsentFileIsSuccess(t *testing.T) {
	_, path := newTestInstaller(t)
	if removed, err := RemoveHooks(path); err != nil || removed != nil {
		t.Fatalf("= %v, %v", removed, err)
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("removal created the file")
	}
}

func TestUninstallRefusesUnreadableSettingsUntouched(t *testing.T) {
	_, path := newTestInstaller(t)
	writeFile(t, path, `{"hooks": `)
	if _, err := RemoveHooks(path); err == nil {
		t.Fatal("removal edited a file Muse cannot read")
	}
	if readFile(t, path) != `{"hooks": ` {
		t.Error("the file was touched")
	}
}

func TestUninstallFindsOwnedHandlersOnAnyEventKey(t *testing.T) {
	_, path := newTestInstaller(t)
	cmd, _ := formatInvocation(testEngine, "", false, HookStop)
	writeFile(t, path, `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"`+strings.ReplaceAll(cmd, `"`, `\"`)+`"}]}]}}`)
	removed, err := RemoveHooks(path)
	if err != nil || len(removed) != 1 {
		t.Fatalf("removed %v, %v", removed, err)
	}
}

func TestEventKeysAreNeverPathSyntax(t *testing.T) {
	_, path := newTestInstaller(t)
	original := `{"hooks":{"a.b*c":[{"hooks":[{"type":"command","command":"keep"}]}],"x":[{"hooks":[{"type":"command","command":"keep2"}]}]}}`
	writeFile(t, path, original)
	if removed, err := RemoveHooks(path); err != nil || len(removed) != 0 {
		t.Fatalf("removed %v, %v", removed, err)
	}
	if readFile(t, path) != original {
		t.Error("an odd event key was rewritten")
	}
}

func TestHookInvocationMarkersMatchWhatInstallWrites(t *testing.T) {
	i, path := newTestInstaller(t)
	if err := i.Install(testRef); err != nil {
		t.Fatal(err)
	}
	raw := readFile(t, path)
	for _, m := range HookInvocationMarkers() {
		if !strings.Contains(raw, m) {
			t.Errorf("marker %q is not in what install wrote", m)
		}
	}
	if len(HookInvocationMarkers()) == 0 {
		t.Error("no markers")
	}
}

// The lifecycle hooks are ours to register and ours to remove; Interrupt and
// PostToolBatch are neither.
func TestUninstallRemovesTheLifecycleHandlersInstallWrote(t *testing.T) {
	i, path := newTestInstaller(t)
	if err := i.Install(testRef); err != nil {
		t.Fatal(err)
	}
	hooks := decodeHooks(t, readFile(t, path))
	for _, h := range []HookName{HookPreCompact, HookPostCompact, HookNotification} {
		if len(hooks[string(h)]) != 1 {
			t.Errorf("%s is not registered", h)
		}
	}
	for _, h := range []HookName{HookPostToolBatch, HookInterrupt} {
		if _, ok := hooks[string(h)]; ok {
			t.Errorf("%s is registered", h)
		}
	}
	removed, err := RemoveHooks(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{"PreCompact", "PostCompact", "Notification"} {
		var gone bool
		for _, r := range removed {
			gone = gone || strings.HasPrefix(r, h+" ")
		}
		if !gone {
			t.Errorf("uninstall did not report removing %s: %v", h, removed)
		}
	}
	if _, err := os.Stat(path); err == nil {
		if h := decodeHooks(t, readFile(t, path)); len(h) != 0 {
			t.Errorf("hooks left behind: %v", h)
		}
	}
}
