package claudecode

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/openbox-ai/openbox-shift-left/decision"
)

// Enforcement conformance suite (STORY-E6-S7) — executable INV-3b evidence.
//
// This drives the REAL RunHook PreToolUse path end-to-end against a REAL
// decision.Server (or a deliberately-absent socket) and asserts the exact Claude
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
// | C8| — (removed: in-process decision has no network timeout — ADR-0006)          |
// | C9| on      | fail-closed | STALE real verdict | proceed| staleness never denies          |
// |C10| on      | fail-open   | up + secret in Write| redact | Tier-1 redact-and-continue (E6-S9)|
// |C11| on      | fail-open   | up, detection OFF  | proceed| opt-out → no redaction (E6-S9)  |

// blockRuleBundle blocks a `rm -rf` shell command; the canonical enforce BLOCK.
func blockRuleBundle() *decision.Bundle {
	return &decision.Bundle{
		Version:         "conf-block",
		DefaultDecision: "allow",
		Rules: []decision.Rule{{
			ID:       "no-rm-rf",
			Match:    decision.RuleMatch{ToolKind: "shell", AttributeContains: map[string]string{"command": "rm -rf"}},
			Decision: "block",
			Reason:   "destructive recursive delete",
			PolicyID: "conf-policy",
		}},
	}
}

