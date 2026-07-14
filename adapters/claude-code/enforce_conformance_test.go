package claudecode

import (
	"bytes"
	"context"
	"log"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/openbox-ai/openbox-shift-left/client"
	"github.com/openbox-ai/openbox-shift-left/sidecar"
)

// Enforcement conformance suite (STORY-E6-S7) — executable INV-3b evidence.
//
// This drives the REAL RunHook PreToolUse path end-to-end against a REAL
// sidecar.Server (or a deliberately-absent socket) and asserts the exact Claude
// Code stdout contract per quadrant of the enforcement carve-out (ADR-0002 /
// INV-3b). It is the durable proof the carve-out holds: a regression to enforce
// mode, the failure policy, or the fail-open default breaks HERE rather than
// silently in the field. Each case is content-free (INV-1/INV-2): no asserted
// reason may carry the shell command / file body / tool output.
//
// | # | Enforce | Policy      | Sidecar            | Expect | Proves                          |
// |---|---------|-------------|--------------------|--------|---------------------------------|
// | C1| on      | both        | up + BLOCK rule    | deny   | enforced BLOCK denies pre-exec  |
// | C2| on      | fail-open   | absent socket      | proceed| outage fails open (OD9)         |
// | C3| on      | fail-open   | up, NO bundle      | proceed| unbundled fails open (default)  |
// | C4| on      | fail-closed | absent socket      | deny   | fail-closed denies on outage    |
// | C5| on      | fail-closed | up + ALLOW default | proceed| fail-closed never denies allow  |
// | C6| on      | fail-closed | up, NO bundle      | deny   | INFO-1: the closed hole         |
// | C7| off     | —           | up + BLOCK rule    | proceed| INV-3 verbatim (observe)        |
// | C8| on      | fail-open   | slow > timeout     | proceed| timeout fails open within bound |
// | C9| on      | fail-closed | up + STALE verdict | proceed| staleness never denies          |

// blockRuleBundle blocks a `rm -rf` shell command; the canonical enforce BLOCK.
func blockRuleBundle() *sidecar.Bundle {
	return &sidecar.Bundle{
		Version:         "conf-block",
		DefaultDecision: "allow",
		Rules: []sidecar.Rule{{
			ID:       "no-rm-rf",
			Match:    sidecar.RuleMatch{ToolKind: "shell", AttributeContains: map[string]string{"command": "rm -rf"}},
			Decision: "block",
			Reason:   "destructive recursive delete",
			PolicyID: "conf-policy",
		}},
	}
}

// slowEvaluator sleeps before answering, so the enforce Client's hard timeout
// trips and it fails open — exercising the INV-3b latency bound end-to-end.
type slowEvaluator struct{ delay time.Duration }

func (s slowEvaluator) Evaluate(sidecar.DecisionRequest) client.Evaluation {
	time.Sleep(s.delay)
	return client.Evaluation{Verdict: client.VerdictBlock, Reason: "would block but answered too late"}
}

