package muse

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
)

const testEngine = "/opt/openbox/bin/openbox"

func versionRunner(out string) Runner {
	return fakeRunner(RunResult{Stdout: []byte(out)}, nil)
}

// newTestInstaller pins every path the installer writes under a temp dir and
// gives it a muse that reports a supported version.
func newTestInstaller(t *testing.T) (Installer, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "muse", "settings.json")
	return Installer{
		EngineBinary: testEngine,
		SettingsPath: path,
		ConfigPath:   filepath.Join(dir, "dev.json"),
		Runner:       versionRunner("muse 1.4.0"),
	}, path
}

var testRef = CredentialRef{AgentID: "3f2504e0-4f89-11d3-9a0c-0305e82c3301"}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

type writtenSuccessor struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	Timeout int    `json:"timeout"`
}

type writtenHandler struct {
	Type      string            `json:"type"`
	Command   string            `json:"command"`
	Timeout   int               `json:"timeout"`
	OnFailure *writtenSuccessor `json:"onFailure"`
}

type writtenGroup struct {
	Matcher *string          `json:"matcher"`
	Hooks   []writtenHandler `json:"hooks"`
}

// decodeHooks reads the hooks block of a written settings file.
func decodeHooks(t *testing.T, raw string) map[string][]writtenGroup {
	t.Helper()
	var doc struct {
		Hooks map[string][]writtenGroup `json:"hooks"`
	}
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("written settings do not decode: %v\n%s", err, raw)
	}
	return doc.Hooks
}

func TestInstalledEventsComeFromTheAdaptersOwnTable(t *testing.T) {
	var got []string
	for _, ev := range installedEvents() {
		got = append(got, string(ev))
	}
	want := []string{"PermissionRequest", "PostLLMCall", "PostToolUse", "PostToolUseFailure", "PreLLMCall",
		"PreToolUse", "SessionEnd", "SessionStart", "Stop", "StopFailure", "SubagentStart", "SubagentStop", "UserPromptSubmit"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("installed events = %v, want %v", got, want)
	}
	for _, never := range []HookName{HookPreCompact, HookPostCompact, HookNotification, HookPostToolBatch, HookInterrupt} {
		for _, ev := range installedEvents() {
			if ev == never {
				t.Errorf("%s has no contract type and must never be registered", never)
			}
		}
	}
	if ExpectedHandlers() != len(want) {
		t.Errorf("ExpectedHandlers = %d", ExpectedHandlers())
	}
}

// Muse refuses a synchronous Interrupt handler (it must be async: true), and
// this adapter reports nothing for one, so the installer registers none.
func TestInstallNeverRegistersInterrupt(t *testing.T) {
	i, path := newTestInstaller(t)
	if err := i.Install(testRef); err != nil {
		t.Fatal(err)
	}
	raw := readFile(t, path)
	if _, ok := decodeHooks(t, raw)[string(HookInterrupt)]; ok {
		t.Errorf("Interrupt is registered:\n%s", raw)
	}
	if strings.Contains(raw, "hook muse Interrupt") {
		t.Errorf("a handler runs the Interrupt hook:\n%s", raw)
	}
	managed, err := os.ReadFile(managedBundle + "hooks.json")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(managed), "Interrupt") {
		t.Error("the managed hooks file registers Interrupt")
	}
}

