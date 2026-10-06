package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	obgit "github.com/openbox-ai/openbox-shift-left/internal/adapters/common/git"
	"github.com/openbox-ai/openbox-shift-left/internal/adapters/muse"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/gatewayemit"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/providers"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/telemetryemit"
	"github.com/openbox-ai/openbox-shift-left/internal/client"
	"github.com/openbox-ai/openbox-shift-left/internal/client/fakecore"
	"github.com/openbox-ai/openbox-shift-left/internal/gateway"
	"github.com/openbox-ai/openbox-shift-left/internal/provider"
	"github.com/openbox-ai/openbox-shift-left/internal/telemetry"
)

// The activity-parity matrix gives "Muse and Codex match Claude Code" an
// executable definition. Claude Code's delivered classes and content keys are
// the reference. For each provider every class is exactly one of:
//
//	produced       asserted on the wire body core would receive
//	na(reason)     the tool cannot produce it, with the reason written down
//	pending(n, …)  the plan has not produced it yet; phase n owns the fix
//
// A pending row that is not produced is reported as a skip naming its owner.
// A pending row that IS produced fails, so the phase that makes it true is
// forced to flip the row to produced() in the same change.
//
// Every assertion reads the raw bytes fakecore accepted. A struct is not the
// wire, so nothing here inspects a DevEvent.

const (
	provCC    = "claude-code"
	provCodex = "codex"
	provMuse  = "muse"
)

var matrixProviders = []string{provCC, provCodex, provMuse}

// Fixture values the predicates look for. Plain words, not secret-shaped: the
// local redactor must leave them alone with capture on, and the same strings
// are what the capture-off sentinel proves never egress.
const (
	sentPrompt   = "MATRIXPROMPT"
	sentToolIn   = "MATRIXTOOLIN"
	sentToolOut  = "MATRIXTOOLOUT"
	sentReply    = "MATRIXREPLY"
	sentThinking = "MATRIXTHINKING"
	sentNotify   = "MATRIXNOTIFY"
	sentLaneReq  = "MATRIXLANEREQ"
	sentLaneResp = "MATRIXLANERESP"
)

var matrixSentinels = []string{sentPrompt, sentToolIn, sentToolOut, sentReply, sentThinking, sentNotify, sentLaneReq, sentLaneResp}

// matrixContentKeys mirrors client's contentMetadataKeys (unexported) plus the
// turn keys that ride activity_output. A copy, because the gate's own list is
// private to its package; the sentinel strings are the check that does not
// depend on this list being complete.
var matrixContentKeys = map[string]bool{
	"message": true, "prompt": true, "output": true, "content": true, "file_text": true,
	"diff": true, "patch": true, "body": true, "stdout": true, "stderr": true,
	"command": true, "input_text": true, "denial_reason": true, "error_details": true,
	"arguments": true, "thinking": true, "reply_text": true,
	"requested_tool_input": true, "notification_message": true, "task_subject": true,
	"compact_instructions": true, "compact_summary": true, "elicitation_message": true,
	"elicitation_response": true, "notification_title": true, "task_description": true,
	"message_previews": true,
}

// wire is one request body fakecore accepted, decoded just far enough to ask
// the questions the matrix asks.
type wire struct {
	Raw          string
	EventType    string
	ActivityType string
	ActivityID   string
	SignalName   string
	In, Out, Sig map[string]any
	Meta         map[string]any
}

func decodeWires(inbox []fakecore.Received) []wire {
	out := make([]wire, 0, len(inbox))
	for _, r := range inbox {
		var b struct {
			EventType      string         `json:"event_type"`
			ActivityType   string         `json:"activity_type"`
			ActivityID     string         `json:"activity_id"`
			SignalName     string         `json:"signal_name"`
			ActivityInput  map[string]any `json:"activity_input"`
			ActivityOutput map[string]any `json:"activity_output"`
			SignalArgs     map[string]any `json:"signal_args"`
			Metadata       map[string]any `json:"metadata"`
		}
		_ = json.Unmarshal(r.Raw, &b)
		out = append(out, wire{
			Raw: string(r.Raw), EventType: b.EventType, ActivityType: b.ActivityType,
			ActivityID: b.ActivityID, SignalName: b.SignalName,
			In: b.ActivityInput, Out: b.ActivityOutput, Sig: b.SignalArgs, Meta: b.Metadata,
		})
	}
	return out
}

