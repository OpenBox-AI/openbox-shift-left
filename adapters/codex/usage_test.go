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
// testdata rollout (shaped from codex-rs @ rust-v0.145.0) and proves the three
// Codex-specific aggregation rules:
//
//   - total_token_usage is CUMULATIVE, so the rollup is the LAST snapshot, NOT
//     the sum of both token_count lines (which would be 260/65/325);
//   - cached_input_tokens (40) and cache_write_input_tokens (5) are SUB-COUNTS
//     already inside input_tokens (160), so contract v1.1 reports them in their
//     own fields and SUBTRACTS them from Input: 160 − 40 − 5 = 115 pure input.
//     This is the inverse of Claude Code, whose cache counts are additive
//     siblings — adding them here would double-count the cache on every session.
//     The evidence is the arithmetic: total_tokens (195) == input + output
//     (160 + 35), so neither cache count contributes on top;
//   - reasoning_output_tokens (12) is likewise a sub-count of output_tokens and
//     is not bound at all, so Output stays 35 (NOT 35 + 12).
func TestAggregateRolloutUsage_LastCumulativeWinsFromFixture(t *testing.T) {
	tokens, model, err := readRolloutUsage(poisonedRolloutPath)
	if err != nil {
		t.Fatalf("readRolloutUsage: %v", err)
	}
	if tokens == nil {
		t.Fatal("tokens nil, want the final cumulative rollup")
	}
	if got := *tokens.Input; got != 115 {
		t.Errorf("Input = %d, want 115 (160 − cached 40 − cache_write 5 = pure input)", got)
	}
	if got := *tokens.Output; got != 35 {
		t.Errorf("Output = %d, want 35 (reasoning subset NOT added)", got)
	}
	if tokens.CacheRead == nil || *tokens.CacheRead != 40 {
		t.Errorf("CacheRead = %v, want 40 (cached_input_tokens in its own field)", tokens.CacheRead)
	}
	if tokens.CacheCreationInput == nil || *tokens.CacheCreationInput != 5 {
		t.Errorf("CacheCreationInput = %v, want 5 (cache_write_input_tokens in its own field)", tokens.CacheCreationInput)
	}
	if got := *tokens.Total; got != 195 {
		t.Errorf("Total = %d, want 195 (reported total_tokens, carried verbatim)", got)
	}
	// The invariant that makes both providers comparable: Total == the four parts.
	// If this ever fails, one of the sub-counts is being double-counted or dropped.
	if sum := *tokens.Input + *tokens.Output + *tokens.CacheRead + *tokens.CacheCreationInput; sum != *tokens.Total {
		t.Errorf("Input+Output+caches = %d but Total = %d — a sub-count is double-counted or dropped",
			sum, *tokens.Total)
	}
	// The fixture carries no turn_context, so it names no model. Absence is a
	// legitimate state, handled by the core-side `unknown` bucket.
	if model != "" {
		t.Errorf("model = %q, want empty (the fixture has no turn_context line)", model)
	}
}

// The model id lives at turn_context.payload.model, and NOTHING else in that
// content-rich payload may be reachable. Verified against 12 real rollouts
// (~/.codex/sessions, codex 0.145.x): payload.model occurs exactly as often as
// turn_context lines do, so no other line type puts a model there.
func TestAggregateRolloutUsage_BindsModelFromTurnContextOnly(t *testing.T) {
	// A real-shaped turn_context, including the content siblings that must stay
	// unreachable, plus the nested `model` keys that must NOT be picked up.
	turnContext := `{"timestamp":"2026-08-11T09:00:00.000Z","type":"turn_context","payload":{` +
		`"turn_id":"t1","cwd":"/work/SENTINEL_CWD","workspace_roots":["/work/SENTINEL_CWD"],` +
		`"approval_policy":"never","model":"gpt-5.6-sol","personality":"pragmatic",` +
		`"collaboration_mode":{"mode":"default","settings":{"model":"SENTINEL_NESTEDMODEL",` +
		`"developer_instructions":"SENTINEL_INSTRUCTIONS body"}}}}`
	body := turnContext + "\n" + tokenLine(100, 30, 130) + "\n"

	tokens, model := aggregateRolloutUsage([]byte(body))
	if tokens == nil {
		t.Fatal("tokens nil")
	}
	if model != "gpt-5.6-sol" {
		t.Errorf("model = %q, want gpt-5.6-sol from turn_context.payload.model", model)
	}
	if strings.Contains(model, "SENTINEL") {
		t.Errorf("a NESTED model key was bound: %q — the projection reaches too deep", model)
	}

	// Last non-empty wins: the model in effect when the session ended.
	switched := body + strings.Replace(turnContext, `"model":"gpt-5.6-sol"`, `"model":"gpt-5.5"`, 1) + "\n"
	if _, m2 := aggregateRolloutUsage([]byte(switched)); m2 != "gpt-5.5" {
		t.Errorf("model = %q after a switch, want gpt-5.5 (last non-empty wins)", m2)
	}
}

