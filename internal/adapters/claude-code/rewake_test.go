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

// rewakePayload mirrors a real Claude Code payload, tool_use_id included.
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

// TestRunRewake_InertWhenNothingCanFileAnApproval the watcher runs alongside
// the gate on every tool call, so the cases where it has nothing to do must
// cost nothing. It must also never wake a session on its own account: exit 0
// with no output is the silent path.
func TestRunRewake_InertWhenNothingCanFileAnApproval(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  map[string]string
		tool string
	}{
		{"enforce off — no gate, so no approval", map[string]string{devconfig.EnvEnforce: "0"}, "Bash"},
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

// TestApprovalKeyIsStableAcrossProcessesAndRetries the load-bearing cross-
// process property (E9 §2.5): the gate and the watcher are separate processes
// that map the same payload independently, and later a retry maps it a third
// time.
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
		t.Errorf("approval key is not stable across a retry — an approved request can never be consumed:\n"+
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
	t.Setenv(envDID, testDID)

	var wake bytes.Buffer
	if code := RunRewake(strings.NewReader("not json"), &wake, log.New(&bytes.Buffer{}, "", 0)); code != 0 {
		t.Errorf("exit = %d on an unparseable payload, want 0", code)
	}
	if wake.Len() != 0 {
		t.Errorf("wrote %q on an unparseable payload", wake.String())
	}
}