// str reads a non-empty string key.
func str(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

func anyWire(ws []wire, pred func(wire) bool) bool {
	for _, w := range ws {
		if pred(w) {
			return true
		}
	}
	return false
}

func hasSignal(name string) func([]wire) bool {
	return func(ws []wire) bool {
		return anyWire(ws, func(w wire) bool { return w.EventType == "SignalReceived" && w.SignalName == name })
	}
}

// isLane reports an llm_completion row from one of the three model-call
// lanes. A hook turn is `:turn:`, and is a different class.
func isLane(w wire) bool {
	if w.ActivityType != "llm_completion" {
		return false
	}
	for _, ns := range []string{":gateway:", ":otel:", ":proxy:"} {
		if strings.Contains(w.ActivityID, ns) {
			return true
		}
	}
	return false
}

func isTurn(w wire) bool {
	return w.ActivityType == "llm_completion" && strings.Contains(w.ActivityID, ":turn:")
}

// ---- the table -------------------------------------------------------------

type cell struct {
	kind   string // "produced" | "na" | "pending"
	reason string // na: why the tool cannot; pending: what is missing
	phase  int    // pending: the owning phase
}

func produced() cell        { return cell{kind: "produced"} }
func na(reason string) cell { return cell{kind: "na", reason: reason} }
func pending(phase int, missing string) cell {
	return cell{kind: "pending", phase: phase, reason: missing}
}

type matrixRow struct {
	class string
	// want says what the predicate looks for, so a red row is readable.
	want  string
	check func([]wire) bool
	by    map[string]cell
}

func row(class, want string, check func([]wire) bool, cc, codex, muse cell) matrixRow {
	return matrixRow{class: class, want: want, check: check,
		by: map[string]cell{provCC: cc, provCodex: codex, provMuse: muse}}
}

const (
	naMuseNoHook   = "Muse has no such hook; its hook surface is the one in internal/adapters/muse/hookevent.go"
	naCodexNoHook  = "Codex has no such hook; its hook surface is the one in internal/adapters/codex/hookevent.go"
	naMuseSubagent = "harness limitation, not a Muse gap: a live Muse folds a subagent into its parent when the parent's journal links them (muse/subagentparent.go, proven by TestSubagentEventsFoldIntoTheParentSession), and this driver has no parent journal, so its child is its own SessionStarted"
	naMuseBatch    = "PostToolBatch is registered by Muse 1.4.2 but never fires: parallel tool batches in exec and the TUI produced zero fires, and the binary calls it production-dark (plans/261001-1440-muse-codex-claude-code-parity/reports/phase-03-step0-live-capture.md)"
)

func matrixRows() []matrixRow {
	return []matrixRow{
		row("SessionStarted", "WorkflowStarted row with activity_type SessionStarted",
			func(ws []wire) bool {
				return anyWire(ws, func(w wire) bool { return w.EventType == "WorkflowStarted" && w.ActivityType == "SessionStarted" })
			}, produced(), produced(), produced()),
		row("SessionEnded", "WorkflowCompleted row with activity_type SessionEnded",
			func(ws []wire) bool {
				return anyWire(ws, func(w wire) bool { return w.EventType == "WorkflowCompleted" && w.ActivityType == "SessionEnded" })
			}, produced(), produced(), produced()),
		row("PromptSubmitted prompt", "prompt_submitted signal with signal_args.prompt",
			func(ws []wire) bool {
				return anyWire(ws, func(w wire) bool {
					return w.SignalName == "prompt_submitted" && strings.Contains(str(w.Sig, "prompt"), sentPrompt)
				})
			}, produced(), produced(), produced()),
		row("ToolCall tool input", "ActivityStarted with activity_input.command",
			func(ws []wire) bool {
				return anyWire(ws, func(w wire) bool {
					return w.EventType == "ActivityStarted" && strings.Contains(str(w.In, "command"), sentToolIn)
				})
			}, produced(), produced(), produced()),
		row("ToolResult tool output", "ActivityCompleted with activity_output.output",
			func(ws []wire) bool {
				return anyWire(ws, func(w wire) bool {
					return w.EventType == "ActivityCompleted" && strings.Contains(str(w.Out, "output"), sentToolOut)
				})
			}, produced(), produced(), produced()),
		row("PermissionRequest", "permission_request signal", hasSignal("permission_request"),
			produced(), produced(), produced()),
		row("PermissionDenied", "permission_denied signal", hasSignal("permission_denied"),
			produced(), na(naCodexNoHook+" (no PermissionDenied)"), na(naMuseNoHook+" (no PermissionDenied)")),
		row("SubagentStarted", "subagent_started signal", hasSignal("subagent_started"),
			produced(), produced(), na(naMuseSubagent)),
		row("APIError", "api_error signal", hasSignal("api_error"),
			produced(), na(naCodexNoHook+" (no StopFailure)"), produced()),
		row("PreCompact", "pre_compact signal", hasSignal("pre_compact"),
			produced(), produced(), produced()),
		row("PostCompact", "post_compact signal", hasSignal("post_compact"),
			produced(), produced(), produced()),
		row("Notification", "notification signal", hasSignal("notification"),
			produced(), na(naCodexNoHook+" (no Notification)"), produced()),
		row("PostToolBatch", "post_tool_batch signal", hasSignal("post_tool_batch"),
			produced(), na(naCodexNoHook+" (no PostToolBatch)"), na(naMuseBatch)),
		row("v1.8 lifecycle signals", "setup, instructions_loaded, task_created, teammate_idle, cwd_changed, file_changed signals",
			func(ws []wire) bool {
				for _, n := range []string{"setup", "instructions_loaded", "task_created", "teammate_idle", "cwd_changed", "file_changed"} {
					if !hasSignal(n)(ws) {
						return false
					}
				}
				return true
			}, produced(), na("Codex never emits the v1.8 lifecycle hooks (Setup, InstructionsLoaded, Task*, TeammateIdle, CwdChanged, FileChanged)"),
			na("Muse never emits the v1.8 lifecycle hooks (Setup, InstructionsLoaded, Task*, TeammateIdle, CwdChanged, FileChanged)")),
		row("Interrupt", "interrupt signal", hasSignal("interrupt"),
			na("not a Claude Code class; Interrupt is a Muse-only hook"), na("not a Codex class; Interrupt is a Muse-only hook"),
			na("Interrupt is in Muse's hook surface but openbox never installs it (internal/adapters/muse/hookevent.go)")),
		row("hook turn pair", "llm_completion Started+Completed pair under a :turn: activity id",
			func(ws []wire) bool {
				return anyWire(ws, func(w wire) bool { return isTurn(w) && w.EventType == "ActivityStarted" }) &&
					anyWire(ws, func(w wire) bool { return isTurn(w) && w.EventType == "ActivityCompleted" })
			}, produced(), produced(), produced()),
		row("hook turn reply", "turn ActivityCompleted with activity_output.content and reply_text",
			func(ws []wire) bool {
				return anyWire(ws, func(w wire) bool {
					return isTurn(w) && w.EventType == "ActivityCompleted" &&
						strings.Contains(str(w.Out, "content"), sentReply) && strings.Contains(str(w.Out, "reply_text"), sentReply)
				})
			}, produced(), produced(), produced()),
		row("hook turn thinking", "turn ActivityCompleted with activity_output.thinking",
			func(ws []wire) bool {
				return anyWire(ws, func(w wire) bool {
					return isTurn(w) && w.EventType == "ActivityCompleted" && strings.Contains(str(w.Out, "thinking"), sentThinking)
				})
			}, produced(), produced(), produced()),
		row("lane llm_completion request content", "lane ActivityStarted with activity_input.content",
			func(ws []wire) bool {
				return anyWire(ws, func(w wire) bool {
					return isLane(w) && w.EventType == "ActivityStarted" && str(w.In, "content") != ""
				})
			}, produced(), produced(), produced()),
		row("lane llm_completion response content", "lane ActivityCompleted with activity_output.content",
			func(ws []wire) bool {
				return anyWire(ws, func(w wire) bool {
					return isLane(w) && w.EventType == "ActivityCompleted" && str(w.Out, "content") != ""
				})
			}, produced(), produced(), produced()),
		row("proxy (transport) lane", "lane llm_completion row under a :proxy: activity id",
			func(ws []wire) bool {
				return anyWire(ws, func(w wire) bool { return isLane(w) && strings.Contains(w.ActivityID, ":proxy:") })
			}, produced(),
			na("Codex's transport arm is macOS-only and PAC-gated; the cross-platform capture path is telemetry plus the rollout (the rows above)"),
			na("Muse ignores the system PAC and rejects the relay's CA (rustls roots), so there is no transport arm")),
		row("CommitCreated", "CommitCreated signal from the post-commit sink",
			hasSignal("commit_created"), produced(), produced(), produced()),
		row("ModelCallRequested/Finished pair", "model_call_gate Started+Completed pair",
			func(ws []wire) bool {
				return anyWire(ws, func(w wire) bool { return w.ActivityType == "model_call_gate" && w.EventType == "ActivityStarted" }) &&
					anyWire(ws, func(w wire) bool { return w.ActivityType == "model_call_gate" && w.EventType == "ActivityCompleted" })
			}, na("Muse-only extension: Claude Code has no model-call gate"), na("Muse-only extension: Codex has no model-call gate"), produced()),
		row("ModelCallRequested content", "model_call_gate ActivityStarted with activity_input.content",
			func(ws []wire) bool {
				return anyWire(ws, func(w wire) bool {
					return w.ActivityType == "model_call_gate" && w.EventType == "ActivityStarted" && str(w.In, "content") != ""
				})
			}, na("Muse-only extension: Claude Code has no model-call gate"), na("Muse-only extension: Codex has no model-call gate"),
			produced()),
	}
}

// ---- drivers ---------------------------------------------------------------

// hookPayloads is one session per provider through the provider's own native
// hook surface. A hook a tool does not have is not sent.
func hookPayloads(t *testing.T, prov string) []fakecore.HookPayload {
	t.Helper()
	model, extraCommon := "claude-opus-4-8", ""
	switch prov {
	case provCodex:
		model = "gpt-5.3-codex"
	case provMuse:
		model, extraCommon = "muse-spark-1.3", `,"model_provider":"meta"`
	}
	mk := func(event, extra string) fakecore.HookPayload {
		return hook(event, `{"hook_event_name":"`+event+`","session_id":"`+evalSession+`","turn_id":"turn-1","cwd":"/repo","model":"`+model+
			`","permission_mode":"default"`+extraCommon+extra+`}`)
	}
	tool := `,"tool_name":"Bash","tool_use_id":"` + okToolUseID + `"`
	in := `,"tool_input":{"command":"` + sentToolIn + `"}`
	resp := `,"tool_response":{"stdout":"` + sentToolOut + `","output":"` + sentToolOut + `"}`

	// The Stop payload points at a transcript the tool's own adapter reads for
	// usage and thinking. Claude Code's is a conversation transcript, Codex's
	// a rollout; Muse's Stop carries none.
	transcript := ""
	switch prov {
	case provCC:
		transcript = writeFixture(t, "transcript.jsonl",
			`{"timestamp":"2026-10-01T00:00:00.000Z","isSidechain":false,"message":{"model":"`+model+`","content":[{"type":"thinking","thinking":"`+sentThinking+`"},{"type":"text","text":"`+sentReply+`"}],"usage":{"input_tokens":11,"output_tokens":7}}}`+"\n")
	case provCodex:
		transcript = writeFixture(t, "rollout.jsonl",
			`{"timestamp":"2026-10-01T00:00:01.000Z","type":"response_item","payload":{"type":"reasoning","summary":[{"type":"summary_text","text":"`+sentThinking+`"}]}}`+"\n"+
				`{"timestamp":"2026-10-01T00:00:02.000Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":100,"output_tokens":30,"total_tokens":130},"last_token_usage":{"input_tokens":100,"output_tokens":30,"total_tokens":130}}}}`+"\n")
	}
	tp := `,"transcript_path":null`
	if transcript != "" {
		tp = fmt.Sprintf(`,"transcript_path":%q`, transcript)
	}

	ps := []fakecore.HookPayload{
		mk("SessionStart", `,"source":"startup"`),
		mk("UserPromptSubmit", `,"prompt":"`+sentPrompt+`"`),
		mk("PreToolUse", tool+in),
		mk("PermissionRequest", `,"tool_name":"Bash"`+in),
		mk("PostToolUse", tool+in+resp),
		mk("SubagentStart", `,"agent_id":"agent-1","agent_type":"general"`),
	}
	// The compaction hooks carry the trigger each tool names: Claude Code and
	// Codex say auto, Muse says soft (captured live on Muse 1.4.2).
	trigger := "auto"
	if prov == provMuse {
		trigger = "soft"
	}
	ps = append(ps,
		mk("PreCompact", `,"trigger":"`+trigger+`"`),
		mk("PostCompact", `,"trigger":"`+trigger+`"`),
	)
	if prov == provCC {
		ps = append(ps,
			mk("PermissionDenied", `,"tool_name":"Bash","tool_use_id":"toolu_denied","tool_input":{"command":"x"},"reason":"policy"`),
			mk("Setup", `,"trigger":"init"`),
			mk("InstructionsLoaded", `,"file_path":"/repo/CLAUDE.md","memory_type":"Project","load_reason":"session_start"`),
			mk("TaskCreated", `,"task_id":"task-1","task_subject":"subject"`),
			mk("TeammateIdle", `,"teammate_name":"mate"`),
			mk("CwdChanged", `,"old_cwd":"/repo","new_cwd":"/repo/sub"`),
			mk("FileChanged", `,"file_path":"/repo/a.txt","event":"change"`),
		)
	}
	if prov == provCC {
		ps = append(ps,
			mk("Notification", `,"message":"`+sentNotify+`","notification_type":"idle"`),
			mk("PostToolBatch", `,"tool_calls":[]`),
		)
	}
	if prov == provMuse {
		// Muse's captured Notification: a permission_prompt with a title and a
		// message. PostToolBatch is never sent: Muse never fires it.
		ps = append(ps, mk("Notification", `,"notification_type":"permission_prompt","title":"Muse Code needs approval","message":"`+sentNotify+`"`))
	}
	if prov == provCC || prov == provMuse {
		ps = append(ps, mk("StopFailure", `,"error":"overloaded"`))
	}
	if prov == provMuse {
		ps = append(ps, musePreLLMCall("matrix-req"), musePostLLMCall("matrix-req"))
	}
	ps = append(ps,
		mk("Stop", `,"stop_hook_active":false,"last_assistant_message":"`+sentReply+`"`+tp),
		mk("SessionEnd", `,"reason":"other"`+tp),
	)
	return ps
}

func writeFixture(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func driveHooks(t *testing.T, prov string, capture bool) []wire {
	t.Helper()
	flag := "0"
	if capture {
		flag = "1"
	}
	run := runScenario(t, fakecore.Scenario{
		Name: "activity-matrix-" + prov, Provider: prov, Payloads: hookPayloads(t, prov),
		Posture: fakecore.Posture{ContentCapture: flag}, Provenance: authoredProvenance,
		Setup: museJournalSetup(prov),
	})
	return decodeWires(run.Inbox)
}

// emitToWire sends events through the real client at the given capture posture
// and returns what the fake core accepted.
func emitToWire(t *testing.T, capture bool, events []client.DevEvent) []wire {
	t.Helper()
	fc := fakecore.New(t, fakecore.Script{})
	c, err := client.New(client.Config{
		BaseURL: fc.URL(), APIKey: fakecore.APIKey(),
		WorkloadPrivateKey: fakecore.WorkloadPrivateKey(), ContentCaptureEnabled: capture,
	})
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}
	for _, ev := range events {
		if _, err := c.Emit(context.Background(), ev); err != nil {
			t.Fatalf("Emit %s: %v", ev.EventType, err)
		}
	}
	return decodeWires(fc.Inbox())
}

const matrixLaneSession = "matrix-lane-s1"

// driveLane runs one model-call record through the lane that provider has.
// Claude Code's is the transport relay, whose emitter carries the request and
// response bodies. Muse and Codex have the telemetry lane, driven with the
// export each tool produces.
func driveLane(t *testing.T, prov string, capture bool) []wire {
	t.Helper()
	switch prov {
	case provCC:
		var got []client.DevEvent
		em := &gatewayemit.Emitter{
			Elected:  func() bool { return true },
			Lane:     gatewayemit.LaneProxy,
			Deliver:  func(_ context.Context, ev client.DevEvent) bool { got = append(got, ev); return true },
			DID:      func() string { return routeDID },
			Warn:     func(string, ...any) {},
			RunStore: obgit.RunStore{Dir: t.TempDir()},
		}
		em.Emit(context.Background(), gateway.Captured{
			HTTPMethod: "POST", HTTPURL: "https://api.anthropic.com/v1/messages", HTTPStatus: 200,
			RequestHeaders:  map[string]string{"Anthropic-Version": "2023-06-01", "X-Claude-Code-Session-Id": matrixLaneSession},
			ResponseHeaders: map[string]string{"Request-Id": "req_matrix"},
			RequestBody:     `{"model":"claude-opus-4-8","messages":[{"role":"user","content":"` + sentLaneReq + `"}]}`,
			ResponseBody:    `{"type":"message","role":"assistant","content":[{"type":"text","text":"` + sentLaneResp + `"}]}`,
		})
		if len(got) == 0 {
			t.Fatal("the proxy-lane emitter delivered nothing")
		}
		return emitToWire(t, capture, got)
	case provCodex, provMuse:
		delivered := &deliveredEvents{}
		emitters := routedEmitters(delivered, provider.Name(prov))
		rec, err := telemetry.New(telemetry.Config{Addr: "127.0.0.1:0"}, telemetry.WithEmitter(newTelemetryRouter(emitters)))
		if err != nil {
			t.Fatal(err)
		}
		export := museFixtureJSON(t)
		if prov == provCodex {
			// Codex's own export (codex.sse_event, response.completed), mapped by the
			// daemon's mapper and enriched from a rollout the way the daemon does.
			emitters[prov].Mapper = codexTelemetryMapper(telemetryemit.Policy{Elected: func() bool { return true }})
			codexLaneContent(t, emitters[prov], capture)
			export = []byte(otlpCodexSSECompleted(matrixLaneSession, codexLaneCallAt, 1200, 30, 1100))
		}
		if prov == provMuse {
			museLaneContent(t, emitters[prov], capture)
		}
		if err := rec.ConsumeLogsJSON(context.Background(), export); err != nil {
			t.Fatal(err)
		}
		if prov != provCC {
			emitters[prov].Wait()
		}
		return emitToWire(t, capture, delivered.evts)
	}
	t.Fatalf("no lane driver for %q", prov)
	return nil
}

// driveCommit runs the post-commit sink with the environment markers the tool
// leaves in a commit's shell, and returns what the sink spooled for the tool,
// sent through the client. The sink is what decides whether a commit is an
// agent's, so it is driven rather than building the event by hand.
func driveCommit(t *testing.T, prov string, capture bool) []wire {
	t.Helper()
	isolateHomeUnbound(t)
	seedCredentials(t, prov)
	t.Setenv(devconfig.EnvSpoolDir, "")
	t.Setenv(devconfig.EnvSpoolRoot, t.TempDir())
	t.Setenv(obgit.EnvSessionDir, t.TempDir())
	t.Setenv(devconfig.EnvHaltDir, t.TempDir())

	env := map[string]string{}
	switch prov {
	case provCC:
		env["CLAUDECODE"] = "1"
	case provCodex:
		env[obgit.EnvCodexThreadID] = "thread-matrix"
	case provMuse:
		env["MUSE_TOOL_USE_ID"] = "toolu_matrix"
	}
	var logs strings.Builder
	sink := newCommitSink(func(k string) string { return env[k] }, log.New(&logs, "", 0))
	sink(obgit.CommitFacts{SHA: "matrixsha", Tree: "matrixtree", Repo: "acme/app", Branch: "main"},
		[]obgit.ResolvedSession{{ID: "matrix-commit-s1", Tier: obgit.TierRegistry, Tool: prov}})

	var events []client.DevEvent
	dir := providers.SpoolDirFor(prov)
	files, _ := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	sort.Strings(files)
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
			var ev client.DevEvent
			if line != "" && json.Unmarshal([]byte(line), &ev) == nil {
				events = append(events, ev)
			}
		}
	}
	if len(events) == 0 {
		return nil // the sink skipped the commit; its reason is in logs
	}
	return emitToWire(t, capture, events)
}

