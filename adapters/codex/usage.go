package codex

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/openbox-ai/openbox-shift-left/client"
)

// STORY-SL7-C — opt-in Codex finops / token-usage extraction (SL-16 parity).
//
// Codex hooks expose NO token/cost usage (capabilities.go), but the session's
// on-disk ROLLOUT JSONL — the file `transcript_path` points at on SessionEnd —
// carries running token counts. Behind the off-by-default finops opt-in
// (ResolveFinops — the SAME separate flag Claude Code's SL-16 uses, NOT
// content_capture), the adapter reads it on SessionEnd (off the hot path, after
// Codex has flushed the transcript — spike S5 addendum #10) to populate the
// otherwise-unused client.Tokens on the SessionEnded event.
//
// GROUNDED WIRE SHAPE (real source, codex-rs @ rust-v0.145.0 — recorded in the
// testdata fixture, NOT guessed; the box carried no live rollout to sample so
// the shape was pinned from the shipped structs and the 0.145.0 binary strings):
//
//	rollout line   = {"timestamp":..,"type":"event_msg","payload":<EventMsg>}
//	                 (RolloutItem is #[serde(tag="type",content="payload")]; the
//	                  token line's item tag is "event_msg")
//	EventMsg       = {"type":"token_count","info":<TokenUsageInfo>,"rate_limits":..}
//	                 (EventMsg is #[serde(tag="type",rename_all="snake_case")])
//	TokenUsageInfo = {"total_token_usage":<TokenUsage>,"last_token_usage":<TokenUsage>,
//	                  "model_context_window":<int|null>}
//	TokenUsage     = {"input_tokens","cached_input_tokens","cache_write_input_tokens",
//	                  "output_tokens","reasoning_output_tokens","total_tokens"}  (all i64)
//
// Two Codex-specific facts drive the aggregation (both source-verified, and the
// deliberate DIVERGENCE from the Claude Code reader):
//
//  1. `total_token_usage` is a CUMULATIVE running session total, NOT a per-turn
//     delta: TokenUsageInfo::append_last_usage does
//     `total_token_usage.add_assign(last)` (protocol.rs @ rust-v0.145.0). Each
//     successive token_count line carries a larger cumulative snapshot. So the
//     rollup is the LAST valid snapshot — NOT a sum across lines (summing would
//     multiply-count every prior turn). CC, by contrast, sums per-turn usages.
//  2. `cached_input_tokens` / `cache_write_input_tokens` / `reasoning_output_tokens`
//     are SUBSETS already folded inside input_tokens / output_tokens (OpenAI
//     accounting): TokenUsage::non_cached_input == input_tokens - cached_input_tokens
//     (protocol.rs). total_tokens is Codex's own independently-summed field (we
//     carry it verbatim, not recomputed). So we carry
//     input_tokens / output_tokens / total_tokens DIRECTLY and must NOT add the
//     cache/reasoning sub-counts (adding would double-count). CC's cache tokens are
//     ADDITIVE and folded into Input; Codex's are already inside it.
//
// COST: the token path carries NO cost/price field at all — TokenCountEvent is
// only {info, rate_limits} (protocol.rs). So Cost is ALWAYS nil for Codex (even
// more strongly than CC, whose costUSD field merely happens to be absent). Cost
// is never derived from a pricing table (that would be a fabricated number).
//
// INV-2 (the load-bearing invariant) is enforced STRUCTURALLY, not by filtering:
// the rollout is decoded into `rolloutLine` / `rolloutTokenInfo` / `rolloutTokenUsage`
// — structs that contain ONLY numeric fields (and nested structs of numeric
// fields). Because encoding/json silently ignores unknown keys, every
// content-bearing location in the rollout (session_meta instructions, the
// agent_message text, response_item content, apply_patch bodies, shell commands,
// tool output, cwd, …) has NOWHERE to land — content cannot enter memory through
// this path, let alone reach an event, metadata, span, or the wire. The
// sentinel-content-absent test (usage_test.go) proves this end-to-end against the
// real signed wire body with content-capture ON (stripper disabled).
//
// INV-3: best-effort. A missing / null / oversized / malformed / partially-written
// rollout yields an error the caller logs and skips; it never fails the flush,
// blocks a tool call, or writes stdout. The read is bounded (maxRolloutBytes) so a
// giant rollout cannot exhaust memory.

// maxRolloutBytes bounds the rollout read so a pathological/huge rollout cannot
// exhaust memory (INV-3). A rollout larger than this is skipped WHOLE (honest: no
// partial/undercounted numbers) rather than truncated mid-stream. Mirrors the CC
// reader's 64 MiB cap.
const maxRolloutBytes = 64 << 20 // 64 MiB

// rolloutTokenUsage is the numbers-only projection of a Codex `TokenUsage`
// (codex-rs @ rust-v0.145.0). It has NO string/content fields BY DESIGN (INV-2).
// Only the three top-line counts are bound: the cache/reasoning sub-counts are
// subsets already inside input/output (see file doc), so binding them would be
// harmless-but-useless — they are deliberately dropped so the projection carries
// exactly the numbers we use.
type rolloutTokenUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

// rolloutTokenInfo is the numbers-only projection of `TokenUsageInfo`. Only the
// cumulative `total_token_usage` is bound; `last_token_usage` (the per-turn
// delta) and `model_context_window` are not needed for the session rollup and
// are dropped on decode.
type rolloutTokenInfo struct {
	TotalTokenUsage *rolloutTokenUsage `json:"total_token_usage"`
}

