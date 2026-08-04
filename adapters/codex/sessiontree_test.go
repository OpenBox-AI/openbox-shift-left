package codex

import (
	"testing"
	"time"
)

// E8-S4. Codex's app-server rule is that a forked thread keeps the root's
// session id, so the hook payload's session_id is the right OpenBox session
// identity either way; what the payload cannot tell us is *which* thread we are
// on. The hook process inherits that as CODEX_THREAD_ID, and when it differs
// from session_id the run is a fork — the one case where the git trailer
// (attributed by thread id) and the event stream (keyed by root session id)
// would otherwise not join up.
func TestSessionTree_ForkRecordsBothIDs(t *testing.T) {
	m := testMapper()
	m.ThreadID = "thread-fork-9"

	for _, hook := range []HookName{
		HookSessionStart, HookUserPromptSubmit, HookPreToolUse, HookPostToolUse, HookSessionEnd,
	} {
		ev, ok := m.Map(hook, &HookEvent{
			SessionID: "sess-root-1", ToolName: "Bash", PermissionMode: "default", Source: "startup",
		})
		if !ok {
			t.Fatalf("%s did not map", hook)
		}
		if ev.SessionID != "sess-root-1" {
			t.Errorf("%s: session identity must stay the root id, got %q", hook, ev.SessionID)
		}
		if ev.Metadata["thread_id"] != "thread-fork-9" {
			t.Errorf("%s: missing thread_id, metadata=%v", hook, ev.Metadata)
		}
		if ev.Metadata["root_session_id"] != "sess-root-1" {
			t.Errorf("%s: missing root_session_id, metadata=%v", hook, ev.Metadata)
		}
	}
}

// An unforked run — the ambient thread id equals the session id, or no Codex
// env at all — must be byte-identical to before the story: no extra keys.
func TestSessionTree_UnforkedEmitsNothing(t *testing.T) {
	for name, threadID := range map[string]string{
		"root thread (ids equal)": "sess-1",
		"no ambient env":          "",
	} {
		m := testMapper()
		m.ThreadID = threadID
		ev, ok := m.Map(HookPreToolUse, &HookEvent{
			SessionID: "sess-1", ToolName: "Bash", PermissionMode: "default",
		})
		if !ok {
			t.Fatalf("%s: did not map", name)
		}
		for _, k := range []string{"thread_id", "root_session_id"} {
			if _, present := ev.Metadata[k]; present {
				t.Errorf("%s: %s must be absent for an unforked run, metadata=%v", name, k, ev.Metadata)
			}
		}
	}
}

// A fork's events must not collide with the root's under the idempotency
// derivation (INV-5): same session, same tool, same instant, different thread.
func TestSessionTree_ForkEventIDsDoNotCollide(t *testing.T) {
	payload := func() *HookEvent {
		return &HookEvent{SessionID: "sess-1", ToolName: "Bash", PermissionMode: "default"}
	}
	// Deliberately not testMapper(): that pins NewID, which would bypass the
	// deterministic derivation this test is about.
	derivingMapper := func(threadID string) Mapper {
		m := NewMapper(Identity{DeveloperDID: testDID})
		m.Now = func() time.Time { return time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC) }
		m.ThreadID = threadID
		return m
	}
	root := derivingMapper("sess-1")        // unforked
	fork := derivingMapper("thread-fork-9") // forked off the same session

	rootEv, _ := root.Map(HookPreToolUse, payload())
	forkEv, _ := fork.Map(HookPreToolUse, payload())
	if rootEv.EventID == forkEv.EventID {
		t.Errorf("root and fork events share event_id %q — the fork linkage must feed the derivation",
			rootEv.EventID)
	}
}

// The ids are externally influenced, so they are bounded like every other
// identifier before egress (maxIdentLen).
func TestSessionTree_IdentifiersBounded(t *testing.T) {
	long := ""
	for len(long) < maxIdentLen*2 {
		long += "x"
	}
	m := testMapper()
	m.ThreadID = long
	ev, _ := m.Map(HookPreToolUse, &HookEvent{SessionID: "sess-1", ToolName: "Bash"})
	if got := ev.Metadata["thread_id"].(string); len(got) > maxIdentLen {
		t.Errorf("thread_id not bounded: len %d > %d", len(got), maxIdentLen)
	}
}
