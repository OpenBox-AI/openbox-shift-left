package sessionkey

import "testing"

// TestOTelAttrPerProviderCell pins the OTel attribute per provider: Claude
// Code exports session.id, Codex exports conversation.id instead, and Muse
// session_id.
func TestOTelAttrPerProviderCell(t *testing.T) {
	for _, tc := range []struct {
		provider Provider
		want     string
	}{
		{ClaudeCode, "session.id"},
		{Codex, "conversation.id"},
		{Muse, "session_id"},                         // underscore, not the dotted CC key: the three never overlap
		{Provider("some-future-tool"), "session.id"}, // unnamed provider ⇒ the default every caller had
	} {
		if got := OTelAttr(tc.provider); got != tc.want {
			t.Errorf("OTelAttr(%q) = %q, want %q", tc.provider, got, tc.want)
		}
	}
}

// TestProxyHeaderPerProviderCell: Claude Code's carrier is a single named
// header; Codex's proxy-lane carrier is not (it is folded out of a different
// header by ResolveProxy), so ProxyHeader names nothing for it.
func TestProxyHeaderPerProviderCell(t *testing.T) {
	if got := ProxyHeader(ClaudeCode); got != "X-Claude-Code-Session-Id" {
		t.Errorf("ProxyHeader(ClaudeCode) = %q", got)
	}
	if got := ProxyHeader(Codex); got != "" {
		t.Errorf("ProxyHeader(Codex) = %q, want \"\" (its carrier is not a single header)", got)
	}
}

// TestResolveProxyPerProviderCell pins the proxy-lane carrier per provider,
// table-driven per cell, including the Codex session-id negative:
// codex-rs's own "session-id" header is a prompt-cache key, never identity.
func TestResolveProxyPerProviderCell(t *testing.T) {
	for _, tc := range []struct {
		name     string
		provider Provider
		headers  map[string]string
		wantID   string
		wantOK   bool
	}{
		{
			name:     "claude code reads its own session header",
			provider: ClaudeCode,
			headers:  map[string]string{"X-Claude-Code-Session-Id": "s-1"},
			wantID:   "s-1",
			wantOK:   true,
		},
		{
			name:     "claude code headerless call resolves no key",
			provider: ClaudeCode,
			headers:  map[string]string{},
			wantOK:   false,
		},
		{
			name:     "codex reads the thread id off x-client-request-id",
			provider: Codex,
			headers:  map[string]string{"X-Client-Request-Id": "thread-1"},
			wantID:   "thread-1",
			wantOK:   true,
		},
		{
			name:     "codex request carrying only session-id resolves no key",
			provider: Codex,
			headers:  map[string]string{"Session-Id": "cache-key-not-identity"},
			wantOK:   false,
		},
		{
			name:     "codex request carrying both never reads session-id",
			provider: Codex,
			headers:  map[string]string{"Session-Id": "cache-key", "X-Client-Request-Id": "thread-2"},
			wantID:   "thread-2",
			wantOK:   true,
		},
		{
			name:     "codex headerless call resolves no key",
			provider: Codex,
			headers:  map[string]string{},
			wantOK:   false,
		},
		{
			name:     "claude code's own header never leaks into a codex resolve",
			provider: Codex,
			headers:  map[string]string{"X-Claude-Code-Session-Id": "s-1"},
			wantOK:   false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id, ok := ResolveProxy(tc.provider, tc.headers)
			if ok != tc.wantOK || (ok && id != tc.wantID) {
				t.Errorf("ResolveProxy(%q, %v) = (%q, %v), want (%q, %v)",
					tc.provider, tc.headers, id, ok, tc.wantID, tc.wantOK)
			}
		})
	}
}

// TestHooksKeyIsSessionID pins the hooks-lane carrier name both providers
// share (a Codex root session's hook session_id equals its thread id by
// construction).
func TestHooksKeyIsSessionID(t *testing.T) {
	if HooksKey != "session_id" {
		t.Errorf("HooksKey = %q, want \"session_id\"", HooksKey)
	}
}

// TestLaneStringIsReadable is a thin sanity check on the diagnostic label,
// not the wire.
func TestLaneStringIsReadable(t *testing.T) {
	for lane, want := range map[Lane]string{
		LaneHooks: "hooks",
		LaneOTel:  "otel",
		LaneProxy: "proxy",
		Lane(99):  "unknown",
	} {
		if got := lane.String(); got != want {
			t.Errorf("Lane(%d).String() = %q, want %q", lane, got, want)
		}
	}
}