// serveConfiguredSidecar starts a real sidecar.Server with the given config and a
// configure hook (to set a bundle or evaluator) and returns its socket path. Torn
// down on cleanup. serveSidecarEval / serveStaleSidecar are thin wrappers.
func serveConfiguredSidecar(t *testing.T, cfg sidecar.ServerConfig, configure func(*sidecar.Server)) string {
	t.Helper()
	socket := filepath.Join(t.TempDir(), "s.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	srv := sidecar.NewServer(cfg)
	configure(srv)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = srv.Serve(ctx, ln); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return socket
}

// serveSidecarEval serves a custom Evaluator (used for the slow-decision case).
func serveSidecarEval(t *testing.T, eval sidecar.Evaluator) string {
	return serveConfiguredSidecar(t, sidecar.ServerConfig{}, func(s *sidecar.Server) { s.SetEvaluator(eval, "conf-eval") })
}

// serveStaleSidecar serves a bundle with a sub-nanosecond freshness window so every
// decision is immediately Stale — a REAL verdict that is merely old (sourceLocalBundle,
// Stale=true), used to prove staleness never triggers fail-closed.
func serveStaleSidecar(t *testing.T, b *sidecar.Bundle) string {
	return serveConfiguredSidecar(t, sidecar.ServerConfig{Freshness: time.Nanosecond}, func(s *sidecar.Server) { s.SetBundle(b) })
}

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
	// absentSocket points the enforce client at a path with no daemon behind it.
	absentSocket := func(t *testing.T) string { return filepath.Join(t.TempDir(), "absent.sock") }
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
		// identical: a real verdict is FailOpen=false → applyFailurePolicy is a no-op),
		// and with the POLICY reason — never the fail-closed outage reason (fail-closed
		// engages on no-verdict only; the Q1-vs-Q4 distinction).
		socket, _ := serveSidecar(t, blockRuleBundle())
		t.Setenv(envSidecarSocket, socket)
		t.Setenv(envEnforce, "1")
		for _, fc := range []string{"0", "1"} {
			t.Setenv(envFailClosed, fc)
			out := run(t, dangerPayload)
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
		t.Setenv(envSidecarSocket, absentSocket(t))
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
		socket, _ := serveSidecar(t, nil) // reachable daemon, NO bundle
		t.Setenv(envSidecarSocket, socket)
		t.Setenv(envEnforce, "1")
		t.Setenv(envFailClosed, "0")
		if out := run(t, dangerPayload); strings.TrimSpace(out) != "" {
			t.Errorf("fail-open + unbundled must proceed (byte-identical to pre-fix); got %q", out)
		}
	})

	t.Run("C4 fail-closed + outage denies", func(t *testing.T) {
		t.Setenv(envSidecarSocket, absentSocket(t))
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
		// A reachable, BUNDLED sidecar whose default is allow → sourceLocalBundle →
		// a real verdict → PROCEEDS even under fail-closed (the crux clause).
		socket, _ := serveSidecar(t, &sidecar.Bundle{Version: "conf-allow", DefaultDecision: "allow"})
		t.Setenv(envSidecarSocket, socket)
		t.Setenv(envEnforce, "1")
		t.Setenv(envFailClosed, "1")
		benign := `{"hook_event_name":"PreToolUse","session_id":"s","cwd":"/tmp","tool_name":"Bash","tool_input":{"command":"echo hi"}}`
		if out := run(t, benign); strings.TrimSpace(out) != "" {
			t.Errorf("fail-closed must NOT block a real allow; got %q", out)
		}
	})

	t.Run("C6 fail-closed + unbundled denies (E6-S3 INFO-1 closed)", func(t *testing.T) {
		// The regression guard for the reconciliation: a reachable-but-UNBUNDLED
		// daemon returns no real verdict (Source=fail-open:no-bundle → FailOpen), so
		// a fail-closed org DENIES rather than being silently ungoverned. Pre-fix this
		// proceeded (the hole).
		socket, _ := serveSidecar(t, nil) // reachable daemon, NO bundle
		t.Setenv(envSidecarSocket, socket)
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
		socket, _ := serveSidecar(t, blockRuleBundle())
		t.Setenv(envSidecarSocket, socket)
		t.Setenv(envEnforce, "0")
		t.Setenv(envFailClosed, "1") // even fail_closed=1 must not matter with enforce off
		if out := run(t, dangerPayload); strings.TrimSpace(out) != "" {
			t.Errorf("observe mode must write nothing to stdout even for a BLOCK-worthy tool; got %q", out)
		}
	})

	t.Run("C9 fail-closed + STALE real verdict proceeds (staleness never denies)", func(t *testing.T) {
		// A reachable, bundled sidecar whose decision is immediately Stale is still a
		// REAL verdict (sourceLocalBundle, Stale=true → FailOpen=false), so it PROCEEDS
		// even under fail-closed. Pins stop-condition #5: staleness never triggers
		// fail-closed (isRealVerdictSource keys on source, not Stale).
		socket := serveStaleSidecar(t, &sidecar.Bundle{Version: "conf-stale", DefaultDecision: "allow"})
		t.Setenv(envSidecarSocket, socket)
		t.Setenv(envEnforce, "1")
		t.Setenv(envFailClosed, "1")
		benign := `{"hook_event_name":"PreToolUse","session_id":"s","cwd":"/tmp","tool_name":"Bash","tool_input":{"command":"echo hi"}}`
		if out := run(t, benign); strings.TrimSpace(out) != "" {
			t.Errorf("a STALE but real allow (sourceLocalBundle) must NOT trigger fail-closed; got %q", out)
		}
	})

	t.Run("C8 slow decision fails open within the bound", func(t *testing.T) {
		// A daemon that answers well past the hard timeout → the Client times out and
		// fails open. Under fail-open (default) the tool proceeds, and the whole hook
		// stays far under CC's 5s kill (INV-3b bound).
		socket := serveSidecarEval(t, slowEvaluator{delay: 500 * time.Millisecond})
		t.Setenv(envSidecarSocket, socket)
		t.Setenv(envEnforce, "1")
		t.Setenv(envFailClosed, "0")
		t.Setenv(envEnforceTimeout, "30") // 30ms hard budget << the 500ms server delay
		start := time.Now()
		out := run(t, dangerPayload)
		elapsed := time.Since(start)
		if strings.TrimSpace(out) != "" {
			t.Errorf("a slow decision must fail open (proceed); got %q", out)
		}
		if elapsed > 3*time.Second {
			t.Errorf("enforce wait %v exceeds the INV-3b bound (CC kills the hook at 5s)", elapsed)
		}
	})
}
