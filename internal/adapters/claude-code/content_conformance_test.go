package claudecode

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"log"

	"github.com/openbox-ai/openbox-shift-left/internal/client/fakecore"
	"github.com/openbox-ai/openbox-shift-left/internal/client/workloadauth"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestContentCaptureConformance tool-content conformance suite; executable
// evidence for the content gate on the fields this adapter used to throw away.
func TestContentCaptureConformance(t *testing.T) {
	isolateConfig(t)
	t.Setenv(envAgentID, testAgentID)
	t.Setenv("OPENBOX_SPOOL_DIR", t.TempDir())
	t.Setenv("OPENBOX_SESSION_DIR", t.TempDir())
	t.Setenv(envEnforcementFile, filepath.Join(t.TempDir(), "enf.jsonl"))

	// serveCapturing is a v3-aware fake core (real bootstrap + Keycloak token
	// exchange + evaluate, over fakecore's process-wide identity) that answers
	// every accepted /evaluate request "allow" and records its raw wire body.
	// A hand-rolled single-path memhttptest handler cannot stand in for this
	// once the client speaks the v3 dance: it would answer the bootstrap
	// document and the token exchange with the same canned evaluate verdict,
	// so the client's cold path fails before ever building an evaluate request.
	serveCapturing := func(t *testing.T) func() []string {
		t.Helper()
		srv := fakecore.New(t, fakecore.Script{})
		evalCreds(t, srv.URL())
		return func() []string {
			inbox := srv.Inbox()
			bodies := make([]string, len(inbox))
			for i, r := range inbox {
				bodies[i] = string(r.Raw)
			}
			return bodies
		}
	}

	// `capture` is set after serveCapturing, deliberately: serveCapturing calls
	// evalCreds, which forces content capture OFF.
	observeThenFlush := func(t *testing.T, session, capture, hook, payload string) []string {
		t.Helper()
		getBodies := serveCapturing(t)
		t.Setenv(envContentCapture, capture)
		t.Setenv(envRealtime, "0")
		t.Setenv(envEnforce, "0") // observe path; no gate, no deferred spool
		var out bytes.Buffer
		RunHook(hook, strings.NewReader(payload), &out, log.New(&bytes.Buffer{}, "", 0))
		RunHook("SessionEnd", strings.NewReader(
			`{"hook_event_name":"SessionEnd","session_id":"`+session+`","cwd":"/tmp","reason":"other"}`),
			&out, log.New(&bytes.Buffer{}, "", 0))
		return getBodies()
	}

	activityOutput := func(t *testing.T, bodies []string) (map[string]any, bool) {
		t.Helper()
		for _, b := range bodies {
			var p struct {
				EventType      string         `json:"event_type"`
				ActivityOutput map[string]any `json:"activity_output"`
			}
			if err := json.Unmarshal([]byte(b), &p); err != nil {
				continue
			}
			if p.EventType == "ActivityCompleted" {
				return p.ActivityOutput, true
			}
		}
		return nil, false
	}

	activityInput := func(t *testing.T, bodies []string) (map[string]any, bool) {
		t.Helper()
		for _, b := range bodies {
			var p struct {
				EventType     string         `json:"event_type"`
				ActivityInput map[string]any `json:"activity_input"`
			}
			if err := json.Unmarshal([]byte(b), &p); err != nil {
				continue
			}
			if p.EventType == "ActivityStarted" {
				return p.ActivityInput, true
			}
		}
		return nil, false
	}

	postToolUse := func(session, toolResponse string) string {
		return `{"hook_event_name":"PostToolUse","session_id":"` + session + `","cwd":"/tmp",` +
			`"tool_name":"Bash","tool_use_id":"toolu_cc","tool_input":{"command":"cat config"},` +
			`"tool_response":` + toolResponse + `}`
	}

	t.Run("C32 tool output reaches activity_output.output with capture on", func(t *testing.T) {
		const output = "TOOL-OUTPUT-SENTINEL total 4 drwxr-xr-x"
		bodies := observeThenFlush(t, "cc-on", "1", "PostToolUse",
			postToolUse("cc-on", `{"stdout":"`+output+`","stderr":"","interrupted":false}`))

		out, found := activityOutput(t, bodies)
		if !found {
			t.Fatalf("no ActivityCompleted reached /evaluate at all; bodies=%v", bodies)
		}
		got, _ := out["output"].(string)
		if !strings.Contains(got, output) {
			t.Errorf("activity_output.output = %q, want it to carry the tool's output; "+
				"the field core stores as the row's `output` and runs Guardrails stage 1 over", got)
		}
	})

	t.Run("C33 content_capture:false carries no tool output, but the activity still ships", func(t *testing.T) {
		const canary = "CANARY-TOOL-OUTPUT-must-not-egress"
		bodies := observeThenFlush(t, "cc-off", "0", "PostToolUse",
			postToolUse("cc-off", `{"stdout":"`+canary+`","stderr":"","interrupted":false}`))

		for i, b := range bodies {
			if strings.Contains(b, canary) {
				t.Errorf("tool output egressed with capture OFF in body #%d: %s", i, b)
			}
		}
		// Without this the case would pass for a client that stopped emitting tool
		// results entirely.
		if _, found := activityOutput(t, bodies); !found {
			t.Errorf("no ActivityCompleted with capture off; the gate is on the tool's "+
				"OUTPUT, not on the tool event: %v", bodies)
		}
	})

	t.Run("C34 a secret in tool output never reaches /evaluate", func(t *testing.T) {
		os.Unsetenv(envSecretDetection) // detection default ON
		awsKey := "AKIA" + "IOSFODNN7EXAMPLE"
		bodies := observeThenFlush(t, "cc-secret", "1", "PostToolUse",
			postToolUse("cc-secret", `{"stdout":"AWS_ACCESS_KEY_ID=`+awsKey+`","stderr":"","interrupted":false}`))

		for i, b := range bodies {
			if strings.Contains(b, awsKey) {
				t.Errorf("the raw secret reached /evaluate in body #%d; redaction must run "+
					"BEFORE attachment: %s", i, b)
			}
		}
		out, found := activityOutput(t, bodies)
		if !found {
			t.Fatal("no ActivityCompleted was sent at all; the case proves nothing if the " +
				"output never egressed")
		}
		got, _ := out["output"].(string)
		if !strings.Contains(got, "OPENBOX_REDACTED") {
			t.Errorf("no redaction placeholder in activity_output.output: %q", got)
		}
	})

	t.Run("C35 oversized tool output is capped before signing", func(t *testing.T) {
		const over = 70000 // > maxBodySize (65536)
		bodies := observeThenFlush(t, "cc-cap", "1", "PostToolUse",
			postToolUse("cc-cap", `{"stdout":"`+strings.Repeat("x", over)+`","stderr":"","interrupted":false}`))

		out, found := activityOutput(t, bodies)
		if !found {
			t.Fatalf("no ActivityCompleted reached /evaluate; bodies=%d", len(bodies))
		}
		got, _ := out["output"].(string)
		if got == "" {
			t.Fatal("activity_output.output empty; a capped body must still be sent")
		}
		if len([]rune(got)) > 65536 {
			t.Errorf("activity_output.output is %d runes, want ≤ 65536 (maxBodySize)", len([]rune(got)))
		}
	})

	t.Run("C36 tool input egresses on the observe path under the gate", func(t *testing.T) {
		const cmd = "cat /etc/hosts && echo OBSERVE-INPUT-SENTINEL"
		payload := func(session string) string {
			return `{"hook_event_name":"PreToolUse","session_id":"` + session + `","cwd":"/tmp",` +
				`"tool_name":"Bash","tool_use_id":"toolu_obs","tool_input":{"command":"` + cmd + `"}}`
		}

		on := observeThenFlush(t, "cc-in-on", "1", "PreToolUse", payload("cc-in-on"))
		in, found := activityInput(t, on)
		if !found {
			t.Fatalf("no ActivityStarted reached /evaluate at all; bodies=%v", on)
		}
		got, _ := in["command"].(string)
		if !strings.Contains(got, "OBSERVE-INPUT-SENTINEL") {
			t.Errorf("activity_input.command = %q, want the observe-path command under capture", got)
		}

		off := observeThenFlush(t, "cc-in-off", "0", "PreToolUse", payload("cc-in-off"))
		for i, b := range off {
			if strings.Contains(b, "OBSERVE-INPUT-SENTINEL") {
				t.Errorf("tool input egressed on the observe path with capture OFF in body #%d: %s", i, b)
			}
		}
		if _, found := activityInput(t, off); !found {
			t.Errorf("no ActivityStarted with capture off; the gate is on the INPUT, not on "+
				"the tool event: %v", off)
		}
	})

	t.Run("C37 a failed call's free-text error egresses gated, never as error_type", func(t *testing.T) {
		// Binding the free text as gated content must not widen the enum field; a
		// non-enum value on metadata.error_type would be ungated egress.
		const detail = "ENOENT: no such file /home/dev/.ssh/id_rsa"
		fail := func(session string) string {
			return `{"hook_event_name":"PostToolUseFailure","session_id":"` + session + `","cwd":"/tmp",` +
				`"tool_name":"Read","tool_use_id":"toolu_fail","tool_input":{"file_path":"/home/dev/.ssh/id_rsa"},` +
				`"error":` + jsonQuote(detail) + `}`
		}

		on := observeThenFlush(t, "cc-err-on", "1", "PostToolUseFailure", fail("cc-err-on"))
		out, found := activityOutput(t, on)
		if !found {
			t.Fatalf("no ActivityCompleted for the failed call; bodies=%v", on)
		}
		got, _ := out["output"].(string)
		if !strings.Contains(got, "ENOENT") {
			t.Errorf("activity_output.output = %q, want the tool's error text; a failed "+
				"activity's output IS its error", got)
		}
		for _, b := range on {
			var p struct {
				Metadata map[string]any `json:"metadata"`
			}
			if err := json.Unmarshal([]byte(b), &p); err != nil {
				continue
			}
			if v, present := p.Metadata["error_type"]; present {
				t.Errorf("metadata.error_type = %v; free text must never reach the "+
					"provider-enum field", v)
			}
		}

		off := observeThenFlush(t, "cc-err-off", "0", "PostToolUseFailure", fail("cc-err-off"))
		for i, b := range off {
			if strings.Contains(b, "ENOENT") {
				t.Errorf("the free-text error egressed with capture OFF in body #%d: %s", i, b)
			}
		}
	})

	t.Run("C38 signal free text egresses as both metadata and signal_args, gated", func(t *testing.T) {
		// Inverted at v1.9. It used to assert signal_args stayed empty, because
		// core read any non-empty signal_args as a new user goal and a denial
		// reason landing there would silently replace the developer's prompt as
		// the thing every later turn is scored against. Core's source-and-name
		// gate now stops that, and signal_args is the only field OPA and
		// Guardrails read on a signal — so the free text must reach BOTH
		// destinations, under the same content gate. The capture-OFF half below
		// is unchanged and is the INV-2 assertion.
		for _, tc := range []struct {
			name, hook, session, payload, metaKey, sentinel string
		}{
			{
				name: "PermissionDenied.reason", hook: "PermissionDenied", session: "cc-den",
				payload: `{"hook_event_name":"PermissionDenied","session_id":"cc-den","cwd":"/tmp",` +
					`"tool_name":"Bash","tool_use_id":"toolu_den",` +
					`"reason":"DENIAL-REASON-SENTINEL: destructive command"}`,
				metaKey: "denial_reason", sentinel: "DENIAL-REASON-SENTINEL",
			},
			{
				name: "StopFailure.error_details", hook: "StopFailure", session: "cc-apierr",
				payload: `{"hook_event_name":"StopFailure","session_id":"cc-apierr","cwd":"/tmp",` +
					`"error":"rate_limit","error_details":"ERROR-DETAILS-SENTINEL: retry after 60s"}`,
				metaKey: "error_details", sentinel: "ERROR-DETAILS-SENTINEL",
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				on := observeThenFlush(t, tc.session, "1", tc.hook, tc.payload)

				var inMeta, inArgs bool
				for _, b := range on {
					var p struct {
						SignalName string         `json:"signal_name"`
						SignalArgs map[string]any `json:"signal_args"`
						Metadata   map[string]any `json:"metadata"`
					}
					if err := json.Unmarshal([]byte(b), &p); err != nil || p.SignalName == "" {
						continue
					}
					if v, ok := p.Metadata[tc.metaKey].(string); ok && strings.Contains(v, tc.sentinel) {
						inMeta = true
					}
					if v, ok := p.SignalArgs[tc.metaKey].(string); ok && strings.Contains(v, tc.sentinel) {
						inArgs = true
					}
				}
				if !inMeta {
					t.Errorf("metadata.%s did not carry the free text with capture on; bodies=%v",
						tc.metaKey, on)
				}
				if !inArgs {
					t.Errorf("signal_args.%s did not carry the free text with capture on, so no "+
						"policy engine can match it; bodies=%v", tc.metaKey, on)
				}

				off := observeThenFlush(t, tc.session+"-off", "0", tc.hook,
					strings.ReplaceAll(tc.payload, tc.session, tc.session+"-off"))
				for i, b := range off {
					if strings.Contains(b, tc.sentinel) {
						t.Errorf("free text egressed with capture OFF in body #%d: %s", i, b)
					}
				}
			})
		}
	})

	stopWithThinking := func(t *testing.T, session, thinking string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "transcript.jsonl")
		line := `{"type":"assistant","isSidechain":false,"timestamp":"2026-08-25T09:00:01.000Z",` +
			`"message":{"model":"claude-opus-4-8","content":[{"type":"thinking","thinking":"` +
			thinking + `"},{"type":"text","text":"the visible answer"}],` +
			`"usage":{"input_tokens":100,"output_tokens":10}}}` + "\n"
		if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
			t.Fatalf("write transcript: %v", err)
		}
		return `{"hook_event_name":"Stop","session_id":"` + session + `","cwd":"/tmp",` +
			`"transcript_path":"` + path + `","last_assistant_message":"the visible answer"}`
	}

	t.Run("C40 the turn's thinking reaches activity_output.thinking with capture on", func(t *testing.T) {
		const thinking = "THINKING-SENTINEL the lock must outlive the rename"
		t.Setenv(envFinops, "1") // turn events are finops-gated; the pair is the carrier
		bodies := observeThenFlush(t, "cc-think-on", "1", "Stop",
			stopWithThinking(t, "cc-think-on", thinking))

		out, found := activityOutput(t, bodies)
		if !found {
			t.Fatalf("no ActivityCompleted reached /evaluate at all; bodies=%v", bodies)
		}
		got, _ := out["thinking"].(string)
		if !strings.Contains(got, thinking) {
			t.Errorf("activity_output.thinking = %q, want the turn's thinking block", got)
		}
		for _, b := range bodies {
			var p struct {
				Spans []struct {
					ResponseBody string `json:"response_body"`
				} `json:"spans"`
			}
			if json.Unmarshal([]byte(b), &p) != nil {
				continue
			}
			for _, sp := range p.Spans {
				if strings.Contains(sp.ResponseBody, thinking) {
					t.Errorf("thinking rode the assistant span, where core reads it as the "+
						"turn's REPLY: %s", sp.ResponseBody)
				}
			}
		}
	})

	// C56: a v1.9 metadata-native content key sits BESIDE its signal_detail
	// sibling, and both reach signal_args.
	//
	// The two live on one class each and could plausibly have been implemented
	// by reusing content.signal_detail, which holds one string — so the failure
	// this rules out is the new key silently replacing the old one. Both halves
	// asserted on the outbound bytes; a mapper-level check would not see the
	// projection.
	t.Run("C56 a v1.9 content key sits beside its sibling and both reach signal_args", func(t *testing.T) {
		for _, tc := range []struct {
			name, hook, signalName, session, payload string
			pairs                                    map[string]string // metadata key -> expected text
		}{
			{
				name: "Notification", hook: "Notification", signalName: "notification",
				session: "cc-c56-notif",
				payload: `{"hook_event_name":"Notification","session_id":"cc-c56-notif","cwd":"/tmp",` +
					`"notification_type":"idle_prompt","message":"C56-MESSAGE","title":"C56-TITLE"}`,
				pairs: map[string]string{
					"notification_message": "C56-MESSAGE",
					"notification_title":   "C56-TITLE",
				},
			},
			{
				name: "TaskCreated", hook: "TaskCreated", signalName: "task_created",
				session: "cc-c56-task",
				payload: `{"hook_event_name":"TaskCreated","session_id":"cc-c56-task","cwd":"/tmp",` +
					`"task_id":"tid1","task_subject":"C56-SUBJECT","task_description":"C56-DESCRIPTION"}`,
				pairs: map[string]string{
					"task_subject":     "C56-SUBJECT",
					"task_description": "C56-DESCRIPTION",
				},
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				bodies := observeThenFlush(t, tc.session, "1", tc.hook, tc.payload)

				var seen bool
				for _, b := range bodies {
					var p struct {
						SignalName string         `json:"signal_name"`
						Metadata   map[string]any `json:"metadata"`
						SignalArgs map[string]any `json:"signal_args"`
					}
					if err := json.Unmarshal([]byte(b), &p); err != nil || p.SignalName != tc.signalName {
						continue
					}
					seen = true
					for k, want := range tc.pairs {
						if got, _ := p.Metadata[k].(string); got != want {
							t.Errorf("metadata[%q] = %q, want %q; the two content fields did not coexist",
								k, got, want)
						}
						if got, _ := p.SignalArgs[k].(string); got != want {
							t.Errorf("signal_args[%q] = %q, want %q; a policy cannot match this field",
								k, got, want)
						}
					}
				}
				if !seen {
					t.Fatalf("no %s signal reached /evaluate; bodies=%v", tc.signalName, bodies)
				}
			})
		}
	})

	// C53: the hook lane's turn carries reply_text, and only the hook lane does.
	//
	// Core judges an assistant turn from activity_output.reply_text and refuses
	// any fallback to `content`, so a turn without the key is a turn that is
	// never judged — which every turn this client ever emitted was. The key's
	// PRESENCE is also what costs a judge call, so the negative half is the cost
	// bound, not a tidiness check: a live session ran 137 span-sourced model
	// calls to 2 hook-sourced turns.
	t.Run("C53 the hook lane's turn carries reply_text, gated, and the span lane does not", func(t *testing.T) {
		const answer = "REPLY-TEXT-SENTINEL: the spool lock now outlives the rename"
		t.Setenv(envFinops, "1") // turn events are finops-gated; the pair is the carrier

		stopWithReply := func(t *testing.T, session, reply string) string {
			t.Helper()
			path := filepath.Join(t.TempDir(), "transcript.jsonl")
			line := `{"type":"assistant","isSidechain":false,"timestamp":"2026-09-08T09:00:01.000Z",` +
				`"message":{"model":"claude-opus-5","content":[{"type":"text","text":"` + reply + `"}],` +
				`"usage":{"input_tokens":100,"output_tokens":10}}}` + "\n"
			if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
				t.Fatalf("write transcript: %v", err)
			}
			return `{"hook_event_name":"Stop","session_id":"` + session + `","cwd":"/tmp",` +
				`"transcript_path":"` + path + `","last_assistant_message":"` + reply + `"}`
		}

		on := observeThenFlush(t, "cc-reply-on", "1", "Stop", stopWithReply(t, "cc-reply-on", answer))
		out, found := activityOutput(t, on)
		if !found {
			t.Fatalf("no ActivityCompleted reached /evaluate at all; bodies=%v", on)
		}
		got, _ := out["reply_text"].(string)
		if !strings.Contains(got, answer) {
			t.Errorf("activity_output.reply_text = %q, want the reassembled reply; without it "+
				"core never judges an assistant turn", got)
		}
		// Additive: `content` is the surface OPA reads on an activity_output today.
		if c, _ := out["content"].(string); !strings.Contains(c, answer) {
			t.Errorf("activity_output.content = %q, want the reply unchanged alongside reply_text", c)
		}

		off := observeThenFlush(t, "cc-reply-off", "0", "Stop", stopWithReply(t, "cc-reply-off", answer))
		for i, b := range off {
			if strings.Contains(b, answer) {
				t.Errorf("the reply egressed with capture OFF in body #%d: %s", i, b)
			}
			if strings.Contains(b, "reply_text") {
				t.Errorf("the reply_text key survived the content gate in body #%d: %s", i, b)
			}
		}
	})

	t.Run("C41 content_capture:false carries no thinking, but the turn still ships", func(t *testing.T) {
		const canary = "CANARY-THINKING-must-not-egress"
		t.Setenv(envFinops, "1")
		bodies := observeThenFlush(t, "cc-think-off", "0", "Stop",
			stopWithThinking(t, "cc-think-off", canary))

		for i, b := range bodies {
			if strings.Contains(b, canary) {
				t.Errorf("thinking egressed with capture OFF in body #%d: %s", i, b)
			}
		}
		// Without this the case would pass for a client that stopped emitting turns
		// entirely; and the token numbers are the reason turns exist.
		out, found := activityOutput(t, bodies)
		if !found {
			t.Fatalf("no ActivityCompleted with capture off; the gate is on the turn's "+
				"THINKING, not on the turn event: %v", bodies)
		}
		if _, has := out["thinking"]; has {
			t.Errorf("activity_output carries a thinking key with capture off: %v", out)
		}
		if _, has := out["usage"]; !has {
			t.Errorf("the turn's token usage went missing with capture off: %v", out)
		}
	})

	// The secret absent AND the placeholder present, because absence alone also
	// passes for a client that stopped sending prompts; and the surrounding text
	// intact, because "redacted" must be distinguishable from "dropped".
	t.Run("C42 the prompt is redacted before egress", func(t *testing.T) {
		const secret = "AKIAIOSFODNN7EXAMPLE"
		bodies := observeThenFlush(t, "cc-prompt-redact", "1", "UserPromptSubmit",
			`{"hook_event_name":"UserPromptSubmit","session_id":"cc-prompt-redact","cwd":"/tmp",`+
				`"prompt":`+jsonQuote("deploy with key "+secret)+`}`)

		joined := strings.Join(bodies, "\n")
		if joined == "" {
			t.Fatal("nothing reached /evaluate; the case proves nothing")
		}
		if strings.Contains(joined, secret) {
			t.Errorf("the prompt's credential reached the wire unredacted:\n%s", joined)
		}
		if !strings.Contains(joined, "OPENBOX_REDACTED") {
			t.Errorf("no redaction placeholder on the wire; the prompt was never scanned:\n%s", joined)
		}
		if !strings.Contains(joined, "deploy with key") {
			t.Errorf("the prompt's non-secret text did not egress, so this proves nothing about redaction:\n%s", joined)
		}
	})

	// C43-C49: the seven new v1.8 content keys, C32/C33-style: present under
	// metadata.<key> with capture on; absent with capture off while the
	// signal still ships; a planted secret never appears; an oversized body
	// is capped at maxBodySize. C50 is deliberately unused (retired when
	// UserPromptExpansion became structural-only, D1) -- the gap is left so a
	// future reader does not "fix" the numbering and lose that history.
	type newContentKeyCase struct {
		label        string // "C43 requested_tool_input", etc.
		hook         string
		signalName   string
		metaKey      string
		buildPayload func(session, text string) string
	}
	newContentKeyCases := []newContentKeyCase{
		{
			label: "C43 requested_tool_input", hook: "PermissionRequest", signalName: "permission_request",
			metaKey: "requested_tool_input",
			buildPayload: func(session, text string) string {
				return `{"hook_event_name":"PermissionRequest","session_id":"` + session + `","cwd":"/tmp",` +
					`"tool_name":"Bash","permission_mode":"default","tool_input":{"command":` + jsonQuote(text) + `}}`
			},
		},
		{
			label: "C44 notification_message", hook: "Notification", signalName: "notification",
			metaKey: "notification_message",
			buildPayload: func(session, text string) string {
				return `{"hook_event_name":"Notification","session_id":"` + session + `","cwd":"/tmp",` +
					`"notification_type":"idle_prompt","message":` + jsonQuote(text) + `}`
			},
		},
		{
			label: "C45 task_subject", hook: "TaskCreated", signalName: "task_created",
			metaKey: "task_subject",
			buildPayload: func(session, text string) string {
				return `{"hook_event_name":"TaskCreated","session_id":"` + session + `","cwd":"/tmp",` +
					`"task_id":"tid1","task_subject":` + jsonQuote(text) + `}`
			},
		},
		{
			label: "C46 compact_instructions", hook: "PreCompact", signalName: "pre_compact",
			metaKey: "compact_instructions",
			buildPayload: func(session, text string) string {
				return `{"hook_event_name":"PreCompact","session_id":"` + session + `","cwd":"/tmp",` +
					`"trigger":"manual","custom_instructions":` + jsonQuote(text) + `}`
			},
		},
		{
			label: "C47 compact_summary", hook: "PostCompact", signalName: "post_compact",
			metaKey: "compact_summary",
			buildPayload: func(session, text string) string {
				return `{"hook_event_name":"PostCompact","session_id":"` + session + `","cwd":"/tmp",` +
					`"trigger":"auto","compact_summary":` + jsonQuote(text) + `}`
			},
		},
		{
			label: "C48 elicitation_message", hook: "Elicitation", signalName: "elicitation",
			metaKey: "elicitation_message",
			buildPayload: func(session, text string) string {
				return `{"hook_event_name":"Elicitation","session_id":"` + session + `","cwd":"/tmp",` +
					`"mcp_server_name":"srv1","mode":"form","message":` + jsonQuote(text) + `}`
			},
		},
		{
			label: "C49 elicitation_response", hook: "ElicitationResult", signalName: "elicitation_result",
			metaKey: "elicitation_response",
			buildPayload: func(session, text string) string {
				return `{"hook_event_name":"ElicitationResult","session_id":"` + session + `","cwd":"/tmp",` +
					`"mcp_server_name":"srv1","action":"accept","mode":"form","content":{"field":` + jsonQuote(text) + `}}`
			},
		},
	}
	if len(newContentKeyCases) != 7 {
		t.Fatalf("newContentKeyCases has %d entries, want 7 (C43-C49)", len(newContentKeyCases))
	}

	// C54-C55: the two v1.9 metadata-native content keys. Same four properties
	// as C43-C49, so they ride the same driver; kept in their own table so the
	// count above stays a statement about v1.8 rather than a running total.
	//
	// What makes them a different KIND of key: each shares its class with a
	// signalDetailKeyFor sibling that already owns content.signal_detail, so
	// these have no signalDetailKeyFor entry at all and contentMetadataKeys is
	// their only gate.
	v19ContentKeyCases := []newContentKeyCase{
		{
			label: "C54 notification_title", hook: "Notification", signalName: "notification",
			metaKey: "notification_title",
			buildPayload: func(session, text string) string {
				return `{"hook_event_name":"Notification","session_id":"` + session + `","cwd":"/tmp",` +
					`"notification_type":"idle_prompt","message":"please respond","title":` + jsonQuote(text) + `}`
			},
		},
		{
			label: "C55 task_description", hook: "TaskCreated", signalName: "task_created",
			metaKey: "task_description",
			buildPayload: func(session, text string) string {
				return `{"hook_event_name":"TaskCreated","session_id":"` + session + `","cwd":"/tmp",` +
					`"task_id":"tid1","task_subject":"investigate the flake","task_description":` + jsonQuote(text) + `}`
			},
		},
	}
	if len(v19ContentKeyCases) != 2 {
		t.Fatalf("v19ContentKeyCases has %d entries, want 2 (C54-C55)", len(v19ContentKeyCases))
	}
	newContentKeyCases = append(newContentKeyCases, v19ContentKeyCases...)

	signalMetadata := func(t *testing.T, bodies []string, signalName string) (map[string]any, bool) {
		t.Helper()
		for _, b := range bodies {
			var p struct {
				SignalName string         `json:"signal_name"`
				Metadata   map[string]any `json:"metadata"`
			}
			if err := json.Unmarshal([]byte(b), &p); err != nil || p.SignalName != signalName {
				continue
			}
			return p.Metadata, true
		}
		return nil, false
	}

	for _, tc := range newContentKeyCases {
		t.Run(tc.label, func(t *testing.T) {
			t.Run("present with capture on", func(t *testing.T) {
				const sentinel = "SENTINEL-CONTENT-present"
				session := "cc-" + tc.hook + "-on"
				bodies := observeThenFlush(t, session, "1", tc.hook, tc.buildPayload(session, sentinel))
				meta, found := signalMetadata(t, bodies, tc.signalName)
				if !found {
					t.Fatalf("no %s signal reached /evaluate; bodies=%v", tc.signalName, bodies)
				}
				got, _ := meta[tc.metaKey].(string)
				if !strings.Contains(got, sentinel) {
					t.Errorf("metadata[%q] = %q, want it to carry the sentinel text", tc.metaKey, got)
				}
			})

			t.Run("absent with capture off, signal still ships", func(t *testing.T) {
				const canary = "CANARY-CONTENT-must-not-egress"
				session := "cc-" + tc.hook + "-off"
				bodies := observeThenFlush(t, session, "0", tc.hook, tc.buildPayload(session, canary))
				for i, b := range bodies {
					if strings.Contains(b, canary) {
						t.Errorf("content egressed with capture OFF in body #%d: %s", i, b)
					}
				}
				if _, found := signalMetadata(t, bodies, tc.signalName); !found {
					t.Errorf("no %s signal shipped with capture off; the gate is on the CONTENT, "+
						"not on the signal: %v", tc.signalName, bodies)
				}
			})

			t.Run("a planted secret never appears", func(t *testing.T) {
				os.Unsetenv(envSecretDetection) // detection default ON
				awsKey := "AKIA" + "IOSFODNN7EXAMPLE"
				session := "cc-" + tc.hook + "-secret"
				bodies := observeThenFlush(t, session, "1", tc.hook,
					tc.buildPayload(session, "leaked key "+awsKey))
				for i, b := range bodies {
					if strings.Contains(b, awsKey) {
						t.Errorf("the raw secret reached /evaluate in body #%d; redaction must run "+
							"BEFORE attachment: %s", i, b)
					}
				}
				meta, found := signalMetadata(t, bodies, tc.signalName)
				if !found {
					t.Fatalf("no %s signal reached /evaluate; the case proves nothing", tc.signalName)
				}
				got, _ := meta[tc.metaKey].(string)
				if !strings.Contains(got, "OPENBOX_REDACTED") {
					t.Errorf("no redaction placeholder in metadata[%q]: %q", tc.metaKey, got)
				}
			})

			t.Run("an oversized body is capped at maxBodySize", func(t *testing.T) {
				const over = 70000 // > maxBodySize (65536)
				session := "cc-" + tc.hook + "-cap"
				bodies := observeThenFlush(t, session, "1", tc.hook,
					tc.buildPayload(session, strings.Repeat("x", over)))
				meta, found := signalMetadata(t, bodies, tc.signalName)
				if !found {
					t.Fatalf("no %s signal reached /evaluate; bodies=%d", tc.signalName, len(bodies))
				}
				got, _ := meta[tc.metaKey].(string)
				if got == "" {
					t.Fatal("metadata value empty; a capped body must still be sent")
				}
				if len([]rune(got)) > 65536 {
					t.Errorf("metadata[%q] is %d runes, want <= 65536 (maxBodySize)", tc.metaKey, len([]rune(got)))
				}
			})
		})
	}

	// C51: the structural-only guarantee must search the WHOLE payload, not
	// one key. With capture on and a redactor installed, a MessageDisplay
	// payload carrying a delta and a PostToolBatch payload carrying
	// tool_calls[].tool_response produce bytes containing neither string
	// anywhere -- D1/A1's guarantee is that these classes carry no content at
	// all, structural or otherwise.
	t.Run("C51 MessageDisplay and PostToolBatch never carry their raw text, anywhere in the payload", func(t *testing.T) {
		const deltaSentinel = "DELTA-SENTINEL-must-never-appear"
		mdSession := "cc-c51-md"
		mdBodies := observeThenFlush(t, mdSession, "1", "MessageDisplay",
			`{"hook_event_name":"MessageDisplay","session_id":"`+mdSession+`","cwd":"/tmp",`+
				`"turn_id":"t1","message_id":"m1","delta":`+jsonQuote(deltaSentinel)+`}`)
		for i, b := range mdBodies {
			if strings.Contains(b, deltaSentinel) {
				t.Errorf("MessageDisplay: delta text found anywhere in body #%d: %s", i, b)
			}
		}
		if _, found := signalMetadata(t, mdBodies, "message_display"); !found {
			t.Errorf("no message_display signal shipped; the case proves nothing: %v", mdBodies)
		}

		const responseSentinel = "TOOL-RESPONSE-SENTINEL-must-never-appear"
		batchSession := "cc-c51-batch"
		batchBodies := observeThenFlush(t, batchSession, "1", "PostToolBatch",
			`{"hook_event_name":"PostToolBatch","session_id":"`+batchSession+`","cwd":"/tmp",`+
				`"tool_calls":[{"tool_use_id":"tu1","tool_response":`+jsonQuote(responseSentinel)+`}]}`)
		for i, b := range batchBodies {
			if strings.Contains(b, responseSentinel) {
				t.Errorf("PostToolBatch: tool_response text found anywhere in body #%d: %s", i, b)
			}
		}
		if _, found := signalMetadata(t, batchBodies, "post_tool_batch"); !found {
			t.Errorf("no post_tool_batch signal shipped; the case proves nothing: %v", batchBodies)
		}
	})

	// C52: every new class carries signal_args and still carries NO activity_id,
	// asserted on the actual bytes that reach /evaluate (CLAUDE.md: asserting a
	// struct is not asserting the wire) -- all 21 v1.8 classes, driven through
	// RunHook.
	//
	// Half of this case inverted at v1.9 and half did not, and the split is the
	// point. signal_args is now required: it is the only field a policy engine
	// reads on a signal, and a class carrying none is unenforceable. activity_id
	// is still forbidden: a signal that acquired one would break "every activity
	// that ran carries exactly two rows", which is what makes SignalReceived
	// legitimately unpaired. The alternative of putting the payload in
	// activity_input was rejected for exactly that reason.
	t.Run("C52 every new class carries signal_args and no activity_id", func(t *testing.T) {
		for _, sc := range signalCases() {
			t.Run(sc.name, func(t *testing.T) {
				session := "cc-c52-" + sc.name
				sc.ev.SessionID = session
				body, err := json.Marshal(sc.ev)
				if err != nil {
					t.Fatalf("marshal HookEvent: %v", err)
				}
				bodies := observeThenFlush(t, session, "1", string(sc.hook), string(body))
				var found bool
				for _, b := range bodies {
					var p struct {
						SignalName string         `json:"signal_name"`
						SignalArgs map[string]any `json:"signal_args"`
						ActivityID string         `json:"activity_id"`
						Metadata   map[string]any `json:"metadata"`
					}
					if err := json.Unmarshal([]byte(b), &p); err != nil || p.SignalName == "" {
						continue
					}
					found = true
					if len(p.SignalArgs) == 0 {
						t.Errorf("signal_args absent: %s is invisible to every policy engine", p.SignalName)
					}
					// Projected, not moved: metadata keeps every projected key,
					// because the SQL forensics path reads metadata.
					for k := range p.SignalArgs {
						if _, kept := p.Metadata[k]; !kept {
							t.Errorf("signal_args[%q] is not in metadata; the projection moved a key "+
								"instead of copying it", k)
						}
					}
					if p.ActivityID != "" {
						t.Errorf("activity_id present: %q; a signal must stay unpaired", p.ActivityID)
					}
					// On an UNGATED core, stringifySignalArgs prefers these five
					// key names and falls back to raw JSON otherwise -- so any
					// projected signal overwrites the goal there, but one of
					// these makes the wrong goal read as prose, which is the
					// version nobody notices. Asserted on the wire, where the
					// mapper-level twin cannot see the projection.
					for _, gk := range coreGoalKeys {
						if v, present := p.SignalArgs[gk]; present {
							t.Errorf("signal_args carries %q = %v; rename it to something structural",
								gk, v)
						}
					}
				}
				if !found {
					t.Fatalf("no SignalReceived event reached /evaluate; bodies=%v", bodies)
				}
			})
		}
	})
}

