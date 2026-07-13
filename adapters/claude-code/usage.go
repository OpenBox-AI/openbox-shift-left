package claudecode

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/openbox-ai/openbox-shift-left/client"
)

// STORY-SL-16 — opt-in transcript usage extraction (OD-FINOPS).
//
// Claude Code hooks expose NO token/cost usage (capabilities.go / README known
// limitations), but the session's `transcript_path` — a JSONL file, one turn per
// line — carries a per-assistant-turn `message.usage` object with token counts.
// Behind the off-by-default finops opt-in (ResolveFinops), the adapter reads it
// on SessionEnd (off the hot path) to populate the otherwise-unused
// client.Tokens / client.Cost on the SessionEnded event.
//
// INV-2 (the load-bearing invariant) is enforced STRUCTURALLY, not by filtering:
// the transcript is decoded into `transcriptLine` / `usageNumbers` — structs that
// contain ONLY numeric fields. Because encoding/json silently ignores unknown
// keys, every content-bearing field in the transcript (prompt text, tool inputs,
// tool_result bodies, file contents, the assistant's message `content`, thinking
// blocks, …) has NOWHERE to land — it is impossible for content to enter memory
// through this path, let alone reach an event, metadata, span, or the wire. The
// sentinel-content-absent test (usage_test.go) proves this end-to-end.
//
// INV-3: best-effort. A missing / oversized / malformed / partially-written
// transcript yields an error the caller logs and skips; it never fails the flush,
// blocks a tool call, or writes stdout. The read is bounded (maxTranscriptBytes)
// so a giant transcript cannot exhaust memory.

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
	// Stat the OPEN fd (not the path) so the regular-file check and the size
	// bound below both refer to the same object we actually read — no stat/read
	// TOCTOU where a symlink swap or post-stat growth could bypass the cap
	// (SEC-16-1; mirrors SL-6's bounded-read pattern).
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
	var in, out int
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
			// Input-side = prompt tokens plus cache tokens consumed. The SL-1
			// Tokens contract has no cache field, so cache read/creation fold into
			// Input (documented rollup; total token throughput is preserved).
			// nonNeg clamps any negative source value to 0 so the emitted numbers
			// always satisfy the SL-1 schema `minimum: 0` (SEC-16-2, data-integrity).
			in += nonNeg(u.InputTokens) + nonNeg(u.CacheReadInputTokens) + nonNeg(u.CacheCreationInputTokens)
			out += nonNeg(u.OutputTokens)
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
		total := in + out
		tokens = &client.Tokens{Input: intPtr(in), Output: intPtr(out), Total: intPtr(total)}
	}
	var cost *client.Cost
	if sawCost {
		cost = &client.Cost{Amount: costUSD, Currency: "USD"}
	}
	return tokens, cost, nil
}

func intPtr(v int) *int { return &v }

// nonNeg clamps a token count to >= 0 (SEC-16-2): a malformed/negative source
// value must never produce a number that violates the SL-1 schema `minimum: 0`.
func nonNeg(v int) int {
	if v < 0 {
		return 0
	}
	return v
}
