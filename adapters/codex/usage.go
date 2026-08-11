package codex

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/openbox-ai/openbox-shift-left/client"
)

// Codex finops / token-usage extraction (default ON; opt out with
// finops:false or OPENBOX_FINOPS=0).
//
// Codex hooks expose no token/cost usage (capabilities.go), but the
// session's on-disk rollout JSONL — the file `transcript_path` points at
// on SessionEnd — carries running token counts. Behind the finops gate
// (ResolveFinops — the same separate flag Claude Code uses, not
// content_capture; default ON as of ADR-0014), the adapter reads it on
// SessionEnd (off the hot path, after Codex has flushed the transcript) to
// populate client.Tokens on the SessionEnded event AND the session-rollup
// llm_completion activity pair.
//
// Grounded wire shape (real source, codex-rs @ rust-v0.145.0 — recorded in
// the testdata fixture, not guessed; the box carried no live rollout to
// sample so the shape was pinned from the shipped structs and the
// 0.145.0 binary strings):
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
// Two Codex-specific facts drive the aggregation (both source-verified,
// and the deliberate divergence from the Claude Code reader):
//
//  1. `total_token_usage` is a cumulative running session total, not a
//     per-turn delta: TokenUsageInfo::append_last_usage does
//     `total_token_usage.add_assign(last)` (protocol.rs @ rust-v0.145.0).
//     Each successive token_count line carries a larger cumulative
//     snapshot. So the rollup is the last valid snapshot — not a sum
//     across lines (summing would multiply-count every prior turn). CC,
//     by contrast, sums per-turn usages.
//  2. `cached_input_tokens` / `cache_write_input_tokens` /
//     `reasoning_output_tokens` are subsets already folded inside
//     input_tokens / output_tokens (OpenAI accounting):
//     TokenUsage::non_cached_input == input_tokens - cached_input_tokens
//     (protocol.rs). total_tokens is Codex's own independently-summed
//     field (we carry it verbatim, not recomputed). So we carry
//     input_tokens / output_tokens / total_tokens directly and must not
//     add the cache/reasoning sub-counts (adding would double-count).
//     CC's cache tokens are additive and folded into Input; Codex's are
//     already inside it.
//
// Cost: the token path carries no cost/price field at all —
// TokenCountEvent is only {info, rate_limits} (protocol.rs). So Cost is
// always nil for Codex (even more strongly than CC, whose costUSD field
// merely happens to be absent). Cost is never derived from a pricing
// table (that would be a fabricated number).
//
// INV-2 (the load-bearing invariant) is enforced structurally, not by
// filtering: the rollout is decoded into `rolloutLine` /
// `rolloutTokenInfo` / `rolloutTokenUsage` — structs that contain only
// numeric fields (and nested structs of numeric fields). Because
// encoding/json silently ignores unknown keys, every content-bearing
// location in the rollout (session_meta instructions, the agent_message
// text, response_item content, apply_patch bodies, shell commands, tool
// output, cwd, …) has nowhere to land — content cannot enter memory
// through this path, let alone reach an event, metadata, span, or the
// wire. The sentinel-content-absent test (usage_test.go) proves this
// end-to-end against the real signed wire body with content-capture on
// (stripper disabled).
//
// INV-3: best-effort. A missing / null / oversized / malformed /
// partially-written rollout yields an error the caller logs and skips; it
// never fails the flush, blocks a tool call, or writes stdout. The read
// is bounded (maxRolloutBytes) so a giant rollout cannot exhaust memory.

// maxRolloutBytes bounds the rollout read so a pathological/huge rollout
// cannot exhaust memory (INV-3). A rollout larger than this is skipped
// whole (honest: no partial/undercounted numbers) rather than truncated
// mid-stream. Mirrors the CC reader's 64 MiB cap.
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
	// CachedInputTokens / CacheWriteInputTokens are SUB-COUNTS of InputTokens,
	// unlike Claude Code's cache counts which are additive siblings. Bound as of
	// contract v1.1 so they can be reported in their own fields — which means
	// Input must have them SUBTRACTED out to be pure input, not added to it.
	// See aggregateRolloutUsage for the arithmetic and the evidence.
	//
	// ReasoningOutputTokens stays unbound: it is a sub-count of OutputTokens with
	// no field in the contract, and binding a number we cannot report would
	// invite someone to add it to Output and double-count.
	CachedInputTokens     int `json:"cached_input_tokens"`
	CacheWriteInputTokens int `json:"cache_write_input_tokens"`
}

