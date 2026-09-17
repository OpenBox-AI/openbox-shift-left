package codex

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
	"github.com/openbox-ai/openbox-shift-left/internal/client"
	"github.com/openbox-ai/openbox-shift-left/internal/decision"
)

const promptToolKind = "prompt"

type promptTarget struct {
	id     Identity
	mapper Mapper
	ev     *HookEvent
}

func (t promptTarget) SessionID() string { return t.ev.SessionID }

// ToolName labels the gate's diagnostics and the pending-approval marker; a
// prompt is not a tool, so the label says what it is.
func (t promptTarget) ToolName() string { return promptToolKind }

// ToolInput: a prompt has no tool_input, so there is nothing the proceed-path
// rewrite could reconstruct (promptOutputContract declares no content fields
// either; the pair keeps updatedInput structurally impossible here).
func (t promptTarget) ToolInput() json.RawMessage { return nil }

func (t promptTarget) HighRisk() bool { return false }

// DecisionRequest carries only identity axes: there is no tool to classify and
// no command to bound, so the local decider has nothing to match on and the
// verdict comes from /evaluate.
func (t promptTarget) DecisionRequest(bool) decision.DecisionRequest {
	return decision.DecisionRequest{
		SessionID:    t.ev.SessionID,
		DeveloperDID: t.id.DeveloperDID,
		EventType:    client.EventPromptSubmitted,
	}
}

// DevEvent maps the prompt for the inline evaluation through the same Mapper
// (and pinned clock) the observe copy uses, so the two derive one event_id and
// the gate's OnDelivered dedupe holds. It inherits the Mapper's RedactContent
// collaborator, so the evaluated copy is scanned exactly like the spooled one.
func (t promptTarget) DevEvent(*client.Content) (client.DevEvent, bool) {
	return t.mapper.Map(HookUserPromptSubmit, t.ev)
}

var _ hookflow.EnforceTarget = promptTarget{}

func recordPromptEnforcement(logger *log.Logger, e *HookEvent, dec decision.Decision, res hookflow.ApplyResult) {
	hookflow.RecordEnforcement(logger, e.SessionID, promptToolKind, dec, res)
}

// haltReplayTriple picks the contract and the labels a halted session replays
// with, per hook. Every gated class must read the latch, not just the prompt
// that wrote it: a session halted at the prompt would otherwise keep running
// tools.
func haltReplayTriple(hook HookName, ev *HookEvent) (hookflow.OutputContract, string, string) {
	switch hook {
	case HookPreToolUse:
		kind, _, _, _, _ := classifyTool(ev.ToolName)
		return contract, ev.ToolName, string(kind)
	case HookPermissionRequest:
		kind, _, _, _, _ := classifyTool(ev.ToolName)
		return permissionContract, ev.ToolName, string(kind)
	}
	return promptContract, promptToolKind, promptToolKind
}

// emitTurn writes one turn's llm_completion pair from the rollout window the
// cursor delimits. Ported from the Claude Code adapter, including two refusals
// worth keeping verbatim in intent:
//
//   - a SubagentStop with no agent_id is SKIPPED, never guessed: without one the
//     sidechain would share the main thread's cursor and corrupt both.
//   - the cursor advances LAST, after both halves are spooled. A crash between
//     them then over-reports into the control plane's dedupe, which is recoverable,
//     rather than losing a turn, which is not.
func emitTurn(ad *Adapter, logger *log.Logger, hook HookName, ev *HookEvent) {
	agentID := ev.AgentID
	sidechain := hook == HookSubagentStop
	if sidechain && agentID == "" {
		logger.Printf("finops: SubagentStop without agent_id, skipping turn (would share the main-thread cursor)")
		return
	}

	pos := ad.Turns.Read(ev.SessionID, agentID)
	window, next, err := readTurnUsage(ev.TranscriptPath, pos)
	if err != nil {
		logger.Printf("finops: turn usage skipped: %v", err)
		return
	}
	if !window.HasUsage {
		// No usage in this window: advance so the next turn measures from here,
		// and emit nothing. A turn that spent nothing is not a turn worth a row.
		if next != pos {
			if wErr := ad.Turns.Write(ev.SessionID, agentID, next); wErr != nil {
				logger.Printf("finops: turn cursor write failed: %v", wErr)
			}
		}
		return
	}

	started, completed, ok := ad.Mapper.MapTurn(ev, window, pos.Index)
	if !ok {
		return
	}
	for _, turnEv := range []client.DevEvent{started, completed} {
		if rErr := ad.Record(turnEv); rErr != nil {
			logger.Printf("finops: spool %s event: %v", turnEv.EventType, rErr)
			return
		}
	}

	next.Index = pos.Index + 1
	if err := ad.Turns.Write(ev.SessionID, agentID, next); err != nil {
		logger.Printf("finops: turn cursor write failed (window may be re-read): %v", err)
	}
}

// turnsEmitted reports whether this session ever emitted a per-turn pair, which
// is exactly the condition the SessionEnd rollup is gated on. The turn cursor is
// the record: an Index only advances when a pair was actually spooled.
//
// It must consider EVERY cursor in the session, not just the main thread's. A
// subagent keeps its own cursor under its own key, so asking only the main
// thread would answer "no turns" for a session whose subagent already reported
// real tokens -- and the rollup would then ship them a second time. That is
// reachable whenever a subagent runs and the enclosing top-level Stop never
// fires: a crash, a kill, or a surface that does not run the hook.
func turnsEmitted(ad *Adapter, sessionID string) bool {
	entries, err := os.ReadDir(ad.Turns.SessionDir(sessionID))
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		raw, rErr := os.ReadFile(filepath.Join(ad.Turns.SessionDir(sessionID), e.Name()))
		if rErr != nil {
			continue
		}
		// Reuse the shared cursor record type rather than re-describing its shape.
		var pos hookflow.TurnPos
		if json.Unmarshal(raw, &pos) == nil && pos.Index > 0 {
			return true
		}
	}
	return false
}
