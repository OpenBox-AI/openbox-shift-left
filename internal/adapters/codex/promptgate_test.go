package codex

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
	"github.com/openbox-ai/openbox-shift-left/internal/client"
	"github.com/openbox-ai/openbox-shift-left/internal/client/fakecore"
)

// serveVerdictServer is serveVerdict's sibling that hands the server back, so a
// case can count round trips. The halt-replay case is only meaningful if it can
// assert the evaluate server was never called.
func serveVerdictServer(t *testing.T, verdictJSON string) *fakecore.Server {
	t.Helper()
	seedB64 := base64.StdEncoding.EncodeToString(make([]byte, 32))
	f := fakecore.New(t, fakecore.Script{Default: verdictJSON, SeedB64: seedB64})
	t.Setenv("OPENBOX_BASE_URL", f.URL())
	t.Setenv("OPENBOX_API_KEY", "obx_test_key")
	t.Setenv("OPENBOX_ED25519_SEED", seedB64)
	return f
}

// gateEnv wires a hook environment with enforce ON and an isolated halt dir,
// and returns that halt dir so a case can inspect or corrupt the latch.
func gateEnv(t *testing.T) string {
	t.Helper()
	setHookEnv(t)
	haltDir := filepath.Join(t.TempDir(), "halted-sessions")
	t.Setenv(devconfig.EnvHaltDir, haltDir)
	t.Setenv(devconfig.EnvEnforce, "1")
	t.Setenv(devconfig.EnvFailClosed, "0")
	return haltDir
}

func parsePromptOutput(t *testing.T, out string) userPromptSubmitOutput {
	t.Helper()
	var o userPromptSubmitOutput
	if err := json.Unmarshal([]byte(out), &o); err != nil {
		t.Fatalf("stdout is not valid UserPromptSubmit JSON: %v (%q)", err, out)
	}
	return o
}

func promptEvent(session string) string {
	return `{"hook_event_name":"UserPromptSubmit","session_id":"` + session + `","cwd":"/r",` +
		`"model":"gpt-5.6-sol","permission_mode":"default","turn_id":"t1",` +
		`"prompt":"ship it","transcript_path":null}`
}

func toolEvent(session string) string {
	return `{"hook_event_name":"PreToolUse","session_id":"` + session + `","cwd":"/r",` +
		`"model":"gpt-5.6-sol","permission_mode":"default","turn_id":"t1",` +
		`"tool_name":"Bash","tool_use_id":"call-1","tool_input":{"command":"ls"},` +
		`"transcript_path":null}`
}

// (a) A refusal renders decision:"block" with a reason, and never a universal
// field that Codex would reject on this surface.
func TestPromptGate_BlockRendersDecisionBlock(t *testing.T) {
	gateEnv(t)
	serveVerdictServer(t, `{"verdict":"block","reason":"secrets in prompt","policy_id":"p1"}`)

	stdout, _ := runHook(t, "UserPromptSubmit", promptEvent("th-block"))
	if strings.TrimSpace(stdout) == "" {
		t.Fatal("a blocking verdict must write the refusal contract to stdout")
	}
	o := parsePromptOutput(t, stdout)
	if o.Decision != codexPromptDecisionBlock {
		t.Errorf("decision = %q, want %q", o.Decision, codexPromptDecisionBlock)
	}
	if o.Reason == "" {
		t.Error("a block must carry a non-empty reason")
	}
	if o.Continue != nil {
		t.Errorf("a plain block must not stop the session; continue was set to %v", *o.Continue)
	}
	if o.StopReason != "" {
		t.Errorf("a plain block must not carry stopReason; got %q", o.StopReason)
	}
}

// (b) A HALT additionally stops the thread. This is the lever phase 00 measured:
// `Blocked` vs `Stopped` are two different states in Codex's own hook log.
func TestPromptGate_HaltAlsoStopsTheThread(t *testing.T) {
	gateEnv(t)
	serveVerdictServer(t, `{"verdict":"halt","reason":"policy violation","policy_id":"p2"}`)

	stdout, _ := runHook(t, "UserPromptSubmit", promptEvent("th-halt"))
	o := parsePromptOutput(t, stdout)
	if o.Decision != codexPromptDecisionBlock {
		t.Errorf("a HALT still renders decision:block for the current prompt; got %q", o.Decision)
	}
	if o.Continue == nil || *o.Continue {
		t.Errorf("a HALT must render continue:false; got %v", o.Continue)
	}
	if o.StopReason == "" {
		t.Error("a HALT must carry stopReason, or the user is never told why the thread stopped")
	}
}