// rolloutTokenInfo is the numbers-only projection of `TokenUsageInfo`. Only the
// cumulative `total_token_usage` is bound; `last_token_usage` (the per-turn
// delta) and `model_context_window` are not needed for the session rollup and
// are dropped on decode.
type rolloutTokenInfo struct {
	TotalTokenUsage *rolloutTokenUsage `json:"total_token_usage"`
}

// rolloutPayload is the projection of a rollout line's payload: the token `info`
// object, plus the model id. The `type` discriminator string ("token_count",
// "agent_message", …) and every content sibling (message, delta, command,
// stdout, instructions, developer_instructions, …) are dropped. A non-token
// event_msg line simply has no `info.total_token_usage`, so it contributes
// nothing — presence of the numeric path IS the classifier, matching the CC
// reader's projection-only discipline.
type rolloutPayload struct {
	Info *rolloutTokenInfo `json:"info"`
	// Model is the ONE string this projection egresses, and it appears at exactly
	// this path: `turn_context.payload.model`. Verified against 12 real rollouts
	// (~/.codex/sessions, codex 0.145.x): `payload.model` occurs 363 times, which
	// is exactly the number of `turn_context` lines — so no other line type puts a
	// model at the top level of its payload.
	//
	// The nested siblings that also spell "model" —
	// `payload.collaboration_mode.settings.model` and
	// `payload.thread_settings.model` — are unreachable from here, which is the
	// point: `turn_context.payload` is a CONTENT-RICH object (it carries
	// `developer_instructions`, `cwd`, `workspace_roots`, `personality`), and this
	// struct can bind exactly one field of it.
	Model string `json:"model"`
}

// rolloutLine is the projection of one JSONL rollout line. Only the nested
// numeric token path (payload.info.total_token_usage) and payload.model are
// reachable; the line's `type`/`timestamp`/`ordinal` and every other RolloutItem
// shape (session_meta, response_item, turn_context, world_state, compacted, …)
// are dropped on decode — none of them can bind content into this struct.
type rolloutLine struct {
	Payload *rolloutPayload `json:"payload"`
}

