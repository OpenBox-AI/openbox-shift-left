package codex

import (
	"context"
	"encoding/base64"
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

// finopsTestSeed is a 32-byte Ed25519 seed (all-zero, as wire_test.go uses) so
// the sentinel test can drive the REAL AIP-signing client and inspect the exact
// bytes that would go on the wire.
var finopsTestSeed = base64.StdEncoding.EncodeToString(make([]byte, 32))

// sentinels are the unique content markers seeded into every content-bearing
// location of testdata/rollout-poisoned.jsonl. The finops parser must extract the
// token NUMBERS and NONE of these strings.
var sentinels = []string{
	"SENTINEL_CWD", "SENTINEL_INSTRUCTIONS", "SENTINEL_PROMPT", "SENTINEL_OUTPUT",
	"SENTINEL_AGENTMSG", "SENTINEL_CMD", "SENTINEL_STDOUT", "SENTINEL_PATCH", "SENTINEL_ARGS",
}

const poisonedRolloutPath = "testdata/rollout-poisoned.jsonl"

// TestAggregateRolloutUsage_LastCumulativeWinsFromFixture reads the GROUNDED
// testdata rollout (shaped from codex-rs @ rust-v0.145.0) and proves the two
// Codex-specific aggregation rules:
//   - total_token_usage is CUMULATIVE, so the rollup is the LAST snapshot
//     (160/35/195), NOT the sum of both token_count lines (which would be
//     260/65/325);
//   - cached_input_tokens (40) / cache_write_input_tokens (5) /
//     reasoning_output_tokens (12) are SUBSETS already inside input/output, so
//     Input stays 160 (NOT 160+40+5=205) and Output stays 35 (NOT 35+12).
func TestAggregateRolloutUsage_LastCumulativeWinsFromFixture(t *testing.T) {
	tokens, cost, err := readRolloutUsage(poisonedRolloutPath)
	if err != nil {
		t.Fatalf("readRolloutUsage: %v", err)
	}
	if tokens == nil {
		t.Fatal("tokens nil, want the final cumulative rollup")
	}
	if got := *tokens.Input; got != 160 {
		t.Errorf("Input = %d, want 160 (last cumulative snapshot; cache subsets NOT added)", got)
	}
	if got := *tokens.Output; got != 35 {
		t.Errorf("Output = %d, want 35 (reasoning subset NOT added)", got)
	}
	if got := *tokens.Total; got != 195 {
		t.Errorf("Total = %d, want 195 (reported total_tokens)", got)
	}
	if cost != nil {
		t.Errorf("Cost must be nil — Codex's token path carries no cost field, got %+v", *cost)
	}
}

// TestFinops_NoContentOnWire is the LOAD-BEARING INV-2 test (SL7-C / SL-16
// acceptance): a rollout seeded with sentinel content must yield usage numbers
// with NONE of the sentinels reaching the emitted event, its metadata, or the
// actual signed wire body. It drives the real AIP-signing client with
// content-capture ON — the adversarial worst case (the client's content stripper
// is disabled), so any leak would pass straight through to the wire. Only the
// projection-only parser can be what keeps content off the wire.
func TestFinops_NoContentOnWire(t *testing.T) {
	tokens, cost, err := readRolloutUsage(poisonedRolloutPath)
	if err != nil {
		t.Fatalf("readRolloutUsage: %v", err)
	}

	// Build the SessionEnded event exactly as the flush path does.
	m := NewMapper(Identity{DeveloperDID: testDID})
	m.NewID = func() string { return "evt-1" }
	m.Finops = &FinopsUsage{Tokens: tokens, Cost: cost}
	ev, ok := m.Map(HookSessionEnd, &HookEvent{SessionID: "th-1", TranscriptPath: poisonedRolloutPath, Reason: "other"})
	if !ok {
		t.Fatal("Map(SessionEnd) not ok")
	}
	if ev.Tokens == nil || *ev.Tokens.Total != 195 {
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
		SeedB64:               finopsTestSeed,
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
	if !strings.Contains(string(body), "195") {
		t.Errorf("expected token total 195 on the wire, got: %s", body)
	}
}

// TestFinops_OffByteIdentical: with no finops usage attached (flag off), the
// SessionEnded event carries no tokens/cost — byte-identical to pre-SL7-C output.
func TestFinops_OffByteIdentical(t *testing.T) {
	m := NewMapper(Identity{DeveloperDID: testDID})
	m.NewID = func() string { return "evt-1" }
	m.Now = func() time.Time { return time.Unix(0, 0).UTC() }
	// m.Finops stays nil (the default) — mirrors ResolveFinops()==false.
	ev, _ := m.Map(HookSessionEnd, &HookEvent{SessionID: "th-1", TranscriptPath: "/should/not/matter", Reason: "other"})
	if ev.Tokens != nil || ev.Cost != nil {
		t.Fatalf("finops off must attach nothing, got tokens=%v cost=%v", ev.Tokens, ev.Cost)
	}
}

// TestFinops_AttachesOnlyOnSessionEnd: even with usage present, non-SessionEnd
// events never carry tokens/cost (the rollup is a session-terminal fact).
func TestFinops_AttachesOnlyOnSessionEnd(t *testing.T) {
	m := NewMapper(Identity{DeveloperDID: testDID})
	m.NewID = func() string { return "id" }
	m.Finops = &FinopsUsage{Tokens: &client.Tokens{Total: intPtrRollout(99)}}
	for _, h := range []HookName{HookSessionStart, HookUserPromptSubmit, HookPreToolUse, HookPostToolUse} {
		ev, ok := m.Map(h, &HookEvent{SessionID: "th-1", ToolName: "Bash", ToolUseID: "c1"})
		if ok && ev.Tokens != nil {
			t.Errorf("%s carried tokens; finops must attach only on SessionEnd", h)
		}
	}
	ev, _ := m.Map(HookSessionEnd, &HookEvent{SessionID: "th-1", Reason: "other"})
	if ev.Tokens == nil || *ev.Tokens.Total != 99 {
		t.Errorf("SessionEnd should carry the rollup, got %v", ev.Tokens)
	}
}

// TestAggregateRolloutUsage_LastWinsNotSum pins the cumulative rule directly on
// inline bytes: three ascending cumulative snapshots ⇒ the LAST one, never a sum.
func TestAggregateRolloutUsage_LastWinsNotSum(t *testing.T) {
	body := tokenLine(50, 20, 70) + "\n" + tokenLine(120, 45, 165) + "\n" + tokenLine(200, 60, 260) + "\n"
	tokens, _, err := aggregateRolloutUsage([]byte(body))
	if err != nil {
		t.Fatalf("aggregateRolloutUsage: %v", err)
	}
	if *tokens.Input != 200 || *tokens.Output != 60 || *tokens.Total != 260 {
		t.Fatalf("want the last cumulative snapshot 200/60/260, got %d/%d/%d",
			*tokens.Input, *tokens.Output, *tokens.Total)
	}
}

// TestAggregateRolloutUsage_MalformedFinalLineSkipped: a truncated/garbage final
// line is skipped, honestly falling back to the previous COMPLETE cumulative
// rather than undercounting or erroring (fault-tolerant, INV-3).
func TestAggregateRolloutUsage_MalformedFinalLineSkipped(t *testing.T) {
	body := "not json at all\n" +
		tokenLine(100, 30, 130) + "\n" +
		`{"type":"event_msg","payload":{"info":{"total_token_usage":` // truncated final line
	tokens, _, err := aggregateRolloutUsage([]byte(body))
	if err != nil {
		t.Fatalf("aggregateRolloutUsage: %v", err)
	}
	if tokens == nil || *tokens.Input != 100 || *tokens.Output != 30 || *tokens.Total != 130 {
		t.Fatalf("want 100/30/130 from the last GOOD line, got %+v", tokens)
	}
}

// TestAggregateRolloutUsage_EmptyNoError: a rollout with no token_count lines is
// valid — nil rollup, no error (the caller then attaches nothing, same as
// finops-off).
func TestAggregateRolloutUsage_EmptyNoError(t *testing.T) {
	body := `{"type":"session_meta","payload":{"id":"x","cwd":"/r"}}` + "\n" +
		`{"type":"event_msg","payload":{"type":"agent_message","message":"hi"}}` + "\n"
	tokens, cost, err := aggregateRolloutUsage([]byte(body))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tokens != nil || cost != nil {
		t.Fatalf("no-token rollout must yield nil rollup, got tokens=%v cost=%v", tokens, cost)
	}
}

// TestAggregateRolloutUsage_CostAlwaysNil: Codex's token path carries NO cost
// field, so Cost is always nil (never fabricated from a pricing table).
func TestAggregateRolloutUsage_CostAlwaysNil(t *testing.T) {
	tokens, cost, err := aggregateRolloutUsage([]byte(tokenLine(5, 2, 7) + "\n"))
	if err != nil {
		t.Fatalf("aggregateRolloutUsage: %v", err)
	}
	if tokens == nil {
		t.Fatal("tokens nil")
	}
	if cost != nil {
		t.Fatalf("Cost must always be nil for Codex, got %+v", cost)
	}
}

// TestAggregateRolloutUsage_NegativeClamped: a malformed/negative source value
// must not produce a number violating the SL-1 schema `minimum: 0`.
func TestAggregateRolloutUsage_NegativeClamped(t *testing.T) {
	tokens, _, err := aggregateRolloutUsage([]byte(tokenLine(-5, 4, -1) + "\n"))
	if err != nil {
		t.Fatalf("aggregateRolloutUsage: %v", err)
	}
	// input clamps to 0, output=4, total clamps to 0 → falls back to in+out=4.
	if *tokens.Input != 0 || *tokens.Output != 4 || *tokens.Total != 4 {
		t.Fatalf("negatives must clamp to 0 (total falls back to in+out), got %d/%d/%d",
			*tokens.Input, *tokens.Output, *tokens.Total)
	}
}

// TestAggregateRolloutUsage_TotalFallsBackToSum: a snapshot missing total_tokens
// (0) derives Total from Input+Output so it is never a spurious 0.
func TestAggregateRolloutUsage_TotalFallsBackToSum(t *testing.T) {
	body := `{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":10,"output_tokens":4}}}}` + "\n"
	tokens, _, err := aggregateRolloutUsage([]byte(body))
	if err != nil {
		t.Fatalf("aggregateRolloutUsage: %v", err)
	}
	if *tokens.Total != 14 {
		t.Fatalf("Total should fall back to Input+Output=14, got %d", *tokens.Total)
	}
}

func TestReadRolloutUsage_Errors(t *testing.T) {
	if _, _, err := readRolloutUsage(""); err == nil {
		t.Error("empty/null transcript_path should error (skipped)")
	}
	if _, _, err := readRolloutUsage(filepath.Join(t.TempDir(), "nope.jsonl")); err == nil {
		t.Error("missing file should error (skipped)")
	}
	if _, _, err := readRolloutUsage(t.TempDir()); err == nil {
		t.Error("non-regular file (directory) should error (skipped)")
	}
}

func TestReadRolloutUsage_OversizedSkipped(t *testing.T) {
	// A sparse file larger than the cap is skipped whole (INV-3, bounded read) —
	// never read into memory, never partially counted.
	p := filepath.Join(t.TempDir(), "huge.jsonl")
	f, err := os.Create(p)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := f.Truncate(maxRolloutBytes + 1); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	_ = f.Close()
	if _, _, err := readRolloutUsage(p); err == nil {
		t.Error("oversized rollout should be skipped with an error")
	}
}

// TestFinops_SessionEndWiring_ReadsRollout is the wiring / read-only smoke: it
// drives RunHook end-to-end for a SessionEnd whose transcript_path points at the
// grounded rollout fixture, finops ON, and asserts (a) stdout stays empty (INV-3),
// (b) the SessionEnded event spooled with the token total, and (c) no sentinel
// content leaked into the spool. Substitutes for a live ~/.codex rollout (none
// exists on this box and one must not be generated — read-only grounding).
func TestFinops_SessionEndWiring_ReadsRollout(t *testing.T) {
	spool := setHookEnv(t)
	t.Setenv("OPENBOX_FINOPS", "1")

	abs, err := filepath.Abs(poisonedRolloutPath)
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	end := `{"hook_event_name":"SessionEnd","session_id":"th-fin","cwd":"/r","reason":"other","transcript_path":` +
		mustJSONString(abs) + `}`

	stdout, stderr := runHook(t, "SessionEnd", end)
	if stdout != "" {
		t.Fatalf("SessionEnd stdout must be empty, got %q (stderr=%q)", stdout, stderr)
	}

	entries, _ := os.ReadDir(spool)
	var spooled string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		raw, _ := os.ReadFile(filepath.Join(spool, e.Name()))
		spooled += string(raw)
	}
	if !strings.Contains(spooled, "SessionEnded") {
		t.Fatalf("SessionEnded not spooled: %s", spooled)
	}
	if !strings.Contains(spooled, "195") {
		t.Errorf("finops-on SessionEnded should carry the token total 195: %s", spooled)
	}
	for _, s := range sentinels {
		if strings.Contains(spooled, s) {
			t.Fatalf("INV-2 breach: sentinel %q leaked into the spool via the finops read: %s", s, spooled)
		}
	}
}

// TestFinops_SessionEndWiring_OffAttachesNothing: with finops OFF (default) the
// SessionEnded event carries no token total even though transcript_path points at
// a rollout full of counts — byte-identical to the pre-SL7-C path.
func TestFinops_SessionEndWiring_OffAttachesNothing(t *testing.T) {
	spool := setHookEnv(t) // OPENBOX_FINOPS unset → ResolveFinops()==false
	abs, _ := filepath.Abs(poisonedRolloutPath)
	end := `{"hook_event_name":"SessionEnd","session_id":"th-off","cwd":"/r","reason":"other","transcript_path":` +
		mustJSONString(abs) + `}`

	if stdout, _ := runHook(t, "SessionEnd", end); stdout != "" {
		t.Fatalf("stdout must be empty, got %q", stdout)
	}
	entries, _ := os.ReadDir(spool)
	var spooled string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".jsonl") {
			raw, _ := os.ReadFile(filepath.Join(spool, e.Name()))
			spooled += string(raw)
		}
	}
	if strings.Contains(spooled, `"tokens"`) {
		t.Errorf("finops off must attach no tokens, spool: %s", spooled)
	}
}

