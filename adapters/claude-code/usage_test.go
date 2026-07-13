package claudecode

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/openbox-ai/openbox-shift-left/client"
)

// A fixed, well-formed Ed25519 seed (matches the client's own test vector) so the
// sentinel test can drive the REAL AIP-signing client and inspect the exact bytes
// that would go on the wire.
const testSeedB64 = "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="

// poisonedTranscript is a JSONL transcript shaped like a real Claude Code
// transcript (verified against ~/.claude/projects/*.jsonl): assistant turns carry
// message.usage; every content-bearing location is seeded with a unique SENTINEL_*
// string. Real usage numbers are interleaved. The finops parser must extract the
// numbers and NONE of the sentinels.
//
// Expected rollup: input = (100+2000+50) + 10 = 2160; output = 30 + 5 = 35;
// total = 2195; cost = 0.0123 USD.
const poisonedTranscript = `{"type":"user","message":{"role":"user","content":"SENTINEL_PROMPT top secret prompt"},"cwd":"/x","sessionId":"s1"}
{"type":"assistant","message":{"model":"claude-opus-4-8","content":[{"type":"text","text":"SENTINEL_OUTPUT assistant reply"},{"type":"tool_use","name":"Bash","input":{"command":"SENTINEL_CMD dangerous"}}],"usage":{"input_tokens":100,"cache_read_input_tokens":2000,"cache_creation_input_tokens":50,"output_tokens":30,"service_tier":"standard","iterations":[{"input_tokens":100,"output_tokens":30}]}},"costUSD":0.0123}
{"type":"user","message":{"role":"user","content":[{"type":"tool_result","content":"SENTINEL_TOOLRESULT captured file body"}]}}
{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"SENTINEL_THINKING private chain of thought"}],"usage":{"input_tokens":10,"output_tokens":5}}}
{"type":"file-history-snapshot","snapshot":{"content":"SENTINEL_FILE entire file contents"}}
`

var sentinels = []string{
	"SENTINEL_PROMPT", "SENTINEL_OUTPUT", "SENTINEL_CMD",
	"SENTINEL_TOOLRESULT", "SENTINEL_THINKING", "SENTINEL_FILE",
}

func writeTranscript(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "transcript.jsonl")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	return p
}

func TestAggregateUsage_SumsTokensAndCost(t *testing.T) {
	tokens, cost, err := aggregateUsage([]byte(poisonedTranscript))
	if err != nil {
		t.Fatalf("aggregateUsage: %v", err)
	}
	if tokens == nil {
		t.Fatal("tokens nil, want a rollup")
	}
	if got := *tokens.Input; got != 2160 {
		t.Errorf("Input = %d, want 2160 (prompt+cache tokens folded in)", got)
	}
	if got := *tokens.Output; got != 35 {
		t.Errorf("Output = %d, want 35", got)
	}
	if got := *tokens.Total; got != 2195 {
		t.Errorf("Total = %d, want 2195", got)
	}
	if cost == nil {
		t.Fatal("cost nil, want 0.0123 (transcript carried costUSD)")
	}
	if cost.Amount != 0.0123 || cost.Currency != "USD" {
		t.Errorf("cost = %+v, want {0.0123 USD}", *cost)
	}
}

