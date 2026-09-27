package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/providers"
	"github.com/openbox-ai/openbox-shift-left/internal/client"
	"github.com/openbox-ai/openbox-shift-left/internal/provider"
)

// TestDoctorWarnsWhenTwoEnginesAreRegistered two engines registered in one
// project is a silent, self-inflicted double-count of every governed tool
// call: both fire, both store, and an operator reading the result sees broken
// tool-health numbers with nothing pointing at the cause.
func TestDoctorWarnsWhenTwoEnginesAreRegistered(t *testing.T) {
	out := inDirWithSettings(t, map[string]any{"hooks": map[string]any{
		"PreToolUse": []any{
			map[string]any{"matcher": "*", "hooks": []any{
				map[string]any{"type": "command", "command": `"/opt/a/bin/openbox" hook claude-code PreToolUse`},
			}},
			map[string]any{"matcher": "*", "hooks": []any{
				map[string]any{"type": "command", "command": `"/opt/b/bin/openbox" hook claude-code PreToolUse`},
			}},
		},
	}})

	if !strings.Contains(out, "WARNING") {
		t.Errorf("two registered engines produced no WARNING:\n%s", out)
	}
	for _, want := range []string{"/opt/a/bin/openbox", "/opt/b/bin/openbox", "openbox init", "TWICE"} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor output does not mention %q:\n%s", want, out)
		}
	}
}

// TestDoctorDoesNotWarnOnASingleEngine a healthy install must not warn;
// PreToolUse legitimately carries two of our handlers (the gate and the
// approval watcher), so counting handlers per event rather than per invocation
// would warn on every correct install and train the reader to ignore the one
// that matters.
func TestDoctorDoesNotWarnOnASingleEngine(t *testing.T) {
	out := inDirWithSettings(t, map[string]any{"hooks": map[string]any{
		"PreToolUse": []any{
			map[string]any{"matcher": "*", "hooks": []any{
				map[string]any{"type": "command", "command": `"/opt/a/bin/openbox" hook claude-code PreToolUse`},
				map[string]any{"type": "command", "command": `"/opt/a/bin/openbox" rewake claude-code`},
			}},
		},
		"Stop": []any{
			map[string]any{"hooks": []any{
				map[string]any{"type": "command", "command": `"/opt/a/bin/openbox" hook claude-code Stop`},
			}},
		},
	}})

	if strings.Contains(out, "WARNING: ") && strings.Contains(out, "engines are registered") {
		t.Errorf("a single-engine install warned:\n%s", out)
	}
	if !strings.Contains(out, "/opt/a/bin/openbox") {
		t.Errorf("doctor did not report the registered engine:\n%s", out)
	}
}

// TestDoctorWarnsWhenOneInvocationIsRegisteredTwice the same invocation
// registered twice at ONE path is the same defect, and it is what an unquoted-
// path edge case or a hand-edited file leaves behind.
func TestDoctorWarnsWhenOneInvocationIsRegisteredTwice(t *testing.T) {
	out := inDirWithSettings(t, map[string]any{"hooks": map[string]any{
		"Stop": []any{
			map[string]any{"hooks": []any{
				map[string]any{"type": "command", "command": `"/opt/a/bin/openbox" hook claude-code Stop`},
				map[string]any{"type": "command", "command": `"/opt/a/bin/openbox" hook claude-code Stop`},
			}},
		},
	}})
	if !strings.Contains(out, "more than once") || !strings.Contains(out, "Stop") {
		t.Errorf("a doubly-registered event produced no warning:\n%s", out)
	}
}

// TestDoctorWarnsOnAShortTimeout a PreToolUse entry installed at 5s predates
// the gate's evaluation budget moving up (currently 30s): Claude Code will
// kill the hook before governance answers, and a killed gated hook is a
// non-blocking error, so the tool call it was supposed to gate proceeds
// ungoverned. `openbox init` already rewrites the timeout; doctor must say so.
func TestDoctorWarnsOnAShortTimeout(t *testing.T) {
	out := inDirWithSettings(t, map[string]any{"hooks": map[string]any{
		"PreToolUse": []any{
			map[string]any{"matcher": "*", "hooks": []any{
				map[string]any{"type": "command", "command": `"/opt/a/bin/openbox" hook claude-code PreToolUse`, "timeout": 5},
			}},
		},
	}})

	if !strings.Contains(out, "WARNING") || !strings.Contains(out, "PreToolUse") {
		t.Errorf("a short-timeout entry produced no WARNING naming the event:\n%s", out)
	}
	if !strings.Contains(out, "openbox init") {
		t.Errorf("doctor did not tell the reader to run `openbox init`:\n%s", out)
	}
}

