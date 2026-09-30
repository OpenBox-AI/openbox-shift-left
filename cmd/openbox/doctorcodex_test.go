package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/cli/activation"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/providers"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/sessionkey"
	"github.com/openbox-ai/openbox-shift-left/internal/trace"
)

// TestDoctorSaysNothingAboutCodexOnAMachineThatNeverConfiguredIt pins the
// byte-identical claim: a machine with no OpenBox-owned Codex [otel] block
// must not gain a Codex section in `doctor`.
func TestDoctorSaysNothingAboutCodexOnAMachineThatNeverConfiguredIt(t *testing.T) {
	isolateHome(t)
	a, out, errb := testApp(nil)
	if code := a.runDoctor(nil); code != exitOK && code != exitError {
		t.Fatalf("doctor exit = %d; stderr=%q", code, errb.String())
	}
	s := out.String()
	if strings.Contains(s, "Codex telemetry lane") {
		t.Errorf("doctor reports a Codex lane section with no owned [otel] block:\n%s", s)
	}
	if strings.Contains(s, "Codex proxy/transport lane") {
		t.Errorf("doctor reports a Codex proxy section with no owned [otel] block:\n%s", s)
	}
}

// TestDoctorReportsAnElectedCodexLaneWithNothingListening is the WARNING this
// section exists for: an owned [otel] block that elects the telemetry lane
// while nothing answers on the port is a machine whose Codex model calls are
// silently unrecorded.
func TestDoctorReportsAnElectedCodexLaneWithNothingListening(t *testing.T) {
	isolateHome(t)
	nothingIsListening(t)
	configPath := providers.CodexConfigTOMLPath()
	if err := providers.WriteCodexOtel(configPath, "http://127.0.0.1:4318/v1/logs"); err != nil {
		t.Fatalf("seed WriteCodexOtel: %v", err)
	}

	a, out, errb := testApp(nil)
	if code := a.runDoctor(nil); code != exitOK && code != exitError {
		t.Fatalf("doctor exit = %d; stderr=%q", code, errb.String())
	}
	s := out.String()
	if !strings.Contains(s, "Codex telemetry lane") {
		t.Fatalf("doctor does not report the Codex lane section with an owned [otel] block:\n%s", s)
	}
	if !strings.Contains(s, "elected") || !strings.Contains(s, "telemetry") {
		t.Errorf("doctor does not report the Codex lane as elected:\n%s", s)
	}
	if !strings.Contains(s, "NO; nothing is listening") {
		t.Errorf("doctor does not report the Codex lane as unreachable:\n%s", s)
	}
	if !strings.Contains(s, "WARNING") {
		t.Errorf("an elected-but-unreachable Codex lane did not warn:\n%s", s)
	}
	if !strings.Contains(s, "Codex proxy/transport lane") || !strings.Contains(s, "telemetry only: no system PAC is activated for Codex") {
		t.Errorf("doctor does not say why the Codex proxy arm is not elected:\n%s", s)
	}
}

// doctorCodexProxy runs doctor on a machine with Codex's telemetry installed
// and the given system-PAC record (raw JSON, "" for none) and relay evidence
// (the record's own commit time when observed), and returns its output.
func doctorCodexProxy(t *testing.T, record string, observed bool) string {
	t.Helper()
	home := isolateHome(t)
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, "codex-home"))
	nothingIsListening(t)
	withSystemPACSupport(t, true)
	if err := providers.WriteCodexOtel(providers.CodexConfigTOMLPath(), "http://127.0.0.1:4318/v1/logs"); err != nil {
		t.Fatalf("seed WriteCodexOtel: %v", err)
	}
	a, out, errb := testApp(map[string]string{"HOME": home})
	paths := a.codexPaths()
	if record != "" {
		if err := os.MkdirAll(filepath.Dir(paths.pacRecord), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(paths.pacRecord, []byte(record), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if observed {
		if _, err := activation.MarkCodexProxyObserved(paths.pacRecord, paths.marker); err != nil {
			t.Fatal(err)
		}
	}
	if code := a.runDoctor(nil); code != exitOK && code != exitError {
		t.Fatalf("doctor exit = %d; stderr=%q", code, errb.String())
	}
	return out.String()
}

const committedCodexRecord = `{"system":{"pac_activated":true,"activated_at":"2026-09-30T10:00:00Z","providers":["claude-code","codex"]}}`

func TestDoctorSaysTheRelayHasNotYetSeenCodex(t *testing.T) {
	s := doctorCodexProxy(t, committedCodexRecord, false)
	if !strings.Contains(s, "telemetry only: relay has not yet seen a Codex request") {
		t.Errorf("doctor does not name the missing evidence:\n%s", s)
	}
	if !strings.Contains(s, "claude-code, codex (the system PAC's union)") {
		t.Errorf("doctor does not list the PAC's provider union:\n%s", s)
	}
}

func TestDoctorSaysTheRelayIsElectedOnceItHasSeenCodex(t *testing.T) {
	s := doctorCodexProxy(t, committedCodexRecord, true)
	if !strings.Contains(s, "proxy elected; telemetry standing by") {
		t.Errorf("doctor does not report the relay as elected:\n%s", s)
	}
	if !strings.Contains(s, "ELECTED for Codex but nothing is listening") {
		t.Errorf("an elected relay with nothing listening did not warn:\n%s", s)
	}
}

func TestDoctorSaysAPendingPACIsNotRouted(t *testing.T) {
	s := doctorCodexProxy(t, `{"system":{"pending":true,"providers":["codex"]}}`, false)
	if !strings.Contains(s, "telemetry only: the system PAC activation is still pending") {
		t.Errorf("doctor does not name the pending activation:\n%s", s)
	}
}

func TestDoctorSaysThereIsNoSystemPACOffMacOS(t *testing.T) {
	home := isolateHome(t)
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, "codex-home"))
	nothingIsListening(t)
	withSystemPACSupport(t, false)
	if err := providers.WriteCodexOtel(providers.CodexConfigTOMLPath(), "http://127.0.0.1:4318/v1/logs"); err != nil {
		t.Fatal(err)
	}
	a, out, _ := testApp(map[string]string{"HOME": home})
	if code := a.runDoctor(nil); code != exitOK && code != exitError {
		t.Fatalf("doctor exit = %d", code)
	}
	if !strings.Contains(out.String(), "telemetry only: there is no system PAC on") {
		t.Errorf("doctor does not say why a non-macOS Codex is telemetry-only:\n%s", out.String())
	}
}

