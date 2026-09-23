package codex

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
)

// rolloutLineJSON builds one cumulative token snapshot, the shape Codex writes.
func rolloutLineJSON(in, out, cacheRead, total int, model string) string {
	s := fmt.Sprintf(`{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":`+
		`{"input_tokens":%d,"output_tokens":%d,"cached_input_tokens":%d,"cache_write_input_tokens":0,"total_tokens":%d}}}}`,
		in, out, cacheRead, total)
	if model != "" {
		s = fmt.Sprintf(`{"type":"turn_context","payload":{"model":%q}}`, model) + "\n" + s
	}
	return s
}

func writeRollout(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("write rollout: %v", err)
	}
	return path
}

func stopPayload(session, rollout, msg string) string {
	b, _ := json.Marshal(msg)
	return `{"hook_event_name":"Stop","session_id":"` + session + `","cwd":"/r","model":"gpt-5.6-sol",` +
		`"permission_mode":"default","turn_id":"t1","stop_hook_active":false,` +
		`"last_assistant_message":` + string(b) + `,"transcript_path":"` + rollout + `"}`
}

func spoolRecords(t *testing.T, spool string) []map[string]any {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(spool, "*.jsonl"))
	var out []map[string]any
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read spool: %v", err)
		}
		for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
			if strings.TrimSpace(line) == "" {
				continue
			}
			var m map[string]any
			if err := json.Unmarshal([]byte(line), &m); err != nil {
				t.Fatalf("spool line is not JSON: %v (%s)", err, line)
			}
			out = append(out, m)
		}
	}
	return out
}

// (a) Three Stops ⇒ three pairs, indices 0/1/2, each pair sharing one activity
// id, and each carrying only ITS window's delta rather than the running total.
func TestTurn_ThreeStopsEmitThreePairs(t *testing.T) {
	spool := setHookEnv(t)
	t.Setenv(devconfig.EnvContentCapture, "0")

	// Cumulative snapshots: 100, 300, 600 total ⇒ deltas 100, 200, 300.
	rollout := writeRollout(t, rolloutLineJSON(100, 0, 0, 100, "gpt-5.6-sol"))
	runHook(t, "Stop", stopPayload("th-turn", rollout, "one"))

	appendRollout(t, rollout, rolloutLineJSON(300, 0, 0, 300, "gpt-5.6-sol"))
	runHook(t, "Stop", stopPayload("th-turn", rollout, "two"))

	appendRollout(t, rollout, rolloutLineJSON(600, 0, 0, 600, "gpt-5.6-sol"))
	runHook(t, "Stop", stopPayload("th-turn", rollout, "three"))

	// Group by turn_index, which is what the wire turns into <session>:turn:N.
	// Checked per index, never by a global row count (CLAUDE.md): six rows would
	// also pass for three started and three completed halves that never pair up.
	byIndex := map[float64][]string{}
	for _, r := range spoolRecords(t, spool) {
		et, _ := r["event_type"].(string)
		if et != "TurnStarted" && et != "TurnCompleted" {
			continue
		}
		if _, isRollup := r["session_rollup"]; isRollup {
			t.Errorf("a per-turn pair must never be marked session_rollup: %v", r)
		}
		idx, ok := r["turn_index"].(float64)
		if !ok {
			t.Fatalf("a turn row carries no turn_index, so it cannot derive <session>:turn:N: %v", r)
		}
		byIndex[idx] = append(byIndex[idx], et)
	}
	if len(byIndex) != 3 {
		t.Fatalf("three turns must produce three distinct turn indices; got %d: %v", len(byIndex), byIndex)
	}
	for i := 0; i < 3; i++ {
		kinds := byIndex[float64(i)]
		if len(kinds) != 2 {
			t.Errorf("turn %d has %d rows, want exactly 2 (one started, one completed): %v", i, len(kinds), kinds)
		}
	}
}

