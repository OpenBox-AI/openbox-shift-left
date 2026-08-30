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

// Its two mutation controls are the proof; deleting the redaction, or deleting
// the cap, must each turn it red.

const maxTranscriptBytes = 64 << 20 // 64 MiB

// usageNumbers is the numbers-only projection of a transcript turn's
// `message.usage`.
type usageNumbers struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

// transcriptLine is the numbers-only projection of one jsonl transcript line.
type transcriptLine struct {
	CostUSD *float64 `json:"costUSD"`
	Message *struct {
		Usage *usageNumbers `json:"usage"`
	} `json:"message"`
}

type turnLine struct {
	// Timestamp is parsed to a time.Time to compute the turn's duration_ms and
	// then discarded.
	Timestamp string `json:"timestamp"`
	// IsSidechain marks a line produced inside a subagent. Present on every
	// session line in real transcripts (measured: 13,439 of 13,439). It
	// partitions subagent usage out of the parent's window so the two are never
	// counted twice; see readTurnUsage.
	IsSidechain bool `json:"isSidechain"`
	Message     *struct {
		Model string `json:"model"`
		// 2.
		Content json.RawMessage `json:"content"`
		Usage   *usageNumbers   `json:"usage"`
	} `json:"message"`
}

// maxThinkingBytes bounds the thinking text one window may hold. A rune is at
// most 4 bytes, so this can never discard a byte the wire cap would have kept;
// the two bounds compose instead of fighting.
const maxThinkingBytes = 4 * 65536

// thinkingBlock is the one content-block shape this projection decodes. No
// other field of a content block is bound, so `text`, `tool_use` inputs and
// `tool_result` bodies cannot land here.
type thinkingBlock struct {
	Type     string `json:"type"`
	Thinking string `json:"thinking"`
}

// appendThinking folds one block's text onto the window's accumulator,
// bounded.
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
	if block = hookflow.TruncateBytes(block, room); block == "" {
		return acc
	}
	return acc + sep + block
}

// thinkingFrom lifts the thinking text out of one message's content blocks, in
// file order.
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

// turnWindow is one turn's worth of transcript, aggregated. So these are
// window sums, and no caller should read them as per-model-call numbers; hooks
// cannot deliver that.
type turnWindow struct {
	Input              int
	Output             int
	CacheCreationInput int
	CacheRead          int
	// Model is the last non-empty model id IN this window. Never carried across
	// windows and never back-filled from the session's SessionStart model:
	// attributing a window's tokens to a model that may not have spent them is a
	// fabricated number, the same class of error as deriving a cost.
	Model string
	// Open is the first parsable line timestamp in the window; the turn's real
	// start, used only to compute duration_ms.
	Open time.Time
	// HasUsage reports whether any line in the window carried usage at all.
	HasUsage bool
	// Thinking is the window's `thinking` content blocks, concatenated in file
	// order and bounded at maxThinkingBytes. The lift is deliberately ungated and
	// the gate sits at attachment (Mapper.MapTurn's CaptureContent, then the
	// client's stripContent).
	Thinking string
}

func (w turnWindow) total() int {
	return w.Input + w.Output + w.CacheCreationInput + w.CacheRead
}

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
// usage numbers in that window only, returning the next cursor position so the
// caller can advance after a successful spool (never before; see
// hookflow.TurnCursor for why that ordering is the correctness argument).
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
	fi, err := f.Stat()
	if err != nil {
		return turnWindow{}, next, fmt.Errorf("stat transcript: %w", err)
	}
	if !fi.Mode().IsRegular() {
		return turnWindow{}, next, fmt.Errorf("transcript is not a regular file")
	}
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

	// Memory stays bounded (turnChunkBytes at a time) because the aggregation is
	// a running sum; the window never has to be resident.
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
			lastNL := bytes.LastIndexByte(chunk, '\n')
			if lastNL < 0 {
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
			return turnWindow{}, next, fmt.Errorf("read transcript: %w", readErr)
		}
	}

	if consumed == 0 {
		return turnWindow{}, next, nil // only an incomplete line so far
	}
	next = hookflow.TurnPos{Offset: offset + consumed, Index: from.Index}
	return w, next, nil
}

// turnChunkBytes bounds one read of the transcript window. MaxTranscriptBytes
// still bounds a single pathological line, which is the only thing that cannot
// be streamed.
const turnChunkBytes = 4 << 20 // 4 MiB

// aggregateTurnWindow sums one window's usage from jsonl bytes. Bad lines are
// skipped, never fatal.
func aggregateTurnWindow(raw []byte, sidechain bool) turnWindow {
	var w turnWindow
	aggregateTurnWindowInto(&w, raw, sidechain)
	return w
}

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
		if tl.IsSidechain != sidechain {
			continue
		}
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
		w.Thinking = appendThinking(w.Thinking, thinkingFrom(tl.Message.Content))
		if u := tl.Message.Usage; u != nil {
			w.Input += nonNeg(u.InputTokens)
			w.Output += nonNeg(u.OutputTokens)
			w.CacheCreationInput += nonNeg(u.CacheCreationInputTokens)
			w.CacheRead += nonNeg(u.CacheReadInputTokens)
			w.HasUsage = true
		}
	}
}

func readTranscriptUsage(path string) (*client.Tokens, *client.Cost, error) {
	if path == "" {
		return nil, nil, fmt.Errorf("no transcript_path")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, fmt.Errorf("open transcript: %w", err)
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil {
		return nil, nil, fmt.Errorf("stat transcript: %w", err)
	} else if !fi.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("transcript is not a regular file")
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxTranscriptBytes+1))
	if err != nil {
		return nil, nil, fmt.Errorf("read transcript: %w", err)
	}
	if len(raw) > maxTranscriptBytes {
		return nil, nil, fmt.Errorf("transcript exceeds %d-byte cap", maxTranscriptBytes)
	}
	return aggregateUsage(raw)
}

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
			continue
		}
		if tl.Message != nil && tl.Message.Usage != nil {
			u := tl.Message.Usage
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

func nonNeg(v int) int {
	if v < 0 {
		return 0
	}
	return v
}
