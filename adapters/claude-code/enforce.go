package claudecode

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"os"
	"path/filepath"
	"unicode/utf8"

	"github.com/openbox-ai/openbox-shift-left/client"
	"github.com/openbox-ai/openbox-shift-left/sidecar"
)

// Enforcement — the synchronous pre-execution gate (STORY-E6-S1, Phase-2).
//
// In ENFORCE mode (ResolveEnforce), a PreToolUse hook must obtain a governance
// decision from the local sidecar BEFORE the tool runs — the INV-3b carve-out to
// INV-3 ("observation never blocks"): an enforce path MAY block, but only
// pre-execution, only within a hard timeout, and fail-open by default (OD9). This
// mirrors the reference SDK's activity-boundary gate, which awaits
// GovernanceClient.evaluate_event on ActivityStarted and then runs enforce_verdict
// BEFORE the activity executes (activity_interceptor.py). The decisive difference:
// spike S2 proved a synchronous round-trip to core's /evaluate is ~0.8–1.6 s (a
// Temporal workflow) — 16–33× over budget — so the decision is served by the
// resident LOCAL sidecar (Unix socket, single-digit ms), never a network call.
//
// SCOPE OF THIS FILE (E6-S1): OBTAIN + record the decision only. It returns the
// sidecar.Decision (carrying the client.Evaluation) and NEVER writes a blocking
// signal — turning a BLOCK/HALT verdict into an actual Claude Code `deny`/`ask`
// (the enforce_verdict cascade) is E6-S2's `apply`, which consumes this Decision.
// So enforce mode here is safe by construction: the tool always proceeds, exactly
// as observe mode does, while the sync path + fail-open + latency bound are
// exercised and validated.

// maxCommandLen bounds the shell command carried on the LOCAL decision request,
// measured in BYTES (not runes) so the marshaled DecisionRequest stays under the
// sidecar server's 64 KiB byte read-limit (server.go defaultMaxRequestBytes) even
// after JSON escaping expands control bytes (up to ×6) — a rune cap would let an
// adversarial multibyte/control-heavy command overrun that limit (G_SEC LOW-1).
// 8 KiB leaves ample headroom for escaping + the request's other fields; Bash
// commands are far smaller in practice. Truncation can only ever cause a policy
// to MISS a match (→ allow), never a wrong block — consistent with fail-open
// (OD9). The command is local-only and never egressed (see HookEvent.command /
// INV-2).
const maxCommandLen = 8 << 10 // 8 KiB (bytes)

// EnforceDecision is the PreToolUse enforce gate: it SYNCHRONOUSLY obtains a
// governance decision from the local sidecar for the tool that is about to run,
// bounded by cl's hard timeout (~50 ms, INV-3b). It NEVER errors and NEVER blocks
// — the sidecar.Client fails open (VerdictUnknown/allow) on every fault (socket
// absent, dial refused, timeout, malformed reply), so the returned Decision is
// always safe to proceed on. The returned Decision is the seam E6-S2 consumes to
// map the verdict onto a Claude Code permissionDecision.
//
// It reads NO secret (identity is the DID only, already resolved on the hot path
// — INV-1) and takes NO network I/O (only the local per-user socket — INV-3b).
func EnforceDecision(ctx context.Context, cl *sidecar.Client, id Identity, e *HookEvent) sidecar.Decision {
	return cl.Decide(ctx, buildDecisionRequest(id, e))
}

// newSidecarClient builds the fail-open enforce-hook client for the configured
// socket. It never fails: an empty/absent socket path simply means every Decide
// fails open (the daemon is treated as absent). The timeout is the configured
// hard per-call budget (E6-S3, ResolveEnforceTimeout); 0 ⇒
// sidecar.DefaultDecisionTimeout (~50 ms, ADR-0002).
func newSidecarClient() *sidecar.Client {
	return sidecar.NewClient(sidecar.ClientConfig{
		SocketPath: ResolveSidecarSocket(),
		Timeout:    ResolveEnforceTimeout(),
	})
}

// ── E6-S3: fail-open / fail-closed failure policy ────────────────────────────
//
// The failure policy decides what the enforce gate does when the local sidecar
// could NOT deliver a real verdict (absent, dial refused, timeout, malformed
// reply — i.e. sidecar.Decision.FailOpen==true). It is the Go port of the
// reference SDK's governance_policy / _handle_api_error (client.py:204-208): on an
// evaluate failure the SDK returns either None (fail-open → no verdict → the
// action proceeds) or a SYNTHESIZED Verdict.HALT (fail-closed → the SAME
// enforce_verdict cascade runs → the action is blocked). We mirror that shape
// exactly so the E6-S2 apply cascade (mapVerdict/applyDecision) stays entirely
// policy-agnostic — a fail-closed deny travels the identical path as a real BLOCK.