// removed: slowEvaluator + serveConfiguredSidecar/serveSidecarEval/serveStaleSidecar —
// the socket-served sidecar and its network-timeout path are gone (ADR-0006
// in-process decider). The slow-decision C8 case is dropped (no network timeout to
// exercise); C9 staleness is re-expressed at the in-process decider level (a tiny
// Freshness marks a real verdict Stale) rather than via a served socket.

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
		// identical: a real verdict is FailOpen=false → applyFailurePolicy is a no-op),
		// and with the POLICY reason — never the fail-closed outage reason (fail-closed
		// engages on no-verdict only; the Q1-vs-Q4 distinction).
		setBundleEnv(t, blockRuleBundle())
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
		setBundleEnv(t, nil) // no bundle loaded → cold-start fail-open (the outage analog)
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
		setBundleEnv(t, nil) // NO bundle loaded → cold-start fail-open
		t.Setenv(envEnforce, "1")
		t.Setenv(envFailClosed, "0")
		if out := run(t, dangerPayload); strings.TrimSpace(out) != "" {
			t.Errorf("fail-open + unbundled must proceed (byte-identical to pre-fix); got %q", out)
		}
	})

	t.Run("C4 fail-closed + outage denies", func(t *testing.T) {
		setBundleEnv(t, nil) // no bundle loaded → cold-start fail-open → fail-closed denies
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
		setBundleEnv(t, &decision.Bundle{Version: "conf-allow", DefaultDecision: "allow"})
		t.Setenv(envEnforce, "1")
		t.Setenv(envFailClosed, "1")
		benign := `{"hook_event_name":"PreToolUse","session_id":"s","cwd":"/tmp","tool_name":"Bash","tool_input":{"command":"echo hi"}}`
		if out := run(t, benign); strings.TrimSpace(out) != "" {
			t.Errorf("fail-closed must NOT block a real allow; got %q", out)
		}
	})

	t.Run("C6 fail-closed + unbundled denies (E6-S3 INFO-1 closed)", func(t *testing.T) {
		// The regression guard for the reconciliation: no real verdict (no bundle
		// loaded → Source=fail-open:no-bundle → FailOpen), so a fail-closed org DENIES
		// rather than being silently ungoverned. Pre-fix this proceeded (the hole).
		setBundleEnv(t, nil) // NO bundle loaded → cold-start fail-open
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
		setBundleEnv(t, blockRuleBundle())
		t.Setenv(envEnforce, "0")
		t.Setenv(envFailClosed, "1") // even fail_closed=1 must not matter with enforce off
		if out := run(t, dangerPayload); strings.TrimSpace(out) != "" {
			t.Errorf("observe mode must write nothing to stdout even for a BLOCK-worthy tool; got %q", out)
		}
	})

	t.Run("C9 fail-closed + STALE real verdict proceeds (staleness never denies)", func(t *testing.T) {
		// A bundled allow decider whose bundle is immediately Stale (tiny Freshness) is
		// still a REAL verdict (sourceLocalBundle, Stale=true → FailOpen=false), so the
		// fail-closed policy is a NO-OP on it. Pins stop-condition #5: staleness never
		// triggers fail-closed (isRealVerdictSource keys on source, not Stale). Since the
		// full-hook decider uses the default freshness, this is exercised at the
		// in-process decider level (ADR-0006: there is no served-socket freshness knob).
		dec := decision.NewInProcessDecider(decision.InProcessConfig{
			BundlePath: writeBundleFile(t, &decision.Bundle{Version: "conf-stale", DefaultDecision: "allow"}),
			Freshness:  time.Nanosecond,
		}).Decide(context.Background(), buildDecisionRequest(
			Identity{DeveloperDID: testDID},
			&HookEvent{SessionID: "s", ToolName: "Bash", ToolInput: []byte(`{"command":"echo hi"}`)},
			false))
		if !dec.Stale {
			t.Fatalf("expected a Stale decision (tiny freshness), got %+v", dec)
		}
		if dec.FailOpen || dec.Source != "local-bundle" {
			t.Fatalf("a STALE but real verdict must stay FailOpen=false/local-bundle, got %+v", dec)
		}
		if got := applyFailurePolicy(dec, FailClosed); got.Evaluation.WouldBlock() {
			t.Errorf("a STALE but real allow must NOT trigger fail-closed; got %+v", got.Evaluation)
		}
	})

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

	t.Run("C10 secret in Write body → redact-and-continue (E6-S9)", func(t *testing.T) {
		// A reachable, BUNDLED allow decision. Secret detection is DEFAULT ON and
		// DECOUPLED from content_capture (which stays OFF): the file body reaches only
		// the local socket, the scanner redacts it, and the hook emits an updatedInput
		// with the content field sanitized and NO permissionDecision (proceed).
		setBundleEnv(t, &decision.Bundle{Version: "conf-allow", DefaultDecision: "allow"})
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
		setBundleEnv(t, &decision.Bundle{Version: "conf-allow", DefaultDecision: "allow"})
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

// ── STORY-E6-S8 conformance: native builder policy + fail-closed stale gate ──

// builderBlockBundle blocks a `rm -rf` shell command via a native policy_builder
// config (FIRST-MATCH), the E6-S8 analog of blockRuleBundle.
func builderBlockBundle(verdict string) *decision.Bundle {
	return &decision.Bundle{
		Version:  "conf-builder",
		PolicyID: "conf-builder-policy",
		PolicyBuilder: &decision.PolicyBuilderConfig{Version: 1, Rules: []decision.PolicyBuilderRule{{
			Decision: verdict, Reason: "destructive recursive delete", MatchMode: "all",
			Conditions: []decision.PolicyBuilderCondition{{
				Field: "spans[_].attributes.command", Operator: "contains",
				Transform: "value", Value: "rm -rf", ValueType: "string",
			}},
		}}},
	}
}

// TestEnforcementConformance_BuilderPolicy drives the REAL RunHook PreToolUse
// path against a REAL sidecar serving a native builder policy: BLOCK→deny,
// no-match→proceed, REQUIRE_APPROVAL→ask (STORY-E6-S8 AC-1/AC-8 end-to-end).
func TestEnforcementConformance_BuilderPolicy(t *testing.T) {
	isolateConfig(t)
	t.Setenv(envDID, testDID)
	t.Setenv("OPENBOX_SPOOL_DIR", t.TempDir())
	t.Setenv("OPENBOX_SESSION_DIR", t.TempDir())
	t.Setenv(envEnforcementFile, filepath.Join(t.TempDir(), "enf.jsonl"))
	t.Setenv(envContentCapture, "0")
	t.Setenv(envEnforce, "1")
	t.Setenv(envFailClosed, "0")

	run := func(payload string) string {
		var stdout bytes.Buffer
		RunHook("PreToolUse", strings.NewReader(payload), &stdout, log.New(&bytes.Buffer{}, "", 0))
		return stdout.String()
	}
	danger := `{"hook_event_name":"PreToolUse","session_id":"s","cwd":"/tmp","tool_name":"Bash","tool_input":{"command":"rm -rf /tmp/x"}}`
	benign := `{"hook_event_name":"PreToolUse","session_id":"s","cwd":"/tmp","tool_name":"Bash","tool_input":{"command":"echo hi"}}`

	t.Run("builder BLOCK denies", func(t *testing.T) {
		setBundleEnv(t, builderBlockBundle("BLOCK"))
		d, reason := parsePermissionDecision(t, []byte(run(danger)))
		if d != ccDecisionDeny {
			t.Fatalf("builder BLOCK: decision = %q, want deny", d)
		}
		if !strings.Contains(reason, "destructive recursive delete") || !strings.Contains(reason, "conf-builder-policy") {
			t.Errorf("reason = %q, want the builder rule reason + policy id", reason)
		}
		if strings.Contains(run(danger), "rm -rf") {
			t.Errorf("command leaked to stdout (INV-2)")
		}
	})

	t.Run("builder no-match proceeds", func(t *testing.T) {
		setBundleEnv(t, builderBlockBundle("BLOCK"))
		if out := run(benign); strings.TrimSpace(out) != "" {
			t.Errorf("no-match builder rule must proceed (empty stdout); got %q", out)
		}
	})

	t.Run("builder REQUIRE_APPROVAL asks", func(t *testing.T) {
		setBundleEnv(t, builderBlockBundle("REQUIRE_APPROVAL"))
		d, _ := parsePermissionDecision(t, []byte(run(danger)))
		if d != ccDecisionAsk {
			t.Fatalf("builder REQUIRE_APPROVAL: decision = %q, want ask", d)
		}
	})
}

// TestEnforcementConformance_StaleGate drives AC-5 end-to-end: under fail-closed a
// stale-marked session DENIES at the PreToolUse gate with a content-free reason,
// and `dev sync` clearing the marker restores proceed — all with a reachable ALLOW
// sidecar (so the deny is attributable to staleness, not policy).
func TestEnforcementConformance_StaleGate(t *testing.T) {
	isolateConfig(t)
	t.Setenv(envDID, testDID)
	t.Setenv("OPENBOX_SPOOL_DIR", t.TempDir())
	t.Setenv("OPENBOX_SESSION_DIR", t.TempDir())
	t.Setenv(envEnforcementFile, filepath.Join(t.TempDir(), "enf.jsonl"))
	t.Setenv(envStaleDir, filepath.Join(t.TempDir(), "stale"))
	t.Setenv(envContentCapture, "0")
	t.Setenv(envEnforce, "1")
	t.Setenv(envFailClosed, "1")

	setBundleEnv(t, &decision.Bundle{Version: "allow", DefaultDecision: "allow"})

	run := func() string {
		var stdout bytes.Buffer
		RunHook("PreToolUse", strings.NewReader(
			`{"hook_event_name":"PreToolUse","session_id":"stale-sess","cwd":"/tmp","tool_name":"Bash","tool_input":{"command":"echo hi"}}`),
			&stdout, log.New(&bytes.Buffer{}, "", 0))
		return stdout.String()
	}

	// No marker yet → the reachable ALLOW sidecar proceeds.
	if out := run(); strings.TrimSpace(out) != "" {
		t.Fatalf("pre-marker: fail-closed + reachable allow must proceed; got %q", out)
	}
	// Mark the session stale (what a fail-closed SessionStart would do) → deny.
	if err := writeStaleMarker("stale-sess"); err != nil {
		t.Fatal(err)
	}
	d, reason := parsePermissionDecision(t, []byte(run()))
	if d != ccDecisionDeny {
		t.Fatalf("stale + fail-closed: decision = %q, want deny", d)
	}
	if !strings.Contains(reason, "dev sync") {
		t.Errorf("stale deny reason = %q, want the run-`dev sync` nudge", reason)
	}
	// `dev sync` clears the marker → proceed again.
	if err := ClearAllStaleMarkers(); err != nil {
		t.Fatal(err)
	}
	if out := run(); strings.TrimSpace(out) != "" {
		t.Errorf("after clearing the marker the tool must proceed; got %q", out)
	}
}
