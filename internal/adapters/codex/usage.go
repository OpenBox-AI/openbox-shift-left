package codex

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
	"github.com/openbox-ai/openbox-shift-left/internal/client"
)

// So we carry input_tokens / output_tokens / total_tokens directly and must
// not add the cache/reasoning sub-counts (adding would double-count).

const maxRolloutBytes = 64 << 20 // 64 MiB

// rolloutTokenUsage is the numbers-only projection of a Codex `TokenUsage`
// (codex-rs @ rust-v0.145.0).
type rolloutTokenUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
	// CachedInputTokens / CacheWriteInputTokens are SUB-counts of InputTokens,
	// unlike Claude Code's cache counts which are additive siblings. Bound as of
	// contract v1.1 so they can be reported in their own fields; which means
	// Input must have them subtracted out to be pure input, not added to it.
	CachedInputTokens     int `json:"cached_input_tokens"`
	CacheWriteInputTokens int `json:"cache_write_input_tokens"`
}

type rolloutTokenInfo struct {
	TotalTokenUsage *rolloutTokenUsage `json:"total_token_usage"`
}

const rolloutReasoningType = "reasoning"

// rolloutReasoningSummary is the ONLY reasoning field bound. A reasoning record
// also carries `encrypted_content`, an opaque blob that is deliberately absent
// from this struct: a field with no Go name cannot be egressed by any later
// edit, which is a stronger guarantee than remembering not to send it.
type rolloutReasoningSummary struct {
	Text string `json:"text"`
}

// rolloutPayload is the ALLOWLIST of rollout fields this reader may bind.
//
// It used to be a numbers-only projection, and that structure was itself the
// guarantee that no content escaped. Capturing reasoning gives that up, so the
// guarantee becomes declarative instead: this struct is the complete list, and
// TestRolloutAllowlistIsExhaustive fails when the rollout grows a payload field
// that nobody has classified as bind-or-ignore. A new field must be looked at by
// a person before it can ride along.
type rolloutPayload struct {
	Info *rolloutTokenInfo `json:"info"`
	// Model is the ONE string the numbers projection egresses, and it appears at
	// exactly this path: `turn_context.payload.model`.
	Model string `json:"model"`
	// Type discriminates a response_item; only "reasoning" is read.
	Type string `json:"type"`
	// Summary is the reasoning summary text (content, gated on content_capture and
	// redacted before attachment, like every other content field).
	Summary []rolloutReasoningSummary `json:"summary"`
}

// rolloutAllowedPayloadFields is the classification the exhaustiveness test
// checks against: every payload key the reader may encounter, and whether it is
// bound or deliberately ignored.
var rolloutAllowedPayloadFields = map[string]string{
	"info":    "bound: token counts (numbers only)",
	"model":   "bound: model id",
	"type":    "bound: discriminator",
	"summary": "bound: reasoning summary text (content-gated, redacted)",

	"encrypted_content":                          "IGNORED: opaque provider blob, never egressed",
	"internal_chat_message_metadata_passthrough": "IGNORED: internal correlation only",
	"id":               "IGNORED: provider-local record id",
	"content":          "IGNORED: message bodies are not a Codex content class",
	"role":             "IGNORED: structural",
	"call_id":          "IGNORED: structural",
	"name":             "IGNORED: structural",
	"arguments":        "IGNORED: tool arguments are not egressed by this reader",
	"output":           "IGNORED: tool output is not a Codex content class",
	"status":           "IGNORED: structural",
	"cwd":              "IGNORED: structural",
	"message":          "IGNORED: structural",
	"last_token_usage": "IGNORED: per-CALL usage; the turn number is a window delta (P0.7b)",
}

type rolloutLine struct {
	Payload *rolloutPayload `json:"payload"`
}

