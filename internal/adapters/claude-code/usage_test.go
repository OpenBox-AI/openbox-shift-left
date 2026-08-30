package claudecode

import (
	"context"
	"encoding/json"
	"io"
	"net/http"

	"github.com/openbox-ai/openbox-shift-left/internal/client/memhttptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
	"github.com/openbox-ai/openbox-shift-left/internal/client"
	"github.com/openbox-ai/openbox-shift-left/internal/decision"
)

const testPrivateKeyB64 = "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="

// poisonedTranscript is a jsonl transcript shaped like a real Claude Code
// transcript (verified against ~/.claude/projects/*.jsonl; including the
// `cache_creation` sub-object and `iterations[]` breakdown, both of which are
// deliberately unbound because summing them alongside the top-level counts
// would double-count): assistant turns carry message.usage; every content-
// bearing location is seeded with a unique SENTINEL_* string.
const poisonedTranscript = `{"type":"user","isSidechain":false,"timestamp":"2026-08-11T09:00:00.000Z","message":{"role":"user","content":"SENTINEL_PROMPT top secret prompt"},"cwd":"/x","sessionId":"s1"}
{"type":"assistant","isSidechain":false,"timestamp":"2026-08-11T09:00:01.500Z","message":{"model":"claude-opus-4-8","content":[{"type":"text","text":"SENTINEL_OUTPUT assistant reply"},{"type":"tool_use","name":"Bash","input":{"command":"SENTINEL_CMD dangerous"}}],"usage":{"input_tokens":100,"cache_read_input_tokens":2000,"cache_creation_input_tokens":50,"output_tokens":30,"service_tier":"standard","cache_creation":{"ephemeral_1h_input_tokens":50,"ephemeral_5m_input_tokens":0},"iterations":[{"input_tokens":100,"output_tokens":30}]}},"costUSD":0.0123}
{"type":"user","isSidechain":false,"message":{"role":"user","content":[{"type":"tool_result","content":"SENTINEL_TOOLRESULT captured file body"}]}}
{"type":"assistant","isSidechain":true,"timestamp":"2026-08-11T09:00:02.000Z","message":{"model":"SENTINEL_SIDEMODEL-sonnet","content":[{"type":"text","text":"SENTINEL_SIDETEXT subagent reply"},{"type":"thinking","thinking":"SENTINEL_SIDETHINKING subagent chain of thought"}],"usage":{"input_tokens":7000,"output_tokens":700}}}
{"type":"assistant","isSidechain":false,"timestamp":"2026-08-11T09:00:03.000Z","message":{"content":[{"type":"thinking","thinking":"SENTINEL_THINKING private chain of thought"}],"usage":{"input_tokens":10,"output_tokens":5}}}
{"type":"file-history-snapshot","isSidechain":false,"snapshot":{"content":"SENTINEL_FILE entire file contents"}}
`

// sentinels are the transcript strings that must never egress, at any posture.
var sentinels = []string{
	"SENTINEL_PROMPT", "SENTINEL_OUTPUT", "SENTINEL_CMD",
	"SENTINEL_TOOLRESULT", "SENTINEL_FILE",
	"SENTINEL_SIDEMODEL", "SENTINEL_SIDETEXT", "SENTINEL_SIDETHINKING",
}

// thinkingSentinel is deliberately absent from the never-egress list above: the
// amendment authorised exactly this one field, so the test asserts it PRESENT
// with capture on while every other sentinel stays absent. Folding it into that
// list makes the widening unmeasurable and both directions vacuous.
const thinkingSentinel = "SENTINEL_THINKING"

func writeTranscript(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "transcript.jsonl")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	return p
}