func TestInstallWritesACatchAllHandlerPerEvent(t *testing.T) {
	i, path := newTestInstaller(t)
	if err := i.Install(testRef); err != nil {
		t.Fatal(err)
	}
	raw := readFile(t, path)
	if !strings.Contains(raw, `"schema_version": 1`) {
		t.Errorf("a new file must carry schema_version 1:\n%s", raw)
	}
	if !strings.HasSuffix(raw, "\n") {
		t.Error("a new file ends without a newline")
	}
	if _, err := ValidateSettings([]byte(raw)); err != nil {
		t.Fatalf("what install wrote does not validate: %v", err)
	}
	hooks := decodeHooks(t, raw)
	if len(hooks) != ExpectedHandlers() {
		t.Fatalf("%d events registered, want %d", len(hooks), ExpectedHandlers())
	}
	for _, ev := range installedEvents() {
		groups := hooks[string(ev)]
		if len(groups) != 1 || len(groups[0].Hooks) != 1 {
			t.Fatalf("%s: groups = %+v", ev, groups)
		}
		g, h := groups[0], groups[0].Hooks[0]
		if g.Matcher != nil {
			t.Errorf("%s: a matcher would leave tools ungoverned: %q", ev, *g.Matcher)
		}
		if h.Type != "command" {
			t.Errorf("%s: type = %q", ev, h.Type)
		}
		if want := `"` + testEngine + `" hook muse ` + string(ev); h.Command != want {
			t.Errorf("%s: command = %q, want %q", ev, h.Command, want)
		}
		wantTimeout := map[bool]int{true: 30, false: 5}[ev.Gated()]
		if h.Timeout != wantTimeout {
			t.Errorf("%s: timeout = %d, want %d", ev, h.Timeout, wantTimeout)
		}
		switch {
		case ev.Gated():
			if h.OnFailure == nil {
				t.Fatalf("%s is gated and has no onFailure successor", ev)
			}
			want := `"` + testEngine + `" hook muse --fail-closed ` + string(ev)
			if h.OnFailure.Command != want || h.OnFailure.Type != "command" || h.OnFailure.Timeout == 0 {
				t.Errorf("%s: successor = %+v, want command %q", ev, *h.OnFailure, want)
			}
		case h.OnFailure != nil:
			t.Errorf("%s gates nothing and must not carry a successor", ev)
		}
	}
}

func TestEveryGatedHandlerHasASuccessorThatParsesAsFailClosed(t *testing.T) {
	i, path := newTestInstaller(t)
	if err := i.Install(testRef); err != nil {
		t.Fatal(err)
	}
	doc, err := parseSettings([]byte(readFile(t, path)))
	if err != nil {
		t.Fatal(err)
	}
	gated := 0
	for _, h := range doc.Handlers {
		if !h.Owned {
			t.Fatalf("a handler install wrote does not parse as owned: %q", h.Command)
		}
		if !h.Invocation.Event.Gated() {
			continue
		}
		gated++
		inv, ok := parseInvocation(h.OnFailure.Command)
		if !ok || !inv.FailClosed || inv.Event != h.Invocation.Event {
			t.Errorf("%s: successor %q is not its own fail-closed form", h.Invocation.Event, h.OnFailure.Command)
		}
	}
	if gated != 4 {
		t.Errorf("%d gated handlers, want 4", gated)
	}
}

func TestSecondInstallIsByteIdentical(t *testing.T) {
	for name, seed := range map[string]string{
		"new file":         "",
		"foreign settings": "{\n\t\"schema_version\": 1,\n\t\"theme\": \"dark\",\n\t\"hooks\": {\n\t\t\"PreToolUse\": [\n\t\t\t{\"matcher\": \"Bash\", \"hooks\": [{\"type\": \"command\", \"command\": \"/usr/local/bin/audit\"}]}\n\t\t]\n\t}\n}\n",
	} {
		t.Run(name, func(t *testing.T) {
			i, path := newTestInstaller(t)
			if seed != "" {
				writeFile(t, path, seed)
			}
			if err := i.Install(testRef); err != nil {
				t.Fatal(err)
			}
			first := readFile(t, path)
			if err := i.Install(testRef); err != nil {
				t.Fatal(err)
			}
			if second := readFile(t, path); second != first {
				t.Errorf("a re-install changed the file:\n--- first\n%s\n--- second\n%s", first, second)
			}
			if n := strings.Count(first, "hook muse "); n != ExpectedHandlers()+4 {
				t.Errorf("%d OpenBox commands, want %d handlers plus 4 successors", n, ExpectedHandlers())
			}
		})
	}
}