// collect returns every wire body one provider produces across all three
// drivers, at one capture posture. Each driver runs in its own subtest so the
// environment it sets is restored before the next.
func collect(t *testing.T, prov string, capture bool) []wire {
	t.Helper()
	var all []wire
	for _, d := range []struct {
		name string
		fn   func(*testing.T, string, bool) []wire
	}{{"hooks", driveHooks}, {"lane", driveLane}, {"commit", driveCommit}} {
		t.Run(d.name, func(t *testing.T) { all = append(all, d.fn(t, prov, capture)...) })
	}
	return all
}

// ---- tests -----------------------------------------------------------------

func TestActivityMatrix(t *testing.T) {
	rows := matrixRows()
	for _, prov := range matrixProviders {
		t.Run(prov, func(t *testing.T) {
			wires := collect(t, prov, true)
			if len(wires) == 0 {
				t.Fatal("nothing reached the fake; every row below would be vacuous")
			}
			for _, r := range rows {
				c, ok := r.by[prov]
				if !ok {
					t.Errorf("%s/%s: row has no entry for this provider", prov, r.class)
					continue
				}
				got := r.check(wires)
				switch c.kind {
				case "produced":
					if !got {
						t.Errorf("%s/%s: not produced (want: %s)", prov, r.class, r.want)
					}
				case "na":
					if strings.TrimSpace(c.reason) == "" {
						t.Errorf("%s/%s: N/A without a reason", prov, r.class)
					}
				case "pending":
					if c.phase == 0 || strings.TrimSpace(c.reason) == "" {
						t.Errorf("%s/%s: pending row names no owning phase or no missing piece", prov, r.class)
					} else if got {
						t.Errorf("%s/%s: now produced; flip the row to produced() (was owned by phase %d: %s)",
							prov, r.class, c.phase, c.reason)
					} else {
						t.Logf("RED %s/%s: not produced (want: %s); owner phase %d: %s", prov, r.class, r.want, c.phase, c.reason)
					}
				default:
					t.Errorf("%s/%s: unknown cell kind %q", prov, r.class, c.kind)
				}
			}
		})
	}
}