// TestWire_TurnPairSharesOneActivityID proves the id shape on the bytes the
// client actually sends. The spool assertions above stop one layer short:
// activity_id is derived at the payload layer, so asserting turn_index in the
// spool is not asserting the id the control plane dedupes on.
func TestWire_TurnPairSharesOneActivityID(t *testing.T) {
	cl, fc := newWireCapture(t)
	m := testMapper()
	m.NewID = nil

	for i := 0; i < 2; i++ {
		started, completed, ok := m.MapTurn(&HookEvent{SessionID: "th-wire-turn"},
			turnWindow{HasUsage: true, Total: 10, Model: "m"}, i)
		if !ok {
			t.Fatalf("MapTurn %d returned not-ok", i)
		}
		emit(t, cl, started)
		emit(t, cl, completed)
	}

	ids := map[string][]string{}
	var order []string
	for _, r := range fc.Inbox() {
		p := decodeBody(t, r.Raw)
		id, _ := p["activity_id"].(string)
		et, _ := p["event_type"].(string)
		if _, seen := ids[id]; !seen {
			order = append(order, id)
		}
		ids[id] = append(ids[id], et)
	}
	if len(ids) != 2 {
		t.Fatalf("two turns must produce two activity ids, got %d: %v", len(ids), order)
	}
	for i, id := range order {
		if want := fmt.Sprintf("th-wire-turn:turn:%d", i); id != want {
			t.Errorf("activity id %d = %q, want %q", i, id, want)
		}
		if got := ids[id]; len(got) != 2 || got[0] != "ActivityStarted" || got[1] != "ActivityCompleted" {
			t.Errorf("activity %s rows = %v, want exactly one started then one completed", id, got)
		}
	}
}

func appendRollout(t *testing.T, path, line string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("append rollout: %v", err)
	}
	defer f.Close()
	if _, err := f.WriteString(line + "\n"); err != nil {
		t.Fatalf("append rollout: %v", err)
	}
}

// (b)+(c) A Stop whose window carried no new usage emits nothing but still
// advances, and replaying the same rollout emits nothing at all. Together these
// are the exactly-once property.
func TestTurn_NoNewUsageEmitsNothingAndReplayIsIdempotent(t *testing.T) {
	spool := setHookEnv(t)
	rollout := writeRollout(t, rolloutLineJSON(100, 0, 0, 100, "gpt-5.6-sol"))

	runHook(t, "Stop", stopPayload("th-once", rollout, "one"))
	first := len(spoolRecords(t, spool))
	if first == 0 {
		t.Fatal("the first Stop must emit a pair; the case proves nothing otherwise")
	}

	for i := 0; i < 3; i++ {
		runHook(t, "Stop", stopPayload("th-once", rollout, "again"))
	}
	if got := len(spoolRecords(t, spool)); got != first {
		t.Errorf("replaying the same rollout emitted %d extra rows; the cursor must make this exactly-once", got-first)
	}
}

// (d) Cache counts are SUB-counts of input on Codex and must never be added in.
func TestTurn_CacheCountsAreNeverAddedToInput(t *testing.T) {
	spool := setHookEnv(t)
	// input_tokens 100 of which 40 were cache reads ⇒ pure input 60.
	rollout := writeRollout(t, rolloutLineJSON(100, 10, 40, 110, "gpt-5.6-sol"))
	runHook(t, "Stop", stopPayload("th-cache", rollout, "x"))

	for _, r := range spoolRecords(t, spool) {
		if et, _ := r["event_type"].(string); et != "TurnCompleted" {
			continue
		}
		tok, _ := r["tokens"].(map[string]any)
		if tok == nil {
			t.Fatalf("completed half carries no tokens: %v", r)
		}
		if got := tok["input"]; got != float64(60) {
			t.Errorf("input = %v, want 60 (100 minus the 40 cached sub-count)", got)
		}
		if got := tok["cache_read"]; got != float64(40) {
			t.Errorf("cache_read = %v, want 40 reported in its own field", got)
		}
		return
	}
	t.Fatal("no TurnCompleted row found")
}

