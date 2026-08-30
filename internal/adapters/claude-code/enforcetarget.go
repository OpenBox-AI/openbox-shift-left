package claudecode

import (
	"encoding/json"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
	"github.com/openbox-ai/openbox-shift-left/internal/client"
	"github.com/openbox-ai/openbox-shift-left/internal/decision"
)

// enforceTarget adapts a native hook event to the shared enforce gate. Reading
// the provider's own event shape is the only provider-specific part of gating
// a tool call; the order of the gate's steps is shared (hookflow.EnforceGate).
type enforceTarget struct {
	id     Identity
	mapper Mapper
	ev     *HookEvent
}

func (t enforceTarget) SessionID() string          { return t.ev.SessionID }
func (t enforceTarget) ToolName() string           { return t.ev.ToolName }
func (t enforceTarget) ToolInput() json.RawMessage { return t.ev.ToolInput }
func (t enforceTarget) HighRisk() bool             { return isHighRiskClass(t.ev.ToolName) }

func (t enforceTarget) DecisionRequest(localRedaction bool) decision.DecisionRequest {
	return buildDecisionRequest(t.id, t.ev, localRedaction)
}

// DevEvent maps the call for the inline evaluation and attaches the content the
// server needs to judge it.
//
// This used to be the ONLY place a tool's input reached an outbound event, and
// the observe copy's emptiness was what made SL3-SEC-3 hold by construction.
// ADR-0019 P1 retires that: Mapper.Map now attaches the same extract to the
// observe copy under the content gate, and this method OVERWRITES it here.
//
// **What is attached differs by class, and the difference is not cosmetic:**
//
//   - A FILE write carries the REDACTED body — rebuilt through the same
//     RedactToolInput the tool-call rewrite uses, from the same detection
//     result, so the server judges exactly the bytes the developer's file was
//     written with. That is why the overwrite exists at all.
//   - A SHELL or MCP call carries the command/arguments VERBATIM. buildDecisionRequest
//     populates DecisionRequest.Content only for a file semantic, so `redacted`
//     is nil here for these classes and no rebuild happens. A token on a `curl`
//     command line reaches /evaluate in the clear.
//
// That asymmetry predates ADR-0019 and is arguably deliberate — a policy matching
// on a dangerous command should see the true command, and unlike a file body
// nothing here is replayed into the developer's machine. But ADR-0019 makes it
// VISIBLE in a new way: the observe copy of the very same call now runs the text
// redactor (Mapper.Map), so ordinary telemetry is better protected than the copy
// sent for a governance decision. Recorded, not silently fixed — changing it
// changes what policy can match on, which is an owner decision, not a cleanup.
//
// Evaluation is a different question from telemetry. The org has asked OpenBox
// to decide about this call; it cannot decide on content it cannot see.
// Content-gated all the same — the client's stripContent drops it when the org
// has content capture off (INV-2 at the choke point, not by adapter
// convention).
func (t enforceTarget) DevEvent(redacted *client.Content) (client.DevEvent, bool) {
	ev, ok := t.mapper.Map(HookPreToolUse, t.ev)
	if !ok {
		return ev, false
	}
	if in := evaluationContext(t.ev, redacted); in != "" {
		ev.Content = &client.Content{ToolInput: in}
	}
	return ev, true
}

// evaluationContext is what the server needs to decide about this call: the
// command for a shell tool, the arguments for an MCP one, the file body for a
// write.
//
// It covered only shell and MCP while those were the only escalated classes.
// ADR-0017 evaluates every gated class, so a Write's content is attached now —
// which is a real change in what leaves the machine, disclosed in the ADR and
// gated on content capture.
//
// **Redaction happens first, and this returns the redacted bytes.** The body is
// rebuilt through the same RedactToolInput the tool-call rewrite uses, from the
// same detection result, so the server judges exactly the text the developer's
// tool was rewritten to — not the original, and not a second redaction that
// could differ from it. When nothing was scanned or nothing matched, redacted is
// nil and the original stands.
// The bound is MaxRedactBody, not MaxCommandLen. MaxCommandLen (8 KiB) is
// documented as bounding the LOCAL DecisionRequest command and being "never
// egressed"; this string IS egressed, on Content.ToolInput, so using it here made
// the gated copy 8x smaller than the 64KB every document describes — and tied
// what the server can see to a constant chosen for local matching. MaxRedactBody
// is the bound the observe copy already lands on (m.redact truncates there), so
// the two copies of one call stay the same size as well as the same text.
func evaluationContext(e *HookEvent, redacted *client.Content) string {
	return hookflow.TruncateBytes(toolInputExtract(e, redacted), hookflow.MaxRedactBody)
}

// toolInputExtract is evaluationContext without the cap, so a caller that has to
// redact the text itself can do so BEFORE it is truncated.
//
// The split exists for the observe path (Mapper.Map): it has no detection result
// to rebuild from, so it runs the text redactor over this output and only then
// caps. Capping first would cut a secret straddling the boundary into a fragment
// no pattern matches — and ship the fragment. Same detect → redact → cap order
// the gated path gets for free from the rebuild above.
func toolInputExtract(e *HookEvent, redacted *client.Content) string {
	kind, _, _, _, _ := classifyTool(e.ToolName)
	input := e.ToolInput
	if redacted != nil && redacted.FileText != "" {
		if rebuilt := hookflow.RedactToolInput(input, redacted.FileText, contentFieldKeys); len(rebuilt) > 0 {
			input = rebuilt
		}
	}
	if kind == client.ToolShell {
		// The command alone, not the enclosing object: it is the field an
		// approver and every existing dashboard read. Claude Code declares no
		// redactable shell field (contentFieldKeys is content/new_string), so
		// input is unchanged here and reading from the event is equivalent —
		// stated rather than assumed, because adding "command" to those keys
		// would silently make it wrong.
		return commandOf(input, e)
	}
	return string(input)
}

// commandOf reads the shell command from a possibly-rebuilt tool_input, falling
// back to the event's own accessor when the rebuild did not happen.
func commandOf(input json.RawMessage, e *HookEvent) string {
	if len(input) == 0 || jsonEqualRaw(input, e.ToolInput) {
		return e.command()
	}
	var obj struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(input, &obj); err != nil {
		return e.command()
	}
	return obj.Command
}

func jsonEqualRaw(a, b json.RawMessage) bool { return string(a) == string(b) }

var _ hookflow.EnforceTarget = enforceTarget{}
