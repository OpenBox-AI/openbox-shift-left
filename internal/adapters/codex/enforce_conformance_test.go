package codex

import (
	"bytes"
	"encoding/base64"
	"io/fs"
	"log"
	"net/http"

	"github.com/openbox-ai/openbox-shift-left/internal/client/memhttptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
)

// Enforcement conformance suite for the Codex adapter (STORY-SL7-B; the C1–C11
// analogue of the shipped Claude Code E6-S7 suite) — executable INV-3b evidence.
//
// It drives the REAL RunHook PreToolUse path end-to-end against a REAL
// decision.InProcessDecider (or a deliberately-absent bundle) and asserts the exact
// Codex stdout contract per quadrant of the enforcement carve-out (ADR-0002 /
// INV-3b). Each case is content-free (INV-1/INV-2): no asserted reason may carry the
// shell command / patch body. The degraded-state quadrants (LESSON-E6E7-04) are
// present: reachable-but-unbundled under fail-closed (CDX-C6), stale-policy gate
// (TestEnforcementConformance_StaleGate_Codex), and the PROBED hook-timeout behavior
// (CDX-C8).
//
// | #   | Enforce | Policy      | Bundle              | Expect  | Proves                              |
// |-----|---------|-------------|---------------------|---------|-------------------------------------|
// | C1  | on      | both        | BLOCK rule          | deny    | enforced BLOCK denies pre-exec      |
// | C2  | on      | fail-open   | absent (cold)       | proceed | outage fails open (OD9)             |
// | C3  | on      | fail-open   | present, no rule    | proceed | unbundled/no-match fails open       |
// | C4  | on      | fail-closed | absent (cold)       | deny    | fail-closed denies on outage        |
// | C5  | on      | fail-closed | ALLOW default       | proceed | fail-closed never denies a real allow|
// | C6  | on      | fail-closed | absent (cold)       | deny    | reachable-but-unbundled hole closed |
// | C7  | off     | —           | BLOCK rule          | proceed | INV-3 observe byte-parity           |
// | C8  | on      | (probe)     | —                   | (assert)| hook-timeout fail-open bound (P1)   |
// | C9  | on      | fail-closed | STALE real verdict  | proceed | staleness never denies              |
// | C10 | on      | fail-open   | secret in apply_patch| redact | Tier-1 redact-and-continue (E6-S9)  |
// | C11 | on      | fail-open   | detection OFF       | proceed | opt-out → no redaction              |
// | C12 | on      | fail-open   | REQUIRE_APPROVAL    | deny    | OD-SL7-ASK: approval→deny on Codex  |

// isolateEnforce pins every default-real-path sink under temp dirs (hermeticity;
// the TestMain sentinel is the structural backstop) and sets identity. Content
// capture OFF by default so egress stays metadata-only; individual cases flip
// enforce/fail-closed/detection.
func isolateEnforce(t *testing.T) {
	t.Helper()
	isolateConfig(t)
	t.Setenv(devconfig.EnvDID, testDID)
	t.Setenv(devconfig.EnvSpoolDir, t.TempDir())
	t.Setenv("OPENBOX_SESSION_DIR", t.TempDir())
	t.Setenv(envEnforcementFile, filepath.Join(t.TempDir(), "enf.jsonl"))
	t.Setenv("OPENBOX_ADVISORY_FILE", filepath.Join(t.TempDir(), "advisories.jsonl"))
	// A server REQUIRE_APPROVAL files a pending-approval marker before holding
	// (ADR-0017 put every class on that path), so this sink needs pinning too —
	// the hermeticity sentinel caught it writing to the real config dir.
	t.Setenv(devconfig.EnvPendingApprovalDir, t.TempDir())
	t.Setenv(devconfig.EnvContentCapture, "0")
}

