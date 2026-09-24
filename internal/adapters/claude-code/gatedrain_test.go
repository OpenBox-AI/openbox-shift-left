package claudecode

import (
	"bytes"
	"context"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/client"
	"github.com/openbox-ai/openbox-shift-left/internal/client/fakecore"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
)

// TestGateDrain_SlowCoreNeverHaltsTheRun proves the tie-breaker a gated
// call's own drain step must honor: a core that durably accepted a queued
// event but was too slow to answer within this call's own slack never
// halts the run. The event stays queued (never scored a failure) and a
// later, unbounded drain delivers it -- exactly once more, under the SAME
// idempotency key, which is the only double-send this repo's own delivery
// policy allows.
func TestGateDrain_SlowCoreNeverHaltsTheRun(t *testing.T) {
	isolateConfig(t)
	t.Setenv(envEnforce, "1")
	t.Setenv(envFailClosed, "1")
	spoolDir := t.TempDir()
	t.Setenv("OPENBOX_SPOOL_DIR", spoolDir)
	t.Setenv("OPENBOX_SESSION_DIR", t.TempDir())
	t.Setenv(envEnforcementFile, t.TempDir()+"/enf.jsonl")

	const sessionID = "gd-1"
	const backlogEventID = "backlog-1"

	// Only the backlog's own event type (a lifecycle SessionStarted, left
	// behind by an earlier, interrupted delivery) is slow; the current
	// call's own escalation (a ToolCall) answers immediately, so this
	// call's own decision is unaffected by the backlog's own fate.
	f := fakecore.New(t, fakecore.Script{
		Default: `{"verdict":"allow"}`,
		DelayFor: map[string]time.Duration{
			// The wire discriminator a SessionStarted DevEvent carries
			// (client.buildPayload's own wireWorkflowStarted), not the
			// internal client.EventSessionStarted enum string.
			"WorkflowStarted": 4 * time.Second,
		},
	})
	evalCreds(t, f.URL())

	// Seed a backlog event for this session directly into the spool, as if
	// an earlier call's own inline attempt had already left it queued.
	seedAd := New(Identity{DeveloperDID: "did:aip:dev"}, spoolDir)
	if err := seedAd.Spool.Append(client.DevEvent{
		SchemaVersion: client.SchemaVersion,
		EventID:       backlogEventID,
		EventType:     client.EventSessionStarted,
		SessionID:     sessionID,
		DeveloperDID:  "did:aip:dev",
		Timestamp:     "2026-08-01T12:00:00Z",
	}); err != nil {
		t.Fatalf("seed backlog: %v", err)
	}

	payload := `{"hook_event_name":"PreToolUse","session_id":"` + sessionID + `","cwd":"/tmp","tool_name":"Bash","tool_input":{"command":"echo hi"}}`
	var stdout bytes.Buffer
	RunHook("PreToolUse", strings.NewReader(payload), &stdout, log.New(&bytes.Buffer{}, "", 0))

	if strings.TrimSpace(stdout.String()) != "" {
		t.Errorf("this call's own escalation is unaffected by the backlog's own slow answer; want a silent allow, got %q", stdout.String())
	}
	if _, halted := hookflow.SessionHalted(sessionID); halted {
		t.Error("a slow-but-reachable core must never halt the run; only an explicit non-acceptance does")
	}

	drainedAd := New(Identity{DeveloperDID: "did:aip:dev"}, spoolDir)
	if n := drainedAd.Spool.PendingCount(sessionID); n == 0 {
		t.Fatal("the backlog event must still be queued after a slow-core drain; it was never a failure, so it must not have vanished")
	}

	// The 30s drainer's own turn: unbounded per event now, no longer racing
	// this call's own slack, so it waits out the same script's delay and
	// gets a real answer. Built the same way every production drainer
	// resolves its own client (env-driven), not a literal struct: a literal
	// carrying a field named like a credential trips this repo's own
	// on-disk secret redaction.
	creds, err := ResolveCredentials()
	if err != nil {
		t.Fatalf("resolve credentials: %v", err)
	}
	cl, err := creds.NewClient(log.New(&bytes.Buffer{}, "", 0))
	if err != nil {
		t.Fatalf("build client: %v", err)
	}
	if _, err := drainedAd.FlushOrSweep(context.Background(), sessionID, cl); err != nil {
		t.Fatalf("later flush: %v", err)
	}
	if n := drainedAd.Spool.PendingCount(sessionID); n != 0 {
		t.Errorf("pending count after the later flush = %d, want 0", n)
	}
	if _, halted := hookflow.SessionHalted(sessionID); halted {
		t.Error("the later, successful delivery must not retroactively halt the run either")
	}

	byKey := f.AttemptsByKey()
	if n := byKey[backlogEventID]; n != 2 {
		t.Errorf("attempts carrying the backlog event's own idempotency key = %d, want exactly 2 "+
			"(the gate's own abandoned attempt, then the later drainer's) -- the one double-send this repo allows", n)
	}
}