// readRolloutUsage reads a Codex rollout JSONL and returns the session's final
// cumulative usage numbers plus the model that ran it. It is the production
// reader wired into the SessionEnd flush path (hookrun.go), fed the SessionEnd
// payload's transcript_path.
//
// Returns (nil, "", nil) when the rollout carries no token counts at all (a
// valid session that never recorded usage — the caller then attaches nothing,
// same as finops-off). Returns an error (best-effort skip, INV-3) when the path
// is empty/null, missing, oversized, or unreadable.
//
// Cost is not returned at all, because Codex's token path carries no cost field
// (TokenCountEvent is only {info, rate_limits} — protocol.rs) and cost is never
// derived from a pricing table here.
func readRolloutUsage(path string) (*client.Tokens, string, error) {
	if path == "" {
		return nil, "", fmt.Errorf("no transcript_path")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, "", fmt.Errorf("open rollout: %w", err)
	}
	defer f.Close()
	// Stat the OPEN fd (not the path) so the regular-file check and the size bound
	// both refer to the same object we actually read — no stat/read TOCTOU where a
	// symlink swap or post-stat growth could bypass the cap (mirrors the CC reader).
	if fi, err := f.Stat(); err != nil {
		return nil, "", fmt.Errorf("stat rollout: %w", err)
	} else if !fi.Mode().IsRegular() {
		return nil, "", fmt.Errorf("rollout is not a regular file")
	}
	// Read at most cap+1 bytes: reaching cap+1 means the file exceeds the cap, so
	// it is skipped WHOLE (honest — no truncated/undercounted parse) rather than
	// partially aggregated (INV-3, bounded so a giant rollout can't OOM).
	raw, err := io.ReadAll(io.LimitReader(f, maxRolloutBytes+1))
	if err != nil {
		return nil, "", fmt.Errorf("read rollout: %w", err)
	}
	if len(raw) > maxRolloutBytes {
		return nil, "", fmt.Errorf("rollout exceeds %d-byte cap", maxRolloutBytes)
	}
	tokens, model := aggregateRolloutUsage(raw)
	return tokens, model, nil
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
// It also returns the session's model id — the last non-empty
// `turn_context.payload.model` in the rollout, which is the model in effect when
// the session ended. Empty when the rollout names none, in which case the pair is
// still emitted and the core-side extractor buckets it as unknown; never
// substituted from anywhere else.
func aggregateRolloutUsage(raw []byte) (*client.Tokens, string) {
	var latest rolloutTokenUsage
	var seen bool
	var model string

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
		if rl.Payload == nil {
			continue
		}
		if rl.Payload.Model != "" {
			model = rl.Payload.Model // last non-empty turn_context wins
		}
		if rl.Payload.Info != nil && rl.Payload.Info.TotalTokenUsage != nil {
			latest = *rl.Payload.Info.TotalTokenUsage // last cumulative snapshot wins
			seen = true
		}
	}

	if !seen {
		return nil, model // valid, but nothing to report
	}

	// The four counts, and why Codex's arithmetic is the INVERSE of Claude Code's.
	//
	// Claude Code's cache counts are additive siblings of input_tokens, so its
	// pure input is input_tokens as given. Codex's are SUB-COUNTS already inside
	// input_tokens, so pure input is input_tokens MINUS them. Adding them here —
	// the shape of the Claude Code reader — would double-count the cache on every
	// Codex session.
	//
	// Evidence, from 12 real rollouts (~/.codex/sessions, codex 0.145.x) and the
	// pinned fixture:
	//
	//	input 14718, cached 9984, output 232, total 14950 → 14718 + 232 == 14950
	//	input   160, cached   40, cache_write 5, output 35, total 195 → 160 + 35 == 195
	//
	// total_tokens == input_tokens + output_tokens exactly, with cached and
	// cache_write contributing nothing on top — so both are inside input_tokens.
	// (`TokenUsage::non_cached_input == input_tokens - cached_input_tokens`,
	// protocol.rs @ rust-v0.145.0, states it for cached; the arithmetic above is
	// what establishes it for cache_write, which that formula does not mention and
	// which is ABSENT from every real rollout sampled — only the fixture carries
	// it.) reasoning_output_tokens is likewise a sub-count of output_tokens and is
	// not bound at all.
	//
	// nonNegRollout clamps any negative source value to 0 so emitted numbers
	// satisfy the schema `minimum: 0`; the subtraction is clamped for the same
	// reason, since a malformed snapshot could report caches exceeding input.
	rawIn := nonNegRollout(latest.InputTokens)
	cacheRead := nonNegRollout(latest.CachedInputTokens)
	cacheWrite := nonNegRollout(latest.CacheWriteInputTokens)
	in := nonNegRollout(rawIn - cacheRead - cacheWrite)
	out := nonNegRollout(latest.OutputTokens)
	total := nonNegRollout(latest.TotalTokens)
	// total_tokens is Codex's own independently-summed field and is carried
	// verbatim, not recomputed. If a snapshot omitted it (0), fall back to the
	// derived sum so Total is never a spurious 0 while the parts are non-zero.
	if total == 0 {
		total = in + out + cacheRead + cacheWrite
	}
	tokens := &client.Tokens{
		Input:              intPtrRollout(in),
		Output:             intPtrRollout(out),
		CacheCreationInput: intPtrRollout(cacheWrite),
		CacheRead:          intPtrRollout(cacheRead),
		Total:              intPtrRollout(total),
	}
	return tokens, model
}

func intPtrRollout(v int) *int { return &v }

// nonNegRollout clamps a token count to >= 0: a malformed/negative source
// value must never produce a number that violates the schema `minimum: 0`.
func nonNegRollout(v int) int {
	if v < 0 {
		return 0
	}
	return v
}
