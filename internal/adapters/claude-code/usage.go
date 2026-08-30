package claudecode

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
	"github.com/openbox-ai/openbox-shift-left/internal/client"
)

// Transcript usage extraction — per turn (Stop/SubagentStop) and as a SessionEnd rollup.
//
// Claude Code hooks expose no token/cost usage (capabilities.go / README known limitations),
// but the session's `transcript_path` — a JSONL file, one line per assistant/user event —
// carries a `message.usage` object with token counts. Behind the finops gate (ResolveFinops),
// the adapter reads it at two granularities:
//
// - per turn, from a cursor position, on Stop/SubagentStop → the TurnStarted/TurnCompleted
// pair; - whole-session, on SessionEnd → the rollup on SessionEnded, retained because it is the
// independent second derivation Phase 06's reconciliation compares the per-turn sum against.
//
// # INV-2: this projection is an allowlist now, not an impossibility
//
// It used to be enforceable structurally: the projection structs held only numeric fields, so
// every content-bearing field in the transcript (prompt text, tool inputs, tool_result bodies,
// file contents, the assistant's message `content`, thinking blocks) had nowhere to land and
// could not enter memory at all. That decision narrowed that, and its 2026-08-25 amendment
// widened it once more. `turnLine` binds four non-numeric fields, and two of them egress:
//
// message.model                 identifier → EGRESSES (capStr-bounded)
// message.content[].thinking    CONTENT    → EGRESSES, GATED (see below) timestamp
// timestamp  → parsed to a time.Time for duration_ms, discarded isSidechain
// bool       → partitions subagent lines out of the parent's sums
//
// Everything else still has nowhere to land. `text`, `tool_input`, `tool_result`, `cwd`,
// `service_tier`, `inference_geo` and `speed` are unbound, and so are two numeric siblings that
// would double-count if bound: `usage.cache_creation.ephemeral_*` (sums to
// cache_creation_input_tokens) and `usage.iterations[]` (a per-model-call breakdown of the same
// line). `message.content` is bound only as raw JSON, decoded no further than the thinking
// blocks — so its siblings stay unreachable rather than unused.
//
// Thinking is the first genuinely FREE-FORM content string here, and what protects it is no
// longer a property of the type. It is three fallible mechanisms in a fixed order — gate
// (Mapper.CaptureContent, then the client's stripContent), redact (Mapper.RedactContent, before
// attachment), cap (capBody, 65,536 runes) — and all three are asserted on outbound bytes,
// because a redaction applied after attachment passes every unit test in this package and still
// ships the secret.
//
// TestFinops_NoContentOnWire proves the CURRENT claim, in both directions: with capture off
// every sentinel is absent from the real signed wire body, including the thinking one; with
// capture on the thinking sentinel is present in activity_output.thinking, redacted and capped,
// and every OTHER transcript sentinel is still absent — so the widening is exactly one field
// wide. It is load-bearing rather than supplementary: a change that makes it pass trivially is
// a defect, because the guarantee no longer defends itself. Its two mutation controls are the
// proof — deleting the redaction, or deleting the cap, must each turn it red.
//
// INV-3: best-effort. A missing / oversized / malformed / partially-written transcript yields
// an error the caller logs and skips; it never fails the flush, blocks a tool call, or writes
// stdout. The read is bounded (maxTranscriptBytes) so a giant transcript cannot exhaust memory.

// maxTranscriptBytes bounds the transcript read so a pathological/huge transcript
// cannot exhaust memory (INV-3). A transcript larger than this is skipped whole
// (honest: no partial/undercounted numbers) rather than truncated mid-stream.
const maxTranscriptBytes = 64 << 20 // 64 MiB

// usageNumbers is the numbers-only projection of a transcript turn's
// `message.usage`. It has NO string/content fields BY DESIGN (INV-2): unknown
// keys in the source usage object (service_tier, iterations, …) are ignored by
// the JSON decoder and cannot be captured.
type usageNumbers struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

