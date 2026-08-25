package claudecode

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/openbox-ai/openbox-shift-left/adapters/common/hookflow"
	"github.com/openbox-ai/openbox-shift-left/client"
	"github.com/openbox-ai/openbox-shift-left/decision"
)

// A fixed, well-formed Ed25519 seed (matches the client's own test vector) so the
// sentinel test can drive the REAL AIP-signing client and inspect the exact bytes
// that would go on the wire.
const testPrivateKeyB64 = "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="

// poisonedTranscript is a JSONL transcript shaped like a real Claude Code
// transcript (verified against ~/.claude/projects/*.jsonl — including the
// `cache_creation` sub-object and `iterations[]` breakdown, both of which are
// deliberately unbound because summing them alongside the top-level counts would
// double-count): assistant turns carry message.usage; every content-bearing
// location is seeded with a unique SENTINEL_* string. Real usage numbers are
// interleaved. The parser must extract the numbers and NONE of the sentinels.
//
// Expected rollup (contract v1.1 — the four counts are separate, `input` is
// pure): input = 100+10 = 110; output = 30+5 = 35; cache_creation = 50;
// cache_read = 2000; total = 2195; cost = 0.0123 USD.
//
// The sidechain line is the partition case: its 7000/700 must appear in NEITHER
// the main-thread window NOR anywhere on a main-thread turn's wire bytes.
const poisonedTranscript = `{"type":"user","isSidechain":false,"timestamp":"2026-08-11T09:00:00.000Z","message":{"role":"user","content":"SENTINEL_PROMPT top secret prompt"},"cwd":"/x","sessionId":"s1"}
{"type":"assistant","isSidechain":false,"timestamp":"2026-08-11T09:00:01.500Z","message":{"model":"claude-opus-4-8","content":[{"type":"text","text":"SENTINEL_OUTPUT assistant reply"},{"type":"tool_use","name":"Bash","input":{"command":"SENTINEL_CMD dangerous"}}],"usage":{"input_tokens":100,"cache_read_input_tokens":2000,"cache_creation_input_tokens":50,"output_tokens":30,"service_tier":"standard","cache_creation":{"ephemeral_1h_input_tokens":50,"ephemeral_5m_input_tokens":0},"iterations":[{"input_tokens":100,"output_tokens":30}]}},"costUSD":0.0123}
{"type":"user","isSidechain":false,"message":{"role":"user","content":[{"type":"tool_result","content":"SENTINEL_TOOLRESULT captured file body"}]}}
{"type":"assistant","isSidechain":true,"timestamp":"2026-08-11T09:00:02.000Z","message":{"model":"SENTINEL_SIDEMODEL-sonnet","content":[{"type":"text","text":"SENTINEL_SIDETEXT subagent reply"},{"type":"thinking","thinking":"SENTINEL_SIDETHINKING subagent chain of thought"}],"usage":{"input_tokens":7000,"output_tokens":700}}}
{"type":"assistant","isSidechain":false,"timestamp":"2026-08-11T09:00:03.000Z","message":{"content":[{"type":"thinking","thinking":"SENTINEL_THINKING private chain of thought"}],"usage":{"input_tokens":10,"output_tokens":5}}}
{"type":"file-history-snapshot","isSidechain":false,"snapshot":{"content":"SENTINEL_FILE entire file contents"}}
`

// sentinels are the transcript strings that must NEVER egress, at any posture.
// SENTINEL_THINKING is deliberately absent from this list — the ADR-0014
// amendment (ADR-0019 P3) authorised exactly that one field, so it egresses under
// the content gate and has its own two-directional assertions below. Everything
// here stays unreachable whether capture is on or off, which is how the sentinel
// proves the widening is one field wide rather than "content is fine now".
var sentinels = []string{
	"SENTINEL_PROMPT", "SENTINEL_OUTPUT", "SENTINEL_CMD",
	"SENTINEL_TOOLRESULT", "SENTINEL_FILE",
	// The sidechain line's model, text and thinking: a main-thread turn must
	// carry none of them, which proves the partition holds on the wire and not
	// merely in the sums. The thinking one matters most — it is the only
	// content class that egresses, so it is the only one where a broken
	// partition would put a subagent's reasoning on the parent's turn.
	"SENTINEL_SIDEMODEL", "SENTINEL_SIDETEXT", "SENTINEL_SIDETHINKING",
}

// thinkingSentinel is the ONE transcript string whose expected presence depends
// on posture: absent with capture off, present (redacted, capped) with it on.
const thinkingSentinel = "SENTINEL_THINKING"

func writeTranscript(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "transcript.jsonl")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	return p
}

// The SessionEnd rollup sums EVERY usage line — sidechain included — because it
// is the whole-session total, and it is the independent second derivation the
// live reconciliation compares Σ(per-turn) against. Contract v1.1 stopped folding
// the cache counts into Input, so each count is asserted in its own field; the
// old fold-in made this rollup and the per-turn records sum different quantities
// under the same field name.
func TestAggregateUsage_SumsTokensAndCost(t *testing.T) {
	tokens, cost, err := aggregateUsage([]byte(poisonedTranscript))
	if err != nil {
		t.Fatalf("aggregateUsage: %v", err)
	}
	if tokens == nil {
		t.Fatal("tokens nil, want a rollup")
	}
	// 100 + 10 main + 7000 sidechain: the rollup is the whole session.
	if got := *tokens.Input; got != 7110 {
		t.Errorf("Input = %d, want 7110 (pure input, every line)", got)
	}
	if got := *tokens.Output; got != 735 {
		t.Errorf("Output = %d, want 735", got)
	}
	if tokens.CacheCreationInput == nil || *tokens.CacheCreationInput != 50 {
		t.Errorf("CacheCreationInput = %v, want 50 in its own field (not folded into Input)", tokens.CacheCreationInput)
	}
	if tokens.CacheRead == nil || *tokens.CacheRead != 2000 {
		t.Errorf("CacheRead = %v, want 2000 in its own field", tokens.CacheRead)
	}
	// Total is whole throughput, so it is unchanged by the un-folding: a consumer
	// reading only `total` sees the same number the old rollup produced.
	if got := *tokens.Total; got != 7110+735+50+2000 {
		t.Errorf("Total = %d, want %d (input+output+both caches)", got, 7110+735+50+2000)
	}
	if cost == nil {
		t.Fatal("cost nil, want 0.0123 (transcript carried costUSD)")
	}
	if cost.Amount != 0.0123 || cost.Currency != "USD" {
		t.Errorf("cost = %+v, want {0.0123 USD}", *cost)
	}
}