// (e) A truncated or rotated rollout re-anchors instead of emitting a negative
// or absurd delta. /compact rewrites rollouts underneath us.
func TestTurn_TruncatedRolloutReAnchors(t *testing.T) {
	spool := setHookEnv(t)
	rollout := writeRollout(t,
		rolloutLineJSON(100, 0, 0, 100, "gpt-5.6-sol"),
		rolloutLineJSON(500, 0, 0, 500, "gpt-5.6-sol"))
	runHook(t, "Stop", stopPayload("th-trunc", rollout, "one"))
	before := len(spoolRecords(t, spool))

	// Rewrite the file much shorter: the stored offset now points past its end.
	if err := os.WriteFile(rollout, []byte(rolloutLineJSON(7, 0, 0, 7, "gpt-5.6-sol")+"\n"), 0o600); err != nil {
		t.Fatalf("truncate rollout: %v", err)
	}
	runHook(t, "Stop", stopPayload("th-trunc", rollout, "two"))

	for _, r := range spoolRecords(t, spool)[before:] {
		if et, _ := r["event_type"].(string); et != "TurnCompleted" {
			continue
		}
		tok, _ := r["tokens"].(map[string]any)
		if tok == nil {
			continue
		}
		if tot, _ := tok["total"].(float64); tot < 0 {
			t.Errorf("a re-anchored window must never emit a negative delta; got %v", tot)
		}
	}
}

// (f) Assistant text rides the COMPLETED half only, is gated on content capture,
// and is redacted before attachment.
func TestTurn_AssistantTextIsGatedAndRedacted(t *testing.T) {
	t.Run("capture off attaches nothing", func(t *testing.T) {
		spool := setHookEnv(t)
		t.Setenv(devconfig.EnvContentCapture, "0")
		rollout := writeRollout(t, rolloutLineJSON(100, 0, 0, 100, "m"))
		runHook(t, "Stop", stopPayload("th-c-off", rollout, "the assistant said this"))

		for _, r := range spoolRecords(t, spool) {
			if c, has := r["content"]; has {
				t.Errorf("content_capture=0 must attach no content: %v", c)
			}
		}
	})

	t.Run("capture on attaches redacted output", func(t *testing.T) {
		spool := setHookEnv(t)
		t.Setenv(devconfig.EnvContentCapture, "1")
		os.Unsetenv(devconfig.EnvSecretDetection) // default ON
		secret := awsSecretFixture()
		rollout := writeRollout(t, rolloutLineJSON(100, 0, 0, 100, "m"))
		runHook(t, "Stop", stopPayload("th-c-on", rollout, "the key is "+secret+" ok"))

		joined := ""
		for _, r := range spoolRecords(t, spool) {
			b, _ := json.Marshal(r)
			joined += string(b)
		}
		if strings.Contains(joined, secret) {
			t.Errorf("assistant text reached the spool unredacted:\n%s", joined)
		}
		if !strings.Contains(joined, "OPENBOX_REDACTED") {
			t.Errorf("assistant text was never scanned:\n%s", joined)
		}
		if !strings.Contains(joined, "the key is") {
			t.Errorf("non-secret assistant text did not survive:\n%s", joined)
		}
	})
}

// TestTurn_StopWritesNoStdout: Stop is observe-only, always. `decision:"block"`
// there injects a continuation prompt built from our reason text, which would be
// OpenBox driving the agent. stop_hook_active exists to break exactly that kind
// of loop, and it stays a non-issue only while this holds.
func TestTurn_StopWritesNoStdout(t *testing.T) {
	setHookEnv(t)
	t.Setenv(devconfig.EnvEnforce, "1")
	rollout := writeRollout(t, rolloutLineJSON(100, 0, 0, 100, "m"))
	if stdout, _ := runHook(t, "Stop", stopPayload("th-silent", rollout, "x")); stdout != "" {
		t.Errorf("Stop must never write stdout, even with enforce on; got %q", stdout)
	}
}

