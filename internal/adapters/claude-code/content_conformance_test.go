package claudecode

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"

	"github.com/openbox-ai/openbox-shift-left/internal/client/memhttptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// TestContentCaptureConformance tool-content conformance suite; executable
// evidence for the content gate on the fields this adapter used to throw away.
func TestContentCaptureConformance(t *testing.T) {
	isolateConfig(t)
	t.Setenv(envDID, testDID)
	t.Setenv("OPENBOX_SPOOL_DIR", t.TempDir())
	t.Setenv("OPENBOX_SESSION_DIR", t.TempDir())
	t.Setenv(envEnforcementFile, filepath.Join(t.TempDir(), "enf.jsonl"))

	serveCapturing := func(t *testing.T) *[]string {
		t.Helper()
		var mu sync.Mutex
		var bodies []string
		srv := memhttptest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, _ := io.ReadAll(r.Body)
			mu.Lock()
			bodies = append(bodies, string(raw))
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"verdict":"allow"}`))
		}))
		t.Cleanup(srv.Close)
		evalCreds(t, srv.URL)
		return &bodies
	}

	// `capture` is set after serveCapturing, deliberately: serveCapturing calls
	// evalCreds, which forces content capture OFF.
	observeThenFlush := func(t *testing.T, session, capture, hook, payload string) []string {
		t.Helper()
		bodies := serveCapturing(t)
		t.Setenv(envContentCapture, capture)
		t.Setenv(envRealtime, "0")
		t.Setenv(envEnforce, "0") // observe path; no gate, no deferred spool
		var out bytes.Buffer
		RunHook(hook, strings.NewReader(payload), &out, log.New(&bytes.Buffer{}, "", 0))
		RunHook("SessionEnd", strings.NewReader(
			`{"hook_event_name":"SessionEnd","session_id":"`+session+`","cwd":"/tmp","reason":"other"}`),
			&out, log.New(&bytes.Buffer{}, "", 0))
		return *bodies
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

	t.Run("C38 signal free text egresses as metadata, never as signal_args", func(t *testing.T) {
		// Routing a denial reason there would silently replace the developer's
		// prompt as the thing every later turn is scored against; the failure would
		// look like drift, not like a bug.
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

				var found bool
				for _, b := range on {
					var p struct {
						SignalName string          `json:"signal_name"`
						SignalArgs json.RawMessage `json:"signal_args"`
						Metadata   map[string]any  `json:"metadata"`
					}
					if err := json.Unmarshal([]byte(b), &p); err != nil || p.SignalName == "" {
						continue
					}
					if len(p.SignalArgs) > 0 {
						t.Errorf("signal %q carries signal_args %s; core would overwrite the "+
							"alignment goal with it", p.SignalName, p.SignalArgs)
					}
					if v, ok := p.Metadata[tc.metaKey].(string); ok && strings.Contains(v, tc.sentinel) {
						found = true
					}
				}
				if !found {
					t.Errorf("metadata.%s did not carry the free text with capture on; bodies=%v",
						tc.metaKey, on)
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
}

// TestContentCaptureCredentialCoverage c39 is a detection-coverage case, not
// an ordering case, and it is separate from C34 deliberately.
//   - **Unlabelled hex.** A token with no recognized key name is redacted only
//     if
func TestContentCaptureCredentialCoverage(t *testing.T) {
	isolateConfig(t)
	t.Setenv(envDID, testDID)
	t.Setenv("OPENBOX_SPOOL_DIR", t.TempDir())
	t.Setenv("OPENBOX_SESSION_DIR", t.TempDir())
	t.Setenv(envEnforcementFile, filepath.Join(t.TempDir(), "enf.jsonl"))

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
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var bodies []string
			srv := memhttptest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, _ := io.ReadAll(r.Body)
				mu.Lock()
				bodies = append(bodies, string(raw))
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"verdict":"allow"}`))
			}))
			t.Cleanup(srv.Close)
			evalCreds(t, srv.URL)
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

			if len(bodies) == 0 {
				t.Fatal("nothing reached /evaluate; the case proves nothing")
			}
			var leaked bool
			for _, b := range bodies {
				if strings.Contains(b, tc.secret) {
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