// TestDoctorCountsUnattributedRelayedCalls: the emitter skips a shared-host
// call it cannot attribute and records the reason in the local trace; that
// trace is the only place the count exists.
func TestDoctorCountsUnattributedRelayedCalls(t *testing.T) {
	dir := t.TempDir()
	restore := trace.SetDefault(&trace.Writer{Dir: dir})
	t.Cleanup(restore)
	emit := func(outcome, reason string) {
		trace.Emit(trace.Record{Stage: trace.StageCapture, Outcome: outcome, Detail: map[string]any{"reason": reason}})
	}
	emit("skipped", sessionkey.SkipNoProviderCarrier)
	emit("skipped", sessionkey.SkipNoProviderCarrier)
	emit("skipped", sessionkey.SkipAmbiguousCarrier)
	emit("skipped", "no_session_id")
	emit("recorded", "")

	none, ambiguous, err := countCarrierSkips(dir)
	if err != nil {
		t.Fatal(err)
	}
	if none != 2 || ambiguous != 1 {
		t.Errorf("counted %d no-carrier and %d ambiguous, want 2 and 1", none, ambiguous)
	}
}

func TestDoctorCountsCodexHostCallsLostWhileTheRelayIsElected(t *testing.T) {
	dir := t.TempDir()
	restore := trace.SetDefault(&trace.Writer{Dir: dir})
	t.Cleanup(restore)
	emit := func(outcome, reason, url string) {
		trace.Emit(trace.Record{Stage: trace.StageCapture, Outcome: outcome,
			Detail: map[string]any{"reason": reason, "url": url}})
	}
	emit("skipped", "no_session_id", "https://api.openai.com/v1/responses")
	emit("skipped", "no_session_id", "https://chatgpt.com/backend-api/conversation")
	emit("skipped", "not_elected", "https://api.openai.com/v1/responses")      // telemetry's turn: not a loss
	emit("skipped", "no_provider_carrier", "https://api.meta.ai/v1/responses") // shared host: not counted
	emit("skipped", "no_session_id", "https://api.anthropic.com/v1/messages")  // another provider
	emit("recorded", "", "https://api.openai.com/v1/responses")

	n, err := countCodexHostSkips(dir)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("counted %d lost Codex-host calls, want 2", n)
	}
}

func TestDoctorReportsLostCodexCallsWhenTheRelayIsElected(t *testing.T) {
	t.Cleanup(trace.SetDefault(&trace.Writer{Dir: t.TempDir()}))
	s := doctorCodexProxy(t, committedCodexRecord, true)
	if !strings.Contains(s, "Codex-host calls skipped while the relay is elected") {
		t.Errorf("doctor does not report the relay's loss counter:\n%s", s)
	}
}

func TestSystemPACDisclosureNamesApiMetaAiForEveryProviderAndChatGPTOnlyWithCodex(t *testing.T) {
	for _, tc := range []struct {
		name      string
		providers []string
		chatgpt   bool
	}{
		{"claude-code only", []string{"claude-code"}, false},
		{"with codex", []string{"claude-code", "codex"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, out, _ := testApp(nil)
			a.printSystemPACActive(activation.Outcome{Class: activation.Activated,
				Entry: &activation.SystemEntry{PACURL: "http://127.0.0.1:8790/proxy.pac", Providers: tc.providers}})
			s := out.String()
			if !strings.Contains(s, "api.meta.ai") || !strings.Contains(s, "kept raw in the local trace for 7 days") {
				t.Errorf("the api.meta.ai disclosure is missing:\n%s", s)
			}
			if got := strings.Contains(s, "chatgpt.com"); got != tc.chatgpt {
				t.Errorf("chatgpt.com disclosure present = %v, want %v:\n%s", got, tc.chatgpt, s)
			}
		})
	}
}