// The main-thread window excludes sidechain lines exactly. This is the
// double-count guard: if a subagent's lines land in the parent transcript, the
// parent must not claim their tokens, because SubagentStop's own record carries
// them.
func TestTurnWindow_PartitionsSidechainOut(t *testing.T) {
	main := aggregateTurnWindow([]byte(poisonedTranscript), false)
	if !main.HasUsage {
		t.Fatal("main window reports no usage")
	}
	if main.Input != 110 || main.Output != 35 {
		t.Errorf("main window = in %d/out %d, want 110/35 (sidechain excluded)", main.Input, main.Output)
	}
	if main.CacheCreationInput != 50 || main.CacheRead != 2000 {
		t.Errorf("main window caches = %d/%d, want 50/2000", main.CacheCreationInput, main.CacheRead)
	}
	if main.Model != "claude-opus-4-8" {
		t.Errorf("main window model = %q, want claude-opus-4-8 (never the sidechain's)", main.Model)
	}

	side := aggregateTurnWindow([]byte(poisonedTranscript), true)
	if !side.HasUsage {
		t.Fatal("sidechain window reports no usage")
	}
	if side.Input != 7000 || side.Output != 700 {
		t.Errorf("sidechain window = in %d/out %d, want 7000/700", side.Input, side.Output)
	}
	// The content class splits with the numbers. The subagent's own record carries
	// its reasoning; the parent's must not — and the wire-level absence asserted
	// in TestFinops_NoContentOnWire is only meaningful if the block is genuinely
	// there to be leaked, which is what this pair proves.
	if !strings.Contains(side.Thinking, "SENTINEL_SIDETHINKING") {
		t.Errorf("the sidechain window did not lift its own thinking (%q) — every "+
			"main-thread assertion about its absence is then vacuous", side.Thinking)
	}
	if strings.Contains(main.Thinking, "SENTINEL_SIDETHINKING") {
		t.Errorf("a main-thread turn carried the subagent's reasoning: %q", main.Thinking)
	}

	// The partitions are exhaustive: together they equal the rollup, field by
	// field. This is the invariant that makes the live reconciliation assertion
	// meaningful — if it did not hold here it could not hold against a stack.
	rollup, _, err := aggregateUsage([]byte(poisonedTranscript))
	if err != nil {
		t.Fatalf("aggregateUsage: %v", err)
	}
	for _, c := range []struct {
		name       string
		part, want int
	}{
		{"input", main.Input + side.Input, *rollup.Input},
		{"output", main.Output + side.Output, *rollup.Output},
		{"cache_creation", main.CacheCreationInput + side.CacheCreationInput, *rollup.CacheCreationInput},
		{"cache_read", main.CacheRead + side.CacheRead, *rollup.CacheRead},
	} {
		if c.part != c.want {
			t.Errorf("Σ(main,sidechain) %s = %d, rollup = %d — the partitions are not exhaustive", c.name, c.part, c.want)
		}
	}
}

// The window's open time is the FIRST parsable line timestamp, and it arrives as
// a time.Time — never as the transcript's string.
func TestTurnWindow_OpenTimeIsFirstParsableTimestamp(t *testing.T) {
	w := aggregateTurnWindow([]byte(poisonedTranscript), false)
	want := time.Date(2026, 8, 11, 9, 0, 0, 0, time.UTC)
	if !w.Open.Equal(want) {
		t.Errorf("window open = %v, want %v (first parsable line)", w.Open, want)
	}

	// No parsable timestamp ⇒ zero, which is the caller's signal to fall back to
	// hook wall time and OMIT duration_ms rather than report a fabricated one.
	noTS := aggregateTurnWindow([]byte(
		`{"type":"assistant","isSidechain":false,"timestamp":"not-a-time","message":{"usage":{"input_tokens":1,"output_tokens":1}}}`+"\n"), false)
	if !noTS.Open.IsZero() {
		t.Errorf("unparsable timestamp yielded open = %v, want zero", noTS.Open)
	}
	if !noTS.HasUsage {
		t.Error("an unparsable timestamp must not discard the line's usage")
	}
}

// Model is last-non-empty WITHIN the window and is never carried across windows
// or back-filled: attributing tokens to a model that may not have spent them is a
// fabricated number.
func TestTurnWindow_ModelIsLastNonEmptyInWindow(t *testing.T) {
	body := `{"isSidechain":false,"message":{"model":"claude-opus-4-8","usage":{"input_tokens":5,"output_tokens":1}}}
{"isSidechain":false,"message":{"usage":{"input_tokens":5,"output_tokens":1}}}
{"isSidechain":false,"message":{"model":"claude-sonnet-4-8","usage":{"input_tokens":5,"output_tokens":1}}}
`
	if got := aggregateTurnWindow([]byte(body), false).Model; got != "claude-sonnet-4-8" {
		t.Errorf("model = %q, want claude-sonnet-4-8 (last non-empty)", got)
	}

	// A window whose lines name no model emits none. The core-side extractor
	// buckets those tokens under `unknown` rather than under a guess.
	noModel := aggregateTurnWindow([]byte(`{"isSidechain":false,"message":{"usage":{"input_tokens":9,"output_tokens":2}}}`+"\n"), false)
	if noModel.Model != "" {
		t.Errorf("model = %q, want empty", noModel.Model)
	}
	if !noModel.HasUsage || noModel.Input != 9 {
		t.Errorf("a model-less window must still report its usage: %+v", noModel)
	}
}

// --- thinking blocks (ADR-0019 P3 / the ADR-0014 amendment) ---

// Thinking is lifted from `message.content[]` blocks of type "thinking", in file
// order, across the whole window — not one block per turn. Everything else in
// that array stays unreachable, which is the width of the amendment.
func TestTurnWindow_LiftsThinkingBlocksInFileOrder(t *testing.T) {
	body := `{"isSidechain":false,"message":{"model":"claude-opus-4-8","content":[{"type":"thinking","thinking":"FIRST thought"},{"type":"text","text":"MUST_NOT_LIFT visible answer"}],"usage":{"input_tokens":5,"output_tokens":1}}}
{"isSidechain":false,"message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"MUST_NOT_LIFT rm -rf"}}],"usage":{"input_tokens":1,"output_tokens":1}}}
{"isSidechain":false,"message":{"content":[{"type":"thinking","thinking":"SECOND thought"}],"usage":{"input_tokens":5,"output_tokens":1}}}
`
	w := aggregateTurnWindow([]byte(body), false)
	if want := "FIRST thought\n\nSECOND thought"; w.Thinking != want {
		t.Errorf("thinking = %q, want %q (both blocks, file order)", w.Thinking, want)
	}
	if strings.Contains(w.Thinking, "MUST_NOT_LIFT") {
		t.Errorf("a sibling content block was lifted: %q", w.Thinking)
	}
	// The numbers are unaffected by the widening.
	if w.Input != 11 || w.Output != 3 {
		t.Errorf("usage arithmetic changed with the projection: %+v", w)
	}
}