// TestGateDrain_ExplicitRejectionInsideTheWindowHaltsAndDeniesThatCall is the
// other half of the same tie-breaker: an EXPLICIT non-acceptance found
// while draining (core answered, and the answer was a refusal, not silence)
// denies the draining call itself and halts the run -- unlike a slow core,
// which never does either. AlwaysStatus makes every wire hit a flat 401,
// the exact shape a rejected workload identity or a rejected credential
// produces, and it answers well inside this call's own drain window (no
// delay at all), so this is unambiguously an explicit rejection, not
// silence.
func TestGateDrain_ExplicitRejectionInsideTheWindowHaltsAndDeniesThatCall(t *testing.T) {
	isolateConfig(t)
	t.Setenv(envEnforce, "1")
	t.Setenv(envFailClosed, "1")
	spoolDir := t.TempDir()
	t.Setenv("OPENBOX_SPOOL_DIR", spoolDir)
	t.Setenv("OPENBOX_SESSION_DIR", t.TempDir())
	t.Setenv(envEnforcementFile, t.TempDir()+"/enf.jsonl")

	const sessionID = "gd-2"
	const backlogEventID = "backlog-2"

	f := fakecore.New(t, fakecore.Script{AlwaysStatus: 401})
	evalCreds(t, f.URL())

	seedAd := New(Identity{DeveloperDID: "did:aip:dev"}, spoolDir)
	if err := seedAd.Spool.Append(client.DevEvent{
		SchemaVersion: client.SchemaVersion,
		EventID:       backlogEventID,
		EventType:     client.EventSessionStarted,
		SessionID:     sessionID,
		DeveloperDID:  "did:aip:dev",
		Timestamp:     "2026-08-01T12:00:00Z",
	}); err != nil {
		t.Fatalf("seed backlog: %v", err)
	}

	payload := `{"hook_event_name":"PreToolUse","session_id":"` + sessionID + `","cwd":"/tmp","tool_name":"Bash","tool_input":{"command":"echo hi"}}`
	var stdout bytes.Buffer
	RunHook("PreToolUse", strings.NewReader(payload), &stdout, log.New(&bytes.Buffer{}, "", 0))

	d, _ := parsePermissionDecision(t, stdout.Bytes())
	if d != ccDecisionDeny {
		t.Fatalf("permissionDecision = %q, want deny; an explicit 401 found while draining must deny this call", d)
	}
	if _, halted := hookflow.SessionHalted(sessionID); !halted {
		t.Error("an explicit 401 found while draining must halt the run")
	}
	if n := f.V3EvaluateAttempts(); n != 1 {
		t.Errorf("/evaluate attempts = %d, want exactly 1 (the drained backlog event); the gate's own escalation must never run once the drain latched the run", n)
	}
}