// transcriptLine is the numbers-only projection of one JSONL transcript line.
// Only the two fields below are bound; every content-bearing sibling
// (message.content, tool_input, tool_result, text, thinking, cwd, …) is dropped
// on decode. CostUSD is optional and absent in current Claude Code transcripts
// (verified), so Cost stays nil ("absent when unknown") unless a transcript
// carries it — never derived from a pricing table (would be a fabricated number).
type transcriptLine struct {
	CostUSD *float64 `json:"costUSD"`
	Message *struct {
		Usage *usageNumbers `json:"usage"`
	} `json:"message"`
}

// turnLine is the per-turn projection: transcriptLine's numbers plus the three
// fields that decision authorised, and nothing else. The struct IS the allowlist
// — see the INV-2 section in this file's doc comment for what each field costs
// and which one egresses.
type turnLine struct {
	// Timestamp is parsed to a time.Time to compute the turn's duration_ms and
	// then DISCARDED. The string never reaches an event, metadata, or the wire —
	// TestFinops_NoContentOnWire asserts its absence from the payload bytes
	// explicitly, because "we only use it locally" is a property of today's code
	// rather than of the type.
	Timestamp string `json:"timestamp"`
	// IsSidechain marks a line produced inside a subagent. Present on every
	// session line in real transcripts (measured: 13,439 of 13,439). It
	// partitions subagent usage out of the parent's window so the two are never
	// counted twice — see readTurnUsage.
	IsSidechain bool `json:"isSidechain"`
	Message     *struct {
		// Model is the ONE IDENTIFIER string this projection egresses.
		// Provider-controlled free text, so the mapper capStr-bounds it at the
		// boundary. Values seen in real transcripts include "<synthetic>", which
		// is passed through unchanged: filtering it would drop real tokens and
		// rewriting it would fabricate an attribution.
		Model string `json:"model"`
		// Content is the message's content blocks, bound as RAW JSON and decoded
		// no further than thinkingFrom needs (that decision amendment,
		// 2026-08-25). Two reasons it is a RawMessage rather than a typed slice:
		//
		// 1. Correctness. `message.content` is a STRING on user lines and an
		// ARRAY on assistant ones. A typed slice fails the whole line's
		// unmarshal on a string, which would silently drop that line's token
		// counts — a finops regression with no error anywhere. 2. Scope.
		// Decoding on demand keeps every sibling block type (`text`, `tool_use`,
		// `tool_result`) unreachable rather than merely unused, which is what
		// the amendment authorised and no more.
		Content json.RawMessage `json:"content"`
		Usage   *usageNumbers   `json:"usage"`
	} `json:"message"`
}

// maxThinkingBytes bounds the thinking text one window may hold.
//
// It exists to preserve readTurnUsage's memory guarantee. That reader streams a
// window of ANY size in turnChunkBytes-sized chunks precisely because the first
// firing after a mid-session enable reads the whole transcript to date as one
// window; an unbounded concatenation across such a window would put hundreds of
// megabytes back in memory and undo the design.
//
// The budget is in BYTES and sits at 4x the client's 65,536-RUNE egress cap
// (capBody). A rune is at most 4 bytes, so this can never discard a byte the
// wire cap would have kept — the two bounds compose instead of fighting.
const maxThinkingBytes = 4 * 65536

// thinkingBlock is the one content-block shape this projection decodes. Type is
// bound to select the block; Thinking is the text. No other field of a content
// block is bound, so `text`, `tool_use` inputs and `tool_result` bodies cannot
// land here.
//
// The `type` filter also excludes `redacted_thinking`, whose payload is
// provider-encrypted base64 — capturing it would spend the 64KB egress budget on
// ciphertext nothing downstream can read.
type thinkingBlock struct {
	Type     string `json:"type"`
	Thinking string `json:"thinking"`
}