// The regression this widening could most easily cause: `message.content` is a
// STRING on user lines and an ARRAY on assistant ones. Binding it as a typed
// array would fail the whole line's unmarshal and silently drop that line's
// token counts — a finops bug with no error anywhere.
func TestTurnWindow_StringMessageContentStillCounts(t *testing.T) {
	body := `{"isSidechain":false,"message":{"role":"user","content":"a plain string body","usage":{"input_tokens":7,"output_tokens":2}}}
{"isSidechain":false,"message":{"content":null,"usage":{"input_tokens":1,"output_tokens":1}}}
{"isSidechain":false,"message":{"content":[{"type":"thinking","thinking":"still lifted"}],"usage":{"input_tokens":2,"output_tokens":0}}}
`
	w := aggregateTurnWindow([]byte(body), false)
	if w.Input != 10 || w.Output != 3 {
		t.Errorf("a string/null message.content dropped a line's usage: %+v", w)
	}
	if w.Thinking != "still lifted" {
		t.Errorf("thinking = %q; a tolerated non-array sibling must not stop the lift", w.Thinking)
	}
	if strings.Contains(w.Thinking, "a plain string body") {
		t.Errorf("string content was lifted as thinking: %q", w.Thinking)
	}
}

// The collection bound must stay STRICTLY LARGER than the wire cap, and this is
// not tidiness — it is what keeps the cap's mutation control alive. If
// maxThinkingBytes were <= the 65,536-rune cap, the collector would truncate
// first and deleting capBody would become undetectable: the sentinel would stay
// green with the cap gone. A future commit that "unifies the two constants" is
// the change this test exists to stop.
func TestThinkingCollectionBoundExceedsTheWireCap(t *testing.T) {
	const wireCapRunes = 65536 // client.maxBodySize, counted in runes by capBody
	// A rune is at most 4 bytes, so the byte budget must cover the widest text
	// the rune cap would have allowed — otherwise the two bounds fight instead
	// of composing, and the collector silently owns the truncation.
	if maxThinkingBytes < 4*wireCapRunes {
		t.Fatalf("maxThinkingBytes = %d, want >= %d (4 bytes/rune x the wire cap): the "+
			"collector must never be the thing that truncates, or the cap's mutation "+
			"control in TestFinops_NoContentOnWire goes vacuous",
			maxThinkingBytes, 4*wireCapRunes)
	}
}

// The streaming reader aggregates a window of ANY size in bounded memory. An
// unbounded concatenation would undo that — the first firing after capture is
// enabled mid-session reads the whole transcript to date as one window. The
// budget is in BYTES and sits above the client's 65,536-RUNE wire cap, so it can
// never drop a byte the wire would have kept.
func TestTurnWindow_ThinkingAccumulationIsBounded(t *testing.T) {
	huge := strings.Repeat("t", maxThinkingBytes*2)
	body := `{"isSidechain":false,"message":{"content":[{"type":"thinking","thinking":"` + huge + `"}],"usage":{"input_tokens":1,"output_tokens":1}}}` + "\n" +
		`{"isSidechain":false,"message":{"content":[{"type":"thinking","thinking":"TAIL_PAST_BUDGET"}],"usage":{"input_tokens":1,"output_tokens":1}}}` + "\n"

	w := aggregateTurnWindow([]byte(body), false)
	if len(w.Thinking) > maxThinkingBytes {
		t.Errorf("thinking held %d bytes, want <= %d", len(w.Thinking), maxThinkingBytes)
	}
	if strings.Contains(w.Thinking, "TAIL_PAST_BUDGET") {
		t.Error("a block past the budget was appended anyway")
	}
	if !w.HasUsage || w.Input != 2 {
		t.Errorf("bounding the text must not drop the numbers: %+v", w)
	}
	// The cut must not leave invalid UTF-8 behind: json.Marshal would silently
	// substitute U+FFFD, so a mid-rune truncation corrupts the body rather than
	// shortening it.
	if !utf8.ValidString(w.Thinking) {
		t.Error("bounded thinking is not valid UTF-8")
	}
}

// A multi-byte rune straddling the budget boundary is dropped whole, not split.
func TestTurnWindow_ThinkingBoundaryDoesNotSplitARune(t *testing.T) {
	// Fill to one byte short of the budget, then a 3-byte rune: it cannot fit,
	// and a byte-slice cut would leave one third of it behind.
	fill := strings.Repeat("t", maxThinkingBytes-1)
	body := `{"isSidechain":false,"message":{"content":[{"type":"thinking","thinking":"` + fill + `世"}],"usage":{"input_tokens":1,"output_tokens":1}}}` + "\n"

	w := aggregateTurnWindow([]byte(body), false)
	if !utf8.ValidString(w.Thinking) {
		t.Errorf("a rune was split at the budget boundary: last bytes %q", w.Thinking[max(0, len(w.Thinking)-4):])
	}
	if strings.Contains(w.Thinking, "世") {
		t.Error("a rune that does not fit the budget was kept")
	}
}

// The partition is what stops a subagent's turn being counted twice. It must
// hold for thinking exactly as it holds for the numbers — otherwise a main-thread
// turn would carry a subagent's reasoning.
func TestTurnWindow_ThinkingRespectsSidechainPartition(t *testing.T) {
	body := `{"isSidechain":false,"message":{"content":[{"type":"thinking","thinking":"MAIN reasoning"}],"usage":{"input_tokens":1,"output_tokens":1}}}
{"isSidechain":true,"message":{"content":[{"type":"thinking","thinking":"SUBAGENT reasoning"}],"usage":{"input_tokens":9,"output_tokens":9}}}
`
	main := aggregateTurnWindow([]byte(body), false)
	if main.Thinking != "MAIN reasoning" {
		t.Errorf("main-thread thinking = %q, want only its own", main.Thinking)
	}
	sub := aggregateTurnWindow([]byte(body), true)
	if sub.Thinking != "SUBAGENT reasoning" {
		t.Errorf("subagent thinking = %q, want only its own", sub.Thinking)
	}
}

