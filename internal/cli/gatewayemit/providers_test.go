package gatewayemit

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/gateway"
)

const (
	ccTestDID    = "did:aip:00000000-0000-5000-a000-0000000000c1"
	codexTestDID = "did:aip:00000000-0000-5000-a000-0000000000c2"
)

// hostCandidates mirrors the relay's host table for the hosts these tests use.
func hostCandidates(host string) []string {
	switch {
	case strings.HasPrefix(host, "api.anthropic.com"), strings.HasPrefix(host, "claude.ai"):
		return []string{"claude-code"}
	case strings.HasPrefix(host, "api.openai.com"):
		return []string{"codex"}
	case strings.HasPrefix(host, "api.meta.ai"):
		return []string{"claude-code", "muse", "codex"}
	}
	return nil
}

type multiProviderRig struct {
	em       *Emitter
	delivery *fakeDelivery
	warnings *bytes.Buffer
	verbose  *bytes.Buffer
}

func newMultiProviderRig(codexElected bool, codexDID string) *multiProviderRig {
	r := &multiProviderRig{delivery: newFakeDelivery(), warnings: &bytes.Buffer{}, verbose: &bytes.Buffer{}}
	r.em = &Emitter{
		Lane:              LaneProxy,
		Deliver:           r.delivery.Deliver,
		Warn:              func(f string, a ...any) { fmt.Fprintf(r.warnings, f+"\n", a...) },
		Verbose:           func(f string, a ...any) { fmt.Fprintf(r.verbose, f+"\n", a...) },
		CandidatesForHost: hostCandidates,
		Providers: map[string]ProviderLane{
			"claude-code": {DID: func() string { return ccTestDID }, Elected: func() bool { return true }},
			"codex": {
				DID:         func() string { return codexDID },
				Elected:     func() bool { return codexElected },
				ElectedName: func() string { return "telemetry" },
			},
		},
	}
	return r
}

func relayedCall(url string, headers map[string]string) gateway.Captured {
	c := sampleCaptured()
	c.HTTPURL = url
	c.RequestHeaders = headers
	return c
}

// TestClaudeCodeEventsAreIdenticalWithAndWithoutTheProviderTable: wiring the
// per-provider table must not change one byte of what a Claude Code call
// records, on its own host or on the shared one.
func TestClaudeCodeEventsAreIdenticalWithAndWithoutTheProviderTable(t *testing.T) {
	at := func() time.Time { return time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC) }
	for _, url := range []string{"https://api.anthropic.com/v1/messages", "https://api.meta.ai/v1/responses"} {
		legacyDelivery := newFakeDelivery()
		legacy := &Emitter{
			Lane: LaneProxy, Deliver: legacyDelivery.Deliver, Now: at, Warn: func(string, ...any) {},
			DID: func() string { return ccTestDID }, Elected: func() bool { return true },
		}
		table := newMultiProviderRig(true, codexTestDID)
		table.em.Now = at

		call := relayedCall(url, map[string]string{
			"X-Claude-Code-Session-Id": "cc-identical", "X-Claude-Code-Agent-Id": "agent-1",
		})
		legacy.Emit(context.Background(), call)
		table.em.Emit(context.Background(), call)

		want, _ := json.Marshal(legacyDelivery.forSession("cc-identical"))
		got, _ := json.Marshal(table.delivery.forSession("cc-identical"))
		if len(want) < 10 {
			t.Fatalf("%s: the legacy emitter recorded nothing; the comparison is vacuous", url)
		}
		if !bytes.Equal(want, got) {
			t.Errorf("%s: events differ\n legacy: %s\n table:  %s", url, want, got)
		}
	}
}

func TestSharedHostCallGoesToTheProviderWhoseCarrierResolves(t *testing.T) {
	r := newMultiProviderRig(true, codexTestDID)
	r.em.Emit(context.Background(), relayedCall("https://api.meta.ai/v1/responses",
		map[string]string{"X-Claude-Code-Session-Id": "cc-session-1"}))

	evs := r.delivery.forSession("cc-session-1")
	if len(evs) != 2 {
		t.Fatalf("recorded %d events for the claude-code session, want the pair", len(evs))
	}
	for _, ev := range evs {
		if ev.Tool.Name != "claude-code" || ev.DeveloperDID != ccTestDID {
			t.Errorf("event is %s signed %s, want claude-code signed %s", ev.Tool.Name, ev.DeveloperDID, ccTestDID)
		}
	}
}

func TestSharedHostCodexCallNeedsItsOwnSignal(t *testing.T) {
	r := newMultiProviderRig(true, codexTestDID)
	r.em.Emit(context.Background(), relayedCall("https://api.meta.ai/v1/responses",
		map[string]string{"X-Client-Request-Id": "thread-1", "Originator": "codex_cli_rs"}))

	evs := r.delivery.forSession("thread-1")
	if len(evs) != 2 {
		t.Fatalf("recorded %d events for the codex thread, want the pair", len(evs))
	}
	for _, ev := range evs {
		if ev.Tool.Name != "codex" || ev.DeveloperDID != codexTestDID {
			t.Errorf("event is %s signed %s, want codex signed %s", ev.Tool.Name, ev.DeveloperDID, codexTestDID)
		}
	}

	r = newMultiProviderRig(true, codexTestDID)
	r.em.Emit(context.Background(), relayedCall("https://api.meta.ai/v1/responses",
		map[string]string{"X-Client-Request-Id": "thread-2"}))
	if r.delivery.any() {
		t.Error("a bare x-client-request-id on a shared host was attributed to a provider")
	}
	if !strings.Contains(r.verbose.String(), "no_provider_carrier") {
		t.Errorf("the skip names no reason: %q", r.verbose.String())
	}
}

