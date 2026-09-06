package claudecode

import (
	"encoding/json"
	"log"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
	"github.com/openbox-ai/openbox-shift-left/internal/client"
	"github.com/openbox-ai/openbox-shift-left/internal/decision"
)

const configToolKind = "config"

// policySettingsSource is the one ConfigChange source the vendor documents as
// unblockable: "policy_settings changes can't be blocked... any blocking
// decision is ignored". Gating it would file an enforcement audit line
// claiming a block that never happened, so hookrun.go's `gated` compares
// ev.Source to this literal RAW -- never through enumOr, which would map an
// unrecognized future source to "" and could accidentally exempt it too.
const policySettingsSource = "policy_settings"

// configSubject is promptTarget's template, not enforceTarget's: a config
// change is not a tool call. No tool_input to rewrite, no HighRisk class, no
// MCP or file semantics.
type configSubject struct {
	id     Identity
	mapper Mapper
	ev     *HookEvent
}

func (t configSubject) SessionID() string { return t.ev.SessionID }

// ToolName labels the gate's diagnostics and the pending-approval marker.
func (t configSubject) ToolName() string { return configToolKind }

// ToolInput: a config change has no tool_input, so there is nothing the
// proceed-path rewrite could reconstruct (configChangeOutputContract declares
// no content fields either).
func (t configSubject) ToolInput() json.RawMessage { return nil }

func (t configSubject) HighRisk() bool { return false }

// DecisionRequest carries only the two structural axes a local rule could
// match on: source and file_path. No Content: a config change has no body we
// read.
func (t configSubject) DecisionRequest(bool) decision.DecisionRequest {
	return decision.DecisionRequest{
		SessionID:    t.ev.SessionID,
		DeveloperDID: t.id.DeveloperDID,
		EventType:    client.EventConfigChange,
		Attributes: hookflow.CompactAny(map[string]any{
			"source":    t.ev.Source,
			"file_path": t.ev.FilePath,
		}),
	}
}

// DevEvent maps the change for the inline evaluation through the same Mapper
// (and pinned clock) the observe copy uses, so the two derive one event_id and
// the gate's OnDelivered dedupe holds.
func (t configSubject) DevEvent(*client.Content) (client.DevEvent, bool) {
	return t.mapper.Map(HookConfigChange, t.ev)
}

var _ hookflow.EnforceTarget = configSubject{}

// recordConfigEnforcement is the fifth RecordEnforcement call site (phase 03).
// D6: Claude Code surfaces no message to the developer for a blocked
// ConfigChange, so a DENY/HALT also gets one human line on stderr naming the
// file, the source, and the policy-authored reason -- the same string already
// rendered to stdout (never tool content; INV-2). Stderr only, exit 0: INV-3
// is unchanged.
func recordConfigEnforcement(logger *log.Logger, e *HookEvent, dec decision.Decision, res hookflow.ApplyResult) {
	hookflow.RecordEnforcement(logger, e.SessionID, configToolKind, dec, res)
	if res.Decision == "" {
		return // proceed → nothing to announce
	}
	_, reason := hookflow.MapVerdict(dec.Evaluation, configContract)
	logger.Printf("OpenBox blocked a Claude Code configuration change (file=%s source=%s): %s",
		hookflow.OrDash(e.FilePath), hookflow.OrDash(e.Source), reason)
}
