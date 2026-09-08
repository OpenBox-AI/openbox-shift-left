package claudecode

import (
	"encoding/json"
	"testing"
)

// TestAnInterruptedToolStillPairs is B1's fix-branch guard.
//
// A live session left 2 of 244 activity_ids single-sided: both Bash, both
// permission_mode auto, each with an ActivityStarted and a goal-alignment
// verdict and no ActivityCompleted. The cheapest explanation would have been an
// unregistered hook, and it is refuted — localhooks.go registers
// PostToolUseFailure with matcher "*", so the installer arms it.
//
// This pins the next link in the chain: IF the interrupt fires
// PostToolUseFailure, the adapter produces a Completed half that pairs by
// activity_id, and pairing does not depend on is_interrupt being set. So a
// single-sided row cannot be caused by the mapper dropping an interrupt it was
// handed — which leaves the vendor firing no post hook, or that one row being
// lost in delivery.
//
// Checked per activity_id, never by parity: two unrelated single-sided rows
// also sum to an even number.
func TestAnInterruptedToolStillPairs(t *testing.T) {
	m := testMapper()
	yes := true

	for _, tc := range []struct {
		name        string
		isInterrupt *bool
	}{
		{"provider said it was an interrupt", &yes},
		{"provider said nothing", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hook := &HookEvent{
				SessionID: "sess-interrupt", ToolName: "Bash", ToolUseID: "toolu_int",
				ToolInput:   json.RawMessage(`{"command":"sleep 60"}`),
				IsInterrupt: tc.isInterrupt,
			}

			pre, ok := m.Map(HookPreToolUse, hook)
			if !ok {
				t.Fatal("PreToolUse did not map")
			}
			post, ok := m.Map(HookPostToolUseFailure, hook)
			if !ok {
				t.Fatal("PostToolUseFailure did not map; an interrupt would leave one row")
			}

			if pre.EventType != "ToolCall" || post.EventType != "ToolResult" {
				t.Fatalf("event types = %s / %s, want ToolCall / ToolResult", pre.EventType, post.EventType)
			}
			if post.Status != "failed" {
				t.Errorf("status = %q, want failed; an interrupt is a failure, distinguished from a "+
					"broken tool by is_interrupt rather than by status", post.Status)
			}

			// The pairing key itself. Both halves are built from the same tool,
			// tool_use_id and structural locator, so the client derives one
			// activity_id from each; a key that differed here would produce
			// exactly the two-single-sided-rows shape B1 observed.
			if pre.Tool != post.Tool {
				t.Errorf("the two halves name different tools: %+v vs %+v", pre.Tool, post.Tool)
			}
			if pre.Span == nil || post.Span == nil {
				t.Fatalf("a half carries no span: pre=%v post=%v", pre.Span, post.Span)
			}
			if pre.Span.OperationID != post.Span.OperationID {
				t.Errorf("operation_id differs across the pair: %q vs %q; the client derives the "+
					"activity_id from it, so the halves would never pair",
					pre.Span.OperationID, post.Span.OperationID)
			}
		})
	}
}
