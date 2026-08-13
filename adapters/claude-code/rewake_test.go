package claudecode

import (
	"bytes"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/openbox-ai/openbox-shift-left/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/client"
)

// rewakePayload mirrors a REAL Claude Code payload, tool_use_id included.
// Omitting it is what made TestApprovalKeyIsStableAcrossProcessesAndRetries
// vacuous: tool_use_id is the one field that differs between a call and its
// retry, so a fixture without it cannot detect a key that is unstable across
// retries — which is precisely the failure the test exists to catch.
func rewakePayload(tool string) string {
	return rewakePayloadWithUse(tool, "toolu_01AAAAAAAAAAAAAAAAAAAAAA")
}

func rewakePayloadWithUse(tool, toolUseID string) string {
	return `{"session_id":"s1","hook_event_name":"PreToolUse","tool_name":"` + tool +
		`","tool_use_id":"` + toolUseID + `","tool_input":{"command":"ls"}}`
}

func runRewake(t *testing.T, tool string) (int, string, time.Duration) {
	t.Helper()
	var wake bytes.Buffer
	start := time.Now()
	code := RunRewake(strings.NewReader(rewakePayload(tool)), &wake, log.New(&bytes.Buffer{}, "", 0))
	return code, wake.String(), time.Since(start)
}

// The watcher runs alongside the gate on EVERY tool call, so the cases where it
// has nothing to do must cost nothing. It must also never wake a session on its
// own account: exit 0 with no output is the silent path.
func TestRunRewake_InertWhenNothingCanFileAnApproval(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  map[string]string
		tool string
	}{
		// Enforce must be turned off EXPLICITLY. It defaults on (ADR-0016), and
		// this case used to pass on an empty env only because the tier-2 toggle
		// it also depended on defaulted off — so it was asserting the inertness
		// of a gate that was in fact enabled. ADR-0017 removed that toggle and
		// exposed it.
		{"enforce off — no gate, so no approval", map[string]string{devconfig.EnvEnforce: "0"}, "Bash"},
		// Enforce-off is the ONLY inert case left. The two that stood beside it
		// — tier-2 off, and a non-high-risk class — are deleted rather than
		// fixed, because neither is inert any more: every class evaluates
		// inline, so every class can hold an approval and every one deserves a
		// watcher. What bounds that cost is rewakeMarkerGrace, not a class test.
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateConfig(t)
			t.Setenv(devconfig.EnvPendingApprovalDir, t.TempDir())
			t.Setenv(envDID, testDID)
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			code, wake, elapsed := runRewake(t, tc.tool)
			if code != 0 {
				t.Errorf("exit = %d, want 0 (a non-zero exit interrupts the session)", code)
			}
			if wake != "" {
				t.Errorf("wrote %q — the silent path must say nothing", wake)
			}
			if elapsed > time.Second {
				t.Errorf("took %v — the no-op path must not wait", elapsed)
			}
		})
	}
}

// The load-bearing cross-process property (E9 §2.5): the gate and the watcher
// are separate processes that map the same payload independently, and later a
// RETRY maps it a third time. All three must derive the same approval key, or
// the watcher never finds the gate's marker and the retry never finds the
// grant — a silent failure of the whole loop. The key is therefore built only
// from stable fields (session, tool, path/function), never the clock.
func TestApprovalKeyIsStableAcrossProcessesAndRetries(t *testing.T) {
	isolateConfig(t)
	t.Setenv(envDID, testDID)

	derive := func(toolUseID string) client.ApprovalKey {
		id, err := ResolveIdentity()
		if err != nil {
			t.Fatalf("identity: %v", err)
		}
		ev, err := ParseHookEvent(strings.NewReader(rewakePayloadWithUse("Bash", toolUseID)))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		m := New(id, t.TempDir()).Mapper
		devEv, ok := m.Map(HookPreToolUse, ev)
		if !ok {
			t.Fatal("payload must map")
		}
		return client.ApprovalKeyFor(devEv)
	}

	// Same invocation, two processes: the gate and the watcher must agree, or
	// the watcher never finds the gate's marker.
	gate := derive("toolu_01AAAAAAAAAAAAAAAAAAAAAA")
	time.Sleep(2 * time.Millisecond) // a different wall clock, as a real retry has
	watcher := derive("toolu_01AAAAAAAAAAAAAAAAAAAAAA")
	if gate != watcher {
		t.Errorf("approval key drifted between processes:\ngate    %+v\nwatcher %+v", gate, watcher)
	}
	if !gate.Valid() {
		t.Errorf("key from a real payload is not addressable: %+v", gate)
	}

	// A RETRY of the same operation. Claude Code mints a fresh tool_use_id for
	// every tool call, so this is what a retry actually looks like on the wire.
	//
	// The key must still match. It addresses the approval record, and core's
	// bypass grant is keyed on it too, so a key that changes per invocation
	// means an approver's decision can never be consumed: the retry files a
	// NEW request, the developer is asked to approve again, and the rewake's
	// "re-run to proceed" turns that into an unbounded loop. Observed live —
	// three attempts in one session, three approval ids, no output.
	retry := derive("toolu_01BBBBBBBBBBBBBBBBBBBBBB")
	if retry != gate {
		t.Errorf("approval key is not stable across a retry — an approved request can never be consumed:\n"+
			"first attempt %+v\nretry         %+v", gate, retry)
	}
}

// A malformed payload is a misconfiguration, not a reason to interrupt anyone.
func TestRunRewake_SurvivesAnUnusablePayload(t *testing.T) {
	isolateConfig(t)
	t.Setenv(devconfig.EnvPendingApprovalDir, t.TempDir())
	t.Setenv(devconfig.EnvEnforce, "1")
	t.Setenv(devconfig.EnvTier2, "1")
	t.Setenv(envDID, testDID)

	var wake bytes.Buffer
	if code := RunRewake(strings.NewReader("not json"), &wake, log.New(&bytes.Buffer{}, "", 0)); code != 0 {
		t.Errorf("exit = %d on an unparseable payload, want 0", code)
	}
	if wake.Len() != 0 {
		t.Errorf("wrote %q on an unparseable payload", wake.String())
	}
}
