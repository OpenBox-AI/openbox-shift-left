package claudecode

import (
	"context"
	"encoding/json"
	"io"
	"net/http"

	"github.com/openbox-ai/openbox-shift-left/client/memhttptest"
	"regexp"
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/client"
)

// Base-SDK conformance parity matrix (STORY-E7-S6).
//
// ADR-0004 unified shift-left telemetry onto the base SDK's Activity/hook wire
// model so we "can adopt the base conformance kit." This file is the durable,
// executable record of HOW our Go conformance coverage lines up with the base
// SDK's canonical conformance matrix — and, just as importantly, where it does
// NOT (Go-adapter-only extensions the base SDK has no concept of, and base cases
// we deliberately do not mirror). If a reviewer asks "does shift-left pass the
// base conformance behaviors?", this matrix is the answer, and TestConformanceParityMatrix
// guards it from silently drifting out of date.
//
// The base matrix is openbox-sdk-python/tests/conformance/test_required_cases.py
// (READ-ONLY reference), backed by openbox_core/conformance/fake_core.py
// (assert_hook_wire_shape) and the fail-mode suites
// tests/client/test_fail_modes.py + tests/instrumentation/test_hook_failclosed_and_sync_approval.py.
// (The base "conformance matrix" proper covers verdict enforcement + wire shape;
// fail-open/fail-closed live in the adjacent fail-mode suites — parity draws from both.)
//
// Our Go coverage, by file:
//   - enforce_conformance_test.go  — C1..C9, the enforcement carve-out (INV-3b).
//   - TestWire_ToolEventsAreActivityPairs (this file) — the activity envelope
//     every tool event must satisfy on the wire, plus the client's golden
//     fixtures which pin it byte-exactly.
//   - conformance_test.go (TestEmittedEventsAreConformant) — the shift-left-only
//     adapter-facing dev-event schema check (no base analog).
//
// NOTE (tool-call-as-activity): shift-left used to carry a Go mirror of the base
// SDK's assert_hook_wire_shape in client/hookspan.go. It is gone, because the
// shape it guarded is gone: a tool call is now an Activity
// (ToolCall→ActivityStarted, ToolResult→ActivityCompleted) and no shift-left
// payload carries a hook envelope or a span. The rows below record that as a
// base case we no longer mirror, rather than quietly leaving a parity claim that
// stopped being true.
//
// status values:
//   parity        — a base required-case (or fail-mode case) asserts the same behavior.
//   go-extension  — a behavior the base SDK has NO concept of; shift-left-only.
//   base-unmapped — a base case with no Go conformance case yet (why is in note).

type parityStatus string

const (
	statusParity       parityStatus = "parity"
	statusGoExtension  parityStatus = "go-extension"
	statusBaseUnmapped parityStatus = "base-unmapped"
)

type parityRow struct {
	goCase   string // our case id/name ("" for a base-unmapped row)
	baseCase string // base test name/helper ("" for a pure Go extension)
	status   parityStatus
	note     string
}