// Two successive reads over a GROWING transcript must yield disjoint windows:
// this is the property that stops an N-turn session reporting O(N²) tokens.
func TestReadTurnUsage_SuccessiveWindowsAreDisjoint(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "transcript.jsonl")
	line := func(in, out int) string {
		return `{"isSidechain":false,"timestamp":"2026-08-11T09:00:00Z","message":{"model":"m1","usage":{"input_tokens":` +
			strconv.Itoa(in) + `,"output_tokens":` + strconv.Itoa(out) + `}}}` + "\n"
	}
	if err := os.WriteFile(path, []byte(line(10, 1)+line(20, 2)), 0o600); err != nil {
		t.Fatal(err)
	}

	w1, pos1, err := readTurnUsage(path, hookflow.TurnPos{}, false)
	if err != nil {
		t.Fatalf("first read: %v", err)
	}
	if w1.Input != 30 || w1.Output != 3 {
		t.Errorf("first window = %d/%d, want 30/3", w1.Input, w1.Output)
	}

	// The transcript grows; the second read must see ONLY the new lines.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(line(5, 7)); err != nil {
		t.Fatal(err)
	}
	f.Close()

	w2, pos2, err := readTurnUsage(path, pos1, false)
	if err != nil {
		t.Fatalf("second read: %v", err)
	}
	if w2.Input != 5 || w2.Output != 7 {
		t.Errorf("second window = %d/%d, want 5/7 — the cursor did not isolate the window", w2.Input, w2.Output)
	}
	if pos2.Offset <= pos1.Offset {
		t.Errorf("cursor did not advance: %d → %d", pos1.Offset, pos2.Offset)
	}

	// A third read with nothing new reports nothing at all — idempotent locally,
	// so a Stop that fires twice does not emit a second empty pair.
	w3, pos3, err := readTurnUsage(path, pos2, false)
	if err != nil {
		t.Fatalf("third read: %v", err)
	}
	if w3.HasUsage {
		t.Errorf("third window reported usage %+v, want none", w3)
	}
	if pos3 != pos2 {
		t.Errorf("cursor moved with nothing to read: %+v → %+v", pos2, pos3)
	}
}

// A partially-written final line must be left for the next read, never parsed
// half-way and never skipped: the cursor advances only over complete lines.
func TestReadTurnUsage_PartialFinalLineIsNotConsumed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "transcript.jsonl")
	complete := `{"isSidechain":false,"message":{"model":"m1","usage":{"input_tokens":10,"output_tokens":1}}}` + "\n"
	partial := `{"isSidechain":false,"message":{"model":"m1","usage":{"input_tokens":99`
	if err := os.WriteFile(path, []byte(complete+partial), 0o600); err != nil {
		t.Fatal(err)
	}

	w, pos, err := readTurnUsage(path, hookflow.TurnPos{}, false)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if w.Input != 10 {
		t.Errorf("window input = %d, want 10 (the partial line must not contribute)", w.Input)
	}
	if pos.Offset != int64(len(complete)) {
		t.Errorf("cursor = %d, want %d (must stop at the last newline)", pos.Offset, len(complete))
	}

	// Once the line is completed, the next read picks it up whole.
	if err := os.WriteFile(path, []byte(complete+partial+`,"output_tokens":3}}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	w2, _, err := readTurnUsage(path, pos, false)
	if err != nil {
		t.Fatalf("second read: %v", err)
	}
	if w2.Input != 99 || w2.Output != 3 {
		t.Errorf("completed line read as %d/%d, want 99/3", w2.Input, w2.Output)
	}
}

// A window LARGER than one read chunk must be aggregated whole, in one window —
// not truncated at the chunk boundary with the remainder folded into the next
// turn. Cross-chunk truncation would conserve total tokens while making two turns
// carry each other's numbers, and per-turn attribution is the whole point.
//
// This is not a hypothetical size. A cursor that has never been written reads from
// offset 0, so the first firing after usage capture is enabled mid-session sees
// the entire transcript to date as one window.
func TestReadTurnUsage_WindowSpanningManyChunksIsAggregatedWhole(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "transcript.jsonl")

	// Enough lines to cross turnChunkBytes several times, each padded so the byte
	// count is what forces the chunking rather than the line count.
	pad := strings.Repeat("x", 4096)
	const lines = 4000
	var b strings.Builder
	for i := 0; i < lines; i++ {
		model := "claude-opus-4-8"
		if i == lines-1 {
			model = "claude-sonnet-4-8" // last non-empty must win ACROSS chunks
		}
		b.WriteString(`{"isSidechain":false,"timestamp":"2026-08-11T09:00:0` +
			strconv.Itoa(i%10) + `.000Z","pad":"` + pad + `","message":{"model":"` + model +
			`","usage":{"input_tokens":1,"output_tokens":2,"cache_read_input_tokens":3,"cache_creation_input_tokens":4}}}` + "\n")
	}
	body := b.String()
	if len(body) <= turnChunkBytes {
		t.Fatalf("test body is %d bytes, needs to exceed turnChunkBytes (%d) to exercise chunking", len(body), turnChunkBytes)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	w, pos, err := readTurnUsage(path, hookflow.TurnPos{}, false)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if w.Input != lines || w.Output != 2*lines || w.CacheRead != 3*lines || w.CacheCreationInput != 4*lines {
		t.Errorf("window = %d/%d/%d/%d, want %d/%d/%d/%d — the window was split at a chunk boundary",
			w.Input, w.Output, w.CacheRead, w.CacheCreationInput, lines, 2*lines, 3*lines, 4*lines)
	}
	// The whole window was consumed: the cursor is at EOF, so the next firing sees
	// nothing rather than the tail of this turn.
	if pos.Offset != int64(len(body)) {
		t.Errorf("cursor = %d, want %d (EOF) — part of the window was left unread", pos.Offset, len(body))
	}
	// Order-dependent aggregation survives chunking.
	if w.Model != "claude-sonnet-4-8" {
		t.Errorf("model = %q, want the LAST non-empty across chunks", w.Model)
	}
	if w.Open.IsZero() || !w.Open.Equal(time.Date(2026, 8, 11, 9, 0, 0, 0, time.UTC)) {
		t.Errorf("open = %v, want the FIRST line's timestamp across chunks", w.Open)
	}

	// And a second firing over the unchanged file reports nothing.
	w2, pos2, err := readTurnUsage(path, pos, false)
	if err != nil {
		t.Fatalf("second read: %v", err)
	}
	if w2.HasUsage {
		t.Errorf("second read reported usage %+v; the first should have consumed the whole window", w2)
	}
	if pos2 != pos {
		t.Errorf("cursor moved with nothing to read: %+v → %+v", pos, pos2)
	}
}

// A line boundary that falls INSIDE a chunk read must not split the line: the
// carry-over is what keeps a record whole across two reads.
func TestReadTurnUsage_LineStraddlingAChunkBoundaryIsIntact(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "transcript.jsonl")

	// One big line that pushes past the chunk size, then a small line whose numbers
	// would be lost if the carry-over dropped the fragment.
	pad := strings.Repeat("y", turnChunkBytes+1024)
	body := `{"isSidechain":false,"pad":"` + pad + `","message":{"model":"m1","usage":{"input_tokens":11,"output_tokens":0}}}` + "\n" +
		`{"isSidechain":false,"message":{"model":"m2","usage":{"input_tokens":7,"output_tokens":5}}}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	w, pos, err := readTurnUsage(path, hookflow.TurnPos{}, false)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if w.Input != 18 || w.Output != 5 {
		t.Errorf("window = %d/%d, want 18/5 — a line straddling the chunk boundary was lost or split", w.Input, w.Output)
	}
	if w.Model != "m2" {
		t.Errorf("model = %q, want m2", w.Model)
	}
	if pos.Offset != int64(len(body)) {
		t.Errorf("cursor = %d, want %d (EOF)", pos.Offset, len(body))
	}
}

// A single line with no newline that grows past the whole-transcript cap is the
// one thing streaming cannot absorb. It must error rather than buffer without
// bound, and it must NOT advance the cursor — a later firing retries once the
// line is terminated.
func TestReadTurnUsage_UnboundedSingleLineIsRefused(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "transcript.jsonl")
	// No trailing newline anywhere: the reader can never consume a complete line.
	if err := os.WriteFile(path, []byte(strings.Repeat("z", maxTranscriptBytes+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	w, pos, err := readTurnUsage(path, hookflow.TurnPos{Offset: 0, Index: 2}, false)
	if err == nil {
		t.Error("a newline-less line past the cap must error rather than buffer without bound")
	}
	if w.HasUsage {
		t.Errorf("window reported usage %+v", w)
	}
	if pos.Offset != 0 || pos.Index != 2 {
		t.Errorf("cursor advanced to %+v on a refused read; it must stay put", pos)
	}
}

// A transcript that shrank below the cursor was truncated or rotated. Re-read
// from the start: the turn ids are re-minted deterministically and the server
// dedupes them, whereas seeking past EOF would report nothing forever.
func TestReadTurnUsage_TruncatedTranscriptRereadsFromStart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "transcript.jsonl")
	body := `{"isSidechain":false,"message":{"model":"m1","usage":{"input_tokens":4,"output_tokens":2}}}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	w, _, err := readTurnUsage(path, hookflow.TurnPos{Offset: 999999, Index: 7}, false)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if w.Input != 4 {
		t.Errorf("window input = %d, want 4 (re-read from zero after truncation)", w.Input)
	}
}