// TestAggregateUsage_SumsTokensAndCost the SessionEnd rollup sums every usage
// line; sidechain included; because it is the whole-session total, and it is
// the independent second derivation the live reconciliation compares Σ(per-
// turn) against.
func TestAggregateUsage_SumsTokensAndCost(t *testing.T) {
	tokens, cost, err := aggregateUsage([]byte(poisonedTranscript))
	if err != nil {
		t.Fatalf("aggregateUsage: %v", err)
	}
	if tokens == nil {
		t.Fatal("tokens nil, want a rollup")
	}
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

// TestTurnWindow_PartitionsSidechainOut the main-thread window excludes
// sidechain lines exactly. This is the double-count guard: if a subagent's
// lines land in the parent transcript, the parent must not claim their tokens,
// because SubagentStop's own record carries them.
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
	// The subagent's own record carries its reasoning; the parent's must not; and
	// the wire-level absence asserted in TestFinops_NoContentOnWire is only
	// meaningful if the block is genuinely there to be leaked, which is what this
	// pair proves.
	if !strings.Contains(side.Thinking, "SENTINEL_SIDETHINKING") {
		t.Errorf("the sidechain window did not lift its own thinking (%q); every "+
			"main-thread assertion about its absence is then vacuous", side.Thinking)
	}
	if strings.Contains(main.Thinking, "SENTINEL_SIDETHINKING") {
		t.Errorf("a main-thread turn carried the subagent's reasoning: %q", main.Thinking)
	}

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
			t.Errorf("Σ(main,sidechain) %s = %d, rollup = %d; the partitions are not exhaustive", c.name, c.part, c.want)
		}
	}
}

// TestTurnWindow_OpenTimeIsFirstParsableTimestamp the window's open time is
// the first parsable line timestamp, and it arrives as a time.Time; never as
// the transcript's string.
func TestTurnWindow_OpenTimeIsFirstParsableTimestamp(t *testing.T) {
	w := aggregateTurnWindow([]byte(poisonedTranscript), false)
	want := time.Date(2026, 8, 11, 9, 0, 0, 0, time.UTC)
	if !w.Open.Equal(want) {
		t.Errorf("window open = %v, want %v (first parsable line)", w.Open, want)
	}

	noTS := aggregateTurnWindow([]byte(
		`{"type":"assistant","isSidechain":false,"timestamp":"not-a-time","message":{"usage":{"input_tokens":1,"output_tokens":1}}}`+"\n"), false)
	if !noTS.Open.IsZero() {
		t.Errorf("unparsable timestamp yielded open = %v, want zero", noTS.Open)
	}
	if !noTS.HasUsage {
		t.Error("an unparsable timestamp must not discard the line's usage")
	}
}

// TestTurnWindow_ModelIsLastNonEmptyInWindow model is last-non-empty within
// the window and is never carried across windows or back-filled: attributing
// tokens to a model that may not have spent them is a fabricated number.
func TestTurnWindow_ModelIsLastNonEmptyInWindow(t *testing.T) {
	body := `{"isSidechain":false,"message":{"model":"claude-opus-4-8","usage":{"input_tokens":5,"output_tokens":1}}}
{"isSidechain":false,"message":{"usage":{"input_tokens":5,"output_tokens":1}}}
{"isSidechain":false,"message":{"model":"claude-sonnet-4-8","usage":{"input_tokens":5,"output_tokens":1}}}
`
	if got := aggregateTurnWindow([]byte(body), false).Model; got != "claude-sonnet-4-8" {
		t.Errorf("model = %q, want claude-sonnet-4-8 (last non-empty)", got)
	}

	noModel := aggregateTurnWindow([]byte(`{"isSidechain":false,"message":{"usage":{"input_tokens":9,"output_tokens":2}}}`+"\n"), false)
	if noModel.Model != "" {
		t.Errorf("model = %q, want empty", noModel.Model)
	}
	if !noModel.HasUsage || noModel.Input != 9 {
		t.Errorf("a model-less window must still report its usage: %+v", noModel)
	}
}

