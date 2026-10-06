package providers

import (
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/codex"
)

// The Codex rollout content reader, reached by the CLI through this registry
// like every other adapter surface: the telemetry daemon's enricher reads a
// call's request and response from the thread's rollout.

type (
	// CodexCallJoin is what the telemetry record says about a call: its time and counts.
	CodexCallJoin = codex.CallJoin
	// CodexCallContent is one call's items.
	CodexCallContent = codex.CallContent
	// CodexContentState is what a rollout read settled on.
	CodexContentState = codex.ContentState
)

const (
	CodexContentVerified   = codex.ContentVerified
	CodexContentLogAbsent  = codex.ContentLogAbsent
	CodexContentNoJoin     = codex.ContentNoJoin
	CodexContentUnverified = codex.ContentUnverified
	CodexContentTimeout    = codex.ContentTimeout
)

// CodexSessionsRoot is <CODEX_HOME>/sessions as this process resolves it.
func CodexSessionsRoot() string { return codex.SessionsRoot() }

// ReadCodexCallContent finds a call in a thread's rollout; see codex.ReadCallContent.
func ReadCodexCallContent(sessionsRoot, threadID string, join CodexCallJoin, deadline time.Time) (CodexCallContent, CodexContentState) {
	return codex.ReadCallContent(sessionsRoot, threadID, join, deadline)
}

// CodexRequestBodyJSON renders a call's input items as a bounded, redacted body.
func CodexRequestBodyJSON(c CodexCallContent, redact func(string) string) string {
	return codex.RequestBodyJSON(c.Request, redact)
}

// CodexResponseBodyJSON renders a call's output items as a bounded, redacted body.
func CodexResponseBodyJSON(c CodexCallContent, redact func(string) string) string {
	return codex.ResponseBodyJSON(c.ResponseID, c.Response, redact)
}