// TestActivityMatrixPredicatesRejectAnEmptyWire proves each predicate can
// fail: a predicate that is true of nothing would make every produced() cell
// pass vacuously.
func TestActivityMatrixPredicatesRejectAnEmptyWire(t *testing.T) {
	for _, r := range matrixRows() {
		if r.check(nil) {
			t.Errorf("%s: predicate is true of an empty wire", r.class)
		}
		if r.check([]wire{{EventType: "SignalReceived", SignalName: "unrelated"}}) {
			t.Errorf("%s: predicate is true of an unrelated signal", r.class)
		}
	}
}

// TestActivityMatrixIsComplete: every provider has a cell for every row, every
// N/A carries a reason, and the table has no duplicate class.
func TestActivityMatrixIsComplete(t *testing.T) {
	seen := map[string]bool{}
	for _, r := range matrixRows() {
		if seen[r.class] {
			t.Errorf("duplicate row %q", r.class)
		}
		seen[r.class] = true
		for _, prov := range matrixProviders {
			c, ok := r.by[prov]
			if !ok {
				t.Errorf("%s/%s: no cell", prov, r.class)
				continue
			}
			if c.kind == "na" && strings.TrimSpace(c.reason) == "" {
				t.Errorf("%s/%s: N/A without a reason", prov, r.class)
			}
		}
	}
}