// TestTurn_SubagentStopWithoutAgentIDIsSkipped: guessing an agent id would make
// the sidechain share the main thread's cursor and corrupt both.
func TestTurn_SubagentStopWithoutAgentIDIsSkipped(t *testing.T) {
	spool := setHookEnv(t)
	rollout := writeRollout(t, rolloutLineJSON(100, 0, 0, 100, "m"))
	payload := `{"hook_event_name":"SubagentStop","session_id":"th-noagent","cwd":"/r",` +
		`"last_assistant_message":"done","transcript_path":"` + rollout + `"}`
	runHook(t, "SubagentStop", payload)

	if recs := spoolRecords(t, spool); len(recs) != 0 {
		t.Errorf("a SubagentStop with no agent_id must emit nothing; got %d rows", len(recs))
	}
}

// TestTurn_SubagentUsesItsOwnCursor: a sidechain turn lands under its own agent
// namespace and does not move the main thread's cursor.
func TestTurn_SubagentUsesItsOwnCursor(t *testing.T) {
	spool := setHookEnv(t)
	rollout := writeRollout(t, rolloutLineJSON(100, 0, 0, 100, "m"))
	payload := `{"hook_event_name":"SubagentStop","session_id":"th-agent","cwd":"/r","agent_id":"agent-7",` +
		`"agent_type":"general","last_assistant_message":"done","transcript_path":"` + rollout + `"}`
	runHook(t, "SubagentStop", payload)

	var found bool
	for _, r := range spoolRecords(t, spool) {
		if a, _ := r["agent_id"].(string); a == "agent-7" {
			found = true
		}
	}
	if !found {
		t.Error("a subagent turn must carry its agent_id, which is what scopes its activity id")
	}

	// The main thread's cursor must be untouched: its first turn is still index 0.
	before := len(spoolRecords(t, spool))
	runHook(t, "Stop", stopPayload("th-agent", rollout, "main"))
	var sawMain bool
	for _, r := range spoolRecords(t, spool)[before:] {
		if a, _ := r["agent_id"].(string); a != "" {
			continue
		}
		if et, _ := r["event_type"].(string); et != "TurnStarted" {
			continue
		}
		sawMain = true
		if idx, _ := r["turn_index"].(float64); idx != 0 {
			t.Errorf("the subagent moved the main-thread cursor; main turn_index = %v, want 0", idx)
		}
	}
	if !sawMain {
		t.Error("the main thread emitted no turn after the subagent; the case proves nothing")
	}
}

// TestRolloutAllowlistIsExhaustive is the guard the thinking-capture ruling
// traded for. The rollout reader used to be a numbers-only projection, and that
// STRUCTURE was the guarantee that no content escaped. Now that it binds
// reasoning text, the guarantee has to be declarative.
//
// Be precise about what this proves, because an earlier version of this comment
// overclaimed. Two distinct properties:
//
//  1. Nothing binds without being classified: a developer who adds a field to
//     rolloutPayload and forgets to classify it fails here. Checked by reflection
//     over the struct's own tags.
//  2. Every field of every rollout payload shape we have actually OBSERVED is
//     classified. Checked against real captured shapes below.
//
// What it does NOT prove, and what is worth knowing: a brand-new vendor field
// that nobody has declared and nobody has observed cannot fail this test. It is
// also harmless, because json.Unmarshal into a typed struct silently drops
// unknown keys -- so an unclassified field is unreadable, not silently egressed.
// The safety property holds by Go's semantics; this test holds the discipline.
func TestRolloutAllowlistIsExhaustive(t *testing.T) {
	bound := map[string]bool{}
	rt := reflect.TypeOf(rolloutPayload{})
	for i := 0; i < rt.NumField(); i++ {
		tag := rt.Field(i).Tag.Get("json")
		if name := strings.Split(tag, ",")[0]; name != "" && name != "-" {
			bound[name] = true
		}
	}
	for name := range bound {
		if _, classified := rolloutAllowedPayloadFields[name]; !classified {
			t.Errorf("rolloutPayload binds %q but rolloutAllowedPayloadFields does not classify it", name)
		}
	}
	for name, note := range rolloutAllowedPayloadFields {
		isBound := bound[name]
		saysBound := strings.HasPrefix(note, "bound:")
		if isBound != saysBound {
			t.Errorf("field %q: struct says bound=%v but the allowlist says %q", name, isBound, note)
		}
	}
	// The one field that must never be bound, named explicitly so a future edit
	// that adds it has to delete this line and explain itself.
	if bound["encrypted_content"] {
		t.Error("encrypted_content must never be bound: it is an opaque provider blob")
	}

	// Property 2: every key of every payload shape observed on a real rollout is
	// classified. These are the shapes phase 00 captured (keys only, no content).
	observed := []string{
		`{"type":"token_count","info":{"total_token_usage":{},"last_token_usage":{}}}`,
		`{"type":"reasoning","id":"x","summary":[],"encrypted_content":"x","internal_chat_message_metadata_passthrough":{}}`,
		`{"type":"message","role":"assistant","content":[]}`,
		`{"type":"function_call","name":"x","arguments":"x","call_id":"x","status":"x"}`,
		`{"type":"function_call_output","call_id":"x","output":"x"}`,
		`{"model":"x","cwd":"x"}`,
	}
	for _, shape := range observed {
		var m map[string]any
		if err := json.Unmarshal([]byte(shape), &m); err != nil {
			t.Fatalf("bad fixture %q: %v", shape, err)
		}
		for k := range m {
			if _, classified := rolloutAllowedPayloadFields[k]; !classified {
				t.Errorf("observed rollout payload key %q is not classified in "+
					"rolloutAllowedPayloadFields; a person must decide bind-or-ignore", k)
			}
		}
	}
}

