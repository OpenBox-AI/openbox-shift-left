package main

import (
	"testing"

	obgit "github.com/openbox-ai/openbox-shift-left/internal/adapters/common/git"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
	"github.com/openbox-ai/openbox-shift-left/internal/client"
	"github.com/openbox-ai/openbox-shift-left/internal/gateway"
)

// TestHaltedRunAttributesByCarrierOnASharedHost: api.meta.ai is reached by
// three providers, so the latch resolves the call's owner by its carrier
// header. A call with no carrier, or with carriers of two providers, is never
// treated as latched: nothing is guessed.
func TestHaltedRunAttributesByCarrierOnASharedHost(t *testing.T) {
	t.Setenv(devconfig.EnvHaltDir, t.TempDir())
	t.Setenv(obgit.EnvSessionDir, t.TempDir())
	const ccSession = "cc-shared-host-1"
	const codexThread = "codex-thread-shared-1"
	for _, id := range []string{ccSession, codexThread} {
		hookflow.WriteSessionHalt(discardMainLogger(), id, client.Evaluation{
			Verdict: client.VerdictHalt, Reason: "latched for the test",
		})
	}

	for _, tc := range []struct {
		name    string
		url     string
		headers map[string]string
		want    string
	}{
		{"claude code header", "https://api.meta.ai/v1/responses",
			map[string]string{"X-Claude-Code-Session-Id": ccSession}, ccSession},
		{"codex with its originator", "https://api.meta.ai/v1/responses",
			map[string]string{"X-Client-Request-Id": codexThread, "Originator": "codex_cli_rs"}, codexThread},
		{"codex request id alone is not codex-only here", "https://api.meta.ai/v1/responses",
			map[string]string{"X-Client-Request-Id": codexThread}, ""},
		{"no carrier", "https://api.meta.ai/v1/responses", map[string]string{}, ""},
		{"two carriers", "https://api.meta.ai/v1/responses",
			map[string]string{"X-Claude-Code-Session-Id": ccSession, "X-Client-Request-Id": codexThread, "Originator": "codex_cli_rs"}, ""},
		{"codex on its own host needs only the request id", "https://api.openai.com/v1/responses",
			map[string]string{"X-Client-Request-Id": codexThread}, codexThread},
		{"claude code on its own host", "https://api.anthropic.com/v1/messages",
			map[string]string{"X-Claude-Code-Session-Id": ccSession}, ccSession},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _, halted := (haltDecorator{}).haltedRun(gateway.Captured{HTTPURL: tc.url, RequestHeaders: tc.headers})
			if halted != (tc.want != "") || got != tc.want {
				t.Errorf("haltedRun = (%q, halted=%v), want (%q, halted=%v)", got, halted, tc.want, tc.want != "")
			}
		})
	}
}