// conformanceParity is the authoritative cross-repo mapping. Ordered: our C1..C9,
// then wire-shape + schema coverage, then base cases we do not (yet) mirror.
var conformanceParity = []parityRow{
	// --- enforcement: C1..C9 (enforce_conformance_test.go) ---
	{
		goCase:   "C1 enforced BLOCK denies pre-execution",
		baseCase: "test_http_started_block_request_not_sent (+ db/file/function _block variants)",
		status:   statusParity,
		note:     "A real BLOCK verdict suppresses the operation pre-execution. Base proves the request/query/open/call never runs; C1 proves the tool is denied before exec, with the policy reason (not the fail-closed reason).",
	},
	{
		goCase:   "C2 fail-open + outage proceeds (OD9)",
		baseCase: "test_fail_open_api_error_proceeds; TestFailOpen.test_network_error_returns_fallback_allow",
		status:   statusParity,
		note:     "Core/sidecar unreachable under fail-open -> ALLOW/proceed (fallback_used). Same policy, different transport (Core HTTP in base; local UDS sidecar in C2).",
	},
	{
		goCase:   "C3 fail-open + unbundled proceeds (default unchanged)",
		baseCase: "",
		status:   statusGoExtension,
		note:     "The sidecar 'reachable daemon, NO policy bundle' state has no base analog (base evaluates against Core, not a local bundle). Conceptually the same fail-open default as C2.",
	},
	{
		goCase:   "C4 fail-closed + outage denies",
		baseCase: "test_fail_closed_api_error_maps_to_adapter_halt; TestFailClosed.test_network_error_raises",
		status:   statusParity,
		note:     "Unreachable evaluator under fail-closed -> deny/halt. Base raises GovernanceHaltError (fallback_used, activity aborted); C4 emits a content-free fail-closed deny.",
	},
	{
		goCase:   "C5 fail-closed never denies a REAL allow",
		baseCase: "",
		status:   statusGoExtension,
		note:     "Base has no dedicated 'fail-closed + healthy allow proceeds' case (a healthy allow simply returns in any mode). C5 pins that fail-closed engages on no-verdict ONLY, never on a real allow.",
	},
	{
		goCase:   "C6 fail-closed + unbundled denies (INFO-1 hole closed)",
		baseCase: "test_contract_error_fails_closed_even_when_fail_open",
		status:   statusParity,
		note:     "Partial parity: base hard-fails-closed on a ContractError even under fail-open (a 'no real verdict' input). C6 is the sidecar-specific instance — a reachable-but-unbundled daemon yields no real verdict, so fail-closed denies rather than being silently ungoverned.",
	},
	{
		goCase:   "C7 observe mode never blocks (INV-3 verbatim)",
		baseCase: "",
		status:   statusGoExtension,
		note:     "The base SDK has NO observe/monitor-only mode (grep observe -> nothing). Phase-1 observe is a shift-left invariant: enforce off -> empty stdout even for a BLOCK-worthy tool, regardless of fail_closed.",
	},
	{
		goCase:   "C8 slow decision fails open within the bound",
		baseCase: "TestFailOpen (timeout flows through the network-error fail-open path)",
		status:   statusGoExtension,
		note:     "Base has a per-request timeout_seconds but no explicit 'slow evaluate times out -> fail open' case. C8 additionally pins the INV-3b latency bound (well under CC's 5s hook kill).",
	},
	{
		goCase:   "C9 fail-closed + STALE real verdict proceeds",
		baseCase: "",
		status:   statusGoExtension,
		note:     "Base has no verdict-staleness/TTL concept. C9 pins that a stale-but-real bundle verdict (sourceLocalBundle) proceeds under fail-closed — staleness never triggers fail-closed.",
	},

	// --- wire shape (activity envelope; the hook mirror is retired) ---
	{
		goCase:   "TestWire_ToolEventsAreActivityPairs (+ client/testdata/golden/activity_*.json)",
		baseCase: "",
		status:   statusGoExtension,
		note:     "The activity envelope for a tool call: ToolCall->ActivityStarted, ToolResult->ActivityCompleted sharing one activity_id, workflow_type set, no spans/span_count/hook_trigger, no client-set semantic_type. No base analog — the base SDK reserves ActivityCompleted for hook-LESS lifecycle events, so this shape is a deliberate shift-left divergence (see the tool-call-as-activity ADR). adapters/codex/wire_test.go asserts the identical contract; the two are independent copies so a drift in one adapter cannot pass by moving a shared helper.",
	},
	{
		goCase:   "",
		baseCase: "assert_hook_wire_shape (fake_core.py); test_started_and_completed_wire_shape",
		status:   statusBaseUnmapped,
		note:     "No longer mirrored, by design. The base assertion checks a flat hook SpanData under ActivityStarted+hook_trigger. shift-left emits no hook events and no spans: a hook process has no in-process OTel, so the span it used to send was fabricated to satisfy a shape rather than to record a measurement. Retiring it also dissolved ADR-0004's standing obligation to hand-maintain the mirror against upstream. Cost: no span rows, no span-level Merkle leaves, no server-side semantic_type for dev sessions.",
	},

	// --- shift-left-only (no base analog) ---
	{
		goCase:   "conformance_test.go TestEmittedEventsAreConformant (dev-event schema, content-capture off)",
		baseCase: "",
		status:   statusGoExtension,
		note:     "The adapter-facing normalized dev-event contract (SL-1 schema) is a shift-left concept; the base SDK has no separate normalized vocabulary. Guards INV-2 at the adapter boundary (content-free by default).",
	},

	// --- base required cases we do NOT (yet) mirror ---
	{
		goCase:   "",
		baseCase: "test_http_started_halt_not_sent_and_halt_shaped_error",
		status:   statusBaseUnmapped,
		note:     "HALT-specific mapping. shift-left maps HALT->deny/stop (schema $defs.verdict; MAPPING.md §4) but the C1..C9 suite has no dedicated HALT case (C1 covers BLOCK). Follow-up: a C-case that a HALT verdict denies with the halt reason.",
	},
	{
		goCase:   "",
		baseCase: "test_http_require_approval_rejected/allowed_*",
		status:   statusBaseUnmapped,
		note:     "REQUIRE_APPROVAL. E6-S6 built the verdict->CC 'ask' HITL mapping (tested in the E6-S6 apply-verdict tests), but the C1..C9 conformance SUITE has no require-approval case. Follow-up: fold an approval case into the enforcement conformance suite.",
	},
	{
		goCase:   "",
		baseCase: "test_completed_block_marks_future_blocked_op_already_ran",
		status:   statusBaseUnmapped,
		note:     "N/A by design. shift-left enforces on PreToolUse (started stage) ONLY; a ToolResult/completed hook does not gate execution (the tool already ran), so the base 'completed-block affects future work' invariant has no enforcement surface here.",
	},
	{
		goCase:   "",
		baseCase: "TestContextCases (bind before hooks / reset after / trace lookup / executor thread)",
		status:   statusBaseUnmapped,
		note:     "N/A by design. The base SDK threads an in-process ActivityContext across hooks; shift-left hooks are stateless separate processes that derive ids deterministically (client/payload.go activityIDFor) + share state via the SL-4 spool — there is no per-activity context store to bind/reset.",
	},
}