// TestFinops_NoContentOnWire is the LOAD-BEARING INV-2 test (SL-16 acceptance):
// a transcript seeded with sentinel content strings must yield usage numbers with
// NONE of the sentinels reaching the emitted event, its metadata/span, or the
// actual signed wire body. It drives the real AIP-signing client with
// content-capture ON — the adversarial worst case (the client's content stripper
// is disabled), so any leak would pass straight through to the wire.
func TestFinops_NoContentOnWire(t *testing.T) {
	path := writeTranscript(t, poisonedTranscript)
	tokens, cost, err := readTranscriptUsage(path)
	if err != nil {
		t.Fatalf("readTranscriptUsage: %v", err)
	}

	// Build the SessionEnded event exactly as the flush path does.
	m := NewMapper(Identity{DeveloperDID: testDID})
	m.NewID = func() string { return "evt-1" }
	m.Finops = &FinopsUsage{Tokens: tokens, Cost: cost}
	ev, ok := m.Map(HookSessionEnd, &HookEvent{SessionID: "s1", TranscriptPath: path, Reason: "other"})
	if !ok {
		t.Fatal("Map(SessionEnd) not ok")
	}
	if ev.Tokens == nil || *ev.Tokens.Total != 2195 {
		t.Fatalf("SessionEnded event missing token rollup: %+v", ev.Tokens)
	}

	// (a) The normalized event itself carries no sentinel anywhere.
	evJSON, _ := json.Marshal(ev)
	for _, s := range sentinels {
		if strings.Contains(string(evJSON), s) {
			t.Fatalf("INV-2 breach: sentinel %q present in emitted event: %s", s, evJSON)
		}
	}

	// (b) The exact SIGNED WIRE BODY carries no sentinel — content-capture ON so
	// the stripper cannot be what saves us; only the projection-only parser can.
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	cl, err := client.New(client.Config{
		BaseURL:               srv.URL,
		APIKey:                "obx_test_0123456789abcdef0123456789abcdef0123456789abcdef",
		DID:                   testDID,
		SeedB64:               testSeedB64,
		ContentCaptureEnabled: true, // adversarial: stripper OFF
	})
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}
	if _, err := cl.Emit(context.Background(), ev); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if len(body) == 0 {
		t.Fatal("no request body captured")
	}
	for _, s := range sentinels {
		if strings.Contains(string(body), s) {
			t.Fatalf("INV-2 breach: sentinel %q on the wire: %s", s, body)
		}
	}
	// Sanity: the numbers DID make it to the wire (feature actually works).
	if !strings.Contains(string(body), "2195") {
		t.Errorf("expected token total 2195 on the wire, got: %s", body)
	}
}

// TestFinops_OffByteIdentical: with no finops usage attached (flag off), the
// SessionEnded event carries no tokens/cost — byte-identical to pre-SL-16 output.
func TestFinops_OffByteIdentical(t *testing.T) {
	m := NewMapper(Identity{DeveloperDID: testDID})
	m.NewID = func() string { return "evt-1" }
	m.Now = func() time.Time { return time.Unix(0, 0).UTC() }
	// m.Finops stays nil (the default) — mirrors ResolveFinops()==false.
	ev, _ := m.Map(HookSessionEnd, &HookEvent{SessionID: "s1", TranscriptPath: "/should/not/matter", Reason: "other"})
	if ev.Tokens != nil || ev.Cost != nil {
		t.Fatalf("finops off must attach nothing, got tokens=%v cost=%v", ev.Tokens, ev.Cost)
	}
}

// TestFinops_AttachesOnlyOnSessionEnd: even with usage present, non-SessionEnd
// events never carry tokens/cost (the rollup is a session-terminal fact).
func TestFinops_AttachesOnlyOnSessionEnd(t *testing.T) {
	m := NewMapper(Identity{DeveloperDID: testDID})
	m.NewID = func() string { return "id" }
	m.Finops = &FinopsUsage{Tokens: &client.Tokens{Total: intPtr(99)}}
	for _, h := range []HookName{HookSessionStart, HookUserPromptSubmit, HookPreToolUse, HookPostToolUse} {
		ev, ok := m.Map(h, &HookEvent{SessionID: "s1", ToolName: "Bash"})
		if ok && ev.Tokens != nil {
			t.Errorf("%s carried tokens; finops must attach only on SessionEnd", h)
		}
	}
	ev, _ := m.Map(HookSessionEnd, &HookEvent{SessionID: "s1", Reason: "other"})
	if ev.Tokens == nil || *ev.Tokens.Total != 99 {
		t.Errorf("SessionEnd should carry the rollup, got %v", ev.Tokens)
	}
}