// TestDoctorDoesNotWarnOnATimeoutAtSpec an entry installed at the current spec
// must not warn: TestDoctorDoesNotWarnOnASingleEngine already proves a
// timeout-less fixture is silent, so this proves an explicit, current timeout
// is equally silent.
func TestDoctorDoesNotWarnOnATimeoutAtSpec(t *testing.T) {
	out := inDirWithSettings(t, map[string]any{"hooks": map[string]any{
		"PreToolUse": []any{
			map[string]any{"matcher": "*", "hooks": []any{
				map[string]any{"type": "command", "command": `"/opt/a/bin/openbox" hook claude-code PreToolUse`, "timeout": 30},
			}},
		},
	}})

	if strings.Contains(out, "shorter timeout") {
		t.Errorf("a current-spec timeout warned:\n%s", out)
	}
}

// TestDoctorReportsAnAbsentProjectHookFileAsAFact an absent file is the normal
// state; a global-scope install, or any directory that was never initialized.
// It must read as a fact about this directory, not as a fault, and never as
// "not governed".
func TestDoctorReportsAnAbsentProjectHookFileAsAFact(t *testing.T) {
	out, code := runDoctorIn(t, t.TempDir())
	if code != exitOK {
		t.Errorf("doctor exit = %d, want %d; an optional check must not change the exit code", code, exitOK)
	}
	if !strings.Contains(out, "(absent)") {
		t.Errorf("absent settings file not reported:\n%s", out)
	}
	if strings.Contains(out, "WARNING: 1 OpenBox") || strings.Contains(out, "engines are registered") {
		t.Errorf("absent settings file produced a warning:\n%s", out)
	}
}

// TestDoctorSurvivesInvalidProjectSettingsJSON doctor must survive a settings
// file it cannot parse: this command is what a developer runs when something
// is already wrong.
func TestDoctorSurvivesInvalidProjectSettingsJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".claude", "settings.local.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, code := runDoctorIn(t, dir)
	if code != exitOK {
		t.Errorf("doctor exit = %d, want %d", code, exitOK)
	}
	if !strings.Contains(out, "could not be read") {
		t.Errorf("invalid JSON not reported as a condition:\n%s", out)
	}
	// The closing claim, whatever its wording, is the last thing doctor
	// prints: finding it proves the run continued past the unparsable file
	// rather than stopping at it.
	if !strings.Contains(out, "Only `managed` values prove anything") {
		t.Errorf("doctor stopped early instead of continuing past the failed check:\n%s", out)
	}
}

func inDirWithSettings(t *testing.T, settings map[string]any) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, ".claude", "settings.local.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	raw, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	out, code := runDoctorIn(t, dir)
	if code != exitOK {
		t.Fatalf("doctor exit = %d, want %d", code, exitOK)
	}
	return out
}

// runDoctorHere runs doctor against whatever OPENBOX_HOME the caller already
// set, for a case whose whole subject is what is in that home.
func runDoctorHere(t *testing.T) (string, int) {
	t.Helper()
	nothingIsListening(t)
	var out, errb bytes.Buffer
	a := &app{stdout: &out, stderr: &errb, getenv: os.Getenv}
	code := a.runDoctor(nil)
	return out.String() + errb.String(), code
}

func runDoctorIn(t *testing.T, dir string) (string, int) {
	t.Helper()
	// doctor probes the lane ports to report coverage. Unpinned, that dials
	// 127.0.0.1 for real and answers from whatever the developer running the
	// suite has listening, so the result depends on the host rather than the
	// fixture.
	nothingIsListening(t)
	saved, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(saved) })

	t.Setenv("OPENBOX_HOME", t.TempDir())
	return runDoctorHere(t)
}