// rolloutPayload is the numbers-only projection of an EventMsg payload. Only the
// token `info` object is bound — the `type` discriminator string ("token_count",
// "agent_message", …) and every content sibling (message, delta, …) are dropped.
// A non-token event_msg line simply has no `info.total_token_usage`, so it
// contributes nothing (no discriminator read is needed — presence of the numeric
// path IS the classifier, matching the CC reader's projection-only discipline).
type rolloutPayload struct {
	Info *rolloutTokenInfo `json:"info"`
}

// rolloutLine is the numbers-only projection of one JSONL rollout line. Only the
// nested numeric token path (payload.info.total_token_usage) is reachable; the
// line's `type`/`timestamp`/`ordinal` and every other RolloutItem shape
// (session_meta, response_item, turn_context, world_state, …) are dropped on
// decode — none of them can bind content into this struct.
type rolloutLine struct {
	Payload *rolloutPayload `json:"payload"`
}

// readRolloutUsage reads a Codex rollout JSONL and returns the session's final
// cumulative usage NUMBERS ONLY. It is the production reader wired into the
// SessionEnd flush path (hookrun.go), fed the SessionEnd payload's transcript_path.
//
// Returns (nil, nil, nil) when the rollout carries no token counts at all (a
// valid session that never recorded usage — the caller then attaches nothing,
// same as finops-off). Returns an error (best-effort skip, INV-3) when the path
// is empty/null, missing, oversized, or unreadable. Cost is always nil (Codex's
// token path carries no cost field — see file doc).
func readRolloutUsage(path string) (*client.Tokens, *client.Cost, error) {
	if path == "" {
		return nil, nil, fmt.Errorf("no transcript_path")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, fmt.Errorf("open rollout: %w", err)
	}
	defer f.Close()
	// Stat the OPEN fd (not the path) so the regular-file check and the size bound
	// both refer to the same object we actually read — no stat/read TOCTOU where a
	// symlink swap or post-stat growth could bypass the cap (mirrors the CC reader).
	if fi, err := f.Stat(); err != nil {
		return nil, nil, fmt.Errorf("stat rollout: %w", err)
	} else if !fi.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("rollout is not a regular file")
	}
	// Read at most cap+1 bytes: reaching cap+1 means the file exceeds the cap, so
	// it is skipped WHOLE (honest — no truncated/undercounted parse) rather than
	// partially aggregated (INV-3, bounded so a giant rollout can't OOM).
	raw, err := io.ReadAll(io.LimitReader(f, maxRolloutBytes+1))
	if err != nil {
		return nil, nil, fmt.Errorf("read rollout: %w", err)
	}
	if len(raw) > maxRolloutBytes {
		return nil, nil, fmt.Errorf("rollout exceeds %d-byte cap", maxRolloutBytes)
	}
	return aggregateRolloutUsage(raw)
}

// aggregateRolloutUsage parses rollout JSONL bytes into a token rollup using the
// numbers-only projection. Split out from readRolloutUsage so the parser is
// testable without touching the filesystem (and so the sentinel test can feed raw
// bytes directly). Bad lines are skipped; only numeric fields are ever read.
//
// Because `total_token_usage` is a CUMULATIVE running total (see file doc), the
// rollup is the LAST valid snapshot — NOT a sum. Iterating in file (append)
// order and keeping the last snapshot that decodes cleanly yields the session's
// final cumulative total; a truncated/partial final line is skipped, honestly
// falling back to the previous complete cumulative rather than undercounting.
func aggregateRolloutUsage(raw []byte) (*client.Tokens, *client.Cost, error) {
	var latest rolloutTokenUsage
	var seen bool

	for _, line := range bytes.Split(raw, []byte{'\n'}) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var rl rolloutLine
		if err := json.Unmarshal(line, &rl); err != nil {
			// Partial final line / non-JSON marker / schema drift: skip it, keep the
			// last good snapshot (fault-tolerant, INV-3).
			continue
		}
		if rl.Payload != nil && rl.Payload.Info != nil && rl.Payload.Info.TotalTokenUsage != nil {
			latest = *rl.Payload.Info.TotalTokenUsage // last cumulative snapshot wins
			seen = true
		}
	}

	if !seen {
		return nil, nil, nil // valid, but nothing to report
	}

	// input_tokens / output_tokens / total_tokens are carried DIRECTLY: cached /
	// cache_write / reasoning are subsets already inside them (see file doc), so
	// they must NOT be added. nonNegRollout clamps any negative source value to 0
	// so emitted numbers satisfy the SL-1 schema `minimum: 0` (data-integrity).
	in := nonNegRollout(latest.InputTokens)
	out := nonNegRollout(latest.OutputTokens)
	total := nonNegRollout(latest.TotalTokens)
	// total_tokens should equal input+output (source: total_tokens == in + out),
	// but if a snapshot omitted it (0) fall back to the derived sum so Total is
	// never a spurious 0 while Input/Output are non-zero.
	if total == 0 {
		total = in + out
	}
	tokens := &client.Tokens{Input: intPtrRollout(in), Output: intPtrRollout(out), Total: intPtrRollout(total)}
	// Cost is ALWAYS nil: the Codex token path carries no cost field (file doc).
	return tokens, nil, nil
}

func intPtrRollout(v int) *int { return &v }

// nonNegRollout clamps a token count to >= 0: a malformed/negative source value
// must never produce a number that violates the SL-1 schema `minimum: 0`.
func nonNegRollout(v int) int {
	if v < 0 {
		return 0
	}
	return v
}