// The session-rollup activity pair: one per session, the pinned id, the four
// counts, and the model — the same wire shape Claude Code's per-turn pairs use.
func TestMapUsageRollup_EmitsOnePinnedPair(t *testing.T) {
	m := NewMapper(Identity{DeveloperDID: testDID})
	m.NewID = nil // exercise the real deterministic derivation
	m.Now = func() time.Time { return time.Unix(1785000000, 0).UTC() }
	tokens, model := aggregateRolloutUsage([]byte(
		`{"type":"turn_context","payload":{"model":"gpt-5.6-sol"}}` + "\n" + tokenLine(100, 30, 130) + "\n"))
	m.Finops = &FinopsUsage{Tokens: tokens, Model: model}

	started, completed, ok := m.MapUsageRollup(&HookEvent{SessionID: "th-rollup"})
	if !ok {
		t.Fatal("MapUsageRollup not ok")
	}
	if started.EventType != client.EventTurnStarted || completed.EventType != client.EventTurnCompleted {
		t.Errorf("wrong event types: %s / %s", started.EventType, completed.EventType)
	}
	if !started.SessionRollup || !completed.SessionRollup {
		t.Error("both halves must be marked SessionRollup, or the activity_id is wrong")
	}
	// The rollup covers a session, not a turn, so it must carry NO turn index —
	// an index would claim a per-turn granularity Codex does not have.
	if started.TurnIndex != nil || completed.TurnIndex != nil {
		t.Errorf("a session rollup must not claim a turn index: %v / %v", started.TurnIndex, completed.TurnIndex)
	}
	if completed.Model != "gpt-5.6-sol" {
		t.Errorf("model = %q", completed.Model)
	}
	if completed.Tokens == nil || *completed.Tokens.Total != 130 {
		t.Errorf("tokens = %+v, want the session rollup", completed.Tokens)
	}
	if started.Tokens != nil {
		t.Error("the Started half must carry no usage; the numbers are known at the close")
	}
	// Distinct idempotency ids, or a server that dedupes on event_id would drop
	// one half of the pair as a replay of the other.
	if started.EventID == completed.EventID {
		t.Errorf("both halves derived the same event_id %q", started.EventID)
	}

	// An empty session emits no pair rather than a zero-filled one.
	m.Finops = nil
	if _, _, ok := m.MapUsageRollup(&HookEvent{SessionID: "th-rollup"}); ok {
		t.Error("MapUsageRollup returned a pair with no usage to report")
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
	tokens, model, err := readRolloutUsage(poisonedRolloutPath)
	if err != nil {
		t.Fatalf("readRolloutUsage: %v", err)
	}

	// Build the SessionEnded event exactly as the flush path does.
	m := NewMapper(Identity{DeveloperDID: testDID})
	m.NewID = func() string { return "evt-1" }
	m.Finops = &FinopsUsage{Tokens: tokens, Model: model}
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
		PrivateKeyB64:         finopsTestSeed,
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
	tokens, _ := aggregateRolloutUsage([]byte(body))
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
	tokens, _ := aggregateRolloutUsage([]byte(body))
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
	tokens, model := aggregateRolloutUsage([]byte(body))
	if tokens != nil {
		t.Fatalf("no-token rollout must yield nil rollup, got tokens=%v", tokens)
	}
	if model != "" {
		t.Fatalf("no turn_context in this rollout, so no model, got %q", model)
	}
}

// Cost is not merely nil for Codex — the reader has no cost return at all, and
// no event it feeds carries one. Codex's TokenCountEvent is {info, rate_limits}
// with no price field (protocol.rs), and a cost derived from a pricing table
// would be a fabricated number.
func TestCodexUsage_CostIsNeverCarried(t *testing.T) {
	tokens, _ := aggregateRolloutUsage([]byte(tokenLine(5, 2, 7) + "\n"))
	if tokens == nil {
		t.Fatal("tokens nil")
	}

	m := NewMapper(Identity{DeveloperDID: testDID})
	m.NewID = func() string { return "evt-1" }
	m.Finops = &FinopsUsage{Tokens: tokens}
	sessionEnd, _ := m.Map(HookSessionEnd, &HookEvent{SessionID: "th-1", Reason: "other"})
	if sessionEnd.Cost != nil {
		t.Errorf("SessionEnded carried cost %+v", sessionEnd.Cost)
	}
	_, completed, ok := m.MapUsageRollup(&HookEvent{SessionID: "th-1"})
	if !ok {
		t.Fatal("MapUsageRollup not ok")
	}
	if completed.Cost != nil {
		t.Errorf("the rollup activity carried cost %+v", completed.Cost)
	}
}

// TestAggregateRolloutUsage_NegativeClamped: a malformed/negative source value
// must not produce a number violating the SL-1 schema `minimum: 0`.
func TestAggregateRolloutUsage_NegativeClamped(t *testing.T) {
	tokens, _ := aggregateRolloutUsage([]byte(tokenLine(-5, 4, -1) + "\n"))
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
	tokens, _ := aggregateRolloutUsage([]byte(body))
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

// TestFinops_SessionEndWiring_OffAttachesNothing is the OPT-OUT assertion, and it
// is a security assertion rather than a feature test: it proves the documented
// opt-out is real and COMPLETE — no tokens, no model, and no rollup activity.
//
// Usage capture is on by DEFAULT as of ADR-0014, so this test must now disable it
// explicitly. Before the flip it passed by leaving OPENBOX_FINOPS unset, which is
// exactly why the flip needed `Finops` to become a *bool: with a plain bool an
// absent field and an explicit false were indistinguishable, so the default could
// not move at all.
func TestFinops_SessionEndWiring_OffAttachesNothing(t *testing.T) {
	spool := setHookEnv(t)
	t.Setenv("OPENBOX_FINOPS", "0") // the documented opt-out
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
	if !strings.Contains(spooled, "SessionEnded") {
		t.Fatalf("the opt-out must silence USAGE, not telemetry: %s", spooled)
	}
	// Complete silence on the usage path. A disabled flag that still leaked the
	// model id would be worse than the old default, because it would contradict
	// its own documentation.
	for _, forbidden := range []string{`"tokens"`, `"model"`, "TurnStarted", "TurnCompleted", "usage_scope"} {
		if strings.Contains(spooled, forbidden) {
			t.Errorf("finops off still emitted %s, spool: %s", forbidden, spooled)
		}
	}
}

// The other half of the opt-out matrix: with nothing configured at all, usage
// capture is ON. Stated as its own test so a future default change is a
// deliberate edit here rather than a silent behavioural drift.
func TestFinops_DefaultOnWithNoConfiguration(t *testing.T) {
	spool := setHookEnv(t) // OPENBOX_FINOPS deliberately unset
	abs, _ := filepath.Abs(poisonedRolloutPath)
	end := `{"hook_event_name":"SessionEnd","session_id":"th-default","cwd":"/r","reason":"other","transcript_path":` +
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
	if !strings.Contains(spooled, `"tokens"`) {
		t.Errorf("an unconfigured session must capture usage (default on): %s", spooled)
	}
	if !strings.Contains(spooled, "TurnCompleted") {
		t.Errorf("an unconfigured session must emit the rollup activity pair: %s", spooled)
	}
	// And no rollout content rides along, at the default posture.
	for _, s := range sentinels {
		if strings.Contains(spooled, s) {
			t.Fatalf("INV-2 breach at the default posture: sentinel %q in the spool: %s", s, spooled)
		}
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
	tokens, model, err := readRolloutUsage(poisonedRolloutPath)
	if err != nil {
		t.Fatalf("readRolloutUsage: %v", err)
	}
	m := testMapper()
	m.NewID = nil
	m.Finops = &FinopsUsage{Tokens: tokens, Model: model}
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