func TestInstallReplacesAStaleOwnedHandlerInPlace(t *testing.T) {
	i, path := newTestInstaller(t)
	stale := `{"schema_version":1,"hooks":{"PreToolUse":[{"hooks":[{"type":"command","command":"\"/old/openbox\" hook muse PreToolUse","timeout":5}]}]}}`
	writeFile(t, path, stale)
	if err := i.Install(testRef); err != nil {
		t.Fatal(err)
	}
	raw := readFile(t, path)
	if strings.Contains(raw, "/old/openbox") {
		t.Errorf("the stale handler survived:\n%s", raw)
	}
	if n := len(decodeHooks(t, raw)["PreToolUse"]); n != 1 {
		t.Errorf("PreToolUse has %d groups, want exactly the new one", n)
	}
}

func TestInstallKeepsForeignHandlersAndThoseThatOnlyMentionOurs(t *testing.T) {
	i, path := newTestInstaller(t)
	lookalike := `my-audit && \"/elsewhere/openbox\" hook muse PreToolUse`
	seed := `{"schema_version":1,"theme":"dark","hooks":{"PreToolUse":[` +
		`{"matcher":"Bash","hooks":[{"type":"command","command":"/usr/local/bin/audit","timeout":9}]},` +
		`{"hooks":[{"type":"command","command":"` + lookalike + `"}]}]}}`
	writeFile(t, path, seed)
	if err := i.Install(testRef); err != nil {
		t.Fatal(err)
	}
	raw := readFile(t, path)
	for _, must := range []string{`"theme":"dark"`, `"/usr/local/bin/audit"`, lookalike, `"timeout":9`} {
		if !strings.Contains(raw, must) {
			t.Errorf("foreign content %s was lost:\n%s", must, raw)
		}
	}
	if n := len(decodeHooks(t, raw)["PreToolUse"]); n != 3 {
		t.Errorf("PreToolUse has %d groups, want 2 foreign + ours", n)
	}
}

func TestInstallRefusesUnreadableSettingsAndTouchesNothing(t *testing.T) {
	for name, seed := range map[string]string{
		"not json":              `{"hooks": `,
		"empty file":            ``,
		"array top level":       `[]`,
		"future schema":         `{"schema_version":2}`,
		"schema not a number":   `{"schema_version":"1"}`,
		"hooks not an object":   `{"hooks":[]}`,
		"event not an array":    `{"hooks":{"PreToolUse":{}}}`,
		"group not an object":   `{"hooks":{"PreToolUse":["x"]}}`,
		"group without hooks":   `{"hooks":{"PreToolUse":[{"matcher":"Bash"}]}}`,
		"handler not an object": `{"hooks":{"PreToolUse":[{"hooks":["x"]}]}}`,
		"command missing":       `{"hooks":{"PreToolUse":[{"hooks":[{"type":"command"}]}]}}`,
		"timeout not a number":  `{"hooks":{"PreToolUse":[{"hooks":[{"type":"command","command":"x","timeout":"5"}]}]}}`,
	} {
		t.Run(name, func(t *testing.T) {
			i, path := newTestInstaller(t)
			writeFile(t, path, seed)
			err := i.Install(testRef)
			if err == nil {
				t.Fatal("install accepted a file Muse cannot read")
			}
			if !strings.Contains(err.Error(), "Nothing was written") {
				t.Errorf("the refusal does not say nothing changed: %v", err)
			}
			if got := readFile(t, path); got != seed {
				t.Errorf("the file was touched:\n%q", got)
			}
			if _, statErr := os.Stat(i.ConfigPath); statErr == nil {
				t.Error("posture was written although the hooks were refused")
			}
		})
	}
}

func TestInstallNeedsAnAbsoluteEngineAndAnAgent(t *testing.T) {
	for _, engine := range []string{"", "openbox", "./openbox", `/has"quote/openbox`} {
		i, path := newTestInstaller(t)
		i.EngineBinary = engine
		if err := i.Install(testRef); err == nil {
			t.Errorf("engine %q accepted", engine)
		}
		if _, err := os.Stat(path); err == nil {
			t.Errorf("engine %q: settings were written", engine)
		}
	}
	i, _ := newTestInstaller(t)
	if err := i.Install(CredentialRef{}); err == nil {
		t.Error("install without an agent id accepted")
	}
}

