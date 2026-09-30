package muse

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
)

// Muse's hook runtime discards an answer it does not accept, and a discarded
// answer is an allow. Every gated event therefore has its own closed answer
// shape below, built from structs that cannot spell a key Muse rejects, and
// every shape is bounded under the caps Muse enforces on hook output.
//
// Rules shared by all four contracts:
//   - A refusal is a plain deny/block. `continue` and `stopReason` are rejected
//     on PreToolUse and PermissionRequest, and not documented on the other two,
//     so none of them has a field. A HALT renders the same refusal; it is the
//     engine's run-keyed latch, written because Render reports the halt back,
//     that denies the calls that follow.
//   - `allow` is never written, on any event: a bare one is rejected, and
//     PermissionRequest's skips the human approval prompt outright.
//   - REQUIRE_APPROVAL renders a refusal (ApprovalDecision), never an ask.
//   - A proceed writes nothing, or (PreToolUse only) a redacting updatedInput.

const (
	// decisionBlock is the literal of the `decision` key on the prompt and model
	// call events.
	decisionBlock = "block"
	// applyUpdated is what Render reports for a proceed that carries a rewrite.
	applyUpdated = "updatedInput"

	// fallbackReason stands in for an empty reason: Muse rejects a refusal with
	// no text, and a rejected answer is an allow.
	fallbackReason = "denied by OpenBox policy"

	// maxReasonBytes bounds a reason, counted in bytes and cut on a rune
	// boundary, so a CJK reason cannot triple the limit.
	maxReasonBytes = 4 << 10
	// maxSystemMessageBytes is Muse's bound on systemMessage (1000 characters;
	// bytes are the stricter reading).
	maxSystemMessageBytes = 1000
	// maxStdoutBytes is the size at which Muse fails a hook's output. An answer
	// must be strictly smaller, newline included.
	maxStdoutBytes = 16 << 10
)

// contentFieldKeys are the tool_input fields that hold a file body, in the
// precedence fileText reads them and RedactToolInput rewrites them. Every key a
// Muse write, edit or patch tool may carry a body under is listed: a body under
// an unlisted key would route around the secret scan and the content gate.
var contentFieldKeys = []string{"content", "new_string", "new_str", "file_text", "patch", "diff"}

// filePathKeys are the tool_input fields that name the file a tool touches.
var filePathKeys = []string{"file_path", "path", "filename", "notebook_path"}

// refusal reports whether Render was asked for a refusal. Anything else
// (including a literal Muse would read as a grant) renders as a proceed.
func refusal(decision string) bool {
	return decision == hookflow.DecisionDeny || decision == hookflow.DecisionHalt || decision == decisionBlock
}

// applied reports what a refusal is recorded as. A HALT stays a halt: that is
// the literal the gate latches on, and the render is the same plain refusal.
func applied(decision, literal string) string {
	if decision == hookflow.DecisionHalt {
		return hookflow.DecisionHalt
	}
	return literal
}

// marshal encodes v without HTML escaping, which would spend six bytes on each
// of <, > and & against a 16 KiB output cap.
func marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// fitted renders build(reason) with the reason capped at maxReasonBytes and,
// because JSON escaping can expand a byte six-fold, halved until the whole
// answer is under maxStdoutBytes. The answer Muse would fail is never sent.
// Every caller renders a refusal, and no answer at all reads as allow, so a
// refusal that cannot be rendered panics: on a gated event the panic reaches
// runHook, which exits with FaultExitCode and the refusal holds.
func fitted(reason string, build func(reason string) any) []byte {
	if reason == "" {
		reason = fallbackReason
	}
	reason = hookflow.TruncateBytes(reason, maxReasonBytes)
	for {
		line, err := marshal(build(reason))
		if err != nil {
			panic(fmt.Sprintf("muse: cannot render a refusal: %v", err))
		}
		if len(line)+1 < maxStdoutBytes {
			return line
		}
		if len(reason) <= len(fallbackReason) {
			panic("muse: a refusal does not fit the stdout cap")
		}
		reason = hookflow.TruncateBytes(reason, len(reason)/2)
	}
}