var hexNote = regexp.MustCompile(`\S`) // any non-space char -> note is non-empty

// TestConformanceParityMatrix guards the parity record: every row is well-formed,
// every enforcement case C1..C9 is present exactly once, and the base<->go linkage
// is consistent with each row's status. This breaks HERE if a conformance case is
// renamed/dropped or the matrix drifts, rather than leaving a stale doc.
func TestConformanceParityMatrix(t *testing.T) {
	validStatus := map[parityStatus]bool{statusParity: true, statusGoExtension: true, statusBaseUnmapped: true}

	for i, r := range conformanceParity {
		if !validStatus[r.status] {
			t.Errorf("row %d: invalid status %q", i, r.status)
		}
		if !hexNote.MatchString(r.note) {
			t.Errorf("row %d (%q/%q): empty note — every mapping must justify itself", i, r.goCase, r.baseCase)
		}
		switch r.status {
		case statusParity:
			if r.goCase == "" || r.baseCase == "" {
				t.Errorf("row %d: status=parity requires BOTH a goCase and a baseCase", i)
			}
		case statusGoExtension:
			if r.goCase == "" {
				t.Errorf("row %d: status=go-extension requires a goCase (shift-left-only behavior)", i)
			}
		case statusBaseUnmapped:
			if r.baseCase == "" || r.goCase != "" {
				t.Errorf("row %d: status=base-unmapped requires a baseCase and no goCase", i)
			}
		}
	}

	// Every enforcement case C1..C9 must appear exactly once — the suite is the
	// canonical enforcement contract; a dropped case must fail loudly.
	for _, id := range []string{"C1", "C2", "C3", "C4", "C5", "C6", "C7", "C8", "C9"} {
		n := 0
		for _, r := range conformanceParity {
			if len(r.goCase) >= 2 && r.goCase[:2] == id {
				n++
			}
		}
		if n != 1 {
			t.Errorf("enforcement case %s appears %d times in the parity matrix, want exactly 1", id, n)
		}
	}
}

