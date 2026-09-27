package client

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// turnEvent is a TurnCompleted with nothing said about which lane produced it.
// Each test below supplies exactly one source, because the two are mutually
// exclusive and a fixture carrying both would prove nothing about either.
func turnEvent(id string) DevEvent {
	idx := 3
	return DevEvent{
		SchemaVersion: SchemaVersion,
		EventID:       id,
		EventType:     EventTurnCompleted,
		SessionID:     "sess-reply",
		DeveloperDID:  "did:aip:7f3c9b2e-0000-5000-a000-000000000001",
		Timestamp:     "2026-09-08T10:00:12Z",
		Tool:          Tool{Name: "claude-code", Kind: ToolShell},
		TurnIndex:     &idx,
		Model:         "claude-opus-5",
	}
}

// TestTheHookLaneTurnCarriesReplyText is the gap this closes: core judges an
// assistant turn from activity_output.reply_text and refuses any fallback to
// `content`, deliberately, because old producers keep sending SSE frames there.
// This client had never emitted the key, so no assistant turn had ever been
// judged.
func TestTheHookLaneTurnCarriesReplyText(t *testing.T) {
	const reply = "I refactored the spool; all 11 modules are green."
	ev := turnEvent("ev-hook-reply-text")
	ev.Content = &Content{Output: reply, Thinking: "reasoning about the spool"}

	m := wireOf(t, ev)
	if got := nestedString(t, m, "activity_output", "reply_text"); got != reply {
		t.Errorf("activity_output.reply_text = %q, want the reassembled reply", got)
	}
	// Additive: `content` is what OPA reads on an activity_output today, and
	// removing it would be a non-additive wire change.
	if got := nestedString(t, m, "activity_output", "content"); got != reply {
		t.Errorf("activity_output.content = %q, want the reply unchanged", got)
	}
	// Thinking keeps its own key; a reader that conflates the two is corrupted
	// silently.
	if got := nestedString(t, m, "activity_output", "thinking"); got == reply {
		t.Error("thinking and the reply collapsed into one value")
	}
}

// TestTheSpanLaneTurnCarriesNoReplyText is the cost bound, and it is the single
// load-bearing decision of this change.
//
// reply_text is set inside the Content.Output arm, not after the switch, so a
// lane whose text came from Span.ResponseBody never gains it. That matters
// because core judges on the key's PRESENCE: a live session carried 137
// :proxy: llm_completion rows against 2 :turn: ones, so sourcing reply_text
// from the span body too would have turned ~+2 judge calls per session into
// ~+137, against a fleet ceiling of roughly 7 judgements a minute.
func TestTheSpanLaneTurnCarriesNoReplyText(t *testing.T) {
	const sse = "event: message_start\ndata: {\"type\":\"message_start\"}\n"
	ev := turnEvent("ev-span-reply-text")
	ev.Span = &Span{ResponseBody: sse}

	m := wireOf(t, ev)
	out, _ := m["activity_output"].(map[string]any)
	if out == nil {
		t.Fatal("the span lane produced no activity_output at all")
	}
	if v, present := out["reply_text"]; present {
		t.Errorf("reply_text = %v on a span-sourced turn; core would judge every relayed "+
			"model call, not every turn", v)
	}
	if got, _ := out["content"].(string); got != sse {
		t.Errorf("activity_output.content = %q, want the verbatim SSE unchanged", got)
	}
}

// TestReplyTextIsContentGated with capture off the mapper nils Content, so
// neither key survives. Asserted on the outbound bytes: the composed behaviour
// is what INV-2 promises, not the mapper's intent.
func TestReplyTextIsContentGated(t *testing.T) {
	const reply = "REPLY-TEXT-CANARY: I refactored the spool."
	ev := turnEvent("ev-gated-reply-text")
	ev.Content = &Content{Output: reply}

	raw, err := buildPayload(stripContent(ev))
	if err != nil {
		t.Fatalf("buildPayload: %v", err)
	}
	if strings.Contains(string(raw), reply) {
		t.Errorf("the reply egressed with content capture OFF: %s", raw)
	}
	if strings.Contains(string(raw), "reply_text") {
		t.Errorf("the reply_text key survived the content gate: %s", raw)
	}
}

// TestReplyTextUsesTheSameByteCapAsItsSibling. Both keys carry the same string,
// so they must be cut by the same function: capModelCallBody measures BYTES,
// while capBody measures runes. Two copies of one value clipped by two
// different rules is how a truncation bug hides, so test a cap in the unit it
// claims.
func TestReplyTextUsesTheSameByteCapAsItsSibling(t *testing.T) {
	// Multi-byte on purpose: a rune-cap would not truncate this at all, so a
	// wrong cap fails loudly here instead of passing on ASCII.
	long := strings.Repeat("日", maxModelCallBodyBytes)
	ev := turnEvent("ev-capped-reply-text")
	ev.Content = &Content{Output: long}

	m := wireOf(t, ev)
	got := nestedString(t, m, "activity_output", "reply_text")
	sibling := nestedString(t, m, "activity_output", "content")

	if got != sibling {
		t.Errorf("reply_text and content were cut differently:\n reply_text: %d bytes\n content:    %d bytes",
			len(got), len(sibling))
	}
	if len(got) > maxModelCallBodyBytes {
		t.Errorf("reply_text is %d bytes, over the %d-byte cap", len(got), maxModelCallBodyBytes)
	}
	if len(got) == len(long) {
		t.Fatalf("the value was not truncated at all; the fixture proves nothing")
	}
	if !strings.HasSuffix(got, truncationMark) {
		t.Errorf("a clipped reply_text does not read as clipped: %q", got[max(0, len(got)-32):])
	}
	if !utf8.ValidString(got) {
		t.Error("reply_text was cut mid-rune")
	}
}