// PreToolUse.

type toolAnswer struct {
	HookSpecificOutput toolSpecificOutput `json:"hookSpecificOutput"`
}

type toolSpecificOutput struct {
	HookEventName            string `json:"hookEventName"`
	PermissionDecision       string `json:"permissionDecision,omitempty"`
	PermissionDecisionReason string `json:"permissionDecisionReason,omitempty"`
	// UpdatedInput is the redacted replacement tool_input, rebuilt from the
	// original with only the body field swapped; never sourced whole from a
	// decision. It rides alone: a permissionDecision beside it would have to be
	// an allow.
	UpdatedInput json.RawMessage `json:"updatedInput,omitempty"`
}

type outputContract struct{}

// ApprovalDecision: Muse rejects nothing here, but the gate never asks; a
// REQUIRE_APPROVAL is held for a real decision and denies if unanswered.
func (outputContract) ApprovalDecision() string { return hookflow.DecisionDeny }

func (outputContract) ContentFieldKeys() []string { return contentFieldKeys }

// Render builds the PreToolUse answer: a deny, a redacting updatedInput alone,
// or nothing.
func (outputContract) Render(decision, reason string, updatedInput json.RawMessage) ([]byte, string) {
	name := string(HookPreToolUse)
	if refusal(decision) {
		line := fitted(reason, func(r string) any {
			return toolAnswer{toolSpecificOutput{
				HookEventName: name, PermissionDecision: hookflow.DecisionDeny, PermissionDecisionReason: r,
			}}
		})
		return line, applied(decision, hookflow.DecisionDeny)
	}
	if len(updatedInput) == 0 || !isJSONObject(updatedInput) {
		// Nothing to say, or a rewrite Muse would reject (and then run the call
		// with the original input): write nothing rather than a bad answer.
		return nil, ""
	}
	line, err := marshal(toolAnswer{toolSpecificOutput{HookEventName: name, UpdatedInput: updatedInput}})
	if err != nil || len(line)+1 >= maxStdoutBytes {
		return nil, ""
	}
	return line, applyUpdated
}

func isJSONObject(raw json.RawMessage) bool {
	var m map[string]json.RawMessage
	return json.Unmarshal(raw, &m) == nil && m != nil
}

var _ hookflow.OutputContract = outputContract{}

var contract = outputContract{}

// UserPromptSubmit and PreLLMCall share one block shape.

type blockAnswer struct {
	Decision string `json:"decision"`
	Reason   string `json:"reason"`
}

func renderBlock(decision, reason string) ([]byte, string) {
	if !refusal(decision) {
		return nil, ""
	}
	line := fitted(reason, func(r string) any { return blockAnswer{Decision: decisionBlock, Reason: r} })
	return line, applied(decision, decisionBlock)
}

type promptOutputContract struct{}

// ApprovalDecision: a prompt has no native permission prompt, so what would ask
// refuses instead; strictly tighter, never a silent proceed.
func (promptOutputContract) ApprovalDecision() string { return hookflow.DecisionDeny }

// ContentFieldKeys: a prompt has no redactable tool_input, so no rewrite can
// engage; paired with promptTarget.ToolInput() returning nil.
func (promptOutputContract) ContentFieldKeys() []string { return nil }

func (promptOutputContract) Render(decision, reason string, _ json.RawMessage) ([]byte, string) {
	return renderBlock(decision, reason)
}

var _ hookflow.OutputContract = promptOutputContract{}

var promptContract = promptOutputContract{}

type llmOutputContract struct{}

// ApprovalDecision: the model-call gate refuses what would ask, like the rest.
func (llmOutputContract) ApprovalDecision() string { return hookflow.DecisionDeny }

// ContentFieldKeys: there is no tool_input to rewrite on a model call.
func (llmOutputContract) ContentFieldKeys() []string { return nil }

// Render emits the documented block answer, or nothing. A model call that is
// refused is refused for this call only: the latch, not this shape, stops the
// run's later calls.
func (llmOutputContract) Render(decision, reason string, _ json.RawMessage) ([]byte, string) {
	return renderBlock(decision, reason)
}