func TestReadTurnUsage_MissingPathIsAnError(t *testing.T) {
	if _, _, err := readTurnUsage("", hookflow.TurnPos{}, false); err == nil {
		t.Error("empty transcript_path should error (best-effort skip)")
	}
	if _, _, err := readTurnUsage(filepath.Join(t.TempDir(), "nope.jsonl"), hookflow.TurnPos{}, false); err == nil {
		t.Error("missing transcript should error")
	}
	// A directory is not a regular file: must error rather than be read.
	if _, _, err := readTurnUsage(t.TempDir(), hookflow.TurnPos{}, false); err == nil {
		t.Error("a directory should error")
	}
}

// Malformed lines are skipped, never fatal, and never stop the aggregation.
func TestTurnWindow_MalformedLinesSkipped(t *testing.T) {
	body := `{"isSidechain":false,"message":{"model":"m1","usage":{"input_tokens":10,"output_tokens":1}}}
not json at all
{"isSidechain":false,"message":{"usage":"a string where an object belongs"}}
{"isSidechain":false,"message":{"model":"m1","usage":{"input_tokens":5,"output_tokens":1}}}
`
	w := aggregateTurnWindow([]byte(body), false)
	if w.Input != 15 || w.Output != 2 {
		t.Errorf("window = %d/%d, want 15/2 (bad lines skipped, good ones kept)", w.Input, w.Output)
	}
}

// Negative source counts are clamped so emitted numbers always satisfy the
// schema's `minimum: 0`.
func TestTurnWindow_NegativeCountsClamped(t *testing.T) {
	body := `{"isSidechain":false,"message":{"model":"m1","usage":{"input_tokens":-5,"output_tokens":-1,"cache_read_input_tokens":-9,"cache_creation_input_tokens":-2}}}` + "\n"
	w := aggregateTurnWindow([]byte(body), false)
	if w.Input != 0 || w.Output != 0 || w.CacheRead != 0 || w.CacheCreationInput != 0 {
		t.Errorf("negative counts not clamped: %+v", w)
	}
	tok := w.tokens()
	if tok == nil || *tok.Total != 0 {
		t.Errorf("tokens = %+v, want an all-zero rollup", tok)
	}
}