// (c) The halting run writes a latch, and (d) the NEXT gated call in that
// session replays it with zero round trips -- for a prompt AND for a tool.
// Without the latch reader this case cannot pass, which is why the reader and
// the only contract that writes a latch ship together.
func TestPromptGate_HaltLatchesAndReplaysWithoutARoundTrip(t *testing.T) {
	haltDir := gateEnv(t)
	srv := serveVerdictServer(t, `{"verdict":"halt","reason":"policy violation","policy_id":"p2"}`)

	runHook(t, "UserPromptSubmit", promptEvent("th-latch"))
	latches, _ := filepath.Glob(filepath.Join(haltDir, "*"))
	if len(latches) != 1 {
		t.Fatalf("the halting verdict must write exactly one latch under %s; got %v", haltDir, latches)
	}
	hitsAfterHalt := srv.Hits()
	if hitsAfterHalt == 0 {
		t.Fatal("the first gated call must reach /evaluate, or the halt was synthesized locally")
	}

	for _, tc := range []struct{ name, hook, payload string }{
		{"a later prompt", "UserPromptSubmit", promptEvent("th-latch")},
		{"a later tool call", "PreToolUse", toolEvent("th-latch")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := srv.Hits()
			stdout, _ := runHook(t, tc.hook, tc.payload)
			if strings.TrimSpace(stdout) == "" {
				t.Error("a halted session must still refuse on stdout, not fall silent")
			}
			if got := srv.Hits() - before; got != 0 {
				t.Errorf("a latched halt is the decided state: expected 0 evaluate round trips, got %d", got)
			}
		})
	}
}

// (e) A latch that exists but will not parse still halts. The file's presence is
// the decided state; a parse guard that un-halted would turn a corrupted disk
// into an open session.
func TestPromptGate_CorruptLatchStillHalts(t *testing.T) {
	haltDir := gateEnv(t)
	srv := serveVerdictServer(t, `{"verdict":"allow"}`)

	if err := os.MkdirAll(haltDir, 0o700); err != nil {
		t.Fatalf("mkdir halt dir: %v", err)
	}
	// Seed a latch through the real writer, then corrupt it on disk.
	hookflow.WriteSessionHalt(log.New(io.Discard, "", 0), "th-corrupt",
		client.Evaluation{Verdict: client.VerdictHalt, Reason: "seeded", PolicyID: "p"})
	latches, _ := filepath.Glob(filepath.Join(haltDir, "*"))
	if len(latches) != 1 {
		t.Fatalf("expected one seeded latch, got %v", latches)
	}
	if err := os.WriteFile(latches[0], []byte("{not json at all"), 0o600); err != nil {
		t.Fatalf("corrupt the latch: %v", err)
	}

	before := srv.Hits()
	stdout, _ := runHook(t, "UserPromptSubmit", promptEvent("th-corrupt"))
	if strings.TrimSpace(stdout) == "" {
		t.Error("an unparsable latch must still halt; presence is the decided state")
	}
	if got := srv.Hits() - before; got != 0 {
		t.Errorf("a corrupt latch must not re-open the round trip; got %d evaluate calls", got)
	}
}

// (f) With enforce off the new class is inert: stdout stays empty, exactly as
// the observe-only path has always behaved.
func TestPromptGate_EnforceOffWritesNothing(t *testing.T) {
	gateEnv(t)
	t.Setenv(devconfig.EnvEnforce, "0")
	serveVerdictServer(t, `{"verdict":"block","reason":"would have blocked","policy_id":"p1"}`)

	stdout, _ := runHook(t, "UserPromptSubmit", promptEvent("th-off"))
	if stdout != "" {
		t.Errorf("enforce off must write nothing to stdout; got %q", stdout)
	}
}