// appendThinking folds one block's text onto the window's accumulator, bounded.
//
// Truncation is a hard cut with no marker (capBody's rule), trimmed back to a
// rune boundary by hookflow.TruncateBytes: a mid-rune byte cut produces invalid
// UTF-8, which json.Marshal silently rewrites to U+FFFD — corrupting the body
// rather than shortening it. The shared helper is used rather than a local loop
// because this is the same boundary rule the enforce path's body bound and
// CapCommand apply, and three copies of it is three places for an off-by-one to
// hide.
func appendThinking(acc, block string) string {
	if block == "" {
		return acc
	}
	sep := ""
	if acc != "" {
		sep = "\n\n"
	}
	room := maxThinkingBytes - len(acc) - len(sep)
	if room <= 0 {
		return acc
	}
	// A block that does not fit is cut, and a cut that lands mid-rune backs up —
	// so a rune straddling the budget is dropped whole rather than split.
	if block = hookflow.TruncateBytes(block, room); block == "" {
		return acc
	}
	return acc + sep + block
}

// thinkingFrom lifts the thinking text out of one message's content blocks, in
// file order. A non-array content (a user line's plain string), a malformed
// array, or an array with no thinking block all yield "" — never an error
// (INV-3: a transcript shape this projection does not recognise degrades to
// no-thinking, it never fails a flush or blocks a session).
func thinkingFrom(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return ""
	}
	var blocks []thinkingBlock
	if err := json.Unmarshal(trimmed, &blocks); err != nil {
		return ""
	}
	var out string
	for _, b := range blocks {
		if b.Type != "thinking" {
			continue
		}
		out = appendThinking(out, b.Thinking)
	}
	return out
}

// turnWindow is one turn's worth of transcript, aggregated. "Turn" here means
// "everything the transcript gained since the last cursor position" — measured
// against real sessions that is many model calls, not one (~52 usage lines per
// Stop firing in the largest sampled session). So these are WINDOW SUMS, and no
// caller should read them as per-model-call numbers; hooks cannot deliver that.
type turnWindow struct {
	Input              int
	Output             int
	CacheCreationInput int
	CacheRead          int
	// Model is the last non-empty model id IN THIS WINDOW. Never carried across
	// windows and never back-filled from the session's SessionStart model:
	// attributing a window's tokens to a model that may not have spent them is a
	// fabricated number, the same class of error as deriving a cost.
	Model string
	// Open is the first parsable line timestamp in the window — the turn's real
	// start, used only to compute duration_ms. Zero when no line parsed, in
	// which case the caller falls back to hook wall time and omits the duration.
	Open time.Time
	// HasUsage reports whether any line in the window carried usage at all. A
	// window without it is not a turn worth emitting.
	HasUsage bool
	// Thinking is the window's `thinking` content blocks, concatenated in file
	// order and bounded at maxThinkingBytes. It is the first genuinely free-form
	// CONTENT string this projection lifts.
	//
	// The lift is deliberately UNGATED and the gate sits at attachment
	// (Mapper.MapTurn's CaptureContent, then the client's stripContent). Gating
	// the parser too would buy nothing on the wire — the chunk this text was read
	// out of was already resident — while putting a second copy of the posture
	// decision inside a pure function. What the parser owes instead is the bound
	// above.
	Thinking string
}

// total is the whole-throughput figure for the window: input + output + both
// cache counts. Kept as a method so the definition lives in one place and
// matches the contract's `tokens.total` description exactly.
func (w turnWindow) total() int {
	return w.Input + w.Output + w.CacheCreationInput + w.CacheRead
}

// tokens renders the window as the wire type. Every count is emitted, including
// zeros, because a window that reported usage genuinely spent zero of something
// — unlike a provider that does not report a count at all, which is Codex's
// case and stays absent there.
func (w turnWindow) tokens() *client.Tokens {
	if !w.HasUsage {
		return nil
	}
	total := w.total()
	return &client.Tokens{
		Input:              intPtr(w.Input),
		Output:             intPtr(w.Output),
		CacheCreationInput: intPtr(w.CacheCreationInput),
		CacheRead:          intPtr(w.CacheRead),
		Total:              intPtr(total),
	}
}