// FailurePolicy is the per-org enforce failure posture (OD9). FailOpen is the
// zero value and the default: an OpenBox outage degrades to observe (proceed).
type FailurePolicy int

const (
	// FailOpen degrades to observe on an evaluation failure — the tool proceeds
	// (OD9 default). An infra outage never blocks the developer.
	FailOpen FailurePolicy = iota
	// FailClosed denies the tool call on an evaluation failure (explicit per-org
	// opt-in). An OpenBox outage blocks work rather than letting it through
	// ungoverned.
	FailClosed
)

func (p FailurePolicy) String() string {
	if p == FailClosed {
		return "fail_closed"
	}
	return "fail_open"
}

// resolveFailurePolicy reads the configured failure posture (ResolveFailClosed).
func resolveFailurePolicy() FailurePolicy {
	if ResolveFailClosed() {
		return FailClosed
	}
	return FailOpen
}

// applyFailurePolicy is the Go analog of the SDK's _handle_api_error, applied
// between OBTAIN (E6-S1) and APPLY (E6-S2). It touches a decision ONLY when the
// sidecar failed to deliver a real verdict (dec.FailOpen) AND the org opted into
// fail-closed: it then synthesizes a HALT verdict (exactly as the SDK returns a
// synthetic Verdict.HALT) carrying a content-free reason, so the unchanged,
// policy-agnostic mapVerdict cascade denies the call via its normal HALT path.
//
// In every other case it returns the decision UNCHANGED:
//   - fail-open (default): a fail-open decision stays VerdictUnknown → mapVerdict
//     emits nothing → proceed (byte-identical to E6-S2 / observe).
//   - a REAL verdict (dec.FailOpen==false) under either policy: the failure policy
//     governs ONLY the evaluation-unavailable case, never a real ALLOW/CONSTRAIN/
//     BLOCK answer — a reachable sidecar's allow still proceeds under fail-closed.
//
// This only ever converts a would-be PROCEED into a DENY, so it upholds the
// tighten-only invariant (E6-S2) and INV-3b (the block is still synchronous,
// pre-execution, and bounded by the E6-S1 timeout).
func applyFailurePolicy(dec sidecar.Decision, policy FailurePolicy) sidecar.Decision {
	if !dec.FailOpen || policy != FailClosed {
		return dec
	}
	// Synthesize the SDK's fail-closed HALT. WouldBlock() becomes true, so the
	// durable audit records a HALT with FailOpen==true — the unambiguous signature
	// of a fail-closed deny (a real HALT never carries FailOpen==true).
	dec.Evaluation.Verdict = client.VerdictHalt
	dec.Evaluation.Reason = failClosedReason(dec.Evaluation.Reason)
	return dec
}

// failClosedReason builds the content-free deny reason for a fail-closed outage.
// govReason (E6-S2) prepends "OpenBox governance: ". The cause is the fail-open
// fallback's internal diagnostic (allowFailOpen: "sidecar unavailable", "sidecar
// read failed or timed out", …) — a fixed, content-free string, never tool
// content (INV-2).
func failClosedReason(cause string) string {
	r := "request denied — no governance decision could be obtained and this session is fail-closed"
	if cause != "" {
		r += " (" + cause + ")"
	}
	return r
}

// buildDecisionRequest assembles the local decision request from a PreToolUse
// payload, reusing the Mapper's tool classification (classifyTool / filePath) so
// the enforce gate and the observe event classify a tool identically. It carries
// the metadata axes a local policy matches on — tool name/kind, MCP server, file
// path/operation, permission mode, and (LOCAL-ONLY, never egressed) the shell
// command. Content (INV-2) is left nil: E6-S4 populates it, gated on content
// posture, for local redaction only.
func buildDecisionRequest(id Identity, e *HookEvent) sidecar.DecisionRequest {
	kind, sem, fileOp, mcpServer, function := classifyTool(e.ToolName)

	tool := client.Tool{Name: capStr(e.ToolName), Kind: kind}
	if kind == client.ToolMCP {
		tool.MCPServer = capStr(mcpServer)
	}

	// Metadata axes only (INV-2). compact drops empty values so a rule matching on
	// an absent attribute fails to match rather than matching "".
	attrs := map[string]any{
		"permission_mode": enumOr(e.PermissionMode, permissionModes),
	}
	switch {
	case isFileSemantic(sem):
		attrs["file_path"] = capStr(e.filePath()) // structural locator (INV-2)
		attrs["file_operation"] = fileOp
	case kind == client.ToolMCP:
		attrs["mcp_function"] = capStr(function)
	case kind == client.ToolShell:
		// Local-only: the command is the axis a policy matches a dangerous shell
		// action on. It goes ONLY to the local sidecar and is never egressed/logged
		// (HookEvent.command). Bounded to keep the local request small.
		attrs["command"] = capCommand(e.command())
	}

	return sidecar.DecisionRequest{
		Protocol:     sidecar.ProtocolVersion,
		SessionID:    e.SessionID,
		DeveloperDID: id.DeveloperDID,
		EventType:    client.EventToolCall, // the pre-execution gate is a ToolCall decision
		Tool:         tool,
		Attributes:   compactAny(attrs),
	}
}

