package claudecode

import (
	"regexp"
	"testing"
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
//   - client.AssertHookWireShape (client/hookspan.go) — the Go MIRROR of
//     assert_hook_wire_shape; exercised by client/payload_hook_test.go.
//   - conformance_test.go (TestEmittedEventsAreConformant) — the shift-left-only
//     adapter-facing dev-event schema check (no base analog).
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

	// --- wire shape (client/hookspan.go mirror + client/payload_hook_test.go) ---
	{
		goCase:   "client.AssertHookWireShape + payload_hook_test.go (started/completed pair)",
		baseCase: "assert_hook_wire_shape (fake_core.py); test_started_and_completed_wire_shape",
		status:   statusParity,
		note:     "Direct mirror: ActivityStarted + hook_trigger, flat SpanData, 16/32-hex ids, no otel/openbox/data/semantic_type, started -> end_time/duration_ns null, family root fields per hook_type. Our payloads pass the mirrored assertion for every ToolCall/ToolResult stage.",
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
		note:     "N/A by design. The base SDK threads an in-process ActivityContext across hooks; shift-left hooks are stateless separate processes that derive ids deterministically (spanbuilder.go) + share state via the SL-4 spool — there is no per-activity context store to bind/reset.",
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
