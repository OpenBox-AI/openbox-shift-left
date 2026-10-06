package codex

import (
	"bytes"
	"io/fs"
	"log"

	"os"
	"path/filepath"
	"strings"
	"testing"

	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
)

// It drives the real RunHook PreToolUse path end-to-end against a real
// decision.InProcessDecider (or a deliberately-absent bundle) and asserts the
// exact Codex stdout contract per quadrant of the enforcement carve-out and its
// hook latency bound.

func isolateEnforce(t *testing.T) {
	t.Helper()
	isolateConfig(t)
	t.Setenv(devconfig.EnvAgentID, testAgentID)
	t.Setenv(devconfig.EnvSpoolDir, t.TempDir())
	t.Setenv("OPENBOX_SESSION_DIR", t.TempDir())
	t.Setenv(envEnforcementFile, filepath.Join(t.TempDir(), "enf.jsonl"))
	t.Setenv("OPENBOX_ADVISORY_FILE", filepath.Join(t.TempDir(), "advisories.jsonl"))
	t.Setenv(devconfig.EnvPendingApprovalDir, t.TempDir())
	t.Setenv(devconfig.EnvContentCapture, "0")
}

func TestEnforcementConformance_Codex(t *testing.T) {
	isolateEnforce(t)

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

	// CDX-C2 pinned "fail_closed=0 => outage proceeds". Delivery is now
	// always fail-closed per call: the
	// `fail_closed` key is deprecated, parsed only so it can warn, and no
	// longer selects a policy, so this now asserts the same outage-denies
	// behaviour CDX-C4 pins, kept separate for its timing bound.
	t.Run("CDX-C2 an outage denies even with fail_closed=0 (delivery is always fail-closed)", func(t *testing.T) {
		t.Setenv(devconfig.EnvEnforce, "1")
		t.Setenv(devconfig.EnvFailClosed, "0")
		start := time.Now()
		out := run(t, dangerPayload)
		d, _, _ := parsePreToolUse(t, []byte(out))
		if d != codexDecisionDeny {
			t.Fatalf("decision = %q, want deny; delivery is always fail-closed regardless of fail_closed=0 (stdout=%q)", d, out)
		}
		if elapsed := time.Since(start); elapsed > hookflow.EnforceBudget((Engine{}).HookCeilings()) {
			t.Errorf("enforce wait %v exceeds the derived whole-hook budget %v (Codex fails open past it)", elapsed, hookflow.EnforceBudget((Engine{}).HookCeilings()))
		}
		assertNoLeak(t, out)
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
		serveVerdict(t, `{"verdict":"allow"}`)
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

	// CDX-C7 "observe mode never blocks" is gone: enforcement is unconditional
	// now (ResolveEnforce always reports true), so there is no observe mode
	// left for a PreToolUse/UserPromptSubmit/PermissionRequest call. Nothing
	// replaces it; the condition cannot arise.

	t.Run("CDX-C8 hook-timeout fail-open bound (degraded-state)", func(t *testing.T) {
		if hookflow.EnforceBudget((Engine{}).HookCeilings()) >= (Engine{}).HookCeilings().Gating {
			t.Fatalf("whole-hook budget %v must be < installed gate-hook timeout %v (else Codex's fail-open kill defeats fail-closed)",
				hookflow.EnforceBudget((Engine{}).HookCeilings()), (Engine{}).HookCeilings().Gating)
		}
		if worst := hookflow.GateDrainAttemptTimeout + hookflow.DefaultEvaluationTimeout + hookflow.MaxStripeWait; worst >= hookflow.EnforceBudget((Engine{}).HookCeilings()) {
			t.Errorf("contended gate worst case %v must stay under the whole-hook budget %v", worst, hookflow.EnforceBudget((Engine{}).HookCeilings()))
		}
	})

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

	t.Run("CDX-C12 REQUIRE_APPROVAL → deny (Codex rejects 'ask')", func(t *testing.T) {
		serveVerdict(t, `{"verdict":"require_approval","reason":"production deploy needs approval","policy_id":"conf-approval-policy"}`)
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

// TestObserveByteParity_NeverGatedHooks pins the whole-product default for
// every hook enforcement never reaches: stdout is empty for each, byte-
// identical to observe, even against a BLOCK-worthy verdict from a real core.
// Enforcement is unconditional now (ResolveEnforce always reports true), so
// UserPromptSubmit/PreToolUse/PermissionRequest are excluded here -- they are
// ALWAYS gated and their own conformance (deny under a real BLOCK verdict) is
// pinned by the C-series cases above, not by this byte-parity set.
func TestObserveByteParity_NeverGatedHooks(t *testing.T) {
	isolateEnforce(t)
	serveVerdict(t, `{"verdict":"block","reason":"destructive recursive delete","policy_id":"conf-policy"}`)
	payloads := map[string]string{
		"SessionStart":  `{"hook_event_name":"SessionStart","session_id":"s","cwd":"/tmp","source":"startup"}`,
		"PostToolUse":   `{"hook_event_name":"PostToolUse","session_id":"s","cwd":"/tmp","tool_name":"Bash","tool_use_id":"c1"}`,
		"Stop":          `{"hook_event_name":"Stop","session_id":"s","cwd":"/tmp","last_assistant_message":"done","stop_hook_active":false,"turn_id":"t1","transcript_path":null}`,
		"SubagentStart": `{"hook_event_name":"SubagentStart","session_id":"s","cwd":"/tmp","agent_id":"a1","agent_type":"general"}`,
		"SubagentStop":  `{"hook_event_name":"SubagentStop","session_id":"s","cwd":"/tmp","agent_id":"a1","agent_type":"general","last_assistant_message":"done","transcript_path":null}`,
		"PreCompact":    `{"hook_event_name":"PreCompact","session_id":"s","cwd":"/tmp","trigger":"manual","turn_id":"t1","transcript_path":null}`,
		"PostCompact":   `{"hook_event_name":"PostCompact","session_id":"s","cwd":"/tmp","trigger":"manual","turn_id":"t1","transcript_path":null}`,
	}
	// An event missing from this map ships unasserted, so the count is pinned to
	// the installer's own list: a newly registered event must appear here too,
	// less SessionEnd (flushes; covered separately) and the 3 always-gated hooks.
	const gatedCount = 3 // UserPromptSubmit, PreToolUse, PermissionRequest
	if len(payloads) != len(hookedEvents)-1-gatedCount {
		t.Fatalf("byte-parity map covers %d events but the installer registers %d; a wired event is unasserted",
			len(payloads), len(hookedEvents))
	}
	for hook, payload := range payloads {
		var stdout bytes.Buffer
		RunHook(hook, strings.NewReader(payload), &stdout, log.New(&bytes.Buffer{}, "", 0))
		if stdout.Len() != 0 {
			t.Errorf("enforce OFF: %s wrote stdout %q, want empty (observe byte-parity)", hook, stdout.String())
		}
	}
	if _, err := os.Stat(os.Getenv(envEnforcementFile)); err == nil {
		var b []byte
		b, _ = os.ReadFile(os.Getenv(envEnforcementFile))
		if len(bytes.TrimSpace(b)) != 0 {
			t.Errorf("enforce OFF must write no enforcement audit; got %s", b)
		}
	}
}
