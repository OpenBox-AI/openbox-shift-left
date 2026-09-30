package sessionkey

import "strings"

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

// Skip reasons AttributeProxy reports instead of a provider. Both mean the
// call is skipped and counted, never guessed.
const (
	// SkipNoProviderCarrier: no candidate's carrier resolved.
	SkipNoProviderCarrier = "no_provider_carrier"
	// SkipAmbiguousCarrier: more than one candidate's carrier resolved.
	SkipAmbiguousCarrier = "ambiguous_carrier"
)

// codexOriginatorHeader and codexOriginatorPrefix are the Codex-only signal a
// shared host needs on top of the request id. UNVERIFIED against a live
// capture: the header is taken from the client's source. Without it a
// generic-looking x-client-request-id cannot be told from another tool's, so
// the call is skipped rather than attributed.
const (
	codexOriginatorHeader = "Originator"
	codexOriginatorPrefix = "codex"
)

// CarrierHeader names the header a provider's proxy-lane session id rides on,
// for messages that tell an operator what a call lacked: Claude Code's own
// header, Codex's x-client-request-id thread id, and "" for a provider with no
// known carrier.
func CarrierHeader(p Provider) string {
	if p == Codex {
		return codexThreadIDHeader
	}
	return ProxyHeader(p)
}

// HasCodexOriginator reports whether the request carries Codex's own
// originator header: the one signal no other OpenAI-speaking client is
// expected to send, so the relay's evidence that Codex routes through it
// requires it on every host. [UNVERIFIED]: if Codex does not send it, no
// evidence accrues and telemetry simply stays the producer.
func HasCodexOriginator(headers map[string]string) bool {
	return strings.HasPrefix(strings.ToLower(headers[codexOriginatorHeader]), codexOriginatorPrefix)
}

// AttributeProxy picks the one provider a relayed call belongs to among the
// providers whose host rows cover its host (candidates, most specific first),
// by whose session carrier the call actually carries. It returns that
// provider and its session id, or a skip reason and no provider:
//
//   - exactly one carrier resolves: that provider wins;
//   - none resolves: SkipNoProviderCarrier;
//   - more than one resolves: SkipAmbiguousCarrier.
//
// When the host is shared (more than one candidate), Codex's x-client-request-id
// alone does not count, because another tool may send the same header; a
// Codex-only originator header must accompany it. A single-candidate host has
// no other claimant, so the request id alone is enough there. A provider with
// no carrier (Muse) never resolves.
func AttributeProxy(candidates []string, headers map[string]string) (Provider, string, string) {
	var winner Provider
	var winnerID string
	resolved := 0
	for _, name := range candidates {
		p := Provider(name)
		id, ok := ResolveProxy(p, headers)
		if !ok {
			continue
		}
		if p == Codex && len(candidates) > 1 && !HasCodexOriginator(headers) {
			continue
		}
		resolved++
		winner, winnerID = p, id
	}
	switch resolved {
	case 0:
		return "", "", SkipNoProviderCarrier
	case 1:
		return winner, winnerID, ""
	}
	return "", "", SkipAmbiguousCarrier
}