func TestHomeIsBakedIntoEveryHandlerOnlyForANonDefaultHome(t *testing.T) {
	userHome, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	custom := filepath.Join(t.TempDir(), "openbox home")
	cases := []struct {
		name string
		env  string
		want string
	}{
		{"unset", "", ""},
		{"non default", custom, custom},
		{"set to the default", filepath.Join(userHome, ".openbox"), ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv(devconfig.EnvHome, c.env)
			i, path := newTestInstaller(t)
			if err := i.Install(testRef); err != nil {
				t.Fatal(err)
			}
			doc, err := parseSettings([]byte(readFile(t, path)))
			if err != nil {
				t.Fatal(err)
			}
			if len(doc.Handlers) != ExpectedHandlers() {
				t.Fatalf("%d handlers", len(doc.Handlers))
			}
			for _, h := range doc.Handlers {
				if h.Invocation.Home != c.want {
					t.Errorf("%s: home = %q, want %q (%q)", h.Event, h.Invocation.Home, c.want, h.Command)
				}
				if h.OnFailure != nil {
					if inv, _ := parseInvocation(h.OnFailure.Command); inv.Home != c.want {
						t.Errorf("%s: successor home = %q, want %q", h.Event, inv.Home, c.want)
					}
				}
			}
			if c.want != "" {
				want := `hook muse --home "` + c.want + `" PreToolUse`
				if !strings.Contains(readFile(t, path), strings.ReplaceAll(want, `"`, `\"`)) {
					t.Errorf("the command does not carry the quoted home %s", want)
				}
			}
		})
	}
}

func TestInstallRefusesAHomeAHookCommandCannotCarry(t *testing.T) {
	t.Setenv(devconfig.EnvHome, filepath.Join(t.TempDir(), `quo"te`))
	i, path := newTestInstaller(t)
	if err := i.Install(testRef); err == nil {
		t.Fatal("a home with a quote in it was baked into a command")
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("settings were written")
	}
}

func TestVersionGateOnInstall(t *testing.T) {
	cases := []struct {
		name      string
		runner    Runner
		wantErr   bool
		wantWarn  bool
		wantWrite bool
	}{
		{"supported", versionRunner("muse 1.4.3"), false, false, true},
		{"too old refuses", versionRunner("muse 1.3.9"), true, false, false},
		{"not on path installs", fakeRunner(RunResult{}, ErrNotOnPath), false, true, true},
		{"unreadable refuses", versionRunner("muse dev"), true, false, false},
		{"timed out refuses", fakeRunner(RunResult{}, context.DeadlineExceeded), true, false, false},
		{"non zero exit refuses", fakeRunner(RunResult{ExitCode: 2}, nil), true, false, false},
		{"prerelease of the floor refuses", versionRunner("muse 1.4.0-rc.1"), true, false, false},
		{"above tested warns only", versionRunner("muse 1.9.0"), false, true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			i, path := newTestInstaller(t)
			i.Runner = c.runner
			warn, err := i.Preflight()
			if (err != nil) != c.wantErr || (warn != "") != c.wantWarn {
				t.Fatalf("Preflight = %q, %v; wantErr=%v wantWarn=%v", warn, err, c.wantErr, c.wantWarn)
			}
			installErr := i.Install(testRef)
			if (installErr != nil) != c.wantErr {
				t.Fatalf("Install err = %v, wantErr=%v", installErr, c.wantErr)
			}
			if _, statErr := os.Stat(path); (statErr == nil) != c.wantWrite {
				t.Errorf("settings written = %v, want %v", statErr == nil, c.wantWrite)
			}
			if c.wantErr && !strings.Contains(err.Error(), "1.4.0") {
				t.Errorf("the refusal does not name the minimum: %v", err)
			}
		})
	}
}