// TestThinkingBoundStaysAboveTheWireCap: capBody cuts RUNES while measuring
// bytes, so a byte bound below 4x the rune cap would truncate here, in the wrong
// unit, and the client's own cap would never run.
func TestThinkingBoundStaysAboveTheWireCap(t *testing.T) {
	const wireCapRunes = 65536
	if maxThinkingBytes < 4*wireCapRunes {
		t.Errorf("maxThinkingBytes = %d, want >= %d (4 bytes/rune x the wire cap)",
			maxThinkingBytes, 4*wireCapRunes)
	}
}

// TestRollupIsGatedOnZeroTurns is the owner's double-count ruling, as a test.
func TestRollupIsGatedOnZeroTurns(t *testing.T) {
	t.Run("turns emitted ⇒ no rollup", func(t *testing.T) {
		spool := setHookEnv(t)
		rollout := writeRollout(t, rolloutLineJSON(100, 0, 0, 100, "m"))
		runHook(t, "Stop", stopPayload("th-gate", rollout, "one"))
		runHook(t, "SessionEnd", `{"hook_event_name":"SessionEnd","session_id":"th-gate","cwd":"/r",`+
			`"reason":"other","transcript_path":"`+rollout+`"}`)

		for _, r := range spoolRecords(t, spool) {
			if _, isRollup := r["session_rollup"]; isRollup {
				t.Errorf("a session that emitted per-turn pairs must not also ship the rollup: %v", r)
			}
		}
	})

	t.Run("no turns ⇒ rollup still ships", func(t *testing.T) {
		spool := setHookEnv(t)
		rollout := writeRollout(t, rolloutLineJSON(100, 0, 0, 100, "m"))
		runHook(t, "SessionEnd", `{"hook_event_name":"SessionEnd","session_id":"th-norollup","cwd":"/r",`+
			`"reason":"other","transcript_path":"`+rollout+`"}`)

		var sawRollup bool
		for _, r := range spoolRecords(t, spool) {
			if _, isRollup := r["session_rollup"]; isRollup {
				sawRollup = true
			}
		}
		if !sawRollup {
			t.Error("a session where Stop never fired (crash, kill, unsupported surface) must still get its rollup")
		}
	})
}