// TestTurnWindow_LiftsThinkingBlocksInFileOrder thinking is lifted from
// `message.content[]` blocks of type "thinking", in file order, across the
// whole window; not one block per turn.
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
	if w.Input != 11 || w.Output != 3 {
		t.Errorf("usage arithmetic changed with the projection: %+v", w)
	}
}

// TestTurnWindow_StringMessageContentStillCounts the regression this widening
// could most easily cause: `message.content` is a string on user lines and an
// array on assistant ones.
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

// TestThinkingCollectionBoundExceedsTheWireCap the collection bound must cover
// the widest text the wire cap would allow, and this is not tidiness; it is
// what keeps the cap's mutation control alive.
func TestThinkingCollectionBoundExceedsTheWireCap(t *testing.T) {
	const wireCapRunes = 65536 // client.maxBodySize, counted in runes by capBody
	if maxThinkingBytes < 4*wireCapRunes {
		t.Fatalf("maxThinkingBytes = %d, want >= %d (4 bytes/rune x the wire cap): the "+
			"collector must never be the thing that truncates, or the cap's mutation "+
			"control in TestFinops_NoContentOnWire goes vacuous",
			maxThinkingBytes, 4*wireCapRunes)
	}
}

// TestTurnWindow_ThinkingAccumulationIsBounded the streaming reader aggregates
// a window of ANY size in bounded memory. The budget is in bytes and sits
// above the client's 65,536-rune wire cap, so it can never drop a byte the
// wire would have kept.
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
	if !utf8.ValidString(w.Thinking) {
		t.Error("bounded thinking is not valid UTF-8")
	}
}

// TestTurnWindow_ThinkingBoundaryDoesNotSplitARune a multi-byte rune
// straddling the budget boundary is dropped whole, not split.
func TestTurnWindow_ThinkingBoundaryDoesNotSplitARune(t *testing.T) {
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

// TestTurnWindow_ThinkingExactlyAtBudgetIsKeptWhole the exact-fit case: a
// block that fills the budget precisely must be kept whole.
func TestTurnWindow_ThinkingExactlyAtBudgetIsKeptWhole(t *testing.T) {
	exact := strings.Repeat("t", maxThinkingBytes)
	body := `{"isSidechain":false,"message":{"content":[{"type":"thinking","thinking":"` +
		exact + `"}],"usage":{"input_tokens":1,"output_tokens":1}}}` + "\n"

	w := aggregateTurnWindow([]byte(body), false)
	if len(w.Thinking) != maxThinkingBytes {
		t.Errorf("thinking held %d bytes, want exactly %d (a block that fits must not "+
			"be truncated)", len(w.Thinking), maxThinkingBytes)
	}
	if w.Thinking != exact {
		t.Error("a block that fits the budget exactly was altered")
	}
}

// TestTurnWindow_ThinkingRespectsSidechainPartition the partition is what
// stops a subagent's turn being counted twice. It must hold for thinking
// exactly as it holds for the numbers; otherwise a main-thread turn would
// carry a subagent's reasoning.
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

// TestReadTurnUsage_SuccessiveWindowsAreDisjoint two successive reads over a
// growing transcript must yield disjoint windows: this is the property that
// stops an N-turn session reporting O(N²) tokens.
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
		t.Errorf("second window = %d/%d, want 5/7; the cursor did not isolate the window", w2.Input, w2.Output)
	}
	if pos2.Offset <= pos1.Offset {
		t.Errorf("cursor did not advance: %d → %d", pos1.Offset, pos2.Offset)
	}

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

// TestReadTurnUsage_PartialFinalLineIsNotConsumed a partially-written final
// line must be left for the next read, never parsed half-way and never
// skipped: the cursor advances only over complete lines.
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