// capCommand bounds the local-only command to maxCommandLen BYTES, truncating at
// a UTF-8 rune boundary so a multibyte rune is never split (which would corrupt
// the JSON string). Bounding by bytes — not runes — keeps the marshaled request
// under the server's byte read-limit regardless of the command's encoding
// (G_SEC LOW-1). An empty command yields "" (compactAny then drops it).
func capCommand(s string) string {
	if len(s) <= maxCommandLen {
		return s
	}
	cut := maxCommandLen
	// Back up off any continuation byte so we cut on a rune start.
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// compactAny drops empty-string values from an attribute map (absent-when-unknown,
// like the Mapper's compact) and returns nil when nothing is left, so the request
// carries no empty axes for a policy to spuriously not-match on.
func compactAny(m map[string]any) map[string]any {
	for k, v := range m {
		if s, ok := v.(string); ok && s == "" {
			delete(m, k)
		}
	}
	if len(m) == 0 {
		return nil
	}
	return m
}

// logEnforceDecision emits ONE terse, secret-free (INV-1) and content-free (INV-2)
// diagnostic line for an obtained enforce decision — verdict / source / fail_open
// / stale only, never the command, file path, or reason free text. It is the
// observable evidence for E6-S1 (and E6-S7 conformance) that the sync gate ran; it
// goes to stderr (never stdout — INV-3) and never blocks. E6-S2 adds the actual
// apply (stdout permissionDecision) on top of this same Decision.
func logEnforceDecision(logger *log.Logger, e *HookEvent, dec sidecar.Decision, policy FailurePolicy) {
	verdict := string(dec.Evaluation.Verdict)
	if verdict == "" {
		verdict = "UNKNOWN" // VerdictUnknown ("") — a fail-open / unevaluated decision
	}
	// policy is logged so a fail-closed deny (a synthesized HALT with fail_open=true)
	// is legible in the diagnostic — otherwise "would_block=true fail_open=true" looks
	// contradictory. See applyFailurePolicy (E6-S3).
	logger.Printf("enforce decision: tool=%s verdict=%s would_block=%t source=%s fail_open=%t stale=%t policy=%s",
		capStr(e.ToolName), verdict, dec.Evaluation.WouldBlock(), orDash(dec.Source), dec.FailOpen, dec.Stale, policy)
}

// ── E6-S2: apply(verdict) — the enforce leg's teeth ──────────────────────────
//
// E6-S1 OBTAINS a sidecar.Decision; E6-S2 APPLIES it — mapping the governance
// verdict onto a Claude Code PreToolUse `permissionDecision` written to stdout,
// the moment WouldBlock() becomes a real block. This ports the reference SDK's
// enforce_verdict cascade (openbox-temporal-sdk-python verdict_handler.py) — the
// full priority set HALT > BLOCK > guardrails > REQUIRE_APPROVAL > CONSTRAIN >
// ALLOW (OD-ENF-SCOPE) — onto Claude Code's hook contract:
//
//	SDK enforce_verdict                        →  CC PreToolUse permissionDecision
//	───────────────────────────────────────      ────────────────────────────────
//	HALT  → GovernanceHaltError (terminate)    →  deny  (strongest CC signal)
//	BLOCK → GovernanceBlockedError             →  deny
//	guardrails validation_passed == false      →  deny  (checked BEFORE approval)
//	REQUIRE_APPROVAL → requires_hitl            →  ask   (OD-HITL; E6-S6 refines UX)
//	CONSTRAIN        → logged allow             →  (nothing — proceed)
//	ALLOW / UNKNOWN (fail-open)                 →  (nothing — proceed)
//
// INVARIANT — governance only TIGHTENS. A non-blocking verdict writes NOTHING to
// stdout, so Claude Code's own permission flow is left untouched and behaves
// exactly as in observe mode. Only `deny`/`ask` are ever emitted — enforcement
// can add a restriction, never remove one of Claude Code's built-in prompts.
// This upholds INV-3b (blocks only pre-execution, within the E6-S1 timeout bound)
// and keeps the observe/advisory path byte-identical when nothing is blocked.

// Claude Code PreToolUse permissionDecision values (the hook stdout contract).
// Only deny/ask are emitted; allow is intentionally never written (tighten-only).
const (
	ccDecisionDeny = "deny"
	ccDecisionAsk  = "ask"
)

// preToolUseOutput is the Claude Code PreToolUse hook stdout contract: an exit-0
// hook that prints this JSON has its permissionDecision honored — `deny` blocks
// the tool call (Claude sees the reason), `ask` shows the user a permission
// prompt. permissionDecisionReason is shown LOCALLY (stdout → Claude Code on the
// same machine, no egress) and carries the POLICY-authored reason, never the tool
// command/file/output content (INV-2).
type preToolUseOutput struct {
	HookSpecificOutput hookSpecificOutput `json:"hookSpecificOutput"`
}

type hookSpecificOutput struct {
	HookEventName            string `json:"hookEventName"`
	PermissionDecision       string `json:"permissionDecision"`
	PermissionDecisionReason string `json:"permissionDecisionReason,omitempty"`
}

// applyDecision maps an obtained enforce decision onto a Claude Code PreToolUse
// permissionDecision and writes it to stdout — the E6-S2 apply. It returns the
// applied CC decision (`deny`/`ask`) and whether anything was emitted; a
// non-blocking verdict (CONSTRAIN/ALLOW/UNKNOWN) emits nothing and returns
// ("", false), so Claude Code's own permission flow proceeds unchanged.
//
// It NEVER wedges the tool call: a nil stdout or any marshal/write fault degrades
// to "proceed" (fail-open, OD9) — enforcement can only ADD a deny/ask, never hang
// or fail a call on an apply-side error (INV-3b fail-open).
func applyDecision(stdout io.Writer, dec sidecar.Decision) (applied string, emitted bool) {
	decision, reason := mapVerdict(dec.Evaluation)
	if decision == "" || stdout == nil {
		return "", false
	}
	out := preToolUseOutput{HookSpecificOutput: hookSpecificOutput{
		HookEventName:            string(HookPreToolUse),
		PermissionDecision:       decision,
		PermissionDecisionReason: reason,
	}}
	line, err := json.Marshal(out)
	if err != nil {
		return "", false // fail-open: never wedge a tool call on a marshal fault
	}
	if _, err := stdout.Write(append(line, '\n')); err != nil {
		return "", false // fail-open: a write fault degrades to proceed
	}
	return decision, true
}

// mapVerdict is the SDK enforce_verdict cascade (verdict_handler.py:50-103) ported
// to Claude Code decisions, in the SAME priority order. It returns the CC decision
// and a content-free reason, or ("","") meaning "emit nothing — proceed".
//
//   - HALT / BLOCK → deny (the SDK terminates / raises a non-retryable block).
//   - A failed guardrail validation → deny, checked AFTER HALT/BLOCK but BEFORE
//     approval and INDEPENDENT of the verdict value — exactly as the SDK, so a
//     guardrail failure is never silently swallowed by an approval flow
//     (verdict_handler.py:84-90).
//   - REQUIRE_APPROVAL → ask (the SDK's requires_hitl → OD-HITL; E6-S6 refines the
//     interactive prompt experience on top of this mapping).
//   - CONSTRAIN / ALLOW / UNKNOWN (fail-open) → nothing (the SDK logs CONSTRAIN and
//     otherwise proceeds; guardrail redaction of the input is E6-S4, gated on
//     content posture — deliberately not applied here).
func mapVerdict(e client.Evaluation) (decision, reason string) {
	switch e.Verdict {
	case client.VerdictHalt:
		return ccDecisionDeny, govReason(e, "action halted by OpenBox governance policy")
	case client.VerdictBlock:
		return ccDecisionDeny, govReason(e, "action blocked by OpenBox governance policy")
	}
	if g := e.Guardrail; g != nil && !g.Passed {
		return ccDecisionDeny, guardrailReason(g)
	}
	if e.Verdict == client.VerdictRequireApproval {
		return ccDecisionAsk, govReason(e, "action requires approval per OpenBox governance policy")
	}
	return "", ""
}

// govReason builds the LOCAL, content-free permissionDecisionReason shown to the
// developer for a deny/ask. It surfaces the POLICY-authored reason (the bundle/OPA
// rule's own text, e.g. "destructive recursive delete") and the policy id — text
// authored in the policy, not derived from the tool command/file/output content
// (INV-2). It is shown on this machine only (stdout → Claude Code) and is never
// egressed. Falls back to a generic message when the policy carried no reason.
func govReason(e client.Evaluation, fallback string) string {
	reason := e.Reason
	if reason == "" {
		reason = fallback
	}
	msg := "OpenBox governance: " + reason
	if e.PolicyID != "" {
		msg += " (policy: " + e.PolicyID + ")"
	}
	return msg
}

// guardrailReason renders a guardrail-failure deny reason from the CATEGORY types
// only (e.g. "[pii,secrets]") — never the guardrail reason free text, which can
// describe detected content (INV-2). Mirrors advisory.reasonTypes.
func guardrailReason(g *client.GuardrailResult) string {
	return "OpenBox guardrails validation failed " + reasonTypes(g.Reasons)
}

// enforcementRecord is one line in the enforcement audit sink (E6-S2): the
// governance decision that was ACTUALLY APPLIED to a tool call — distinct from an
// Advisory record, which captures what OpenBox WOULD enforce on the observe/flush
// path (SL-9). It is STRICTLY content-free (INV-1/INV-2): verdict/ids/flags plus
// the guardrail CATEGORY types only — never the tool content, the policy reason
// free text, or the guardrail reason free text. (This is deliberately stricter
// than SL-9's advisoryRecord, which serializes the full guardrail reason struct;
// projecting that sink to categories too is a noted fast-follow, out of E6-S2's
// write scope.) Being category-only keeps the sink safe even if a later story
// egresses it (e.g. to the dashboard) — no free text to leak.
type enforcementRecord struct {
	SessionID           string           `json:"session_id"`
	ToolKind            string           `json:"tool_kind,omitempty"`
	Verdict             string           `json:"verdict"`
	WouldBlock          bool             `json:"would_block"`
	AppliedDecision     string           `json:"applied_decision,omitempty"` // deny|ask|"" (proceed)
	Source              string           `json:"source,omitempty"`
	FailOpen            bool             `json:"fail_open"`
	Stale               bool             `json:"stale,omitempty"`
	PolicyID            string           `json:"policy_id,omitempty"`
	Constraints         []map[string]any `json:"constraints,omitempty"`
	GuardrailCategories []string         `json:"guardrail_categories,omitempty"`
}

// DefaultEnforcementPath is the enforcement audit sink, a sibling of the advisory
// sink (~/.config/openbox/enforcements.jsonl), overridable via
// OPENBOX_ENFORCEMENT_FILE (tests point it at a temp file).
func DefaultEnforcementPath() string {
	if p := os.Getenv(envEnforcementFile); p != "" {
		return p
	}
	dir, err := os.UserConfigDir()
	if err != nil || dir == "" {
		dir = filepath.Join(os.Getenv("HOME"), ".config")
	}
	return filepath.Join(dir, "openbox", "enforcements.jsonl")
}

// recordEnforcement appends one enforcement-decision audit line for an applied
// decision. It is the DURABLE enforcement record E6-S1 deferred here (STORY-E6-S1
// AC-5) — a same-machine, owner-only trail of what governance actually did. It is
// best-effort and OFF the blocking path: it runs after the stdout decision is
// already written, and any failure (marshal / mkdir / open / write) is logged to
// stderr and swallowed, never surfaced (INV-3). Content-free (INV-1/INV-2).
func recordEnforcement(logger *log.Logger, e *HookEvent, dec sidecar.Decision, applied string) {
	kind, _, _, _, _ := classifyTool(e.ToolName)
	rec := enforcementRecord{
		SessionID:       e.SessionID,
		ToolKind:        string(kind),
		Verdict:         string(dec.Evaluation.Verdict),
		WouldBlock:      dec.Evaluation.WouldBlock(),
		AppliedDecision: applied,
		Source:          dec.Source,
		FailOpen:        dec.FailOpen,
		Stale:           dec.Stale,
		PolicyID:        dec.Evaluation.PolicyID,
		Constraints:     dec.Evaluation.Constraints,
	}
	if g := dec.Evaluation.Guardrail; g != nil {
		rec.GuardrailCategories = reasonTypeCategories(g.Reasons) // category types only (INV-2)
	}
	line, err := json.Marshal(rec)
	if err != nil {
		logger.Printf("enforcement record skipped (marshal): %v", err)
		return
	}
	if err := appendJSONL(DefaultEnforcementPath(), line); err != nil {
		logger.Printf("enforcement record skipped: %v", err)
	}
}
