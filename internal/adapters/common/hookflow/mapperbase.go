package hookflow

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/client"
)

// NowOr is now() when an adapter's Mapper has an injected clock, else
// time.Now().
func NowOr(now func() time.Time) time.Time {
	if now != nil {
		return now()
	}
	return time.Now()
}

// RedactWith runs a content body through redact before it is attached; a nil
// redact is the identity, the honest `secret_detection:false` case.
func RedactWith(redact func(string) string, s string) string {
	if redact == nil {
		return s
	}
	return redact(s)
}

// EventIDOr is newID() when a test pinned the idempotency-id source, else
// derive(ev).
func EventIDOr(newID func() string, derive func(client.DevEvent) string, ev client.DevEvent) string {
	if newID != nil {
		return newID()
	}
	return derive(ev)
}

// DeriveID is the deterministic idempotency id every adapter derives: prefix
// plus the sha256 of the event's identifying fields joined by 0x1f. beforeSpan
// and afterSpan are the adapter's own distinguishers, each written after a
// separator, before and after the span fields. The same logical event always
// yields the same id, so the byte layout here is a shipped contract.
func DeriveID(prefix string, ev client.DevEvent, beforeSpan, afterSpan []string) string {
	const sep = 0x1f
	var b strings.Builder
	b.WriteString(ev.SessionID)
	b.WriteByte(sep)
	b.WriteString(string(ev.EventType))
	b.WriteByte(sep)
	b.WriteString(ev.Tool.Name)
	b.WriteByte(sep)
	b.WriteString(ev.Timestamp) // RFC3339Nano; the per-event distinguisher
	for _, s := range beforeSpan {
		b.WriteByte(sep)
		b.WriteString(s)
	}
	if ev.Span != nil {
		b.WriteByte(sep)
		b.WriteString(ev.Span.FilePath)
		b.WriteByte(sep)
		b.WriteString(ev.Span.Function) // the MCP function name
		b.WriteByte(sep)
		b.WriteString(ev.Span.InvocationID)
	}
	for _, s := range afterSpan {
		b.WriteByte(sep)
		b.WriteString(s)
	}
	sum := sha256.Sum256([]byte(b.String()))
	return prefix + hex.EncodeToString(sum[:])
}

// SessionStartMetadata is the SessionStarted event's structural metadata:
// provider, the source and permission-mode enums (each folded through its
// adapter's known values), and the bounded model id and cwd.
func SessionStartMetadata(provider, source string, sources map[string]bool, model, cwd, permissionMode string, permissionModes map[string]bool) map[string]any {
	return Compact(map[string]any{
		"provider":        provider,
		"source":          EnumOr(source, sources),
		"model":           CapIdent(model), // free-form model id → bounded
		"cwd":             CapIdent(cwd),   // structural; not content
		"permission_mode": EnumOr(permissionMode, permissionModes),
	})
}

// CanonicalJSONEqual reports whether a and b are the same JSON value once both
// are re-encoded canonically; false when either is empty or does not parse.
func CanonicalJSONEqual(a, b []byte) bool {
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	var av, bv any
	if json.Unmarshal(a, &av) != nil || json.Unmarshal(b, &bv) != nil {
		return false
	}
	ac, aErr := json.Marshal(av)
	bc, bErr := json.Marshal(bv)
	return aErr == nil && bErr == nil && bytes.Equal(ac, bc)
}

// TurnContent is a turn's completed-half content: the reply and the thinking,
// each redacted, under capture only. Nil when capture is off or there is
// nothing to report. A turn's input is the prompt, which already rides
// PromptSubmitted under the same gate, so the started half carries none.
func TurnContent(capture bool, redact func(string) string, output, thinking string) *client.Content {
	if !capture {
		return nil
	}
	var c client.Content
	if output != "" {
		c.Output = RedactWith(redact, output)
	}
	if thinking != "" {
		c.Thinking = RedactWith(redact, thinking)
	}
	if c.Output == "" && c.Thinking == "" {
		return nil
	}
	return &c
}

// GatedText is text redacted for attachment under the capture gate: "" when
// capture is off, when there is no text, or when redaction leaves nothing --
// "" is never a fact anyone asserted, so a caller attaches nothing for it.
func GatedText(capture bool, redact func(string) string, text string) string {
	if !capture || text == "" {
		return ""
	}
	return RedactWith(redact, text)
}
