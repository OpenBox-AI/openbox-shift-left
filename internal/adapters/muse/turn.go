package muse

import (
	"log"
	"os"
	"strings"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
	"github.com/openbox-ai/openbox-shift-left/internal/client"
	"github.com/openbox-ai/openbox-shift-left/internal/trace"
)

// stopContentBudget bounds how long Stop waits for the turn's reasoning
// summaries to reach the session journal. The hook's ceiling is 5s; this leaves
// the reconcile pass and delivery their share.
var stopContentBudget = 500 * time.Millisecond

const stopContentPoll = 50 * time.Millisecond

// MapTurn builds one model turn's TurnStarted/TurnCompleted pair from a Stop
// (or a folded SubagentStop). The reply is the hook payload's own
// last_assistant_message; thinking is the reasoning summaries read from the
// session journal, "" when they could not be. No tokens ride the pair: usage
// reaches core on the telemetry lane's model-call row. The pair is built only
// when the turn has a reply or thinking to report.
func (m Mapper) MapTurn(e *HookEvent, thinking string, index int) (started, completed client.DevEvent, ok bool) {
	if e == nil || e.SessionID == "" || (e.LastAssistantMessage == "" && thinking == "") || !m.Identity.HasDeveloperDID() {
		return client.DevEvent{}, client.DevEvent{}, false
	}

	ts := m.clock().UTC().Format(time.RFC3339Nano)
	turnIndex := index
	base := client.DevEvent{
		SchemaVersion: client.SchemaVersion,
		SessionID:     e.SessionID,
		DeveloperDID:  m.Identity.DeveloperDID,
		Tool:          client.Tool{Name: agentToolName, Kind: client.ToolShell},
		TurnIndex:     &turnIndex,
	}
	if e.folded() {
		base.AgentID = capStr(e.SubagentID)
	}
	if m.Run != nil {
		base.RunID = m.Run.RunID
		base.RunGeneration = m.Run.Generation
	}
	meta := func() map[string]any {
		md := hookflow.Compact(map[string]any{"turn_id": capStr(e.TurnID)})
		md["turn_index"] = turnIndex
		if e.folded() {
			md = mergeMetadata(md, subagentMetadata(e))
		}
		return md
	}

	started = base
	started.EventType = client.EventTurnStarted
	started.Timestamp = ts
	started.StartedAt = ts
	started.Metadata = meta()
	started.EventID = m.eventID(started)

	completed = base
	completed.EventType = client.EventTurnCompleted
	completed.Timestamp = ts
	completed.EndedAt = ts
	completed.Model = capStr(e.Model)
	completed.Metadata = meta()
	// The reply and the thinking ride the completed half only, under capture
	// and redacted: a turn's input is the prompt, which already ships on
	// PromptSubmitted under the same gate.
	completed.Content = hookflow.TurnContent(m.CaptureContent, m.RedactContent, e.LastAssistantMessage, thinking)
	completed.EventID = m.eventID(completed)
	return started, completed, true
}

// stashModelCall records what a turn's later hooks and the telemetry lane need
// from one PostLLMCall: the response id joins the turn's index (ids only,
// always), and the request body is stashed for the model-call row under
// capture. An echo or empty response id is not joinable and is skipped.
func stashModelCall(ad *Adapter, logger *log.Logger, e *HookEvent) {
	if joinableResponseID(e.ResponseID) != nil {
		return
	}
	dir := ad.Spool.Dir
	if ad.Mapper.CaptureContent {
		entry := RequestEntry{
			SessionID:      e.SessionID,
			ChildSessionID: e.SubagentSessionID,
			TurnID:         e.TurnID,
			Body:           RequestBody(e, ad.Mapper.redact),
		}
		if err := PutRequest(dir, e.ResponseID, entry, true); err != nil {
			logger.Printf("stash request body: %v", err)
		}
	}
	if err := AppendTurnResponse(dir, e.SessionID, e.TurnID, e.ResponseID); err != nil {
		logger.Printf("index turn response: %v", err)
	}
}

// clearTurnIndex removes a session's whole turn index, so a session that ends
// leaves no stale files. Stashed request bodies are deliberately left: the last
// call's telemetry record arrives after the session ends, and the TTL retires
// them.
func clearTurnIndex(spoolDir, sessionID string) {
	if spoolDir == "" || sessionID == "" {
		return
	}
	_ = os.RemoveAll(turnIndexDir(spoolDir, sessionID))
}

// readTurnThinking joins the reasoning summaries of a turn's responses from the
// session journal. It polls for the journal to catch up with the hook until
// budget runs out; anything short of a verified read yields no thinking and the
// state that says why.
func readTurnThinking(root, sessionID string, ids []string, budget time.Duration) (string, ContentState) {
	deadline := time.Now().Add(budget)
	reader := NewContentReader(root, sessionID, ids)
	for {
		c, state := reader.Read(deadline)
		caughtUp := state == ContentVerified && (c.Text != "" || len(c.ToolCalls) > 0)
		if state == ContentUnverified || caughtUp || !time.Now().Add(stopContentPoll).Before(deadline) {
			if state != ContentVerified {
				return "", state
			}
			return strings.Join(c.Summaries, "\n\n"), state
		}
		time.Sleep(stopContentPoll)
	}
}

// emitTurn reports the turn a Stop (or a folded SubagentStop) closes. A folded
// event without an agent id is skipped, never guessed: it would share the main
// thread's cursor. The cursor advances last, after both halves are spooled, so
// a crash between them re-reports into core's dedupe instead of losing a turn.
func emitTurn(ad *Adapter, logger *log.Logger, ev *HookEvent) {
	agentID := ""
	if ev.folded() {
		if agentID = capStr(ev.SubagentID); agentID == "" {
			logger.Printf("turn: subagent event without an agent id, skipping turn")
			_ = TakeTurnResponses(ad.Spool.Dir, ev.SessionID, ev.TurnID)
			return
		}
	}

	// Always taken: the index is consumed by the turn it describes, capture or
	// not.
	ids := TakeTurnResponses(ad.Spool.Dir, ev.SessionID, ev.TurnID)

	thinking := ""
	if ad.Mapper.CaptureContent && len(ids) > 0 {
		logSession := ev.SessionID
		if ev.folded() {
			logSession = ev.SubagentSessionID
		}
		var state ContentState
		thinking, state = readTurnThinking(sessionLogRoot(), logSession, ids, stopContentBudget)
		if state != ContentVerified {
			run := ""
			if ad.Mapper.Run != nil {
				run = ad.Mapper.Run.RunID
			}
			trace.Emit(trace.Record{
				Provider:  provider,
				SessionID: ev.SessionID,
				RunID:     run,
				EventType: string(HookStop),
				Stage:     trace.StageCapture,
				Outcome:   "muse.content",
				Detail:    map[string]any{"reason": string(state)},
			})
		}
	}

	pos := ad.Turns.Read(ev.SessionID, agentID)
	started, completed, ok := ad.Mapper.MapTurn(ev, thinking, pos.Index)
	if !ok {
		return
	}
	for _, turnEv := range []client.DevEvent{started, completed} {
		if err := ad.Record(turnEv); err != nil {
			logger.Printf("spool %s event: %v", turnEv.EventType, err)
			return
		}
	}
	pos.Index++
	if err := ad.Turns.Write(ev.SessionID, agentID, pos); err != nil {
		logger.Printf("turn cursor write failed (the turn may be re-reported): %v", err)
	}
}
