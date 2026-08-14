package claudecode

import (
	"bytes"
	"encoding/json"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"time"
)

// Enforcement conformance suite (STORY-E6-S7) — executable INV-3b evidence.
//
// This drives the REAL RunHook PreToolUse path end-to-end against a REAL
// /evaluate stub (or a deliberately-unreachable one) and asserts the exact Claude
// Code stdout contract per quadrant of the enforcement carve-out (ADR-0002 /
// INV-3b). It is the durable proof the carve-out holds: a regression to enforce
// mode, the failure policy, or the fail-open default breaks HERE rather than
// silently in the field. Each case is content-free (INV-1/INV-2): no asserted
// reason may carry the shell command / file body / tool output.
//
// | # | Enforce | Policy      | Control plane      | Expect | Proves                          |
// |---|---------|-------------|--------------------|--------|---------------------------------|
// | C1| on      | both        | up + BLOCK rule    | deny   | enforced BLOCK denies pre-exec  |
// | C2| on      | fail-open   | absent socket      | proceed| outage fails open (OD9)         |
// | C3| on      | fail-open   | up, NO bundle      | proceed| unbundled fails open (default)  |
// | C4| on      | fail-closed | absent socket      | deny   | fail-closed denies on outage    |
// | C5| on      | fail-closed | up + ALLOW default | proceed| fail-closed never denies allow  |
// | C6| on      | fail-closed | up, NO bundle      | deny   | INFO-1: the closed hole         |
// | C7| off     | —           | up + BLOCK rule    | proceed| INV-3 verbatim (observe)        |
// | C8| — (removed: in-process decision had no network timeout — ADR-0006)          |
// | C9| — (removed: nothing local can be stale — ADR-0017)                          |
// |C10| on      | fail-open   | up + secret in Write| redact | Tier-1 redact-and-continue (E6-S9)|
// |C11| on      | fail-open   | up, detection OFF  | proceed| opt-out → no redaction (E6-S9)  |
//
// C18–C19 assert the content gate on the bytes POSTed to /evaluate. C20–C26 do
// the same for ADR-0018, driving the real observe/turn paths and the flush that
// delivers them:
//
// |C20| PostToolUse            | status:"completed" on the wire                  |
// |C21| the same, capture OFF  | status is structural, not gated                 |
// |C22| PostToolUseFailure     | status:"failed" + a duration paired across hooks|
// |C23| the three new signals  | signal_name present, free text and signal_args absent |
// |C24| Stop, capture ON       | ONE span core's alignment extractor can read    |
// |C25| Stop, capture OFF      | no span, no span_count, no text — turns still ship |
// |C26| Stop + a secret        | redacted BEFORE attachment (C18 discipline)     |

// The local bundle helpers are gone with the evaluator they fed (ADR-0017). A
// case's expected outcome is a SERVER verdict now, so setup names the verdict
// directly through serveVerdict rather than building a rule the client would
// have evaluated itself.