// readTurnUsage reads the transcript from a cursor position and aggregates the
// usage NUMBERS in that window only, returning the next cursor position so the
// caller can advance after a successful spool (never before — see
// hookflow.TurnCursor for why that ordering is the correctness argument).
//
// sidechain selects the partition:
//
//	false → the MAIN thread's window: lines with isSidechain == false
//	true  → a SUBAGENT's window:      lines with isSidechain == true
//
// The partition is what stops one set of tokens being counted twice when a
// subagent's lines land in the parent's transcript. Whether they do is not
// settled (measured: the field is present on every line and was false on every
// line across 32 sessions), and this split is safe under every answer: it cannot
// double-count, and its worst case is a subagent that reports nothing. See
// plans/…/reports/measure-260811-transcript-turn-surface.md.
//
// The two partitions are exhaustive, so when both hooks read one file
// Σ(main) + Σ(subagent) equals the SessionEnd rollup exactly — which is the
// invariant Phase 06 asserts field by field.
//
// Only COMPLETE lines are consumed: the returned position never advances past a
// trailing fragment, so a partially-written final line is re-read next time
// rather than parsed half-way (the findings cursor's discipline).
func readTurnUsage(path string, from hookflow.TurnPos, sidechain bool) (turnWindow, hookflow.TurnPos, error) {
	next := from
	if path == "" {
		return turnWindow{}, next, fmt.Errorf("no transcript_path")
	}
	f, err := os.Open(path)
	if err != nil {
		return turnWindow{}, next, fmt.Errorf("open transcript: %w", err)
	}
	defer f.Close()
	// Stat the open fd, not the path: the regular-file check and the size bound
	// must refer to the same object we read (no stat/read TOCTOU).
	fi, err := f.Stat()
	if err != nil {
		return turnWindow{}, next, fmt.Errorf("stat transcript: %w", err)
	}
	if !fi.Mode().IsRegular() {
		return turnWindow{}, next, fmt.Errorf("transcript is not a regular file")
	}
	// A transcript that SHRANK below the cursor was truncated or rotated (Claude
	// Code does not do this today — defensive). Re-read from the start: the turn
	// ids are re-minted deterministically and the server dedupes them, whereas
	// seeking past EOF would silently report nothing forever.
	offset := from.Offset
	if fi.Size() < offset {
		offset = 0
	}
	if fi.Size() <= offset {
		return turnWindow{}, next, nil // nothing new — not an error
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return turnWindow{}, next, fmt.Errorf("seek transcript: %w", err)
	}

	// The window is read in bounded CHUNKS and accumulated, rather than slurped
	// whole or truncated at the cap.
	//
	// Memory stays bounded (turnChunkBytes at a time) because the aggregation is a
	// running sum — the window never has to be resident. And the window stays
	// SEMANTICALLY COMPLETE, which truncating at a cap would not: a window larger
	// than the cap would have its head reported as turn N and its tail folded into
	// turn N+1, mixed with N+1's own usage. Total tokens would still balance, but
	// two turns would carry each other's numbers, and per-turn attribution is the
	// entire point of this feature.
	//
	// That is not a hypothetical window size. A cursor that has never been written
	// reads from offset 0, so the FIRST firing after usage capture is enabled
	// mid-session sees the whole transcript to date as one window — which for a
	// long verbose session is exactly where a multi-hundred-megabyte backlog lives.
	var (
		w        turnWindow
		consumed int64
		carry    []byte // a trailing partial line, prepended to the next chunk
		buf      = make([]byte, turnChunkBytes)
	)
	for {
		n, readErr := f.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			if len(carry) > 0 {
				chunk = append(carry, chunk...)
			}
			// Consume complete lines only; hold any trailing fragment for the next
			// chunk (or, at EOF, leave it unconsumed for the next firing).
			lastNL := bytes.LastIndexByte(chunk, '\n')
			if lastNL < 0 {
				// No newline in the whole chunk. Guard against an unbounded single
				// line: past the cap, stop carrying it — the cursor does not advance
				// over it, so a later firing retries once the line is complete.
				if len(chunk) > maxTranscriptBytes {
					return turnWindow{}, next, fmt.Errorf("transcript line exceeds %d-byte cap", maxTranscriptBytes)
				}
				carry = append([]byte(nil), chunk...)
			} else {
				aggregateTurnWindowInto(&w, chunk[:lastNL+1], sidechain)
				consumed += int64(lastNL + 1)
				carry = append([]byte(nil), chunk[lastNL+1:]...)
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				break
			}
			// A partial read that then failed: report what is known and leave the
			// cursor where the successfully-consumed bytes end, so nothing is lost
			// and nothing is counted twice (INV-3).
			return turnWindow{}, next, fmt.Errorf("read transcript: %w", readErr)
		}
	}

	if consumed == 0 {
		return turnWindow{}, next, nil // only an incomplete line so far
	}
	next = hookflow.TurnPos{Offset: offset + consumed, Index: from.Index}
	return w, next, nil
}