// readRolloutUsage returns (nil, "", nil) when the rollout carries no token
// counts at all (a valid session that never recorded usage; the caller then
// attaches nothing, same as finops-off).
func readRolloutUsage(path string) (*client.Tokens, string, error) {
	if path == "" {
		return nil, "", fmt.Errorf("no transcript_path")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, "", fmt.Errorf("open rollout: %w", err)
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil {
		return nil, "", fmt.Errorf("stat rollout: %w", err)
	} else if !fi.Mode().IsRegular() {
		return nil, "", fmt.Errorf("rollout is not a regular file")
	}
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

// aggregateRolloutUsage empty when the rollout names none, in which case the
// pair is still emitted and the core-side extractor buckets it as unknown;
// never substituted from anywhere else.
func aggregateRolloutUsage(raw []byte) (*client.Tokens, string) {
	var latest rolloutTokenUsage
	var seen bool
	var model string

	for line := range bytes.SplitSeq(raw, []byte{'\n'}) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var rl rolloutLine
		if err := json.Unmarshal(line, &rl); err != nil {
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

	rawIn := nonNegRollout(latest.InputTokens)
	cacheRead := nonNegRollout(latest.CachedInputTokens)
	cacheWrite := nonNegRollout(latest.CacheWriteInputTokens)
	in := nonNegRollout(rawIn - cacheRead - cacheWrite)
	out := nonNegRollout(latest.OutputTokens)
	total := nonNegRollout(latest.TotalTokens)
	// If a snapshot omitted it (0), fall back to the derived sum so Total is
	// never a spurious 0 while the parts are non-zero.
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

func nonNegRollout(v int) int {
	if v < 0 {
		return 0
	}
	return v
}

// ---------------------------------------------------------------------------
// Per-turn windowed reader (phase 03).
//
// aggregateRolloutUsage above stays exactly as it was: the SessionEnd rollup
// still depends on it byte-for-byte. This is a second reader beside it, not a
// rewrite of it.
//
// Codex's rollout carries CUMULATIVE snapshots -- verified on a real 93-snapshot
// rollout: total_token_usage is monotonically non-decreasing, and its delta
// equals last_token_usage on every transition (phase 00 probe P0.7b). So a
// per-turn number is a DELTA between two snapshots, never a read of one.
//
// last_token_usage is deliberately not used as the per-turn value: it is the
// last model CALL's usage, which equals the turn only when the turn made exactly
// one call. The window delta is right in both cases.
// ---------------------------------------------------------------------------

// maxThinkingBytes bounds the reasoning text gathered from one window before it
// is handed to the mapper. It must stay comfortably ABOVE the client's own wire
// cap, which counts 65536 RUNES: a rune is up to 4 bytes, so a byte bound below
// 4x the rune cap would truncate here, in a unit the wire does not measure in,
// and the client's cap would never get the chance to do its job. The bound has
// one owner and it is the client (CLAUDE.md, "Bounds have owners").
const maxThinkingBytes = 4 * 65536

// turnWindow is one turn's usage delta plus the model and reasoning text in
// effect for it.
type turnWindow struct {
	Input              int
	Output             int
	CacheCreationInput int
	CacheRead          int
	Total              int
	// Model is the last non-empty model id IN this window, never carried across
	// windows and never back-filled from the session's start: attributing a
	// window's tokens to a model that may not have spent them is a fabricated
	// number, the same class of error as deriving a cost.
	Model string
	// Thinking is the reasoning SUMMARY text in this window. encrypted_content is
	// never bound (see rolloutAllowedPayloadFields).
	Thinking string
	// HasUsage reports whether the window's delta carried any usage at all.
	HasUsage bool
}

func (w turnWindow) tokens() *client.Tokens {
	return &client.Tokens{
		Input:              intPtrRollout(w.Input),
		Output:             intPtrRollout(w.Output),
		CacheCreationInput: intPtrRollout(w.CacheCreationInput),
		CacheRead:          intPtrRollout(w.CacheRead),
		Total:              intPtrRollout(w.Total),
	}
}

// normalizeRollout applies the SAME cache-as-sub-count normalization the session
// rollup applies, so a turn delta and the rollup total are computed in one unit.
// Codex's cache counts are sub-counts OF input_tokens, unlike Claude Code's
// additive siblings; adding them would double-count.
func normalizeRollout(u rolloutTokenUsage) (in, out, cacheRead, cacheWrite, total int) {
	rawIn := nonNegRollout(u.InputTokens)
	cacheRead = nonNegRollout(u.CachedInputTokens)
	cacheWrite = nonNegRollout(u.CacheWriteInputTokens)
	in = nonNegRollout(rawIn - cacheRead - cacheWrite)
	out = nonNegRollout(u.OutputTokens)
	total = nonNegRollout(u.TotalTokens)
	if total == 0 {
		total = in + out + cacheRead + cacheWrite
	}
	return
}

// readTurnUsage computes one turn's window in a single pass, tracking byte
// position so the cursor's Offset selects the boundary.
//
//   - prev = the last cumulative snapshot whose line ENDS at or before pos.Offset
//   - cur  = the last cumulative snapshot overall
//   - delta = normalize(cur) - normalize(prev), floored at 0
//
// An Offset beyond the file re-anchors to 0 rather than emitting a negative or
// giant delta: /compact and rotation can rewrite a rollout underneath us.
func readTurnUsage(path string, pos hookflow.TurnPos) (turnWindow, hookflow.TurnPos, error) {
	var w turnWindow
	next := pos

	raw, err := readRolloutBytes(path)
	if err != nil {
		return w, next, err
	}
	if pos.Offset > int64(len(raw)) {
		pos.Offset = 0 // rotated or truncated ⇒ re-anchor
	}

	var prev, cur rolloutTokenUsage
	var sawPrev, sawCur bool
	var thinking []string
	thinkingBytes := 0

	// start is where this line's bytes begin; contentEnd is where they END, NOT
	// counting the newline that may or may not follow. That distinction is the
	// whole correctness of the window: the cursor is stored as len(raw), so a
	// rollout whose last line is not yet newline-terminated would, on the next
	// read, recompute that line's end as cursor+1 and re-classify an
	// already-counted line as in-window -- counting its tokens twice AND losing
	// the `prev` baseline, which degrades the delta to the whole cumulative total.
	var start int64
	for _, line := range bytes.Split(raw, []byte{'\n'}) {
		contentEnd := start + int64(len(line))
		start = contentEnd + 1 // the separator bytes.Split consumed
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) == 0 {
			continue
		}
		var rl rolloutLine
		if json.Unmarshal(trimmed, &rl) != nil || rl.Payload == nil {
			continue
		}
		inWindow := contentEnd > pos.Offset

		if rl.Payload.Model != "" && inWindow {
			w.Model = rl.Payload.Model
		}
		if rl.Payload.Info != nil && rl.Payload.Info.TotalTokenUsage != nil {
			cur, sawCur = *rl.Payload.Info.TotalTokenUsage, true
			if !inWindow {
				prev, sawPrev = cur, true
			}
		}
		if inWindow && rl.Payload.Type == rolloutReasoningType {
			for _, s := range rl.Payload.Summary {
				if s.Text == "" || thinkingBytes >= maxThinkingBytes {
					continue
				}
				thinking = append(thinking, s.Text)
				thinkingBytes += len(s.Text)
			}
		}
	}

	next.Offset = int64(len(raw))
	if !sawCur {
		return w, next, nil // nothing to report; the caller advances and emits nothing
	}

	curIn, curOut, curRead, curWrite, curTotal := normalizeRollout(cur)
	var prevIn, prevOut, prevRead, prevWrite, prevTotal int
	if sawPrev {
		prevIn, prevOut, prevRead, prevWrite, prevTotal = normalizeRollout(prev)
	}
	w.Input = nonNegRollout(curIn - prevIn)
	w.Output = nonNegRollout(curOut - prevOut)
	w.CacheRead = nonNegRollout(curRead - prevRead)
	w.CacheCreationInput = nonNegRollout(curWrite - prevWrite)
	w.Total = nonNegRollout(curTotal - prevTotal)
	w.HasUsage = w.Input+w.Output+w.CacheRead+w.CacheCreationInput+w.Total > 0
	w.Thinking = strings.Join(thinking, "\n")
	return w, next, nil
}

// readRolloutBytes is the shared bounded read both readers use.
func readRolloutBytes(path string) ([]byte, error) {
	if path == "" {
		return nil, fmt.Errorf("no transcript_path")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open rollout: %w", err)
	}
	defer f.Close()
	if fi, sErr := f.Stat(); sErr != nil {
		return nil, fmt.Errorf("stat rollout: %w", sErr)
	} else if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("rollout is not a regular file")
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxRolloutBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read rollout: %w", err)
	}
	if len(raw) > maxRolloutBytes {
		return nil, fmt.Errorf("rollout exceeds %d-byte cap", maxRolloutBytes)
	}
	return raw, nil
}