// A posture write that fails must not leave live hooks with no dev.json behind
// them, so it runs first and the settings file is not reached.
func TestFailedPostureWriteLeavesNoHooksBehind(t *testing.T) {
	i, path := newTestInstaller(t)
	blocker := filepath.Join(t.TempDir(), "file")
	writeFile(t, blocker, "x")
	i.ConfigPath = filepath.Join(blocker, "dev.json") // a parent that is a file
	if err := i.Install(testRef); err == nil {
		t.Fatal("install succeeded although the posture could not be written")
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("settings.json was written although the posture was not")
	}
}

func TestRestoreKeepsTheOriginalMode(t *testing.T) {
	i, path := newTestInstaller(t)
	writeFile(t, path, "original")
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("clobbered"), 0o644); err != nil {
		t.Fatal(err)
	}
	i.restore(path, []byte("original"), true, 0o644)
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o644 || readFile(t, path) != "original" {
		t.Errorf("mode %v content %q", info.Mode().Perm(), readFile(t, path))
	}
}

func TestInstallPostureLeavesUnsaidBoolsUnset(t *testing.T) {
	i, _ := newTestInstaller(t)
	for _, run := range []string{"first", "second"} {
		if err := i.Install(testRef); err != nil {
			t.Fatalf("%s: %v", run, err)
		}
		raw := readFile(t, i.ConfigPath)
		for _, stray := range []string{`"enforce"`, `"content_capture"`, `"tier2"`, `"findings"`} {
			if strings.Contains(raw, stray) {
				t.Errorf("%s run wrote %s although it said nothing about it:\n%s", run, stray, raw)
			}
		}
		if !strings.Contains(raw, testRef.AgentID) {
			t.Errorf("%s run did not record the agent id:\n%s", run, raw)
		}
	}
	on := true
	ref := testRef
	ref.ContentCapture = &on
	if err := i.Install(ref); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(readFile(t, i.ConfigPath), `"content_capture"`) {
		t.Error("an explicit posture was not written")
	}
}

func TestInstallWritesThroughASymlinkAndKeepsTheMode(t *testing.T) {
	i, path := newTestInstaller(t)
	real := filepath.Join(t.TempDir(), "dotfiles", "muse-settings.json")
	writeFile(t, real, `{"schema_version":1}`)
	if err := os.Chmod(real, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, path); err != nil {
		t.Skipf("no symlinks here: %v", err)
	}
	if err := i.Install(testRef); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(path); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the symlink was replaced by a regular file: %v %v", info, err)
	}
	if !strings.Contains(readFile(t, real), "hook muse ") {
		t.Error("the link target was not updated")
	}
	if info, _ := os.Stat(real); info.Mode().Perm() != 0o644 {
		t.Errorf("mode = %v, want the file's own 0644", info.Mode().Perm())
	}
}

func TestNewSettingsFileIsPrivate(t *testing.T) {
	i, path := newTestInstaller(t)
	if err := i.Install(testRef); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", info.Mode().Perm())
	}
}

func TestInstallerImplementsTheSeam(t *testing.T) {
	if (Installer{}).Name() != "muse" {
		t.Error("Name")
	}
	// Runner nil falls back to the real binary; with PATH empty that is "not on
	// PATH", which installs with a warning and never reaches a real muse.
	t.Setenv("PATH", t.TempDir())
	warn, err := (Installer{}).Preflight()
	if err != nil || warn == "" {
		t.Errorf("Preflight without muse = %q, %v", warn, err)
	}
}

// TestInlineDeliveryFitsInsideEveryHandlerThatRunsIt a handler Muse kills
// before its inline drain finishes leaves a half-sent spool file, which the
// next drain can only discard as unprovable: the session's last events are lost.
func TestInlineDeliveryFitsInsideEveryHandlerThatRunsIt(t *testing.T) {
	for _, ev := range []HookName{HookSessionStart, HookSessionEnd, HookSubagentStart, HookSubagentStop} {
		ceiling := time.Duration(timeoutFor(ev)) * time.Second
		if inlineWindow+500*time.Millisecond > ceiling {
			t.Errorf("%s: inline delivery may take %s but Muse kills the handler at %s", ev, inlineWindow, ceiling)
		}
	}
}
