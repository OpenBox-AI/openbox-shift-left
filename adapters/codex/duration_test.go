package codex

import (
	"testing"
	"time"
)

// TestDuration_ToolUseIDKeyedPairing proves the E7-S8 stash keyed by
// tool_use_id (AC-5): the completed event recovers the started event's
// timestamp, and two CONCURRENT invocations of the same tool never swap start
// times (the CC adapter's documented ambiguity, closed by tool_use_id).
func TestDuration_ToolUseIDKeyedPairing(t *testing.T) {
	ad := New(Identity{DeveloperDID: testDID}, t.TempDir())
	t0 := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	tick := t0
	ad.Mapper.Now = func() time.Time { tick = tick.Add(time.Second); return tick }

	// Two overlapping Bash calls: pre(call-1), pre(call-2), post(call-2), post(call-1).
	pre1, _ := ad.Mapper.Map(HookPreToolUse, &HookEvent{SessionID: "th-1", ToolName: "Bash", ToolUseID: "call-1"})
	ad.threadDuration(&pre1) // t0+1s
	pre2, _ := ad.Mapper.Map(HookPreToolUse, &HookEvent{SessionID: "th-1", ToolName: "Bash", ToolUseID: "call-2"})
	ad.threadDuration(&pre2) // t0+2s

	post2, _ := ad.Mapper.Map(HookPostToolUse, &HookEvent{SessionID: "th-1", ToolName: "Bash", ToolUseID: "call-2"})
	ad.threadDuration(&post2)
	post1, _ := ad.Mapper.Map(HookPostToolUse, &HookEvent{SessionID: "th-1", ToolName: "Bash", ToolUseID: "call-1"})
	ad.threadDuration(&post1)

	if post1.StartedAt != pre1.StartedAt {
		t.Errorf("post(call-1) StartedAt = %q, want its own pre's %q", post1.StartedAt, pre1.StartedAt)
	}
	if post2.StartedAt != pre2.StartedAt {
		t.Errorf("post(call-2) StartedAt = %q, want its own pre's %q", post2.StartedAt, pre2.StartedAt)
	}
	if post1.StartedAt == post2.StartedAt {
		t.Error("concurrent same-tool calls swapped/shared start times — tool_use_id keying broken")
	}
}

// A stash miss (unpaired completed) keeps the completed event's own timestamp:
// duration degrades to 0, never an error (INV-3).
func TestDuration_StashMissIsGraceful(t *testing.T) {
	ad := New(Identity{DeveloperDID: testDID}, t.TempDir())
	post, _ := ad.Mapper.Map(HookPostToolUse, &HookEvent{SessionID: "th-1", ToolName: "Bash", ToolUseID: "never-started"})
	before := post.EndedAt
	ad.threadDuration(&post)
	if post.StartedAt != "" || post.EndedAt != before {
		t.Errorf("stash miss must leave the event untouched: %+v", post)
	}
}

// SessionEnd sweeps the session's stash so unpaired records don't accumulate.
func TestDuration_SessionEndSweeps(t *testing.T) {
	ad := New(Identity{DeveloperDID: testDID}, t.TempDir())
	pre, _ := ad.Mapper.Map(HookPreToolUse, &HookEvent{SessionID: "th-1", ToolName: "Bash", ToolUseID: "call-1"})
	ad.threadDuration(&pre)

	end, _ := ad.Mapper.Map(HookSessionEnd, &HookEvent{SessionID: "th-1", Reason: "other"})
	ad.threadDuration(&end)

	if got := ad.Durations.takeStart("th-1", toolCallStartKey(pre)); got != "" {
		t.Errorf("stash record survived SessionEnd sweep: %q", got)
	}
}
