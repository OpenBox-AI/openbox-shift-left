package claudecode

import (
	"context"
	"log"
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
// fails open (the daemon is treated as absent). The timeout defaults to
// sidecar.DefaultDecisionTimeout (~50 ms, ADR-0002).
func newSidecarClient() *sidecar.Client {
	return sidecar.NewClient(sidecar.ClientConfig{SocketPath: ResolveSidecarSocket()})
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
func logEnforceDecision(logger *log.Logger, e *HookEvent, dec sidecar.Decision) {
	verdict := string(dec.Evaluation.Verdict)
	if verdict == "" {
		verdict = "UNKNOWN" // VerdictUnknown ("") — a fail-open / unevaluated decision
	}
	logger.Printf("enforce decision: tool=%s verdict=%s would_block=%t source=%s fail_open=%t stale=%t",
		capStr(e.ToolName), verdict, dec.Evaluation.WouldBlock(), orDash(dec.Source), dec.FailOpen, dec.Stale)
}