func TestSharedHostWithoutAnyCarrierIsSkippedNotGuessed(t *testing.T) {
	r := newMultiProviderRig(true, codexTestDID)
	r.em.Emit(context.Background(), relayedCall("https://api.meta.ai/v1/chat/completions", map[string]string{}))
	if r.delivery.any() {
		t.Error("a call with no carrier was recorded")
	}
	if !strings.Contains(r.verbose.String(), "no_provider_carrier") {
		t.Errorf("the skip names no reason: %q", r.verbose.String())
	}
	if r.warnings.Len() != 0 {
		t.Errorf("a skipped, unattributable call must not warn (an unrelated tool's traffic): %q", r.warnings.String())
	}
}

func TestSharedHostWithTwoCarriersIsAmbiguous(t *testing.T) {
	r := newMultiProviderRig(true, codexTestDID)
	r.em.Emit(context.Background(), relayedCall("https://api.meta.ai/v1/responses", map[string]string{
		"X-Claude-Code-Session-Id": "cc-session-2", "X-Client-Request-Id": "thread-3", "Originator": "codex_cli_rs",
	}))
	if r.delivery.any() {
		t.Error("an ambiguous call was recorded")
	}
	if !strings.Contains(r.verbose.String(), "ambiguous_carrier") {
		t.Errorf("the skip names no reason: %q", r.verbose.String())
	}
}

func TestCodexOwnHostRecordsUnderTheThreadIDWhenElected(t *testing.T) {
	r := newMultiProviderRig(true, codexTestDID)
	r.em.Emit(context.Background(), relayedCall("https://api.openai.com/v1/responses",
		map[string]string{"X-Client-Request-Id": "thread-4"}))
	evs := r.delivery.forSession("thread-4")
	if len(evs) != 2 {
		t.Fatalf("recorded %d events, want the pair", len(evs))
	}
	if evs[0].Tool.Name != "codex" || evs[0].DeveloperDID != codexTestDID {
		t.Errorf("got %s signed %s", evs[0].Tool.Name, evs[0].DeveloperDID)
	}
	for _, ev := range evs {
		if ev.ProxyRequestID == "" {
			t.Errorf("a proxy-lane event carries no proxy request id: %+v", ev)
		}
	}
}

func TestCodexCallIsSkippedWhileAnotherLaneIsElected(t *testing.T) {
	r := newMultiProviderRig(false, codexTestDID)
	r.em.Emit(context.Background(), relayedCall("https://api.openai.com/v1/responses",
		map[string]string{"X-Client-Request-Id": "thread-5"}))
	if r.delivery.any() {
		t.Error("an unelected Codex call was recorded")
	}
	if r.warnings.Len() != 0 {
		t.Errorf("another lane winning is healthy and must be quiet: %q", r.warnings.String())
	}
	if !strings.Contains(r.verbose.String(), "another lane") {
		t.Errorf("verbose log does not say another lane produces it: %q", r.verbose.String())
	}
}

// TestMissingDIDWarningNamesTheProvider: a Codex-only or claude-code-only
// machine must be told which provider lacks an identity, and one provider's
// warning must not silence the other's.
func TestMissingDIDWarningNamesTheProvider(t *testing.T) {
	r := newMultiProviderRig(true, "")
	r.em.Providers["claude-code"] = ProviderLane{DID: func() string { return "" }, Elected: func() bool { return true }}

	r.em.Emit(context.Background(), relayedCall("https://api.openai.com/v1/responses",
		map[string]string{"X-Client-Request-Id": "thread-6"}))
	r.em.Emit(context.Background(), relayedCall("https://api.anthropic.com/v1/messages",
		map[string]string{"X-Claude-Code-Session-Id": "cc-session-3"}))

	if r.delivery.any() {
		t.Error("a call without an identity was recorded")
	}
	got := r.warnings.String()
	for _, want := range []string{"for codex", "--provider codex", "for claude-code", "--provider claude-code"} {
		if !strings.Contains(got, want) {
			t.Errorf("warnings do not contain %q:\n%s", want, got)
		}
	}
}

func TestAHostWhoseOnlyClaimantHasNoLaneIsSkipped(t *testing.T) {
	r := newMultiProviderRig(true, codexTestDID)
	r.em.CandidatesForHost = func(string) []string { return []string{"muse"} }
	r.em.Emit(context.Background(), relayedCall("https://api.meta.ai/v1/responses", map[string]string{}))
	if r.delivery.any() {
		t.Error("a provider with no lane recorded a call")
	}
	if !strings.Contains(r.verbose.String(), "no lane for muse") {
		t.Errorf("verbose log: %q", r.verbose.String())
	}
}

// TestChatStaysClaudeCodeOnly: a claude.ai chat is signed by the claude-code
// identity whatever other providers are wired.
func TestChatStaysClaudeCodeOnly(t *testing.T) {
	r := newMultiProviderRig(true, codexTestDID)
	c := sampleCaptured()
	c.HTTPURL = "https://claude.ai/api/organizations/00000000-0000-4000-8000-000000000001/chat_conversations/00000000-0000-4000-8000-000000000002/completion"
	c.RequestHeaders = map[string]string{}
	r.em.Emit(context.Background(), c)

	recorded := false
	r.delivery.mu.Lock()
	for _, evs := range r.delivery.events {
		for _, ev := range evs {
			recorded = true
			if ev.DeveloperDID != ccTestDID {
				t.Errorf("chat event signed %s, want the claude-code identity", ev.DeveloperDID)
			}
		}
	}
	r.delivery.mu.Unlock()
	if !recorded {
		t.Error("a chat completion was not recorded")
	}
}