// turnChunkBytes bounds one read of the transcript window. It caps RESIDENT
// memory, not the window: readTurnUsage loops until the window is fully consumed,
// so a large backlog is aggregated completely rather than split across turns.
// maxTranscriptBytes still bounds a single pathological LINE, which is the only
// thing that cannot be streamed.
const turnChunkBytes = 4 << 20 // 4 MiB

// aggregateTurnWindow sums one window's usage from JSONL bytes. Split out from
// readTurnUsage so the parser is testable without a filesystem (and so the
// sentinel test can feed raw bytes). Bad lines are skipped, never fatal.
func aggregateTurnWindow(raw []byte, sidechain bool) turnWindow {
	var w turnWindow
	aggregateTurnWindowInto(&w, raw, sidechain)
	return w
}

// aggregateTurnWindowInto is the accumulating form: it folds one CHUNK of the
// window into an existing turnWindow, so readTurnUsage can stream an arbitrarily
// large window in bounded memory without splitting it across turns.
//
// Accumulating rather than merging two windows is deliberate: "model = last
// non-empty" and "open = first parsable timestamp" are order-dependent, and
// running them over the chunks in file order gives the same answer a single pass
// over the whole window would. A merge of independently-aggregated chunks would
// have to re-derive that ordering, and could get it wrong.
func aggregateTurnWindowInto(w *turnWindow, raw []byte, sidechain bool) {
	for _, line := range bytes.Split(raw, []byte{'\n'}) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var tl turnLine
		if err := json.Unmarshal(line, &tl); err != nil {
			continue // partial line / non-JSON marker / schema drift (INV-3)
		}
		// The partition. A line belongs to exactly one side, so nothing is
		// counted twice and nothing is dropped when both sides are read.
		if tl.IsSidechain != sidechain {
			continue
		}
		// The turn's open time: the FIRST parsable timestamp in the window. Parsed
		// to a time and the string dropped on the spot.
		if w.Open.IsZero() && tl.Timestamp != "" {
			if t, err := time.Parse(time.RFC3339Nano, tl.Timestamp); err == nil {
				w.Open = t
			}
		}
		if tl.Message == nil {
			continue
		}
		if tl.Message.Model != "" {
			w.Model = tl.Message.Model // last non-empty wins, within this window only
		}
		// Thinking accumulates across the window in file order — a turn is many
		// model calls, so a turn's reasoning is many blocks. Bounded; see
		// maxThinkingBytes.
		w.Thinking = appendThinking(w.Thinking, thinkingFrom(tl.Message.Content))
		if u := tl.Message.Usage; u != nil {
			// nonNeg clamps a malformed negative source value so emitted numbers
			// always satisfy the schema's `minimum: 0`.
			w.Input += nonNeg(u.InputTokens)
			w.Output += nonNeg(u.OutputTokens)
			w.CacheCreationInput += nonNeg(u.CacheCreationInputTokens)
			w.CacheRead += nonNeg(u.CacheReadInputTokens)
			w.HasUsage = true
		}
	}
}