// TestFinops_MissingTranscriptPathSkipped: a null/absent transcript_path with
// finops on is skipped fail-open (logged, no stdout, SessionEnded still spooled).
func TestFinops_MissingTranscriptPathSkipped(t *testing.T) {
	spool := setHookEnv(t)
	t.Setenv("OPENBOX_FINOPS", "1")
	end := `{"hook_event_name":"SessionEnd","session_id":"th-nopath","reason":"other","transcript_path":null}`

	stdout, stderr := runHook(t, "SessionEnd", end)
	if stdout != "" {
		t.Fatalf("stdout must be empty, got %q", stdout)
	}
	if !strings.Contains(stderr, "finops: rollout usage skipped") {
		t.Errorf("expected a fail-open finops skip diagnostic on stderr, got %q", stderr)
	}
	entries, _ := os.ReadDir(spool)
	var spooled string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".jsonl") {
			raw, _ := os.ReadFile(filepath.Join(spool, e.Name()))
			spooled += string(raw)
		}
	}
	if !strings.Contains(spooled, "SessionEnded") {
		t.Errorf("SessionEnded must still spool when finops is skipped: %s", spooled)
	}
}

// TestFinops_ConformanceWithTokens: the SessionEnded event WITH a token rollup
// still validates against the SL-1 contract and passes the E7 hook-wire shape
// through the real client (AC-5 parity — tokens ride metadata, never break the
// lifecycle wire type).
func TestFinops_ConformanceWithTokens(t *testing.T) {
	tokens, cost, err := readRolloutUsage(poisonedRolloutPath)
	if err != nil {
		t.Fatalf("readRolloutUsage: %v", err)
	}
	m := testMapper()
	m.NewID = nil
	m.Finops = &FinopsUsage{Tokens: tokens, Cost: cost}
	ev, ok := m.Map(HookSessionEnd, &HookEvent{SessionID: "th-1", Reason: "other"})
	if !ok {
		t.Fatal("Map not ok")
	}

	// SL-1 contract shape (content-capture disabled), same as conformance_test.go.
	raw := mustMarshalContractShape(t, ev)
	if !strings.Contains(string(raw), "195") {
		t.Errorf("expected token total on the conformance shape: %s", raw)
	}

	// E7 hook wire shape through the real client: lifecycle stays WorkflowCompleted,
	// tokens ride metadata.
	cl, bodies := newWireCapture(t)
	emit(t, cl, ev)
	payload := decodeBody(t, (*bodies)[0])
	if payload["event_type"] != "WorkflowCompleted" {
		t.Errorf("SessionEnded should map to WorkflowCompleted, got %v", payload["event_type"])
	}
	meta, _ := payload["metadata"].(map[string]any)
	if meta == nil || meta["tokens"] == nil {
		t.Errorf("token rollup should ride metadata.tokens on the wire: %v", meta)
	}
}

// ── test helpers ──

// tokenLine builds one token_count rollout line with the given cumulative
// total_token_usage (the grounded wire shape).
func tokenLine(in, out, total int) string {
	return `{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":` +
		`{"input_tokens":` + itoa(in) + `,"output_tokens":` + itoa(out) + `,"total_tokens":` + itoa(total) + `}}}}`
}

func itoa(v int) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func mustJSONString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