// TestTurn_LastLineWithoutTrailingNewlineIsNotRecounted is the regression test
// for a real off-by-one found in review.
//
// The window loop computed each line's end as start+len(line)+1, i.e. it assumed
// every line is newline-terminated. For a rollout whose last line is NOT yet
// terminated -- a writer that has flushed the JSON but not the "\n" -- that end
// is one byte too large, while the cursor is stored as the true len(raw). On the
// next read, once that same line gains its newline, its recomputed end is
// exactly cursor+1, so it tests as "in the window" again: it is counted a second
// time AND never becomes the `prev` baseline. The delta then degrades to the
// whole cumulative total, silently inflating the turn's tokens.
func TestTurn_LastLineWithoutTrailingNewlineIsNotRecounted(t *testing.T) {
	spool := setHookEnv(t)
	path := filepath.Join(t.TempDir(), "rollout.jsonl")

	// Turn 1: one snapshot, deliberately NOT newline-terminated.
	if err := os.WriteFile(path, []byte(rolloutLineJSON(100, 0, 0, 100, "m")), 0o600); err != nil {
		t.Fatalf("write rollout: %v", err)
	}
	runHook(t, "Stop", stopPayload("th-nonl", path, "one"))

	// Turn 2: that line is now terminated, and a second cumulative snapshot lands.
	// Cumulative 100 -> 300, so this turn's delta is 200.
	if err := os.WriteFile(path,
		[]byte(rolloutLineJSON(100, 0, 0, 100, "m")+"\n"+rolloutLineJSON(300, 0, 0, 300, "m")+"\n"), 0o600); err != nil {
		t.Fatalf("rewrite rollout: %v", err)
	}
	runHook(t, "Stop", stopPayload("th-nonl", path, "two"))

	var totals []float64
	for _, r := range spoolRecords(t, spool) {
		if et, _ := r["event_type"].(string); et != "TurnCompleted" {
			continue
		}
		tok, _ := r["tokens"].(map[string]any)
		if tok == nil {
			continue
		}
		tot, _ := tok["total"].(float64)
		totals = append(totals, tot)
	}
	if len(totals) != 2 {
		t.Fatalf("expected two completed turns, got %d (%v)", len(totals), totals)
	}
	if totals[0] != 100 {
		t.Errorf("turn 1 total = %v, want 100", totals[0])
	}
	if totals[1] != 200 {
		t.Errorf("turn 2 total = %v, want 200 (the delta); %v means turn 1's tokens were counted twice",
			totals[1], totals[1])
	}
}

// TestRollup_SubagentOnlySessionDoesNotAlsoShipTheRollup is the regression test
// for the second review finding.
//
// The rollup gate asked only the MAIN thread's cursor whether turns were
// emitted. A subagent's cursor lives under its own key, so a session whose only
// turn activity was a subagent's reported "no turns" and shipped the SessionEnd
// rollup on top of turn rows that had already carried those tokens -- exactly
// the double-count the gate exists to prevent. Reachable whenever a subagent
// runs and the enclosing top-level Stop never fires (crash, kill, or a surface
// that does not run the hook).
func TestRollup_SubagentOnlySessionDoesNotAlsoShipTheRollup(t *testing.T) {
	spool := setHookEnv(t)
	rollout := writeRollout(t, rolloutLineJSON(100, 0, 0, 100, "m"))

	runHook(t, "SubagentStop", `{"hook_event_name":"SubagentStop","session_id":"th-sub-only","cwd":"/r",`+
		`"agent_id":"agent-7","agent_type":"general","last_assistant_message":"done",`+
		`"transcript_path":"`+rollout+`"}`)

	var sawSubagentTurn bool
	for _, r := range spoolRecords(t, spool) {
		if a, _ := r["agent_id"].(string); a == "agent-7" {
			sawSubagentTurn = true
		}
	}
	if !sawSubagentTurn {
		t.Fatal("the subagent turn did not emit; the case proves nothing")
	}

	runHook(t, "SessionEnd", `{"hook_event_name":"SessionEnd","session_id":"th-sub-only","cwd":"/r",`+
		`"reason":"other","transcript_path":"`+rollout+`"}`)

	for _, r := range spoolRecords(t, spool) {
		if _, isRollup := r["session_rollup"]; isRollup {
			t.Errorf("a session whose turns came only from a subagent must not also ship the rollup; "+
				"those tokens are already reported: %v", r)
		}
	}
}