// TestFinops_NoContentOnWire is the LOAD-BEARING INV-2 test, and ADR-0014
// NARROWED what it proves. It used to assert "no content anywhere", which the
// projection guaranteed structurally: the structs held only numbers, so content
// had nowhere to land. The projection now binds three non-numeric fields, so the
// claim this test must prove is the exact allowlist:
//
//	sentinel content        → ABSENT from the signed wire body (4 field classes)
//	message.model           → PRESENT (the one string authorised to egress)
//	the raw timestamp string→ ABSENT (bound, parsed to a time, discarded)
//	sidechain sums + model  → ABSENT from a main-thread turn (the partition)
//
// It drives the real AIP-signing client with content-capture ON — the adversarial
// worst case, because the client's stripper is then disabled and only the
// projection can be what keeps content off the wire.
//
// A change that makes this test pass trivially is a defect: the guarantee no
// longer defends itself structurally, so this is the thing defending it.
func TestFinops_NoContentOnWire(t *testing.T) {
	path := writeTranscript(t, poisonedTranscript)

	var bodies [][]byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, b)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	cl, err := client.New(client.Config{
		BaseURL:               srv.URL,
		APIKey:                "obx_test_0123456789abcdef0123456789abcdef0123456789abcdef",
		DID:                   testDID,
		PrivateKeyB64:         testPrivateKeyB64,
		ContentCaptureEnabled: true, // adversarial: stripper OFF
	})
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}

	m := NewMapper(Identity{DeveloperDID: testDID})
	m.NewID = func() string { return "evt-1" }

	// --- the SessionEnd rollup path ---
	tokens, cost, err := readTranscriptUsage(path)
	if err != nil {
		t.Fatalf("readTranscriptUsage: %v", err)
	}
	m.Finops = &FinopsUsage{Tokens: tokens, Cost: cost}
	sessionEnd, ok := m.Map(HookSessionEnd, &HookEvent{SessionID: "s1", TranscriptPath: path, Reason: "other"})
	if !ok {
		t.Fatal("Map(SessionEnd) not ok")
	}
	wantTotal := 7110 + 735 + 50 + 2000
	if sessionEnd.Tokens == nil || *sessionEnd.Tokens.Total != wantTotal {
		t.Fatalf("SessionEnded event missing token rollup: %+v", sessionEnd.Tokens)
	}

	// --- the per-turn path (a main-thread Stop) ---
	window, _, err := readTurnUsage(path, hookflow.TurnPos{}, false)
	if err != nil {
		t.Fatalf("readTurnUsage: %v", err)
	}
	turnStarted, turnCompleted, ok := m.MapTurn(&HookEvent{SessionID: "s1", TranscriptPath: path}, window, 0)
	if !ok {
		t.Fatal("MapTurn not ok")
	}

	events := []struct {
		name string
		ev   client.DevEvent
	}{
		{"SessionEnded", sessionEnd},
		{"TurnStarted", turnStarted},
		{"TurnCompleted", turnCompleted},
	}

	// (a) No sentinel in any normalized event. The thinking sentinel is asserted
	// alongside them here: this mapper never set CaptureContent, so with capture
	// OFF the amended allowlist must egress exactly as much as the old one did.
	for _, e := range events {
		evJSON, _ := json.Marshal(e.ev)
		for _, s := range append(append([]string{}, sentinels...), thinkingSentinel) {
			if strings.Contains(string(evJSON), s) {
				t.Fatalf("INV-2 breach: sentinel %q present in %s event: %s", s, e.name, evJSON)
			}
		}
	}

	// (b) No sentinel in the exact SIGNED WIRE BODY of any of them.
	for _, e := range events {
		if _, err := cl.Emit(context.Background(), e.ev); err != nil {
			t.Fatalf("Emit(%s): %v", e.name, err)
		}
	}
	if len(bodies) != len(events) {
		t.Fatalf("captured %d request bodies, want %d", len(bodies), len(events))
	}
	for i, body := range bodies {
		if len(body) == 0 {
			t.Fatalf("%s: empty request body", events[i].name)
		}
		for _, s := range append(append([]string{}, sentinels...), thinkingSentinel) {
			if strings.Contains(string(body), s) {
				t.Fatalf("INV-2 breach: sentinel %q on the wire for %s: %s", s, events[i].name, body)
			}
		}
		// The raw transcript timestamp string is BOUND by the projection now, so
		// its absence is no longer structural and must be asserted. Every
		// transcript line's timestamp ends in the millis+Z form below; the
		// timestamps that legitimately appear on the wire are the mapper's own
		// RFC3339Nano stamps, which never carry these exact values.
		for _, ts := range []string{"2026-08-11T09:00:00.000Z", "2026-08-11T09:00:01.500Z", "2026-08-11T09:00:03.000Z"} {
			if strings.Contains(string(body), ts) {
				t.Fatalf("INV-2 breach: raw transcript timestamp %q on the wire for %s: %s", ts, events[i].name, body)
			}
		}
	}

	// (c) The ONE authorised string DID reach the wire — otherwise the allowlist
	// costs its privacy narrowing and buys nothing.
	completedBody := string(bodies[2])
	if !strings.Contains(completedBody, `"model":"claude-opus-4-8"`) {
		t.Errorf("the model id did not reach the wire: %s", completedBody)
	}

	// (d) The numbers reached the wire, in their own fields, and the sidechain's
	// did not — the partition holds on the wire, not just in the sums.
	for _, want := range []string{
		`"input_tokens":110`, `"output_tokens":35`,
		`"cache_creation_input_tokens":50`, `"cache_read_input_tokens":2000`,
	} {
		if !strings.Contains(completedBody, want) {
			t.Errorf("expected %s in the turn's activity_output: %s", want, completedBody)
		}
	}
	for _, forbidden := range []string{`"input_tokens":7000`, `"output_tokens":700`, "7110"} {
		if strings.Contains(completedBody, forbidden) {
			t.Errorf("a main-thread turn carried sidechain numbers (%s): %s", forbidden, completedBody)
		}
	}

	// (e) Sanity on the rollup body: the session total is there.
	if !strings.Contains(string(bodies[0]), strconv.Itoa(wantTotal)) {
		t.Errorf("expected token total %d on the SessionEnded wire body, got: %s", wantTotal, bodies[0])
	}

	// (f) Capture ON — the direction the ADR-0014 amendment changed, and the half
	// where every remaining guarantee lives. Without this section the strongest
	// failure modes are untested and the test passes trivially for the change
	// that introduced them.
	//
	// The adversarial setup: content capture ON, the same poisoned transcript, a
	// hook payload carrying its own assistant message. Three claims must hold at
	// once — the assistant text still comes from the HOOK FIELD (ADR-0018), the
	// ONE amended transcript field egresses, and every OTHER transcript sentinel
	// stays as unreachable as it was.
	const hookMessage = "the hook-supplied assistant answer"
	capturing := NewMapper(Identity{DeveloperDID: testDID})
	capturing.NewID = func() string { return "evt-2" }
	capturing.CaptureContent = true
	_, capturedTurn, ok := capturing.MapTurn(&HookEvent{
		SessionID: "s1", TranscriptPath: path, LastAssistantMessage: hookMessage,
	}, window, 0)
	if !ok {
		t.Fatal("MapTurn(capture on) not ok")
	}
	bodies = nil
	if _, err := cl.Emit(context.Background(), capturedTurn); err != nil {
		t.Fatalf("Emit(captured turn): %v", err)
	}
	if len(bodies) != 1 {
		t.Fatalf("captured %d bodies for the content turn, want 1", len(bodies))
	}
	capturedBody := string(bodies[0])
	for _, s := range sentinels {
		if strings.Contains(capturedBody, s) {
			t.Fatalf("INV-2 breach: transcript sentinel %q reached the wire on a "+
				"content-capturing turn — the ADR-0014 amendment authorised thinking and "+
				"NOTHING else, so the projection must still be unable to see this: %s",
				s, capturedBody)
		}
	}
	// The amended field DID egress, in the place the amendment named. Asserted on
	// the decoded activity_output rather than by substring, so a thinking block
	// that leaked through some other field would fail rather than pass.
	if got := activityOutputThinking(t, capturedBody); !strings.Contains(got, thinkingSentinel) {
		t.Fatalf("activity_output.thinking = %q, want the transcript's thinking block — "+
			"the amendment costs a privacy narrowing and must buy the field: %s",
			got, capturedBody)
	}
	// And the hook-supplied text DID reach the wire, so the absence above is a
	// real result and not "content capture quietly did nothing".
	if !strings.Contains(capturedBody, hookMessage) {
		t.Fatalf("the hook-supplied assistant message did not reach the wire; every "+
			"assertion in this section would then be vacuous: %s", capturedBody)
	}
	// Thinking must NOT have also landed on the assistant span. That span exists
	// for one reader — core's alignment extractor — which takes its
	// choices[0].message.content as the assistant's REPLY and scores every later
	// turn's drift against it. Chain-of-thought there corrupts the reader
	// silently: core logs nothing when the text is merely the wrong text.
	//
	// Without this assertion an implementation that ALSO concatenated thinking
	// into Content.Output would satisfy every other check in this section, which
	// is one sloppy merge away.
	span := assistantSpanBody(t, capturedBody)
	if !strings.Contains(span, hookMessage) {
		t.Errorf("the assistant span lost its own text: %q", span)
	}
	if strings.Contains(span, thinkingSentinel) {
		t.Errorf("thinking rode the assistant span, where core reads it as the turn's "+
			"REPLY — it belongs in activity_output.thinking and nowhere else: %q", span)
	}

	// (g) The two mutation controls. Thinking is the densest content this client
	// captures — it restates prompts, file bodies, and any credential the turn
	// saw — so the gate alone is not the guarantee. Deleting the redaction, or
	// deleting the cap, must each turn this test red.
	t.Run("thinking is redacted before attachment and capped before signing", func(t *testing.T) {
		// Assembled at runtime for the same reason C34 does it: a repo-side secret
		// scanner rewriting a whole key in source would silently turn this into an
		// assertion about a placeholder.
		awsKey := "AKIA" + "IOSFODNN7EXAMPLE"
		const tail = "TAIL_PAST_THE_CAP"
		poisoned := `{"isSidechain":false,"timestamp":"2026-08-11T09:00:01.000Z","message":` +
			`{"model":"claude-opus-4-8","content":[{"type":"thinking","thinking":"` +
			`reasoning about AWS_ACCESS_KEY_ID=` + awsKey + ` then ` +
			strings.Repeat("z", 70000) + tail + `"}],` +
			`"usage":{"input_tokens":1,"output_tokens":1}}}` + "\n"
		capPath := writeTranscript(t, poisoned)

		var got [][]byte
		capSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			got = append(got, b)
			_, _ = w.Write([]byte(`{}`))
		}))
		defer capSrv.Close()
		capClient, err := client.New(client.Config{
			BaseURL:               capSrv.URL,
			APIKey:                "obx_test_0123456789abcdef0123456789abcdef0123456789abcdef",
			DID:                   testDID,
			PrivateKeyB64:         testPrivateKeyB64,
			ContentCaptureEnabled: true,
		})
		if err != nil {
			t.Fatalf("client.New: %v", err)
		}

		w, _, err := readTurnUsage(capPath, hookflow.TurnPos{}, false)
		if err != nil {
			t.Fatalf("readTurnUsage: %v", err)
		}
		red := NewMapper(Identity{DeveloperDID: testDID})
		red.NewID = func() string { return "evt-3" }
		red.CaptureContent = true
		// The real detector, wired exactly as RunHook wires it — a test-local stub
		// would assert the plumbing against itself.
		red.RedactContent = func(s string) string {
			return hookflow.RedactText(decision.NewRedactor(), s)
		}
		_, completed, ok := red.MapTurn(&HookEvent{SessionID: "s1", TranscriptPath: capPath}, w, 0)
		if !ok {
			t.Fatal("MapTurn not ok")
		}
		if _, err := capClient.Emit(context.Background(), completed); err != nil {
			t.Fatalf("Emit: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("captured %d bodies, want 1", len(got))
		}
		body := string(got[0])

		// Redaction, on the outbound bytes. Removing the redact call ships the key.
		if strings.Contains(body, awsKey) {
			t.Errorf("the raw credential reached the wire inside a thinking block — "+
				"redaction must run BEFORE attachment: %s", body)
		}
		thinking := activityOutputThinking(t, body)
		if !strings.Contains(thinking, "OPENBOX_REDACTED") {
			t.Errorf("no redaction placeholder in activity_output.thinking: %q", thinking)
		}
		// Cap, on the outbound bytes. Removing capBody ships the tail.
		if strings.Contains(body, tail) {
			t.Errorf("thinking past the 65,536-rune cap reached the wire: the cap must " +
				"bound the signed bytes, not the struct")
		}
		if n := len([]rune(thinking)); n > 65536 {
			t.Errorf("activity_output.thinking is %d runes, want <= 65536", n)
		}
		if thinking == "" {
			t.Error("a capped thinking block must still be sent, not dropped")
		}
	})
}

