package claudecode

import (
	"bytes"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/client"
)

func rewakePayloadWithUse(tool, toolUseID string) string {
	return `{"session_id":"s1","hook_event_name":"PreToolUse","tool_name":"` + tool +
		`","tool_use_id":"` + toolUseID + `","tool_input":{"command":"ls"}}`
}

// TestRunRewake_InertWhenNothingCanFileAnApproval the watcher runs alongside
// the gate on every tool call, so the cases where the payload cannot even be
// mapped must cost nothing. It must also never wake a session on its own
// account: exit 0 with no output is the silent path.
//
// There is no "enforce off" case left: rewake is unconditional now
// (ResolveEnforce always reports true), so this covers the one fast-return
// path that remains before AwaitRewake's own marker-grace wait -- a payload
// Mapper.Map refuses outright (no session id).
func TestRunRewake_InertWhenNothingCanFileAnApproval(t *testing.T) {
	for _, tc := range []struct {
		name    string
		payload string
	}{
		{"unmappable payload; no session id, so Map refuses before any wait",
			`{"hook_event_name":"PreToolUse","tool_name":"Bash","tool_use_id":"toolu_01AAAAAAAAAAAAAAAAAAAAAA","tool_input":{"command":"ls"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateConfig(t)
			t.Setenv(devconfig.EnvPendingApprovalDir, t.TempDir())
			t.Setenv(envAgentID, testAgentID)

			var wake bytes.Buffer
			start := time.Now()
			code := RunRewake(strings.NewReader(tc.payload), &wake, log.New(&bytes.Buffer{}, "", 0))
			elapsed := time.Since(start)

			if code != 0 {
				t.Errorf("exit = %d, want 0 (a non-zero exit interrupts the session)", code)
			}
			if wake.Len() != 0 {
				t.Errorf("wrote %q; the silent path must say nothing", wake.String())
			}
			if elapsed > time.Second {
				t.Errorf("took %v; the no-op path must not wait", elapsed)
			}
		})
	}
}

// TestApprovalKeyIsStableAcrossProcessesAndRetries the load-bearing cross-
// process property: the gate and the watcher are separate processes
// that map the same payload independently, and later a retry maps it a third
// time.
func TestApprovalKeyIsStableAcrossProcessesAndRetries(t *testing.T) {
	isolateConfig(t)
	t.Setenv(envAgentID, testAgentID)

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

	gate := derive("toolu_01AAAAAAAAAAAAAAAAAAAAAA")
	time.Sleep(2 * time.Millisecond) // a different wall clock, as a real retry has
	watcher := derive("toolu_01AAAAAAAAAAAAAAAAAAAAAA")
	if gate != watcher {
		t.Errorf("approval key drifted between processes:\ngate    %+v\nwatcher %+v", gate, watcher)
	}
	if !gate.Valid() {
		t.Errorf("key from a real payload is not addressable: %+v", gate)
	}

	// The key must still match.
	retry := derive("toolu_01BBBBBBBBBBBBBBBBBBBBBB")
	if retry != gate {
		t.Errorf("approval key is not stable across a retry; an approved request can never be consumed:\n"+
			"first attempt %+v\nretry         %+v", gate, retry)
	}
}

// TestRunRewake_SurvivesAnUnusablePayload a malformed payload is a
// misconfiguration, not a reason to interrupt anyone.
func TestRunRewake_SurvivesAnUnusablePayload(t *testing.T) {
	isolateConfig(t)
	t.Setenv(devconfig.EnvPendingApprovalDir, t.TempDir())
	t.Setenv(devconfig.EnvEnforce, "1")
	t.Setenv(devconfig.EnvTier2, "1")
	t.Setenv(envAgentID, testAgentID)

	var wake bytes.Buffer
	if code := RunRewake(strings.NewReader("not json"), &wake, log.New(&bytes.Buffer{}, "", 0)); code != 0 {
		t.Errorf("exit = %d on an unparseable payload, want 0", code)
	}
	if wake.Len() != 0 {
		t.Errorf("wrote %q on an unparseable payload", wake.String())
	}
}
