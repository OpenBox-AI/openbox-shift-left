package gateway

import (
	"net/http"
	"testing"
)

// TestSessionCarrierHeadersSurviveRedaction: the relay's halt latch resolves a
// session off a provider's carrier header, so none of them may ever be
// treated as a credential.
func TestSessionCarrierHeadersSurviveRedaction(t *testing.T) {
	h := http.Header{}
	h.Set("X-Claude-Code-Session-Id", "cc-session")
	h.Set("X-Client-Request-Id", "codex-thread")
	h.Set("Originator", "codex_cli_rs")
	h.Set("Authorization", "Bearer placeholder")

	got := redactHeaders(h)
	for name, want := range map[string]string{
		"X-Claude-Code-Session-Id": "cc-session",
		"X-Client-Request-Id":      "codex-thread",
		"Originator":               "codex_cli_rs",
	} {
		if got[name] != want {
			t.Errorf("%s = %q after redaction, want %q", name, got[name], want)
		}
	}
	if got["Authorization"] != redactedHeaderValue {
		t.Errorf("Authorization = %q, want it redacted", got["Authorization"])
	}
}