// assertActivityWireShape is the envelope contract every tool event must satisfy
// on the wire. It replaces client.AssertHookWireShape, which checked the flat
// hook-span shape this repo no longer emits.
//
// Its twin lives in adapters/codex/wire_test.go and asserts the identical
// contract. The two are deliberate copies rather than a shared helper: the
// adapters are separate Go modules, and the property under test is that both
// produce the SAME shape independently — a shared helper they both called could
// drift with them and still pass.
func assertActivityWireShape(t *testing.T, payload map[string]any, wantType string) {
	t.Helper()
	if payload["event_type"] != wantType {
		t.Errorf("event_type = %v, want %s", payload["event_type"], wantType)
	}
	// The retired hook envelope. A key here means the span layer grew a caller.
	for _, k := range []string{"spans", "span_count", "hook_trigger"} {
		if v, present := payload[k]; present {
			t.Errorf("payload carries retired key %q = %v", k, v)
		}
	}
	for _, k := range []string{"source", "event_type", "workflow_id", "run_id", "workflow_type", "activity_id", "activity_type", "timestamp"} {
		if v, _ := payload[k].(string); v == "" {
			t.Errorf("missing required envelope field %q", k)
		}
	}
	if payload["workflow_type"] != "developer-session" {
		t.Errorf("workflow_type = %v, want developer-session", payload["workflow_type"])
	}
	// semantic_type was computed by core from the span. With no span there is no
	// classification, and the client must not invent one — an unowned field would
	// be a claim nothing verifies.
	if _, present := payload["semantic_type"]; present {
		t.Error("client must not set semantic_type")
	}
}

