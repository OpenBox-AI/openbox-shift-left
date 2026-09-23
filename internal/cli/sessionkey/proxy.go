package sessionkey

// ccProxyHeader is Claude Code's own relayed-call session header.
const ccProxyHeader = "X-Claude-Code-Session-Id"

// codexThreadIDHeader is Codex's thread id, sent alongside every request
// (codex-rs core/src/client.rs:1304-1305): a direct header insert of
// the same value build_session_headers separately sends as "thread-id".
// Canonicalized (net/textproto.CanonicalMIMEHeaderKey), matching the map
// gateway.Captured.RequestHeaders already carries.
const codexThreadIDHeader = "X-Client-Request-Id"

// codexPromptCacheHeader named for documentation and the negative test only
// -- ResolveProxy never reads it. codex-rs's build_session_headers sends
// Codex's own "session-id" header as responses_session_id(...), which for a
// root session is prompt_cache_key(metadata) (client.rs:587-591), NOT the
// thread id. Keying on it would merge unrelated sessions that happen to
// share a prompt-cache key.
const codexPromptCacheHeader = "Session-Id"

// ProxyHeader returns the single HTTP header a provider's relayed-call
// session id rides on, or "" when the provider's proxy-lane carrier is not a
// single header value (Codex's is the thread id folded out of
// x-client-request-id by ResolveProxy, not a header this function names).
func ProxyHeader(p Provider) string {
	if p == ClaudeCode {
		return ccProxyHeader
	}
	return ""
}

// ResolveProxy extracts the session key for a governed relay call from its
// captured request headers -- the same already-redacted,
// canonical-header-name map gateway.Captured.RequestHeaders carries.
// credentialHeaders (internal/gateway/capture.go) never touches either
// header this function reads, so the carrier survives redaction intact.
//
// ok is false for an absent header, which the caller must treat exactly like
// any other headerless call: skip and count, never mint. This function
// does no further shape validation -- gatewayemit's usableSessionID and
// telemetryemit's safeRequestID apply two DIFFERENT character rules for two
// different reasons (a namespace argument vs. a bare identifier), and
// unifying them here would silently pick one for both; a caller that needs a
// shape check still runs its own after calling this.
//
// Codex's own "session-id" header (codexPromptCacheHeader) is deliberately
// never read here: see its comment. Codex's session key is the thread id on
// x-client-request-id instead.
func ResolveProxy(p Provider, headers map[string]string) (string, bool) {
	var id string
	switch p {
	case Codex:
		id = headers[codexThreadIDHeader]
	default:
		if h := ProxyHeader(p); h != "" {
			id = headers[h]
		}
	}
	if id == "" {
		return "", false
	}
	return id, true
}