// readTranscriptUsage reads a Claude Code transcript and aggregates usage NUMBERS
// ONLY across the session's turns, returning the SessionEnded rollup. It is the
// production reader wired into the SessionEnd flush path (hookrun.go).
//
// Returns (nil, nil, nil) when the transcript carries no usage at all (a valid
// empty session — the caller then emits no tokens/cost, same as finops-off).
// Returns an error (best-effort skip, INV-3) when the path is empty, missing,
// oversized, or unreadable. Malformed individual lines are skipped, not fatal
// (fault-tolerant to partial/streaming JSONL and schema drift).
func readTranscriptUsage(path string) (*client.Tokens, *client.Cost, error) {
	if path == "" {
		return nil, nil, fmt.Errorf("no transcript_path")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, fmt.Errorf("open transcript: %w", err)
	}
	defer f.Close()
	// Stat the open fd (not the path) so the regular-file check and the
	// size bound below both refer to the same object we actually read —
	// no stat/read TOCTOU where a symlink swap or post-stat growth could
	// bypass the cap (mirrors the git action's bounded-read pattern).
	if fi, err := f.Stat(); err != nil {
		return nil, nil, fmt.Errorf("stat transcript: %w", err)
	} else if !fi.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("transcript is not a regular file")
	}
	// Read at most cap+1 bytes: reaching cap+1 means the file exceeds the cap, so
	// it is skipped WHOLE (honest — no truncated/undercounted parse) rather than
	// partially aggregated (INV-3, bounded so a giant transcript can't OOM).
	raw, err := io.ReadAll(io.LimitReader(f, maxTranscriptBytes+1))
	if err != nil {
		return nil, nil, fmt.Errorf("read transcript: %w", err)
	}
	if len(raw) > maxTranscriptBytes {
		return nil, nil, fmt.Errorf("transcript exceeds %d-byte cap", maxTranscriptBytes)
	}
	return aggregateUsage(raw)
}

// aggregateUsage parses JSONL bytes into a token/cost rollup using the
// numbers-only projection. Split out from readTranscriptUsage so the parser is
// testable without touching the filesystem (and so the sentinel test can feed
// raw bytes directly). Bad lines are skipped; only numeric fields are ever read.
func aggregateUsage(raw []byte) (*client.Tokens, *client.Cost, error) {
	var in, out, cacheCreate, cacheRead int
	var costUSD float64
	var sawUsage, sawCost bool

	for _, line := range bytes.Split(raw, []byte{'\n'}) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var tl transcriptLine
		if err := json.Unmarshal(line, &tl); err != nil {
			// Partial final line / non-JSON marker / schema drift: skip it,
			// keep aggregating (fault-tolerant, INV-3).
			continue
		}
		if tl.Message != nil && tl.Message.Usage != nil {
			u := tl.Message.Usage
			// Each count into its own field. This rollup used to fold both cache
			// counts into Input because client.Tokens had nowhere else to put
			// them, which made cache efficiency unmeasurable AND made this rollup
			// incomparable with the per-turn records — the two would have been
			// summing different quantities under the same field name, so Phase
			// 06's reconciliation would have compared unlike things. Contract v1.1
			// gave the cache counts their own fields; Input is pure now.
			//
			// nonNeg clamps any negative source value to 0 so the emitted numbers
			// always satisfy the schema `minimum: 0`.
			in += nonNeg(u.InputTokens)
			out += nonNeg(u.OutputTokens)
			cacheCreate += nonNeg(u.CacheCreationInputTokens)
			cacheRead += nonNeg(u.CacheReadInputTokens)
			sawUsage = true
		}
		if tl.CostUSD != nil {
			costUSD += *tl.CostUSD
			sawCost = true
		}
	}

	if !sawUsage && !sawCost {
		return nil, nil, nil // valid, but nothing to report
	}

	var tokens *client.Tokens
	if sawUsage {
		// Whole throughput, matching the contract's `tokens.total`: input +
		// output + both cache counts. The number is the same one the old
		// fold-in produced, so a consumer reading only `total` sees no change.
		total := in + out + cacheCreate + cacheRead
		tokens = &client.Tokens{
			Input:              intPtr(in),
			Output:             intPtr(out),
			CacheCreationInput: intPtr(cacheCreate),
			CacheRead:          intPtr(cacheRead),
			Total:              intPtr(total),
		}
	}
	var cost *client.Cost
	if sawCost {
		cost = &client.Cost{Amount: costUSD, Currency: "USD"}
	}
	return tokens, cost, nil
}

func intPtr(v int) *int { return &v }

// nonNeg clamps a token count to >= 0: a malformed/negative source value
// must never produce a number that violates the schema `minimum: 0`.
func nonNeg(v int) int {
	if v < 0 {
		return 0
	}
	return v
}