// assistantSpanBody decodes the one span's response_body out of a wire body —
// the field core's alignment extractor reads as the assistant's reply.
func assistantSpanBody(t *testing.T, body string) string {
	t.Helper()
	var p struct {
		Spans []struct {
			ResponseBody string `json:"response_body"`
		} `json:"spans"`
	}
	if err := json.Unmarshal([]byte(body), &p); err != nil {
		t.Fatalf("decode wire body: %v (%s)", err, body)
	}
	if len(p.Spans) == 0 {
		return ""
	}
	return p.Spans[len(p.Spans)-1].ResponseBody // the extractor reads the LAST entry
}

// activityOutputThinking decodes activity_output.thinking out of a wire body. It
// reads the field the amendment named rather than substring-matching the whole
// payload, so thinking arriving anywhere ELSE fails instead of passing.
func activityOutputThinking(t *testing.T, body string) string {
	t.Helper()
	var p struct {
		ActivityOutput struct {
			Thinking string `json:"thinking"`
		} `json:"activity_output"`
	}
	if err := json.Unmarshal([]byte(body), &p); err != nil {
		t.Fatalf("decode wire body: %v (%s)", err, body)
	}
	return p.ActivityOutput.Thinking
}

// Cost must never be derived. The transcript is the only source; a transcript
// without costUSD yields nil, and no pricing table is ever consulted.
func TestTurnUsage_CostIsNeverDerived(t *testing.T) {
	m := NewMapper(Identity{DeveloperDID: testDID})
	m.NewID = func() string { return "evt-1" }
	w := aggregateTurnWindow([]byte(poisonedTranscript), false)
	_, completed, ok := m.MapTurn(&HookEvent{SessionID: "s1"}, w, 0)
	if !ok {
		t.Fatal("MapTurn not ok")
	}
	if completed.Cost != nil {
		t.Errorf("a turn event carried cost %+v; cost is derived server-side, never here", completed.Cost)
	}
}