// TestReadTurnUsage_WindowSpanningManyChunksIsAggregatedWhole a window larger
// than one read chunk must be aggregated whole, in one window; not truncated
// at the chunk boundary with the remainder folded into the next turn.
func TestReadTurnUsage_WindowSpanningManyChunksIsAggregatedWhole(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "transcript.jsonl")

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
		t.Errorf("window = %d/%d/%d/%d, want %d/%d/%d/%d; the window was split at a chunk boundary",
			w.Input, w.Output, w.CacheRead, w.CacheCreationInput, lines, 2*lines, 3*lines, 4*lines)
	}
	if pos.Offset != int64(len(body)) {
		t.Errorf("cursor = %d, want %d (EOF); part of the window was left unread", pos.Offset, len(body))
	}
	if w.Model != "claude-sonnet-4-8" {
		t.Errorf("model = %q, want the LAST non-empty across chunks", w.Model)
	}
	if w.Open.IsZero() || !w.Open.Equal(time.Date(2026, 8, 11, 9, 0, 0, 0, time.UTC)) {
		t.Errorf("open = %v, want the FIRST line's timestamp across chunks", w.Open)
	}

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

// TestReadTurnUsage_LineStraddlingAChunkBoundaryIsIntact a line boundary that
// falls inside a chunk read must not split the line: the carry-over is what
// keeps a record whole across two reads.
func TestReadTurnUsage_LineStraddlingAChunkBoundaryIsIntact(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "transcript.jsonl")

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
		t.Errorf("window = %d/%d, want 18/5; a line straddling the chunk boundary was lost or split", w.Input, w.Output)
	}
	if w.Model != "m2" {
		t.Errorf("model = %q, want m2", w.Model)
	}
	if pos.Offset != int64(len(body)) {
		t.Errorf("cursor = %d, want %d (EOF)", pos.Offset, len(body))
	}
}

// TestReadTurnUsage_UnboundedSingleLineIsRefused a single line with no newline
// that grows past the whole-transcript cap is the one thing streaming cannot
// absorb. It must error rather than buffer without bound, and it must NOT
// advance the cursor; a later firing retries once the line is terminated.
func TestReadTurnUsage_UnboundedSingleLineIsRefused(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "transcript.jsonl")
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

// TestReadTurnUsage_TruncatedTranscriptRereadsFromStart a transcript that
// shrank below the cursor was truncated or rotated.
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
	if _, _, err := readTurnUsage(t.TempDir(), hookflow.TurnPos{}, false); err == nil {
		t.Error("a directory should error")
	}
}

// TestTurnWindow_MalformedLinesSkipped malformed lines are skipped, never
// fatal, and never stop the aggregation.
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

// TestTurnWindow_NegativeCountsClamped negative source counts are clamped so
// emitted numbers always satisfy the schema's `minimum: 0`.
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