func TestAggregateUsage_MalformedLinesSkipped(t *testing.T) {
	// A partial/garbage line and a bare marker are skipped; the good usage line
	// still aggregates (fault-tolerant, INV-3).
	body := "not json at all\n" +
		`{"type":"assistant","message":{"usage":{"input_tokens":7,"output_tokens":3}}}` + "\n" +
		`{"type":"assistant","message":{"usage":` // truncated final line
	tokens, _, err := aggregateUsage([]byte(body))
	if err != nil {
		t.Fatalf("aggregateUsage: %v", err)
	}
	if tokens == nil || *tokens.Input != 7 || *tokens.Output != 3 {
		t.Fatalf("want input=7 output=3 from the one good line, got %+v", tokens)
	}
}

func TestAggregateUsage_EmptySessionNoError(t *testing.T) {
	// A transcript with no usage at all is valid: nil rollup, no error (the caller
	// then emits no tokens/cost, same as finops-off).
	body := `{"type":"user","message":{"role":"user","content":"hi"}}` + "\n" +
		`{"type":"system","subtype":"init"}` + "\n"
	tokens, cost, err := aggregateUsage([]byte(body))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tokens != nil || cost != nil {
		t.Fatalf("empty session must yield nil rollup, got tokens=%v cost=%v", tokens, cost)
	}
}

func TestAggregateUsage_CostAbsentByDefault(t *testing.T) {
	// Real current-CC transcripts carry NO cost field; Cost must stay nil (absent
	// when unknown) rather than be fabricated.
	body := `{"type":"assistant","message":{"usage":{"input_tokens":5,"output_tokens":2}}}` + "\n"
	tokens, cost, err := aggregateUsage([]byte(body))
	if err != nil {
		t.Fatalf("aggregateUsage: %v", err)
	}
	if tokens == nil {
		t.Fatal("tokens nil")
	}
	if cost != nil {
		t.Fatalf("cost must be nil when the transcript carries none, got %+v", cost)
	}
}

func TestAggregateUsage_NegativeClampedToZero(t *testing.T) {
	// A malformed/negative source value must not produce a number violating the
	// SL-1 schema `minimum: 0` (SEC-16-2).
	body := `{"type":"assistant","message":{"usage":{"input_tokens":-5,"cache_read_input_tokens":-1,"output_tokens":-3}}}` + "\n" +
		`{"type":"assistant","message":{"usage":{"input_tokens":10,"output_tokens":4}}}` + "\n"
	tokens, _, err := aggregateUsage([]byte(body))
	if err != nil {
		t.Fatalf("aggregateUsage: %v", err)
	}
	if *tokens.Input != 10 || *tokens.Output != 4 || *tokens.Total != 14 {
		t.Fatalf("negatives must clamp to 0, got input=%d output=%d total=%d",
			*tokens.Input, *tokens.Output, *tokens.Total)
	}
}

func TestReadTranscriptUsage_Errors(t *testing.T) {
	if _, _, err := readTranscriptUsage(""); err == nil {
		t.Error("empty path should error (skipped)")
	}
	if _, _, err := readTranscriptUsage(filepath.Join(t.TempDir(), "nope.jsonl")); err == nil {
		t.Error("missing file should error (skipped)")
	}
	// A directory is not a regular file.
	if _, _, err := readTranscriptUsage(t.TempDir()); err == nil {
		t.Error("non-regular file should error (skipped)")
	}
}

func TestReadTranscriptUsage_OversizedSkipped(t *testing.T) {
	// A sparse file larger than the cap is skipped whole (INV-3, bounded read) —
	// never read into memory, never partially counted.
	p := filepath.Join(t.TempDir(), "huge.jsonl")
	f, err := os.Create(p)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := f.Truncate(maxTranscriptBytes + 1); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	_ = f.Close()
	if _, _, err := readTranscriptUsage(p); err == nil {
		t.Error("oversized transcript should be skipped with an error")
	}
}