// TestActivityMatrixCaptureOffSentinel reruns every provider's scenarios with
// content_capture off and asserts nothing the developer typed or the model
// said is on any wire body, neither as a fixture string nor under a content
// key. It must stay green after every phase: the matrix grows what is
// captured, and this is what says capture-off still captures nothing.
func TestActivityMatrixCaptureOffSentinel(t *testing.T) {
	for _, prov := range matrixProviders {
		t.Run(prov, func(t *testing.T) {
			wires := collect(t, prov, false)
			if len(wires) == 0 {
				t.Fatal("nothing reached the fake; the absences below would prove nothing")
			}
			for i, w := range wires {
				for _, s := range matrixSentinels {
					if strings.Contains(w.Raw, s) {
						t.Errorf("%s: wire #%d (%s %s) carries %q with content_capture off", prov, i, w.EventType, firstNonEmptyStr(w.SignalName, w.ActivityType), s)
					}
				}
				for _, part := range []struct {
					name string
					m    map[string]any
				}{{"activity_input", w.In}, {"activity_output", w.Out}, {"signal_args", w.Sig}, {"metadata", w.Meta}} {
					if k := contentKeyIn(part.m); k != "" {
						t.Errorf("%s: wire #%d (%s %s) carries content key %s.%s with content_capture off", prov, i, w.EventType, firstNonEmptyStr(w.SignalName, w.ActivityType), part.name, k)
					}
				}
			}
			// Capture off must not mean nothing was delivered at all.
			if !hasSignal("prompt_submitted")(wires) {
				t.Error("no prompt_submitted row with capture off; the gate is on the words, not on the event")
			}
		})
	}
}