func TestEnforcementConformance(t *testing.T) {
	// Base isolation shared by every case (identity + spool/session/enforcement
	// sinks under temp dirs; content capture OFF — no redaction engine is in play).
	isolateConfig(t)
	t.Setenv(envDID, testDID)
	t.Setenv("OPENBOX_SPOOL_DIR", t.TempDir())
	t.Setenv("OPENBOX_SESSION_DIR", t.TempDir())
	t.Setenv(envEnforcementFile, filepath.Join(t.TempDir(), "enf.jsonl"))
	t.Setenv(envContentCapture, "0")

	// A dangerous Bash call (matches the BLOCK rule); the axis several cases key on.
	const dangerPayload = `{"hook_event_name":"PreToolUse","session_id":"s","cwd":"/tmp","tool_name":"Bash","tool_input":{"command":"rm -rf /tmp/x"}}`

	// run executes ONE PreToolUse hook with the currently-set env and returns stdout.
	run := func(t *testing.T, payload string) string {
		t.Helper()
		var stdout bytes.Buffer
		RunHook("PreToolUse", strings.NewReader(payload), &stdout, log.New(&bytes.Buffer{}, "", 0))
		return stdout.String()
	}
	// assertNoLeak guards INV-2 across every case: no asserted output carries the
	// shell command that was gated.
	assertNoLeak := func(t *testing.T, out string) {
		t.Helper()
		if strings.Contains(out, "rm -rf") {
			t.Errorf("stdout leaked the shell command (INV-2): %q", out)
		}
	}

	t.Run("C1 enforced BLOCK denies pre-execution (either policy)", func(t *testing.T) {
		// A real BLOCK denies under BOTH fail-open and fail-closed (structurally
		// identical: a real verdict is hookflow.FailOpen=false → applyFailurePolicy is a no-op),
		// and with the POLICY reason — never the fail-closed outage reason (fail-closed
		// engages on no-verdict only; the Q1-vs-Q4 distinction).
		serveVerdict(t, `{"verdict":"block","reason":"destructive recursive delete","policy_id":"conf-policy"}`)
		t.Setenv(envEnforce, "1")
		for _, fc := range []string{"0", "1"} {
			t.Setenv(envFailClosed, fc)
			// A DISTINCT command per iteration. The event id is derived from the
			// call, so replaying the identical payload in one process collapses
			// onto one idempotency key and the second POST is suppressed — the
			// client behaving correctly, and a real session never does it.
			out := run(t, `{"hook_event_name":"PreToolUse","session_id":"s","cwd":"/tmp","tool_name":"Bash","tool_input":{"command":"rm -rf /tmp/x`+fc+`"}}`)
			d, reason := parsePermissionDecision(t, []byte(out))
			if d != ccDecisionDeny {
				t.Fatalf("fail_closed=%s: permissionDecision = %q, want deny (stdout=%q)", fc, d, out)
			}
			if !strings.Contains(reason, "destructive recursive delete") || !strings.Contains(reason, "conf-policy") {
				t.Errorf("fail_closed=%s: reason = %q, want the policy reason + id", fc, reason)
			}
			if strings.Contains(reason, "fail-closed") {
				t.Errorf("fail_closed=%s: a REAL block must carry the policy reason, not the fail-closed reason: %q", fc, reason)
			}
			assertNoLeak(t, out)
		}
	})

	t.Run("C2 fail-open + outage proceeds within bound (OD9)", func(t *testing.T) {
		t.Setenv(envEnforce, "1")
		t.Setenv(envFailClosed, "0")
		start := time.Now()
		out := run(t, dangerPayload)
		if strings.TrimSpace(out) != "" {
			t.Errorf("fail-open outage must proceed (empty stdout); got %q", out)
		}
		if elapsed := time.Since(start); elapsed > 3*time.Second {
			t.Errorf("enforce wait %v exceeds the INV-3b bound (CC kills the hook at 5s)", elapsed)
		}
	})

	t.Run("C3 fail-open + unbundled proceeds (fix leaves default unchanged)", func(t *testing.T) {
		t.Setenv(envEnforce, "1")
		t.Setenv(envFailClosed, "0")
		if out := run(t, dangerPayload); strings.TrimSpace(out) != "" {
			t.Errorf("fail-open + unbundled must proceed (byte-identical to pre-fix); got %q", out)
		}
	})

	t.Run("C4 fail-closed + outage denies", func(t *testing.T) {
		t.Setenv(envEnforce, "1")
		t.Setenv(envFailClosed, "1")
		out := run(t, dangerPayload)
		d, reason := parsePermissionDecision(t, []byte(out))
		if d != ccDecisionDeny {
			t.Fatalf("fail-closed + outage: permissionDecision = %q, want deny (stdout=%q)", d, out)
		}
		if !strings.Contains(reason, "fail-closed") {
			t.Errorf("reason = %q, want it to explain the fail-closed outage", reason)
		}
		assertNoLeak(t, out)
	})

	t.Run("C5 fail-closed never denies a REAL allow", func(t *testing.T) {
		// The crux clause: fail-closed denies only when no real verdict could be
		// obtained, never a verdict that says allow.
		//
		// "Real" moved with ADR-0017. It used to mean a local bundle's allow;
		// the decider is the server now, so a reachable /evaluate returning
		// allow is what has to proceed. A local allow with the server
		// unreachable is no longer a real verdict — it is an outage, and C4/C6
		// cover that it denies.
		serveVerdict(t, `{"verdict":"allow"}`)
		url, hits := serveEvaluate(t, `{"verdict":"allow"}`, 200, 0)
		evalCreds(t, url)
		t.Setenv(envEnforce, "1")
		t.Setenv(envFailClosed, "1")
		benign := `{"hook_event_name":"PreToolUse","session_id":"s","cwd":"/tmp","tool_name":"Bash","tool_input":{"command":"echo hi"}}`
		if out := run(t, benign); strings.TrimSpace(out) != "" {
			t.Errorf("fail-closed must NOT block a real allow; got %q", out)
		}
		if atomic.LoadInt32(hits) != 1 {
			t.Errorf("/evaluate hits = %d, want 1 — the allow must come from the server", atomic.LoadInt32(hits))
		}
	})

	t.Run("C6 fail-closed + unbundled denies (E6-S3 INFO-1 closed)", func(t *testing.T) {
		// The regression guard for the reconciliation: no real verdict (no bundle
		// loaded → Source=fail-open:no-bundle → hookflow.FailOpen), so a fail-closed org DENIES
		// rather than being silently ungoverned. Pre-fix this proceeded (the hole).
		t.Setenv(envEnforce, "1")
		t.Setenv(envFailClosed, "1")
		out := run(t, dangerPayload)
		d, reason := parsePermissionDecision(t, []byte(out))
		if d != ccDecisionDeny {
			t.Fatalf("fail-closed + unbundled: permissionDecision = %q, want deny (INFO-1 hole); stdout=%q", d, out)
		}
		if !strings.Contains(reason, "fail-closed") {
			t.Errorf("reason = %q, want the content-free fail-closed reason", reason)
		}
		assertNoLeak(t, out)
	})

	t.Run("C7 observe mode never blocks (INV-3 verbatim)", func(t *testing.T) {
		// Even with a live BLOCK bundle, enforce OFF is the observe path: nothing to
		// stdout, ever. This is the un-carved-out INV-3.
		serveVerdict(t, `{"verdict":"block","reason":"destructive recursive delete","policy_id":"conf-policy"}`)
		t.Setenv(envEnforce, "0")
		t.Setenv(envFailClosed, "1") // even fail_closed=1 must not matter with enforce off
		if out := run(t, dangerPayload); strings.TrimSpace(out) != "" {
			t.Errorf("observe mode must write nothing to stdout even for a BLOCK-worthy tool; got %q", out)
		}
	})

	// C9 (a STALE local verdict must not trigger fail-closed) is deleted with the
	// bundle it read. Staleness described a local artifact falling behind the
	// control plane; every verdict comes from the control plane now, so there is
	// nothing that can be stale. Nothing replaces it — the condition cannot arise.

	// C8 removed: in-process decision has no network timeout (ADR-0006). The old case
	// exercised the socket Client's hard timeout tripping and failing open; with the
	// synchronous in-memory decider there is no latency path to bound.

	// A Write whose body contains a real-shaped AWS key. The secret string must never
	// survive on stdout / in any egress or audit sink; the placeholder must appear.
	const awsSecret = "AKIAIOSFODNN7EXAMPLE"
	secretWrite := `{"hook_event_name":"PreToolUse","session_id":"s","cwd":"/tmp","tool_name":"Write","tool_input":{"file_path":"/tmp/app.env","content":"AWS_ACCESS_KEY_ID=` + awsSecret + `"}}`

	// assertNoEgress walks the spool + session dirs and the enforcement audit and
	// asserts the raw secret never reached any egress/audit surface (INV-2).
	assertNoEgress := func(t *testing.T) {
		t.Helper()
		for _, dir := range []string{os.Getenv("OPENBOX_SPOOL_DIR"), os.Getenv("OPENBOX_SESSION_DIR"), filepath.Dir(os.Getenv(envEnforcementFile))} {
			if dir == "" {
				continue
			}
			_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
				if err != nil || d.IsDir() {
					return nil
				}
				b, _ := os.ReadFile(p)
				if strings.Contains(string(b), awsSecret) {
					t.Errorf("secret egressed/persisted to %s (INV-2): %s", p, b)
				}
				return nil
			})
		}
	}

	// serveCapturing records every body POSTed to /evaluate, so a case can assert
	// on the actual outbound bytes rather than on what should have been sent.
	serveCapturing := func(t *testing.T, verdict string) *[]string {
		t.Helper()
		var mu sync.Mutex
		var bodies []string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, _ := io.ReadAll(r.Body)
			mu.Lock()
			bodies = append(bodies, string(raw))
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(verdict))
		}))
		t.Cleanup(srv.Close)
		evalCreds(t, srv.URL)
		return &bodies
	}

	// THE tripwire for ADR-0017's E8. Write bodies egress now, so the local
	// redaction is the one control standing between a secret in a file and the
	// control plane's event storage — which is the hardest place to purge
	// anything from.
	//
	// It asserts on the bytes actually POSTed, not on the decision or the
	// rewrite: a correct redaction applied to the tool call while the ORIGINAL
	// body is sent for evaluation would satisfy every other test in this file
	// and still leak. Never weaken this to a substring check on a decision.
	t.Run("C18 a secret in a Write body never reaches /evaluate (E8)", func(t *testing.T) {
		serveVerdict(t, `{"verdict":"allow"}`)
		bodies := serveCapturing(t, `{"verdict":"allow"}`)
		t.Setenv(envEnforce, "1")
		t.Setenv(envFailClosed, "0")
		t.Setenv(envContentCapture, "1") // content ON: the body is attached
		os.Unsetenv(envSecretDetection)  // detection default ON
		run(t, secretWrite)

		if len(*bodies) == 0 {
			t.Fatal("no /evaluate call — a gated Write must be evaluated (ADR-0017)")
		}
		for i, b := range *bodies {
			if strings.Contains(b, awsSecret) {
				t.Errorf("the raw secret reached /evaluate in body #%d — redaction must "+
					"run BEFORE attachment: %s", i, b)
			}
		}
		// The body must actually be attached, or this test would pass by sending
		// nothing at all and prove nothing.
		joined := strings.Join(*bodies, "")
		if !strings.Contains(joined, "OPENBOX_REDACTED") {
			t.Errorf("no redacted body attached; the case proves nothing if content never "+
				"egressed at all: %s", joined)
		}
	})

	// The other half: content_capture is a hard gate, not a best-effort filter.
	// With it off, no class attaches anything and the server decides on the
	// structural axes alone — coarser enforcement, which is the honest trade.
	t.Run("C19 content_capture:false attaches no content, any class", func(t *testing.T) {
		serveVerdict(t, `{"verdict":"allow"}`)
		bodies := serveCapturing(t, `{"verdict":"allow"}`)
		t.Setenv(envEnforce, "1")
		t.Setenv(envFailClosed, "0")
		t.Setenv(envContentCapture, "0")

		const canary = "CANARY-CONTENT-must-not-egress"
		for _, payload := range []string{
			`{"hook_event_name":"PreToolUse","session_id":"s","cwd":"/tmp","tool_name":"Write","tool_input":{"file_path":"/tmp/a","content":"` + canary + `"}}`,
			`{"hook_event_name":"PreToolUse","session_id":"s","cwd":"/tmp","tool_name":"Bash","tool_input":{"command":"` + canary + `"}}`,
			`{"hook_event_name":"PreToolUse","session_id":"s","cwd":"/tmp","tool_name":"mcp__x__y","tool_input":{"arg":"` + canary + `"}}`,
			// The path is deliberately canary-free: a file_path is a structural
			// locator and egresses on every event by design, capture on or off.
			// What must not appear is the tool's CONTENT.
			`{"hook_event_name":"PreToolUse","session_id":"s","cwd":"/tmp","tool_name":"Read","tool_input":{"file_path":"/tmp/plain.txt","pattern":"` + canary + `"}}`,
		} {
			run(t, payload)
		}
		if len(*bodies) == 0 {
			t.Fatal("no /evaluate calls — every class is gated")
		}
		for i, b := range *bodies {
			if strings.Contains(b, canary) {
				t.Errorf("content egressed with capture OFF in body #%d: %s", i, b)
			}
			// Structural axes must still be there, or enforcement would have
			// nothing to decide on and "no content" would be indistinguishable
			// from "no evaluation".
			if !strings.Contains(b, `"tool_name"`) {
				t.Errorf("body #%d carries no structural axes: %s", i, b)
			}
		}
	})

	// ── ADR-0018: the outcome field ────────────────────────────────────────────
	//
	// Core's per-tool success metric reads ONE thing:
	//
	//	metric.IsSuccess = payload.Status != nil && *payload.Status == "completed"
	//
	// so these two cases assert the literal on the bytes actually POSTed, not on
	// the DevEvent or the mapper's return. A field that is correct in the struct
	// and absent from the wire looks identical to every unit test and leaves the
	// dashboard at 0.0% — which is the state this field exists to fix.
	//
	// observeThenFlush drives the real observe path for one hook and then the
	// SessionEnd flush that delivers it, returning what reached /evaluate.
	observeThenFlush := func(t *testing.T, hook, payload string) []string {
		t.Helper()
		bodies := serveCapturing(t, `{"verdict":"allow"}`)
		// The detached realtime flusher would fork a second process mid-test and
		// race the assertion; SessionEnd's flush is the deterministic delivery.
		t.Setenv(envRealtime, "0")
		t.Setenv(envEnforce, "0") // observe path — no gate, no deferred spool
		var out bytes.Buffer
		RunHook(hook, strings.NewReader(payload), &out, log.New(&bytes.Buffer{}, "", 0))
		RunHook("SessionEnd", strings.NewReader(
			`{"hook_event_name":"SessionEnd","session_id":"s-status","cwd":"/tmp","reason":"other"}`),
			&out, log.New(&bytes.Buffer{}, "", 0))
		return *bodies
	}

	t.Run("C20 a completed tool call reports status \"completed\" on the outbound bytes", func(t *testing.T) {
		bodies := observeThenFlush(t, "PostToolUse",
			`{"hook_event_name":"PostToolUse","session_id":"s-status","cwd":"/tmp","tool_name":"Bash","tool_use_id":"toolu_c20","tool_input":{"command":"go test ./..."},"tool_response":{"output":"ok"}}`)

		var completed string
		for _, b := range bodies {
			if strings.Contains(b, `"event_type":"ActivityCompleted"`) && strings.Contains(b, `"activity_type":"Bash"`) {
				completed = b
			}
		}
		if completed == "" {
			t.Fatalf("no ActivityCompleted reached /evaluate; bodies=%v", bodies)
		}
		// The exact literal, on the wire. Not `strings.Contains(b, "completed")`
		// — "ActivityCompleted" contains that substring and would pass forever.
		if !strings.Contains(completed, `"status":"completed"`) {
			t.Errorf("completed tool call carries no \"status\":\"completed\"; core scores it as a failure: %s", completed)
		}
	})

	t.Run("C21 status ships unchanged with content_capture:false", func(t *testing.T) {
		// The whole point of deriving status structurally: an org that sends no
		// content still gets its success metric. If this ever regresses, Tool
		// Health silently becomes a privacy-setting-dependent feature.
		t.Setenv(envContentCapture, "0")
		bodies := observeThenFlush(t, "PostToolUse",
			`{"hook_event_name":"PostToolUse","session_id":"s-status","cwd":"/tmp","tool_name":"Read","tool_use_id":"toolu_c21","tool_input":{"file_path":"/tmp/a.txt"}}`)

		var found bool
		for _, b := range bodies {
			if strings.Contains(b, `"event_type":"ActivityCompleted"`) && strings.Contains(b, `"status":"completed"`) {
				found = true
			}
		}
		if !found {
			t.Errorf("status absent with capture off — it is structural, not content: %v", bodies)
		}
	})

	// observeSeqThenFlush drives several observe hooks in order (so a PreToolUse
	// can leave its duration stash for the failure hook that follows) and then
	// the SessionEnd flush that delivers them.
	observeSeqThenFlush := func(t *testing.T, session string, steps ...[2]string) []string {
		t.Helper()
		bodies := serveCapturing(t, `{"verdict":"allow"}`)
		t.Setenv(envRealtime, "0")
		t.Setenv(envEnforce, "0")
		var out bytes.Buffer
		for _, s := range steps {
			RunHook(s[0], strings.NewReader(s[1]), &out, log.New(&bytes.Buffer{}, "", 0))
		}
		RunHook("SessionEnd", strings.NewReader(
			`{"hook_event_name":"SessionEnd","session_id":"`+session+`","cwd":"/tmp","reason":"other"}`),
			&out, log.New(&bytes.Buffer{}, "", 0))
		return *bodies
	}

	t.Run("C22 a failed tool call reports status \"failed\" with a real duration", func(t *testing.T) {
		bodies := observeSeqThenFlush(t, "s-fail",
			[2]string{"PreToolUse", `{"hook_event_name":"PreToolUse","session_id":"s-fail","cwd":"/tmp","tool_name":"Bash","tool_use_id":"toolu_c22","tool_input":{"command":"exit 3"}}`},
			[2]string{"PostToolUseFailure", `{"hook_event_name":"PostToolUseFailure","session_id":"s-fail","cwd":"/tmp","tool_name":"Bash","tool_use_id":"toolu_c22","tool_input":{"command":"exit 3"},"error":"Error: exit code 3","is_interrupt":false,"duration_ms":562}`},
		)

		var failed string
		for _, b := range bodies {
			if strings.Contains(b, `"event_type":"ActivityCompleted"`) {
				failed = b
			}
		}
		if failed == "" {
			t.Fatalf("a failed tool call produced no ActivityCompleted; bodies=%v", bodies)
		}
		if !strings.Contains(failed, `"status":"failed"`) {
			t.Errorf("failed call does not report status \"failed\": %s", failed)
		}
		// The duration stash must pair across the two DIFFERENT hooks, or a
		// failed call reports no duration and the latency percentiles quietly
		// exclude every failure.
		if !strings.Contains(failed, `"duration_ms"`) {
			t.Errorf("failed call carries no duration_ms — the stash did not pair "+
				"PreToolUse with PostToolUseFailure: %s", failed)
		}
		// The provider's free-text error must not have ridden along.
		if strings.Contains(failed, "exit code 3") {
			t.Errorf("the tool's free-text error egressed (ADR-0019 owns it): %s", failed)
		}
	})

	t.Run("C23 lifecycle signals carry their signal_name and no free text", func(t *testing.T) {
		const denialReason = "DENIAL-REASON-must-not-egress"
		bodies := observeSeqThenFlush(t, "s-signals",
			[2]string{"SubagentStart", `{"hook_event_name":"SubagentStart","session_id":"s-signals","cwd":"/tmp","agent_id":"agt-1","agent_type":"code-reviewer"}`},
			[2]string{"PermissionDenied", `{"hook_event_name":"PermissionDenied","session_id":"s-signals","cwd":"/tmp","tool_name":"Bash","tool_use_id":"toolu_c23","tool_input":{"command":"rm -rf /"},"reason":"` + denialReason + `"}`},
			[2]string{"StopFailure", `{"hook_event_name":"StopFailure","session_id":"s-signals","cwd":"/tmp","error":"rate_limit","error_details":"` + denialReason + `"}`},
		)

		joined := strings.Join(bodies, "\n")
		for _, name := range []string{"subagent_started", "permission_denied", "api_error"} {
			if !strings.Contains(joined, `"signal_name":"`+name+`"`) {
				t.Errorf("no %s signal reached /evaluate: %s", name, joined)
			}
		}
		// Free text from either payload must be nowhere on the wire.
		if strings.Contains(joined, denialReason) {
			t.Errorf("free-text reason/error_details egressed: %s", joined)
		}
		// The structural detail that makes the events useful must be there, or
		// the assertion above would pass for events carrying nothing.
		for _, want := range []string{`"agent_type":"code-reviewer"`, `"error_type":"rate_limit"`, `"tool_use_id":"toolu_c23"`} {
			if !strings.Contains(joined, want) {
				t.Errorf("missing structural detail %s: %s", want, joined)
			}
		}
		// And NO signal_args on any of the three — core reads those as a new
		// user goal and overwrites the alignment session's goal with them
		// (age.go:112-137).
		for _, b := range bodies {
			for _, name := range []string{"subagent_started", "permission_denied", "api_error"} {
				if strings.Contains(b, `"signal_name":"`+name+`"`) && strings.Contains(b, `"signal_args"`) {
					t.Errorf("%s carries signal_args; core would overwrite the alignment goal with it: %s", name, b)
				}
			}
		}
	})

	// ── ADR-0018 Decision 2: the assistant-turn span ───────────────────────────
	//
	// The egress change. These drive a REAL Stop hook — transcript read, turn
	// window, mapper, spool, flush — and assert on what reached /evaluate,
	// because every failure mode here is silent: core logs and returns "" for a
	// span it cannot read, which is indistinguishable from the empty widgets
	// this change exists to fill.
	//
	// A minimal transcript with usage, so the Stop hook has a turn to report.
	turnTranscript := func(t *testing.T) string {
		t.Helper()
		p := filepath.Join(t.TempDir(), "transcript.jsonl")
		const body = `{"type":"assistant","isSidechain":false,"timestamp":"2026-08-13T09:00:01.500Z","message":{"model":"claude-opus-4-8","content":[{"type":"text","text":"reply"}],"usage":{"input_tokens":100,"output_tokens":30}}}` + "\n"
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	// stopThenFlush runs one Stop hook with the given assistant message and
	// returns the bodies that reached /evaluate.
	//
	// `capture` is set AFTER serveCapturing, deliberately: serveCapturing calls
	// evalCreds, which forces content capture OFF. Setting it before would be
	// silently overwritten, and the capture-ON case would then pass by proving
	// the capture-OFF behaviour.
	stopThenFlush := func(t *testing.T, session, transcript, assistantMessage, capture string) []string {
		t.Helper()
		bodies := serveCapturing(t, `{"verdict":"allow"}`)
		t.Setenv(envContentCapture, capture)
		t.Setenv(envRealtime, "0")
		t.Setenv(envEnforce, "0")
		t.Setenv(envFinops, "1") // turn events exist only under finops
		var out bytes.Buffer
		payload, err := json.Marshal(map[string]any{
			"hook_event_name":        "Stop",
			"session_id":             session,
			"cwd":                    "/tmp",
			"transcript_path":        transcript,
			"last_assistant_message": assistantMessage,
		})
		if err != nil {
			t.Fatal(err)
		}
		RunHook("Stop", bytes.NewReader(payload), &out, log.New(&bytes.Buffer{}, "", 0))
		if s := out.String(); strings.TrimSpace(s) != "" {
			t.Errorf("Stop wrote to stdout (%q); it can block a session, so every path "+
				"must stay silent (INV-3)", s)
		}
		RunHook("SessionEnd", strings.NewReader(
			`{"hook_event_name":"SessionEnd","session_id":"`+session+`","cwd":"/tmp","reason":"other"}`),
			&out, log.New(&bytes.Buffer{}, "", 0))
		return *bodies
	}

	t.Run("C24 a turn span carries the assistant text only with content capture on", func(t *testing.T) {
		const answer = "I refactored the spool and all 11 modules are green."
		bodies := stopThenFlush(t, "s-span", turnTranscript(t), answer, "1")

		var turn string
		for _, b := range bodies {
			if strings.Contains(b, `"activity_type":"llm_completion"`) && strings.Contains(b, `"spans"`) {
				turn = b
			}
		}
		if turn == "" {
			t.Fatalf("no turn payload carried a span; bodies=%v", bodies)
		}
		// The four conditions core's extractor checks, on the real outbound bytes.
		for _, want := range []string{
			`"span_count":1`,
			`"stage":"completed"`,
			`"semantic_type":"llm_completion"`,
			`"http.method":"POST"`,
			`api.anthropic.com`,
			`"openbox.span_synthetic":true`,
		} {
			if !strings.Contains(turn, want) {
				t.Errorf("turn span missing %s — core would classify it as something else and "+
					"the extractor would silently yield \"\": %s", want, turn)
			}
		}
		if !strings.Contains(turn, answer) {
			t.Errorf("the assistant text is not in the span: %s", turn)
		}
		// hook_trigger with spans present enters the approval-bypass path.
		if strings.Contains(turn, `"hook_trigger"`) {
			t.Errorf("turn payload carries hook_trigger alongside spans: %s", turn)
		}
	})

	t.Run("C25 content_capture:false emits no span and no span_count", func(t *testing.T) {
		const answer = "CANARY-ASSISTANT-TEXT-must-not-egress"
		bodies := stopThenFlush(t, "s-span-off", turnTranscript(t), answer, "0")

		var sawTurn bool
		for _, b := range bodies {
			if strings.Contains(b, `"activity_type":"llm_completion"`) {
				sawTurn = true
			}
			for _, forbidden := range []string{`"spans"`, `"span_count"`, answer} {
				if strings.Contains(b, forbidden) {
					t.Errorf("capture off but %s reached /evaluate: %s", forbidden, b)
				}
			}
		}
		// The turn events themselves must still ship — this is a content gate,
		// not a telemetry gate. Without this the case would pass for a client
		// that stopped emitting turns entirely.
		if !sawTurn {
			t.Errorf("no turn events at all with capture off; the gate is on CONTENT, "+
				"not on the turn: %v", bodies)
		}
	})

	t.Run("C26 a secret in the assistant text never reaches /evaluate", func(t *testing.T) {
		// Same discipline as C18: assert on the bytes POSTed, not on the mapper's
		// return. A redaction applied after attachment would satisfy every other
		// test in this file and still leak.
		os.Unsetenv(envSecretDetection) // default ON
		bodies := stopThenFlush(t, "s-span-secret", turnTranscript(t),
			"here is the key you asked for: AWS_ACCESS_KEY_ID="+awsSecret, "1")

		var turn string
		for _, b := range bodies {
			if strings.Contains(b, `"spans"`) {
				turn = b
			}
			if strings.Contains(b, awsSecret) {
				t.Errorf("the raw secret reached /evaluate — redaction must run BEFORE "+
					"attachment: %s", b)
			}
		}
		if turn == "" {
			t.Fatal("no span was sent at all; the case proves nothing if the text never egressed")
		}
		if !strings.Contains(turn, "OPENBOX_REDACTED") {
			t.Errorf("no redaction placeholder in the span body: %s", turn)
		}
	})

	t.Run("C10 secret in Write body → redact-and-continue (E6-S9)", func(t *testing.T) {
		// A reachable, BUNDLED allow decision. Secret detection is DEFAULT ON and
		// DECOUPLED from content_capture (which stays OFF): the file body reaches only
		// the local socket, the scanner redacts it, and the hook emits an updatedInput
		// with the content field sanitized and NO permissionDecision (proceed).
		serveVerdict(t, `{"verdict":"allow"}`)
		t.Setenv(envEnforce, "1")
		t.Setenv(envFailClosed, "0")
		t.Setenv(envContentCapture, "0") // egress stays metadata-only
		os.Unsetenv(envSecretDetection)  // default ON
		out := run(t, secretWrite)

		var got preToolUseOutput
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatalf("stdout not valid JSON: %v (%q)", err, out)
		}
		if got.HookSpecificOutput.PermissionDecision != "" {
			t.Errorf("redaction must proceed (no permissionDecision), got %q", got.HookSpecificOutput.PermissionDecision)
		}
		if len(got.HookSpecificOutput.UpdatedInput) == 0 {
			t.Fatalf("expected an updatedInput redaction; stdout=%q", out)
		}
		if strings.Contains(out, awsSecret) {
			t.Errorf("the raw secret must be redacted, not present on stdout: %q", out)
		}
		if !strings.Contains(out, "OPENBOX_REDACTED") {
			t.Errorf("expected the env-var redaction placeholder; got %q", out)
		}
		// Structural field preserved.
		var ui map[string]any
		_ = json.Unmarshal(got.HookSpecificOutput.UpdatedInput, &ui)
		if ui["file_path"] != "/tmp/app.env" {
			t.Errorf("file_path must survive reconstruction verbatim, got %v", ui["file_path"])
		}
		// Content-free audit signal recorded; raw secret never egressed/persisted.
		data, _ := os.ReadFile(os.Getenv(envEnforcementFile))
		if !strings.Contains(string(data), `"redacted":true`) {
			t.Errorf("expected redacted:true in the enforcement audit; got %s", data)
		}
		assertNoEgress(t)
	})

	t.Run("C11 secret detection OFF → no redaction (opt-out, E6-S9)", func(t *testing.T) {
		serveVerdict(t, `{"verdict":"allow"}`)
		t.Setenv(envEnforce, "1")
		t.Setenv(envFailClosed, "0")
		t.Setenv(envContentCapture, "0")
		t.Setenv(envSecretDetection, "0") // explicit opt-out
		if out := run(t, secretWrite); strings.TrimSpace(out) != "" {
			t.Errorf("with detection off + content-capture off the proceed path must write nothing (E6-S3 identical); got %q", out)
		}
		assertNoEgress(t)
	})
}

// ── Deleted with the local evaluator (ADR-0017) ──────────────────────────────
//
// TestEnforcementConformance_BuilderPolicy drove BLOCK / no-match /
// REQUIRE_APPROVAL through the LOCAL implementation of the backend's
// policy_builder semantics — the reimplementation whose permanent parity
// obligation is ADR-0017's central argument. Those three outcomes belong to the
// server now, and C12 / C13 / C14 assert them end to end against a real
// /evaluate.
//
// TestEnforcementConformance_StaleGate asserted that a stale-marked session
// denies under fail-closed and that clearing the marker restores proceed. Both
// are properties of a local bundle's freshness; there is no local bundle, so
// nothing can be stale. `openbox dev sync`, which cleared the marker, is retired
// in the same change.