// TestWire_ToolEventsAreActivityPairs drives the REAL client and asserts the
// activity envelope end to end: a PreToolUse becomes an ActivityStarted, its
// PostToolUse an ActivityCompleted, and the pair shares one activity_id — which
// is what puts them on one dashboard row and what makes one approval cover both
// halves and any retry.
func TestWire_ToolEventsAreActivityPairs(t *testing.T) {
	var bodies [][]byte
	srv := memhttptest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		bodies = append(bodies, raw)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"verdict":"allow"}`))
	}))
	defer srv.Close()

	cl, err := client.New(client.Config{
		BaseURL:       srv.URL,
		APIKey:        "obx_test_0123456789abcdef0123456789abcdef0123456789abcdef",
		DID:           testDID,
		PrivateKeyB64: testPrivateKeyB64,
	})
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}

	m := testMapper()
	hook := &HookEvent{SessionID: "sess-parity", ToolName: "Write", ToolUseID: "toolu_1",
		ToolInput: json.RawMessage(`{"file_path":"/repo/a.go","content":"x"}`)}
	pre, ok := m.Map(HookPreToolUse, hook)
	if !ok {
		t.Fatal("PreToolUse did not map")
	}
	post, ok := m.Map(HookPostToolUse, hook)
	if !ok {
		t.Fatal("PostToolUse did not map")
	}
	for _, ev := range []client.DevEvent{pre, post} {
		if _, err := cl.Emit(context.Background(), ev); err != nil {
			t.Fatalf("Emit: %v", err)
		}
	}
	if len(bodies) != 2 {
		t.Fatalf("expected 2 wire bodies, got %d", len(bodies))
	}

	activityID := func(raw []byte, wantType string) string {
		var payload map[string]any
		if err := json.Unmarshal(raw, &payload); err != nil {
			t.Fatalf("decode wire body: %v\n%s", err, raw)
		}
		assertActivityWireShape(t, payload, wantType)
		id, _ := payload["activity_id"].(string)
		return id
	}

	started := activityID(bodies[0], "ActivityStarted")
	completed := activityID(bodies[1], "ActivityCompleted")
	if started != completed {
		t.Errorf("the two halves of one tool call must share activity_id: %s vs %s", started, completed)
	}
}

// ── STORY-SL7-B: cross-ADAPTER parity (Claude Code ↔ Codex) ──────────────────
//
// SL7-B ports the E6 enforce cascade onto a SECOND provider (Codex,
// adapters/codex/enforce_conformance_test.go, cases CDX-C1..CDX-C12). This matrix
// is the durable record that both adapters assert the SAME invariant set, and
// where Codex's contract forces a documented DELTA. It is data-only (no import of
// the codex module — a separate Go module); TestCrossAdapterParityMatrix_SL7B guards
// it from drifting out of date. goCase = the Codex case; baseCase = the Claude Code
// case it mirrors ("" when Codex-only).
var codexConformanceParity = []parityRow{
	{goCase: "CDX-C1 enforced BLOCK denies pre-execution", baseCase: "C1 enforced BLOCK denies pre-execution", status: statusParity,
		note: "Identical invariant; Codex emits permissionDecision:deny + permissionDecisionReason (schema.rs PreToolUsePermissionDecisionWire) vs CC's deny. Real block carries the policy reason, not the fail-closed reason."},
	{goCase: "CDX-C2 fail-open + outage proceeds (OD9)", baseCase: "C2 fail-open + outage proceeds (OD9)", status: statusParity,
		note: "Cold-start/no-bundle fail-open proceeds under both; bounded by the derived whole-hook budget (probe P1: Codex fails open past the installed hook timeout)."},
	{goCase: "CDX-C3 fail-open + unbundled/no-match proceeds", baseCase: "C3 fail-open + unbundled proceeds (default unchanged)", status: statusParity,
		note: "Same fail-open default; both evaluate a local bundle in-process (ADR-0006)."},
	{goCase: "CDX-C4 fail-closed + outage denies", baseCase: "C4 fail-closed + outage denies", status: statusParity,
		note: "Synthesized HALT → deny under fail-closed on a no-verdict outage; content-free fail-closed reason."},
	{goCase: "CDX-C5 fail-closed never denies a REAL allow", baseCase: "C5 fail-closed never denies a REAL allow", status: statusParity,
		note: "Fail-closed engages on no-verdict ONLY (isRealVerdictSource), never a real allow — identical to CC."},
	{goCase: "CDX-C6 fail-closed + unbundled denies", baseCase: "C6 fail-closed + unbundled denies (INFO-1 hole closed)", status: statusParity,
		note: "Reachable-but-unbundled degraded state (LESSON-E6E7-04): fail-closed denies rather than being silently ungoverned."},
	{goCase: "CDX-C7 observe mode never blocks (byte-parity)", baseCase: "C7 observe mode never blocks (INV-3 verbatim)", status: statusParity,
		note: "Enforce off ⇒ empty stdout even for a BLOCK-worthy tool, regardless of fail_closed. Codex parses hook stdout as output JSON, so the empty-stdout guarantee is load-bearing."},
	{goCase: "CDX-C8 hook-timeout fail-open bound (probe P1)", baseCase: "C8 slow decision fails open within the bound", status: statusParity,
		note: "Degraded state (LESSON-E6E7-04): both providers FAIL OPEN on a hook timeout (CC 5s kill; Codex kills at the installed `timeout` — probe P1, live). CC bounds a network wait; Codex has no in-process network path (ADR-0006), so the case asserts the static invariant that the derived whole-hook budget lands before Codex's kill."},
	{goCase: "CDX-C9 fail-closed + STALE real verdict proceeds", baseCase: "C9 fail-closed + STALE real verdict proceeds", status: statusParity,
		note: "Staleness never triggers fail-closed (keys on source, not Stale) — identical to CC."},
	{goCase: "CDX-C10 secret in apply_patch body → redact-and-continue", baseCase: "C10 secret in Write body → redact-and-continue (E6-S9)", status: statusParity,
		note: "DELTA: the redactable body rides tool_input[\"command\"] (apply_patch patch text) not content/new_string, and Codex requires permissionDecision:allow + updatedInput to carry a rewrite (CC emits updatedInput alone). Same structural guarantee: content-only field swap, raw secret never egresses (INV-2)."},
	{goCase: "CDX-C11 secret detection OFF → no redaction", baseCase: "C11 secret detection OFF → no redaction (opt-out, E6-S9)", status: statusParity,
		note: "Opt-out + capture off ⇒ proceed path writes nothing — identical to CC."},
	{goCase: "CDX-C12 REQUIRE_APPROVAL → deny (OD-SL7-ASK)", baseCase: "", status: statusGoExtension,
		note: "Codex-only DELTA: CC maps REQUIRE_APPROVAL→ask (native prompt). Codex's runtime REJECTS permissionDecision:ask (output_parser.rs 'unsupported permissionDecision:ask') and a no-decision under approval_policy=never auto-runs (probe P3), so per the ruled OD-SL7-ASK every REQUIRE_APPROVAL quadrant DENIES with a content-free reason — strictly tighter. This is the base-unmapped 'require_approval' row the CC matrix noted, now covered on Codex."},
	{goCase: "CDX tighten-only: allow never bare", baseCase: "", status: statusGoExtension,
		note: "Codex-only structural invariant: permissionDecision:allow is emitted ONLY bundled with a redacting updatedInput (never a grant — OD-SL7-ALLOW-REWRITE); a plain allow writes NOTHING. Codex itself rejects a bare allow ('unsupported permissionDecision:allow'), and any-deny-wins + no approval-bypass lever means allow+updatedInput cannot loosen."},
}

// TestCrossAdapterParityMatrix_SL7B guards the CC↔Codex parity record: every row
// well-formed, every Codex enforcement case CDX-C1..CDX-C12 present exactly once,
// and each parity row names its CC analog. Breaks HERE if a Codex conformance case
// is renamed/dropped, rather than leaving a stale cross-repo claim.
func TestCrossAdapterParityMatrix_SL7B(t *testing.T) {
	validStatus := map[parityStatus]bool{statusParity: true, statusGoExtension: true, statusBaseUnmapped: true}
	for i, r := range codexConformanceParity {
		if !validStatus[r.status] {
			t.Errorf("codex row %d: invalid status %q", i, r.status)
		}
		if !hexNote.MatchString(r.note) {
			t.Errorf("codex row %d (%q): empty note", i, r.goCase)
		}
		switch r.status {
		case statusParity:
			if r.goCase == "" || r.baseCase == "" {
				t.Errorf("codex row %d: status=parity requires BOTH a goCase and a baseCase", i)
			}
		case statusGoExtension:
			if r.goCase == "" {
				t.Errorf("codex row %d: status=go-extension requires a goCase", i)
			}
		}
	}
	for _, id := range []string{"CDX-C1 ", "CDX-C2 ", "CDX-C3 ", "CDX-C4 ", "CDX-C5 ", "CDX-C6 ", "CDX-C7 ", "CDX-C8 ", "CDX-C9 ", "CDX-C10 ", "CDX-C11 ", "CDX-C12 "} {
		n := 0
		for _, r := range codexConformanceParity {
			if strings.HasPrefix(r.goCase, id) {
				n++
			}
		}
		if n != 1 {
			t.Errorf("codex enforcement case %q appears %d times, want exactly 1", strings.TrimSpace(id), n)
		}
	}
}