// contentKeyIn returns a content key present anywhere in m, recursively.
func contentKeyIn(m map[string]any) string {
	for k, v := range m {
		if matrixContentKeys[k] {
			return k
		}
		// tokens/usage hold counts: their `output` is a number of output tokens.
		if sub, ok := v.(map[string]any); ok && k != "tokens" && k != "usage" {
			if found := contentKeyIn(sub); found != "" {
				return k + "." + found
			}
		}
	}
	return ""
}

// museJournalSetup gives a Muse scenario the session journal its Stop reads the
// turn's reasoning summaries from: the response the driver's PostLLMCall names
// (resp-eval-1), with sentThinking as its summary. Other providers need none.
func museJournalSetup(prov string) func(fakecore.TB, *fakecore.Server) {
	if prov != provMuse {
		return nil
	}
	return func(t fakecore.TB, _ *fakecore.Server) {
		raw, err := os.ReadFile(filepath.Join("..", "..", "internal", "adapters", "muse", "testdata", "session-jsonl-content.jsonl"))
		if err != nil {
			t.Fatalf("muse journal fixture: %v", err)
		}
		body := strings.NewReplacer("sess-c1", evalSession, "resp_a", "resp-eval-1", "Neutral summary one.", sentThinking).Replace(string(raw))
		now := time.Now()
		path := filepath.Join(os.Getenv("HOME"), ".local", "share", "muse", "sessions",
			now.Format("2006"), now.Format("01"), now.Format("02"), evalSession, "session.jsonl")
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatalf("muse journal dir: %v", err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("muse journal: %v", err)
		}
	}
}