var _ hookflow.OutputContract = llmOutputContract{}

var llmContract = llmOutputContract{}

// PermissionRequest.

// permissionAnswer is deny-only: Muse's `allow` here skips the approval prompt,
// so the allow behaviour, updatedInput, updatedPermissions and interrupt have
// no field at all. A key that cannot be spelled cannot be emitted by a later
// edit.
type permissionAnswer struct {
	HookSpecificOutput permissionSpecificOutput `json:"hookSpecificOutput"`
}

type permissionSpecificOutput struct {
	HookEventName string              `json:"hookEventName"`
	Decision      permissionDecisionW `json:"decision"`
}

type permissionDecisionW struct {
	Behavior string `json:"behavior"`
	Message  string `json:"message"`
}

type permissionOutputContract struct{}

// ApprovalDecision: this surface IS the approval prompt, so "ask" would hand the
// decision back to the human the policy already answered for.
func (permissionOutputContract) ApprovalDecision() string { return hookflow.DecisionDeny }

// ContentFieldKeys is nil: no rewrite lever exists here.
func (permissionOutputContract) ContentFieldKeys() []string { return nil }

// Render emits a deny, or nothing: a proceed writes nothing so the human's own
// approval prompt still happens.
func (permissionOutputContract) Render(decision, reason string, _ json.RawMessage) ([]byte, string) {
	if !refusal(decision) {
		return nil, ""
	}
	line := fitted(reason, func(r string) any {
		return permissionAnswer{permissionSpecificOutput{
			HookEventName: string(HookPermissionRequest),
			Decision:      permissionDecisionW{Behavior: hookflow.DecisionDeny, Message: r},
		}}
	})
	return line, applied(decision, hookflow.DecisionDeny)
}

var _ hookflow.OutputContract = permissionOutputContract{}

var permissionContract = permissionOutputContract{}

// contractFor is the contract a gated hook answers with.
func contractFor(h HookName) hookflow.OutputContract {
	switch h {
	case HookPreToolUse:
		return contract
	case HookPermissionRequest:
		return permissionContract
	case HookPreLLMCall:
		return llmContract
	}
	return promptContract
}

// Findings.

// capFindingsOutput bounds the findings answer under Muse's systemMessage limit.
// hookflow writes the same summary to systemMessage and additionalContext, so
// both are cut. It returns nil for an answer it cannot read, which is then
// dropped: a findings line is never worth an answer Muse may reject.
func capFindingsOutput(line []byte) []byte {
	var m map[string]any
	if err := json.Unmarshal(line, &m); err != nil || m == nil {
		return nil
	}
	if s, ok := m["systemMessage"].(string); ok {
		m["systemMessage"] = hookflow.TruncateBytes(s, maxSystemMessageBytes)
	}
	if hso, ok := m["hookSpecificOutput"].(map[string]any); ok {
		if s, ok := hso["additionalContext"].(string); ok {
			hso["additionalContext"] = hookflow.TruncateBytes(s, maxSystemMessageBytes)
		}
	}
	out, err := marshal(m)
	if err != nil || len(out)+1 >= maxStdoutBytes {
		return nil
	}
	return out
}

// surfaceFindings writes the content-free findings summary through the cap. It
// is called only for a hook that is not gated: an answer Muse discards on a
// gated event is read as a failure and denied, which a status line must never
// cause.
func surfaceFindings(hook HookName, stdout io.Writer, logger *log.Logger) {
	var buf bytes.Buffer
	hookflow.SurfaceFindings(provider, string(hook), &buf, logger)
	if buf.Len() == 0 {
		return
	}
	out := capFindingsOutput(bytes.TrimSpace(buf.Bytes()))
	if out == nil {
		logger.Printf("findings: dropped a summary that could not be bounded")
		return
	}
	if _, err := stdout.Write(append(out, '\n')); err != nil {
		logger.Printf("findings: write failed: %v", err)
	}
}