// TestFinops_OffByteIdentical: with no finops usage attached (flag off), the
// SessionEnded event carries no tokens/cost — byte-identical to pre-SL-16 output.
func TestFinops_OffByteIdentical(t *testing.T) {
	m := NewMapper(Identity{DeveloperDID: testDID})
	m.NewID = func() string { return "evt-1" }
	m.Now = func() time.Time { return time.Unix(0, 0).UTC() }
	// m.Finops stays nil (the default) — mirrors ResolveFinops()==false.
	ev, _ := m.Map(HookSessionEnd, &HookEvent{SessionID: "s1", TranscriptPath: "/should/not/matter", Reason: "other"})
	if ev.Tokens != nil || ev.Cost != nil {
		t.Fatalf("finops off must attach nothing, got tokens=%v cost=%v", ev.Tokens, ev.Cost)
	}
}

// TestFinops_AttachesOnlyOnSessionEnd: even with usage present, non-SessionEnd
// events never carry tokens/cost (the rollup is a session-terminal fact).
func TestFinops_AttachesOnlyOnSessionEnd(t *testing.T) {
	m := NewMapper(Identity{DeveloperDID: testDID})
	m.NewID = func() string { return "id" }
	m.Finops = &FinopsUsage{Tokens: &client.Tokens{Total: intPtr(99)}}
	for _, h := range []HookName{HookSessionStart, HookUserPromptSubmit, HookPreToolUse, HookPostToolUse} {
		ev, ok := m.Map(h, &HookEvent{SessionID: "s1", ToolName: "Bash"})
		if ok && ev.Tokens != nil {
			t.Errorf("%s carried tokens; finops must attach only on SessionEnd", h)
		}
	}
	ev, _ := m.Map(HookSessionEnd, &HookEvent{SessionID: "s1", Reason: "other"})
	if ev.Tokens == nil || *ev.Tokens.Total != 99 {
		t.Errorf("SessionEnd should carry the rollup, got %v", ev.Tokens)
	}
}

func TestAggregateUsage_MalformedLinesSkipped(t *testing.T) {
	// A partial/garbage line and a bare marker are skipped; the good usage line
	// still aggregates (fault-tolerant, INV-3).
	body := "not json at all\n" +
		`{"type":"assistant","message":{"usage":{"input_tokens":7,"output_tokens":3}}}` + "\n" +
		`{"type":"assistant","message":{"usage":` // truncated final line
	tokens, _, err := aggregateUsage([]byte(body))
	if err != nil {
		t.Fatalf("aggregateUsage: %v", err)
	}
	if tokens == nil || *tokens.Input != 7 || *tokens.Output != 3 {
		t.Fatalf("want input=7 output=3 from the one good line, got %+v", tokens)
	}
}

func TestAggregateUsage_EmptySessionNoError(t *testing.T) {
	// A transcript with no usage at all is valid: nil rollup, no error (the caller
	// then emits no tokens/cost, same as finops-off).
	body := `{"type":"user","message":{"role":"user","content":"hi"}}` + "\n" +
		`{"type":"system","subtype":"init"}` + "\n"
	tokens, cost, err := aggregateUsage([]byte(body))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tokens != nil || cost != nil {
		t.Fatalf("empty session must yield nil rollup, got tokens=%v cost=%v", tokens, cost)
	}
}

func TestAggregateUsage_CostAbsentByDefault(t *testing.T) {
	// Real current-CC transcripts carry NO cost field; Cost must stay nil (absent
	// when unknown) rather than be fabricated.
	body := `{"type":"assistant","message":{"usage":{"input_tokens":5,"output_tokens":2}}}` + "\n"
	tokens, cost, err := aggregateUsage([]byte(body))
	if err != nil {
		t.Fatalf("aggregateUsage: %v", err)
	}
	if tokens == nil {
		t.Fatal("tokens nil")
	}
	if cost != nil {
		t.Fatalf("cost must be nil when the transcript carries none, got %+v", cost)
	}
}

func TestAggregateUsage_NegativeClampedToZero(t *testing.T) {
	// A malformed/negative source value must not produce a number violating the
	// SL-1 schema `minimum: 0` (SEC-16-2).
	body := `{"type":"assistant","message":{"usage":{"input_tokens":-5,"cache_read_input_tokens":-1,"output_tokens":-3}}}` + "\n" +
		`{"type":"assistant","message":{"usage":{"input_tokens":10,"output_tokens":4}}}` + "\n"
	tokens, _, err := aggregateUsage([]byte(body))
	if err != nil {
		t.Fatalf("aggregateUsage: %v", err)
	}
	if *tokens.Input != 10 || *tokens.Output != 4 || *tokens.Total != 14 {
		t.Fatalf("negatives must clamp to 0, got input=%d output=%d total=%d",
			*tokens.Input, *tokens.Output, *tokens.Total)
	}
}

func TestReadTranscriptUsage_Errors(t *testing.T) {
	if _, _, err := readTranscriptUsage(""); err == nil {
		t.Error("empty path should error (skipped)")
	}
	if _, _, err := readTranscriptUsage(filepath.Join(t.TempDir(), "nope.jsonl")); err == nil {
		t.Error("missing file should error (skipped)")
	}
	// A directory is not a regular file.
	if _, _, err := readTranscriptUsage(t.TempDir()); err == nil {
		t.Error("non-regular file should error (skipped)")
	}
}

func TestReadTranscriptUsage_OversizedSkipped(t *testing.T) {
	// A sparse file larger than the cap is skipped whole (INV-3, bounded read) —
	// never read into memory, never partially counted.
	p := filepath.Join(t.TempDir(), "huge.jsonl")
	f, err := os.Create(p)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := f.Truncate(maxTranscriptBytes + 1); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	_ = f.Close()
	if _, _, err := readTranscriptUsage(p); err == nil {
		t.Error("oversized transcript should be skipped with an error")
	}
}