// codexOnlyMachine seeds an isolated home with a Codex spool backlog and a
// Codex hooks registration, and neither Claude Code surface, so both
// reportSpool and reportHookRegistration have something new to report. It
// returns doctor's full output.
//
// The "codex-spool" literal mirrors codex/creds.go:77's DefaultSpoolDir; it
// is not re-derived through the adapter here because cmd/openbox must not
// import internal/adapters/codex directly.
func codexOnlyMachine(t *testing.T, backlogLines int) string {
	t.Helper()
	isolateHomeUnbound(t)
	t.Setenv(devconfig.EnvSpoolDir, "")

	codexSpoolDir := devconfig.SpoolDir("codex-spool")
	if err := os.MkdirAll(codexSpoolDir, 0o700); err != nil {
		t.Fatal(err)
	}
	seedLine := `{"seed":1}` + "\n"
	if err := os.WriteFile(filepath.Join(codexSpoolDir, "seed.jsonl"), []byte(strings.Repeat(seedLine, backlogLines)), 0o600); err != nil {
		t.Fatal(err)
	}

	hooksPath := providers.CodexHooksPath()
	if err := os.MkdirAll(filepath.Dir(hooksPath), 0o700); err != nil {
		t.Fatal(err)
	}
	marker := providers.HookMarkers(string(provider.Codex))[0]
	content := `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"/usr/local/bin/openbox ` + marker + `"}]}]}}`
	if err := os.WriteFile(hooksPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	out, code := runDoctorHere(t)
	if code != exitOK {
		t.Fatalf("doctor exit = %d, want %d:\n%s", code, exitOK, out)
	}
	return out
}

// TestDoctorNamesCodexOnACodexOnlyMachine: a machine governed only by Codex must see its own real backlog and its
// own real hook registration, not Claude Code's absence of either.
func TestDoctorNamesCodexOnACodexOnlyMachine(t *testing.T) {
	out := codexOnlyMachine(t, 3)

	t.Run("spool", func(t *testing.T) {
		codexSpoolDir := devconfig.SpoolDir("codex-spool")
		if !strings.Contains(out, codexSpoolDir) {
			t.Errorf("doctor did not name the codex spool directory %q:\n%s", codexSpoolDir, out)
		}
		if !strings.Contains(out, "3 event(s)") {
			t.Errorf("doctor did not report the seeded codex backlog of 3 (still a false 'waiting 0'?):\n%s", out)
		}
		if !strings.Contains(out, "hook codex flush") {
			t.Errorf("the codex remediation does not name codex:\n%s", out)
		}
	})

	t.Run("registration", func(t *testing.T) {
		hooksPath := providers.CodexHooksPath()
		if !strings.Contains(out, "Codex is governed") {
			t.Errorf("doctor did not say Codex is governed:\n%s", out)
		}
		if !strings.Contains(out, hooksPath) {
			t.Errorf("doctor did not name the codex hooks path %q:\n%s", hooksPath, out)
		}
	})

	// The Claude-Code-only remediation ("Run `openbox init --provider claude-code`") must
	// not fire once Codex is named as governing instead. This is narrower than
	// a whole-output substring scan: reportIdentities separately and
	// legitimately tells Claude Code to run its OWN init when IT has no agent,
	// regardless of Codex, and that is not what this checks.
	if strings.Contains(out, "Nothing is governed on this machine. Run `openbox init --provider claude-code`.") {
		t.Errorf("a Codex-only machine must not fall back to the Claude-Code-only remediation:\n%s", out)
	}
}

// TestDoctorOutputUnchangedOnAClaudeCodeOnlyMachine: a machine with only
// Claude Code's spool and no Codex surface at all must render exactly the
// sentences it always has -- the Codex-only branches must stay silent.
func TestDoctorOutputUnchangedOnAClaudeCodeOnlyMachine(t *testing.T) {
	isolateHomeUnbound(t)
	t.Setenv(devconfig.EnvSpoolDir, "")

	ccSpoolDir := devconfig.SpoolDir(transportSpoolSubdir)
	if err := os.MkdirAll(ccSpoolDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ccSpoolDir, "seed.jsonl"), []byte(`{"seed":1}`+"\n"+`{"seed":2}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	out, code := runDoctorHere(t)
	if code != exitOK {
		t.Fatalf("doctor exit = %d, want %d:\n%s", code, exitOK, out)
	}

	if !strings.Contains(out, "2 event(s), of which 0 are in carry-over files from a failed") {
		t.Errorf("the untouched cc-spool block changed its wording:\n%s", out)
	}
	if !strings.Contains(out, "Nothing is governed on this machine. Run `openbox init --provider claude-code`.") {
		t.Errorf("the untouched hook-registration sentence changed:\n%s", out)
	}
	for _, absent := range []string{"Codex is governed", "hook codex flush"} {
		if strings.Contains(out, absent) {
			t.Errorf("a Claude-Code-only machine must not render the new Codex branch (%q found):\n%s", absent, out)
		}
	}
}

// TestDoctorSpoolDoesNotDoubleCountAnOverriddenSharedDirectory is hard
// constraint 2: OPENBOX_SPOOL_DIR overrides the whole path for every
// provider, so Claude Code and Codex resolve to the identical directory. A
// comparison that misses this renders the same backlog twice, once per
// provider name, over-reporting the machine's real backlog by 2x.
func TestDoctorSpoolDoesNotDoubleCountAnOverriddenSharedDirectory(t *testing.T) {
	isolateHomeUnbound(t)
	shared := t.TempDir()
	t.Setenv(devconfig.EnvSpoolDir, shared)

	if err := os.WriteFile(filepath.Join(shared, "seed.jsonl"), []byte(`{"seed":1}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	out, code := runDoctorHere(t)
	if code != exitOK {
		t.Fatalf("doctor exit = %d, want %d:\n%s", code, exitOK, out)
	}

	if got := strings.Count(out, "1 event(s)"); got != 1 {
		t.Errorf("a shared spool directory was counted %d times, want 1:\n%s", got, out)
	}
	if strings.Contains(out, "hook codex flush") {
		t.Errorf("a directory identical to cc-spool must not also render as Codex's own row:\n%s", out)
	}
}

// TestCodexHooksPresent is the direct unit test of the DRY helper
// reportHookRegistration's new branch depends on, isolated from doctor's
// surrounding report so a failure here points at the ownership parse rather
// than at string-matching noise in the full doctor output.
func TestCodexHooksPresent(t *testing.T) {
	isolateHomeUnbound(t)

	if path, present := codexHooksPresent(); present {
		t.Errorf("codexHooksPresent() = (%q, true) on a fresh home with no codex hooks file", path)
	}

	hooksPath := providers.CodexHooksPath()
	if err := os.MkdirAll(filepath.Dir(hooksPath), 0o700); err != nil {
		t.Fatal(err)
	}
	marker := providers.HookMarkers(string(provider.Codex))[0]
	content := `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"/usr/local/bin/openbox ` + marker + `"}]}]}}`
	if err := os.WriteFile(hooksPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	path, present := codexHooksPresent()
	if !present {
		t.Fatalf("codexHooksPresent() = (_, false) after seeding a marker at %s", hooksPath)
	}
	if path != hooksPath {
		t.Errorf("codexHooksPresent() path = %q, want %q", path, hooksPath)
	}
}

// TestDoctorReportsOfflineAlwaysDeniesAndFailClosed: delivery is
// always fail-closed now, so doctor's "if offline" row always says gated
// calls are denied, never the retired "PROCEED" wording for a fail-open
// posture that no longer exists.
func TestDoctorReportsOfflineAlwaysDeniesAndFailClosed(t *testing.T) {
	isolateHomeUnbound(t)
	out, code := runDoctorHere(t)
	if code != exitOK {
		t.Fatalf("doctor exit = %d, want %d:\n%s", code, exitOK, out)
	}
	if !strings.Contains(out, "fail_closed") {
		t.Errorf("doctor does not report fail_closed as the failure policy:\n%s", out)
	}
	if !strings.Contains(out, "gated calls are DENIED and the run halts") {
		t.Errorf("doctor does not name the always-fail-closed consequence:\n%s", out)
	}
	if strings.Contains(out, "PROCEED") {
		t.Errorf("doctor still describes a fail-open posture that no longer exists:\n%s", out)
	}
}

// TestDoctorReportsHaltedRuns covers the latch row: doctor shows the latch
// count and, per latched run, the preserved cause -- never the reason text
// (INV-2 content-free), which a delivery failure's own latch also carries.
func TestDoctorReportsHaltedRuns(t *testing.T) {
	isolateHomeUnbound(t)

	out, code := runDoctorHere(t)
	if code != exitOK {
		t.Fatalf("doctor exit = %d, want %d:\n%s", code, exitOK, out)
	}
	if strings.Contains(out, "Halted runs") {
		t.Errorf("an unlatched machine must not render the halted-runs section:\n%s", out)
	}

	hookflow.HaltOnDeliveryFailure(discardMainLogger(), client.DevEvent{
		EventID: "e1", EventType: client.EventToolCall, SessionID: "sess-doctor-halt",
	}, client.ErrDelivery)

	out, code = runDoctorHere(t)
	if code != exitOK {
		t.Fatalf("doctor exit = %d, want %d:\n%s", code, exitOK, out)
	}
	if !strings.Contains(out, "Halted runs") {
		t.Fatalf("a latched run must render the halted-runs section:\n%s", out)
	}
	if !strings.Contains(out, "1 run(s) total") {
		t.Errorf("doctor does not report the latch count:\n%s", out)
	}
	if !strings.Contains(out, client.FailureClass(client.ErrDelivery)) || !strings.Contains(out, "ToolCall") {
		t.Errorf("doctor does not name the preserved cause and event type:\n%s", out)
	}
	if !strings.Contains(out, "never expire") {
		t.Errorf("doctor does not say latches never expire on their own:\n%s", out)
	}
}

// TestDoctorRendersAnEmptyLatchAsUnreadableNotBlank: WriteSessionHaltIfAbsent's
// create-then-write leaves a brief window where a concurrent reader can see
// a present but empty (0-byte) file; doctor must still count and report it,
// with a generic "(unreadable)" row rather than a blank line or a panic --
// the same treatment a corrupt (non-JSON) latch already gets.
func TestDoctorRendersAnEmptyLatchAsUnreadableNotBlank(t *testing.T) {
	isolateHomeUnbound(t)

	haltDir := hookflow.DefaultHaltDir()
	if err := os.MkdirAll(haltDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(haltDir, "sess-empty-abcd1234.json"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	out, code := runDoctorHere(t)
	if code != exitOK {
		t.Fatalf("doctor exit = %d, want %d:\n%s", code, exitOK, out)
	}
	if !strings.Contains(out, "1 run(s) total") {
		t.Errorf("doctor does not count the empty latch:\n%s", out)
	}
	if !strings.Contains(out, "(unreadable)") {
		t.Errorf("doctor does not render the empty latch generically:\n%s", out)
	}
}

// TestDoctorCapsHaltedRunsListAt10 latches have no expiry and no remove path
// except `openbox uninstall`, so a long-lived machine can accumulate many;
// the report must stay bounded (total count + the 10 most recent) rather
// than growing the doctor output without limit.
func TestDoctorCapsHaltedRunsListAt10(t *testing.T) {
	isolateHomeUnbound(t)

	const total = 13
	for i := 0; i < total; i++ {
		hookflow.HaltOnDeliveryFailure(discardMainLogger(), client.DevEvent{
			EventID: "e", EventType: client.EventToolCall, SessionID: fmt.Sprintf("sess-cap-%02d", i),
		}, client.ErrDelivery)
	}

	out, code := runDoctorHere(t)
	if code != exitOK {
		t.Fatalf("doctor exit = %d, want %d:\n%s", code, exitOK, out)
	}
	if !strings.Contains(out, fmt.Sprintf("%d run(s) total", total)) {
		t.Errorf("doctor does not report the true total count (%d):\n%s", total, out)
	}
	row := fmt.Sprintf("%s (%s) at", client.FailureClass(client.ErrDelivery), client.EventToolCall)
	if got := strings.Count(out, row); got != 10 {
		t.Errorf("doctor rendered %d latch row(s), want exactly the 10 most recent:\n%s", got, out)
	}
	if !strings.Contains(out, "most recent") {
		t.Errorf("doctor does not say the list is capped to the most recent:\n%s", out)
	}
}