// museLaneContent gives the Muse lane driver what its enrichment reads for the
// fixture export's first model call: the request its PostLLMCall hook would
// have stashed, and the reply in the session journal. The enricher is wired
// the way the daemon wires it, with capture as the matrix posture.
func museLaneContent(t *testing.T, em *telemetryemit.Emitter, capture bool) {
	t.Helper()
	const sess, resp = "feed0001-0000-4000-8000-000000000001", "resp_scrubbedfeed0002"
	raw, err := os.ReadFile(filepath.Join("..", "..", "internal", "adapters", "muse", "testdata", "session-jsonl-content.jsonl"))
	if err != nil {
		t.Fatalf("muse journal fixture: %v", err)
	}
	body := strings.NewReplacer("sess-c1", sess, "resp_b", resp, "Neutral final reply.", sentLaneResp).Replace(string(raw))
	root := t.TempDir()
	now := time.Now()
	path := filepath.Join(root, now.Format("2006"), now.Format("01"), now.Format("02"), sess, "session.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	spool := t.TempDir()
	if err := muse.PutRequest(spool, resp, muse.RequestEntry{SessionID: sess,
		Body: `{"messages":[{"role":"user","content":"` + sentLaneReq + `"}]}`}, true); err != nil {
		t.Fatal(err)
	}
	em.Enrich = (&museContentSource{SessionsRoot: root, SpoolDir: spool, Capture: func() bool { return capture },
		Poll: 10 * time.Millisecond}).Enricher()
}

// codexLaneCallAt is the time of the rollout fixture's third call, the one the
// Codex lane driver exports.
var codexLaneCallAt = time.Date(2026, 10, 1, 9, 9, 11, 159e6, time.UTC)

// codexLaneContent gives the Codex lane driver what its enrichment reads for
// the exported call: a rollout, laid out as Codex does, whose third call has
// the matrix's request and reply texts. The enricher is wired the way the
// daemon wires it, with capture as the matrix posture.
func codexLaneContent(t *testing.T, em *telemetryemit.Emitter, capture bool) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "internal", "adapters", "codex", "testdata", "rollout-content.jsonl"))
	if err != nil {
		t.Fatalf("codex rollout fixture: %v", err)
	}
	body := strings.NewReplacer("0000aaaa-0000-4000-8000-000000000001", matrixLaneSession,
		"Neutral user prompt one.", sentLaneReq, "Neutral final reply.", sentLaneResp).Replace(string(raw))
	root := t.TempDir()
	path := filepath.Join(root, "2026", "10", "01", "rollout-2026-10-01T16-08-54-"+matrixLaneSession+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	em.Enrich = (&codexContentSource{SessionsRoot: root, Capture: func() bool { return capture },
		Poll: 10 * time.Millisecond}).Enricher()
}