// TestContentCaptureCredentialCoverage c39 is a detection-coverage case, not
// an ordering case, and it is separate from C34 deliberately.
//   - **Unlabelled hex.** A token with no recognized key name is redacted only
//     if
func TestContentCaptureCredentialCoverage(t *testing.T) {
	isolateConfig(t)
	t.Setenv(envAgentID, testAgentID)
	t.Setenv("OPENBOX_SPOOL_DIR", t.TempDir())
	t.Setenv("OPENBOX_SESSION_DIR", t.TempDir())
	t.Setenv(envEnforcementFile, filepath.Join(t.TempDir(), "enf.jsonl"))

	// The workload-identity fixtures below are generated at runtime, never
	// literals: a committed RSA key or JWT is a secret either way, and a
	// literal secret-shaped string here would be silently rewritten to
	// ${OPENBOX_REDACTED_*} by the local pre-commit hook.
	workloadKey, err := workloadauth.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	workloadKeyB64, err := workloadauth.EncodePrivateKey(workloadKey)
	if err != nil {
		t.Fatal(err)
	}
	workloadKeyDER, err := base64.StdEncoding.DecodeString(workloadKeyB64)
	if err != nil {
		t.Fatal(err)
	}
	workloadKeyPEM := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: workloadKeyDER}))
	workloadToken, err := workloadauth.BuildAssertion(workloadKey, &workloadauth.BootstrapDocument{
		TokenEndpoint: "https://example.test/realms/fake/protocol/openid-connect/token",
		ClientID:      "test-client",
		Kid:           "test-kid",
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name, line, secret string
		caught             bool
		by                 string
	}{
		{
			name:   "obx api key",
			line:   "OPENBOX_API_KEY=obx_" + strings.Repeat("k", 40),
			secret: "obx_" + strings.Repeat("k", 40),
			caught: true, by: "secret_assignment (keyword api_key)",
		},
		{
			name:   "agent signing key; no private_key keyword, so entropy carries it alone",
			line:   "OPENBOX_AGENT_PRIVATE_KEY=" + "aB3xQ9vK2mZ7pL4wR8tY6nH1jF5sD0gC" + "uE7iO2yA4k=",
			secret: "aB3xQ9vK2mZ7pL4wR8tY6nH1jF5sD0gC" + "uE7iO2yA4k=",
			caught: true, by: "entropy fallback (base64 clears 4.5 bits/char)",
		},
		{
			name:   "hex token WITH a recognized keyword",
			line:   "API_KEY=" + strings.Repeat("a1b2c3d4", 8),
			secret: strings.Repeat("a1b2c3d4", 8),
			caught: true, by: "secret_assignment; charset-agnostic, so hex is fine here",
		},
		{
			name:   "hex token with NO recognized keyword; the documented gap",
			line:   "DEPLOY_HEX=" + strings.Repeat("a1b2c3d4", 8),
			secret: strings.Repeat("a1b2c3d4", 8),
			caught: false, by: "nothing; hex cannot reach the entropy floor, by design",
		},
		{
			name:   "high-entropy secret in nested JSON",
			line:   `{"key":"` + "aB3xQ9vK2mZ7pL4wR8tY6nH1jF5sD0gC" + `"}`,
			secret: "aB3xQ9vK2mZ7pL4wR8tY6nH1jF5sD0gC",
			caught: true, by: "entropy fallback; precededByAssignment skips the JSON escape",
		},
		{
			name:   "low-entropy secret under a keyword, in nested JSON",
			line:   `{"password":"hunter2-prod-db-2026"}`,
			secret: "hunter2-prod-db-2026",
			caught: true, by: "secret_assignment; the keyword tolerates the key's closing quote",
		},
		{
			name:   "the same high-entropy token, flat, in the same field",
			line:   "SESSION_KEY=" + "aB3xQ9vK2mZ7pL4wR8tY6nH1jF5sD0gC",
			secret: "aB3xQ9vK2mZ7pL4wR8tY6nH1jF5sD0gC",
			caught: true, by: "entropy fallback; `=` puts the token in a value position",
		},
		{
			name:   "OPENBOX_WORKLOAD_PRIVATE_KEY, base64 PKCS8 DER (runtime key)",
			line:   "OPENBOX_WORKLOAD_PRIVATE_KEY=" + workloadKeyB64,
			secret: workloadKeyB64,
			caught: true, by: "secret_assignment (keyword private_key, base64 value >=64 chars)",
		},
		{
			name:   "a runtime workload key's PEM block (PKCS8 \"PRIVATE KEY\")",
			line:   workloadKeyPEM,
			secret: workloadKeyPEM,
			// Measured, not the gitleaks rule this repo's other private keys rely
			// on: a local BEGIN/END PRIVATE KEY pattern (secrets.go's own
			// `private_key` category) replaces the whole block before gitleaks'
			// own detector ever runs over what is left.
			caught: true, by: "private_key (a local PEM-block pattern, ahead of gitleaks)",
		},
		{
			name:   "X-OpenBox-Workload-Token header, a runtime RS256 assertion (JWT-shaped)",
			line:   "X-OpenBox-Workload-Token: " + workloadToken,
			secret: workloadToken,
			caught: true, by: "jwt (the three-segment eyJ...eyJ...sig shape rule; no keyword needed)",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := fakecore.New(t, fakecore.Script{})
			evalCreds(t, srv.URL())
			t.Setenv(envContentCapture, "1")
			t.Setenv(envRealtime, "0")
			t.Setenv(envEnforce, "0")
			os.Unsetenv(envSecretDetection) // default ON

			sid := "cc-cred"
			payload, err := json.Marshal(map[string]any{
				"hook_event_name": "PostToolUse",
				"session_id":      sid,
				"cwd":             "/tmp",
				"tool_name":       "Bash",
				"tool_use_id":     "toolu_cred",
				"tool_input":      map[string]any{"command": "cat ~/.openbox/.env"},
				"tool_response":   map[string]any{"stdout": tc.line, "stderr": ""},
			})
			if err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			RunHook("PostToolUse", bytes.NewReader(payload), &out, log.New(&bytes.Buffer{}, "", 0))
			RunHook("SessionEnd", strings.NewReader(
				`{"hook_event_name":"SessionEnd","session_id":"`+sid+`","cwd":"/tmp","reason":"other"}`),
				&out, log.New(&bytes.Buffer{}, "", 0))

			inbox := srv.Inbox()
			if len(inbox) == 0 {
				t.Fatal("nothing reached /evaluate; the case proves nothing")
			}
			var leaked bool
			for _, r := range inbox {
				if strings.Contains(string(r.Raw), tc.secret) {
					leaked = true
				}
			}
			switch {
			case tc.caught && leaked:
				t.Errorf("credential reached /evaluate; %s was expected to catch it", tc.by)
			case !tc.caught && !leaked:
				t.Errorf("the unlabelled-hex gap has closed. If that was intended, update "+
					"this case AND docs/data-and-privacy.md; if it was a side effect of "+
					"tuning the entropy floor, check what else now matches (%s)", tc.by)
			}
		})
	}
}

func jsonQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