func TestEnforcementConformance_Codex(t *testing.T) {
	isolateEnforce(t)

	// A dangerous Bash call (matches the BLOCK rule) — the axis several cases key on.
	const dangerPayload = `{"hook_event_name":"PreToolUse","session_id":"s","cwd":"/tmp","tool_name":"Bash","tool_input":{"command":"rm -rf /tmp/x"}}`

	run := func(t *testing.T, payload string) string {
		t.Helper()
		var stdout bytes.Buffer
		RunHook("PreToolUse", strings.NewReader(payload), &stdout, log.New(&bytes.Buffer{}, "", 0))
		return stdout.String()
	}
	assertNoLeak := func(t *testing.T, out string) {
		t.Helper()
		if strings.Contains(out, "rm -rf") {
			t.Errorf("stdout leaked the shell command (INV-2): %q", out)
		}
	}

	t.Run("CDX-C1 enforced BLOCK denies pre-execution (either policy)", func(t *testing.T) {
		serveVerdict(t, `{"verdict":"block","reason":"destructive recursive delete","policy_id":"conf-policy"}`)
		t.Setenv(devconfig.EnvEnforce, "1")
		for _, fc := range []string{"0", "1"} {
			t.Setenv(devconfig.EnvFailClosed, fc)
			out := run(t, dangerPayload)
			d, reason, _ := parsePreToolUse(t, []byte(out))
			if d != codexDecisionDeny {
				t.Fatalf("fail_closed=%s: decision = %q, want deny (stdout=%q)", fc, d, out)
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

	t.Run("CDX-C2 fail-open + outage proceeds within bound (OD9)", func(t *testing.T) {
		t.Setenv(devconfig.EnvEnforce, "1")
		t.Setenv(devconfig.EnvFailClosed, "0")
		start := time.Now()
		if out := run(t, dangerPayload); strings.TrimSpace(out) != "" {
			t.Errorf("fail-open outage must proceed (empty stdout); got %q", out)
		}
		if elapsed := time.Since(start); elapsed > hookflow.EnforceBudget((Engine{}).HookCeilings()) {
			t.Errorf("enforce wait %v exceeds the derived whole-hook budget %v (probe P1: Codex fails open past it)", elapsed, hookflow.EnforceBudget((Engine{}).HookCeilings()))
		}
	})

	t.Run("CDX-C3 fail-open + unbundled/no-match proceeds", func(t *testing.T) {
		serveVerdict(t, `{"verdict":"allow"}`)
		t.Setenv(devconfig.EnvEnforce, "1")
		t.Setenv(devconfig.EnvFailClosed, "0")
		if out := run(t, dangerPayload); strings.TrimSpace(out) != "" {
			t.Errorf("no-match under fail-open must proceed; got %q", out)
		}
	})

	t.Run("CDX-C4 fail-closed + outage denies", func(t *testing.T) {
		t.Setenv(devconfig.EnvEnforce, "1")
		t.Setenv(devconfig.EnvFailClosed, "1")
		out := run(t, dangerPayload)
		d, reason, _ := parsePreToolUse(t, []byte(out))
		if d != codexDecisionDeny {
			t.Fatalf("fail-closed + outage: decision = %q, want deny (stdout=%q)", d, out)
		}
		if !strings.Contains(reason, "fail-closed") {
			t.Errorf("reason = %q, want it to explain the fail-closed outage", reason)
		}
		assertNoLeak(t, out)
	})

	t.Run("CDX-C5 fail-closed never denies a REAL allow", func(t *testing.T) {
		// "Real" means a SERVER allow since ADR-0017 — the local bundle is no
		// longer the decider, so a local allow with nothing reachable is the
		// outage CDX-C4 asserts denies, not an allow.
		serveVerdict(t, `{"verdict":"allow"}`)
		srv := memhttptest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"verdict":"allow"}`))
		}))
		defer srv.Close()
		t.Setenv("OPENBOX_BASE_URL", srv.URL) // loopback http allowed (INV-1 guard)
		t.Setenv("OPENBOX_API_KEY", "obx_test_key")
		t.Setenv("OPENBOX_ED25519_SEED", base64.StdEncoding.EncodeToString(make([]byte, 32)))
		t.Setenv(devconfig.EnvEnforce, "1")
		t.Setenv(devconfig.EnvFailClosed, "1")
		benign := `{"hook_event_name":"PreToolUse","session_id":"s","cwd":"/tmp","tool_name":"Bash","tool_input":{"command":"echo hi"}}`
		if out := run(t, benign); strings.TrimSpace(out) != "" {
			t.Errorf("fail-closed must NOT block a real allow; got %q", out)
		}
	})

	t.Run("CDX-C6 fail-closed + unbundled denies (reachable-but-unbundled hole closed)", func(t *testing.T) {
		t.Setenv(devconfig.EnvEnforce, "1")
		t.Setenv(devconfig.EnvFailClosed, "1")
		out := run(t, dangerPayload)
		d, reason, _ := parsePreToolUse(t, []byte(out))
		if d != codexDecisionDeny {
			t.Fatalf("fail-closed + unbundled: decision = %q, want deny; stdout=%q", d, out)
		}
		if !strings.Contains(reason, "fail-closed") {
			t.Errorf("reason = %q, want the content-free fail-closed reason", reason)
		}
		assertNoLeak(t, out)
	})

	t.Run("CDX-C7 observe mode never blocks (INV-3 byte-parity)", func(t *testing.T) {
		serveVerdict(t, `{"verdict":"block","reason":"destructive recursive delete","policy_id":"conf-policy"}`)
		t.Setenv(devconfig.EnvEnforce, "0")
		t.Setenv(devconfig.EnvFailClosed, "1") // even fail_closed=1 must not matter with enforce off
		if out := run(t, dangerPayload); strings.TrimSpace(out) != "" {
			t.Errorf("observe mode must write nothing even for a BLOCK-worthy tool; got %q", out)
		}
	})

	t.Run("CDX-C8 hook-timeout fail-open bound (probe P1, degraded-state)", func(t *testing.T) {
		// Probe P1 (live, codex-cli 0.145.0): a PreToolUse hook that overruns its
		// `timeout` is KILLED and Codex FAILS OPEN (tool ran; wall ≈ timeout). There is
		// no network path to time out in-process (ADR-0006), so the conformance
		// obligation is the static invariant that our verdict LANDS before Codex's kill:
		// the whole-hook budget is strictly under the installed gate-hook timeout.
		if hookflow.EnforceBudget((Engine{}).HookCeilings()) >= (Engine{}).HookCeilings().Gating {
			t.Fatalf("whole-hook budget %v must be < installed gate-hook timeout %v (else Codex's fail-open kill defeats fail-closed)",
				hookflow.EnforceBudget((Engine{}).HookCeilings()), (Engine{}).HookCeilings().Gating)
		}
		if maxEvaluationTimeout > hookflow.EnforceBudget((Engine{}).HookCeilings()) {
			t.Errorf("T2 clamp %v must stay within the whole-hook budget %v", maxEvaluationTimeout, hookflow.EnforceBudget((Engine{}).HookCeilings()))
		}
	})

	// CDX-C9 (a STALE local verdict must not trigger fail-closed) is deleted with
	// the bundle it read: staleness described a local artifact falling behind the
	// control plane, and every verdict comes from the control plane now.

	// A secret in an apply_patch body (tool_input["command"] — the Codex patch text).
	const awsSecret = "AKIAIOSFODNN7EXAMPLE"
	secretPatch := `{"hook_event_name":"PreToolUse","session_id":"s","cwd":"/tmp","tool_name":"apply_patch","tool_input":{"command":"*** Begin Patch\n*** Add File: app.env\n+AWS_ACCESS_KEY_ID=` + awsSecret + `\n*** End Patch"}}`

	assertNoEgress := func(t *testing.T) {
		t.Helper()
		for _, dir := range []string{os.Getenv(devconfig.EnvSpoolDir), os.Getenv("OPENBOX_SESSION_DIR"), filepath.Dir(os.Getenv(envEnforcementFile))} {
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

	t.Run("CDX-C10 secret in apply_patch body → redact-and-continue (allow+updatedInput)", func(t *testing.T) {
		serveVerdict(t, `{"verdict":"allow"}`)
		t.Setenv(devconfig.EnvEnforce, "1")
		t.Setenv(devconfig.EnvFailClosed, "0")
		t.Setenv(devconfig.EnvContentCapture, "0") // egress stays metadata-only
		os.Unsetenv(devconfig.EnvSecretDetection)  // default ON
		out := run(t, secretPatch)

		d, _, ui := parsePreToolUse(t, []byte(out))
		if d != codexDecisionAllow {
			t.Fatalf("Codex requires permissionDecision:allow to carry a rewrite; got %q (stdout=%q)", d, out)
		}
		if len(ui) == 0 {
			t.Fatalf("expected an updatedInput redaction; stdout=%q", out)
		}
		if strings.Contains(out, awsSecret) {
			t.Errorf("the raw secret must be redacted, not present on stdout: %q", out)
		}
		if !strings.Contains(out, "OPENBOX_REDACTED") {
			t.Errorf("expected the env-var redaction placeholder; got %q", out)
		}
		data, _ := os.ReadFile(os.Getenv(envEnforcementFile))
		if !strings.Contains(string(data), `"redacted":true`) {
			t.Errorf("expected redacted:true in the enforcement audit; got %s", data)
		}
		assertNoEgress(t)
	})

	t.Run("CDX-C11 secret detection OFF + capture OFF → no redaction (proceed)", func(t *testing.T) {
		serveVerdict(t, `{"verdict":"allow"}`)
		t.Setenv(devconfig.EnvEnforce, "1")
		t.Setenv(devconfig.EnvFailClosed, "0")
		t.Setenv(devconfig.EnvContentCapture, "0")
		t.Setenv(devconfig.EnvSecretDetection, "0") // explicit opt-out
		if out := run(t, secretPatch); strings.TrimSpace(out) != "" {
			t.Errorf("detection off + capture off must write nothing on the proceed path; got %q", out)
		}
		assertNoEgress(t)
	})

	t.Run("CDX-C12 REQUIRE_APPROVAL → deny (OD-SL7-ASK; Codex rejects 'ask')", func(t *testing.T) {
		serveVerdict(t, `{"verdict":"require_approval","reason":"production deploy needs approval","policy_id":"conf-approval-policy"}`)
		// A short hold: this case is about how Codex RENDERS an approval, not
		// about how long the gate waits for one.
		t.Setenv(devconfig.EnvApprovalHold, "200")
		t.Setenv(devconfig.EnvEnforce, "1")
		t.Setenv(devconfig.EnvFailClosed, "0")
		payload := `{"hook_event_name":"PreToolUse","session_id":"s","cwd":"/tmp","tool_name":"Bash","tool_input":{"command":"deploy prod"}}`
		out := run(t, payload)
		d, reason, _ := parsePreToolUse(t, []byte(out))
		if d != codexDecisionDeny {
			t.Fatalf("REQUIRE_APPROVAL must map to deny on Codex (no usable 'ask' lever); got %q (stdout=%q)", d, out)
		}
		if !strings.Contains(reason, "approval") {
			t.Errorf("deny reason = %q, want the content-free approval-required reason", reason)
		}
	})

	// Global tighten-only sweep: across every case run above, stdout must NEVER carry
	// a bare permissionDecision:allow without an accompanying updatedInput (Codex would
	// reject it anyway, but the adapter must never even attempt to grant).
	t.Run("tighten-only: allow never appears without a redacting updatedInput", func(t *testing.T) {
		serveVerdict(t, `{"verdict":"allow"}`)
		t.Setenv(devconfig.EnvEnforce, "1")
		t.Setenv(devconfig.EnvFailClosed, "0")
		benign := `{"hook_event_name":"PreToolUse","session_id":"s","cwd":"/tmp","tool_name":"Bash","tool_input":{"command":"echo hi"}}`
		out := run(t, benign)
		if strings.TrimSpace(out) != "" {
			t.Fatalf("a plain allow must write NOTHING (never a bare permissionDecision:allow); got %q", out)
		}
	})
}

// ── Deleted with the local evaluator (ADR-0017) ──────────────────────────────
//
// TestEnforcementConformance_BuilderPolicy_Codex drove BLOCK / no-match /
// REQUIRE_APPROVAL through the LOCAL implementation of the backend's
// policy_builder semantics. Those outcomes are the server's now and CDX-C1 /
// CDX-C12 assert them end to end against a real /evaluate.
//
// TestEnforcementConformance_StaleGate_Codex asserted that a stale-marked
// session denies under fail-closed and that clearing the marker restores
// proceed. Both are properties of a local bundle's freshness; there is no local
// bundle, so nothing can be stale.

// TestObserveByteParity_EnforceOff pins the whole-product default: with enforce OFF,
// the SL7-A observe path is byte-identical — Decide is never reached and stdout is
// empty for every wired hook, even a BLOCK-worthy PreToolUse.
func TestObserveByteParity_EnforceOff(t *testing.T) {
	isolateEnforce(t)
	serveVerdict(t, `{"verdict":"block","reason":"destructive recursive delete","policy_id":"conf-policy"}`)
	t.Setenv(devconfig.EnvEnforce, "0")
	// A bundle path that would panic if loaded proves Decide is never invoked; instead
	// we assert empty stdout across every hook (the observable contract).
	payloads := map[string]string{
		"SessionStart":     `{"hook_event_name":"SessionStart","session_id":"s","cwd":"/tmp","source":"startup"}`,
		"UserPromptSubmit": `{"hook_event_name":"UserPromptSubmit","session_id":"s","cwd":"/tmp","prompt":"hi"}`,
		"PreToolUse":       `{"hook_event_name":"PreToolUse","session_id":"s","cwd":"/tmp","tool_name":"Bash","tool_input":{"command":"rm -rf /tmp/x"}}`,
		"PostToolUse":      `{"hook_event_name":"PostToolUse","session_id":"s","cwd":"/tmp","tool_name":"Bash","tool_use_id":"c1"}`,
	}
	for hook, payload := range payloads {
		var stdout bytes.Buffer
		RunHook(hook, strings.NewReader(payload), &stdout, log.New(&bytes.Buffer{}, "", 0))
		if stdout.Len() != 0 {
			t.Errorf("enforce OFF: %s wrote stdout %q, want empty (observe byte-parity)", hook, stdout.String())
		}
	}
	// The enforcement audit must not exist (Decide/apply never ran).
	if _, err := os.Stat(os.Getenv(envEnforcementFile)); err == nil {
		var b []byte
		b, _ = os.ReadFile(os.Getenv(envEnforcementFile))
		if len(bytes.TrimSpace(b)) != 0 {
			t.Errorf("enforce OFF must write no enforcement audit; got %s", b)
		}
	}
}