// TestOutputContracts_NeverRenderARejectedShape is the fail-open guard, asserted
// on the exact marshalled bytes for every verdict against both contracts.
//
// This is not style. Codex invalidates a PreToolUse output that carries ANY
// universal field, a bare allow, `ask`, `decision:"approve"`, or a deny with an
// empty reason -- and an invalidated output means THE TOOL PROCEEDS. The failure
// is silent in production, so the only place it can be caught is here.
func TestOutputContracts_NeverRenderARejectedShape(t *testing.T) {
	// Every verdict the gate can hand a contract, including the synthesized ones.
	decisions := []string{"", "allow", "deny", "block", hookflow.DecisionHalt}

	t.Run("PreToolUse", func(t *testing.T) {
		for _, d := range decisions {
			line, applied := contract.Render(d, "a reason", nil)
			if len(line) == 0 {
				continue // proceed-with-nothing-to-say is always legal
			}
			s := string(line)
			for _, forbidden := range []string{`"continue"`, `"stopReason"`, `"suppressOutput"`, `"systemMessage"`} {
				if strings.Contains(s, forbidden) {
					t.Errorf("decision %q: PreToolUse output carries universal field %s, which invalidates the whole output and lets the tool run: %s", d, forbidden, s)
				}
			}
			if strings.Contains(s, `"permissionDecision":"ask"`) {
				t.Errorf("decision %q: Codex has no ask verb on PreToolUse: %s", d, s)
			}
			if strings.Contains(s, `"decision":"approve"`) {
				t.Errorf("decision %q: decision:approve is rejected by Codex: %s", d, s)
			}
			var o preToolUseOutput
			if err := json.Unmarshal(line, &o); err != nil {
				t.Fatalf("decision %q: unmarshalable PreToolUse output: %v", d, err)
			}
			h := o.HookSpecificOutput
			if h.PermissionDecision == codexDecisionAllow && len(h.UpdatedInput) == 0 {
				t.Errorf("decision %q: a BARE allow is rejected by Codex and fails the call open: %s", d, s)
			}
			if h.PermissionDecision == codexDecisionDeny && h.PermissionDecisionReason == "" {
				t.Errorf("decision %q: a deny with an empty reason is rejected by Codex: %s", d, s)
			}
			if len(h.UpdatedInput) > 0 && h.PermissionDecision != codexDecisionAllow {
				t.Errorf("decision %q: updatedInput is only legal alongside allow: %s", d, s)
			}
			if applied == hookflow.DecisionHalt {
				t.Errorf("decision %q: PreToolUse must fold HALT into its per-call deny, never report halt (only the prompt contract may latch)", d)
			}
		}
	})

	t.Run("UserPromptSubmit", func(t *testing.T) {
		sawHalt := false
		for _, d := range decisions {
			line, applied := promptContract.Render(d, "a reason", nil)
			if len(line) == 0 {
				if d != "" {
					t.Errorf("decision %q: a refusal must render something", d)
				}
				continue
			}
			o := parsePromptOutput(t, string(line))
			if o.Decision != codexPromptDecisionBlock {
				t.Errorf("decision %q: the only decision verb on this surface is block; got %q", d, o.Decision)
			}
			if o.Reason == "" {
				t.Errorf("decision %q: a block must carry a reason", d)
			}
			isHalt := d == hookflow.DecisionHalt
			if (o.Continue != nil) != isHalt {
				t.Errorf("decision %q: continue:false must appear for HALT and only for HALT; got %v", d, o.Continue)
			}
			if isHalt {
				sawHalt = true
				if applied != hookflow.DecisionHalt {
					t.Errorf("the prompt contract must report halt so the gate writes the latch; got %q", applied)
				}
			}
		}
		if !sawHalt {
			t.Fatal("the HALT case never ran; the table proves nothing about the latch writer")
		}
	})
}

// TestPromptContract_IsTheOnlyLatchWriter states the design ruling as a test:
// one writer, so there is a single place to reason about when a Codex session
// becomes unrunnable. Both tool contracts still READ the latch (covered above).
func TestPromptContract_IsTheOnlyLatchWriter(t *testing.T) {
	if _, applied := contract.Render(hookflow.DecisionHalt, "r", nil); applied == hookflow.DecisionHalt {
		t.Error("the PreToolUse contract must fold HALT to deny, or Codex gains a second latch writer")
	}
	if _, applied := promptContract.Render(hookflow.DecisionHalt, "r", nil); applied != hookflow.DecisionHalt {
		t.Error("the prompt contract must report halt, or nothing ever writes the latch")
	}
}