// TestFinops_NoContentOnWire is the load-bearing INV-2 test, and what it proves
// is an exact allowlist rather than an impossibility: sentinel content absent
// from the signed body, the model id present as the one string authorised to
// egress, the raw timestamp absent, and a sidechain turn's sums absent from a
// main-thread one. It runs with capture on, the adversarial case, because the
// client's stripper is then disabled.
func TestFinops_NoContentOnWire(t *testing.T) {
	path := writeTranscript(t, poisonedTranscript)

	var bodies [][]byte
	srv := memhttptest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

	// The thinking sentinel is asserted alongside them here: this mapper never
	// set CaptureContent, so with capture OFF the amended allowlist must egress
	// exactly as much as the old one did.
	for _, e := range events {
		evJSON, _ := json.Marshal(e.ev)
		for _, s := range append(append([]string{}, sentinels...), thinkingSentinel) {
			if strings.Contains(string(evJSON), s) {
				t.Fatalf("INV-2 breach: sentinel %q present in %s event: %s", s, e.name, evJSON)
			}
		}
	}

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
		// Every transcript line's timestamp ends in the millis+Z form below; the
		// timestamps that legitimately appear on the wire are the mapper's own
		// RFC3339Nano stamps, which never carry these exact values.
		for _, ts := range []string{"2026-08-11T09:00:00.000Z", "2026-08-11T09:00:01.500Z", "2026-08-11T09:00:03.000Z"} {
			if strings.Contains(string(body), ts) {
				t.Fatalf("INV-2 breach: raw transcript timestamp %q on the wire for %s: %s", ts, events[i].name, body)
			}
		}
	}

	completedBody := string(bodies[2])
	if !strings.Contains(completedBody, `"model":"claude-opus-4-8"`) {
		t.Errorf("the model id did not reach the wire: %s", completedBody)
	}

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

	if !strings.Contains(string(bodies[0]), strconv.Itoa(wantTotal)) {
		t.Errorf("expected token total %d on the SessionEnded wire body, got: %s", wantTotal, bodies[0])
	}

	// Three claims must hold at once; the assistant text still comes from the
	// hook field, the ONE amended transcript field egresses, and every other
	// transcript sentinel stays as unreachable as it was.
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
				"content-capturing turn; that decision amendment authorised thinking and "+
				"NOTHING else, so the projection must still be unable to see this: %s",
				s, capturedBody)
		}
	}
	if got := activityOutputThinking(t, capturedBody); !strings.Contains(got, thinkingSentinel) {
		t.Fatalf("activity_output.thinking = %q, want the transcript's thinking block; "+
			"the amendment costs a privacy narrowing and must buy the field: %s",
			got, capturedBody)
	}
	if !strings.Contains(capturedBody, hookMessage) {
		t.Fatalf("the hook-supplied assistant message did not reach the wire; every "+
			"assertion in this section would then be vacuous: %s", capturedBody)
	}
	// Chain-of-thought there corrupts the reader silently: core logs nothing when
	// the text is merely the wrong text.
	span := assistantSpanBody(t, capturedBody)
	if !strings.Contains(span, hookMessage) {
		t.Errorf("the assistant span lost its own text: %q", span)
	}
	if strings.Contains(span, thinkingSentinel) {
		t.Errorf("thinking rode the assistant span, where core reads it as the turn's "+
			"REPLY; it belongs in activity_output.thinking and nowhere else: %q", span)
	}

	// Deleting the redaction, or deleting the cap, must each turn this test red.
	t.Run("thinking is redacted before attachment and capped before signing", func(t *testing.T) {
		awsKey := "AKIA" + "IOSFODNN7EXAMPLE"
		const tail = "TAIL_PAST_THE_CAP"
		poisoned := `{"isSidechain":false,"timestamp":"2026-08-11T09:00:01.000Z","message":` +
			`{"model":"claude-opus-4-8","content":[{"type":"thinking","thinking":"` +
			`reasoning about AWS_ACCESS_KEY_ID=` + awsKey + ` then ` +
			strings.Repeat("z", 70000) + tail + `"}],` +
			`"usage":{"input_tokens":1,"output_tokens":1}}}` + "\n"
		capPath := writeTranscript(t, poisoned)

		var got [][]byte
		capSrv := memhttptest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

		if strings.Contains(body, awsKey) {
			t.Errorf("the raw credential reached the wire inside a thinking block; "+
				"redaction must run BEFORE attachment: %s", body)
		}
		thinking := activityOutputThinking(t, body)
		if !strings.Contains(thinking, "OPENBOX_REDACTED") {
			t.Errorf("no redaction placeholder in activity_output.thinking: %q", thinking)
		}
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

// TestTurnUsage_CostIsNeverDerived cost must never be derived.
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
// SessionEnded event carries no tokens/cost; byte-identical to pre-SL-16
// output.
func TestFinops_OffByteIdentical(t *testing.T) {
	m := NewMapper(Identity{DeveloperDID: testDID})
	m.NewID = func() string { return "evt-1" }
	m.Now = func() time.Time { return time.Unix(0, 0).UTC() }
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
	if _, _, err := readTranscriptUsage(t.TempDir()); err == nil {
		t.Error("non-regular file should error (skipped)")
	}
}

func TestReadTranscriptUsage_OversizedSkipped(t *testing.T) {
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
