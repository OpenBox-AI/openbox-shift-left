package claudecode

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/client"
	"github.com/openbox-ai/openbox-shift-left/internal/client/fakecore"
)

// notificationPrompt is a synthetic <task-notification> wrapper shaped like
// the ones the audit corpus measured: a subagent's own lifecycle signal that
// arrives on UserPromptSubmit instead of any of the 21 other lifecycle hooks,
// because core's goal gate exempts only prompt_submitted from suppression.
const notificationPrompt = "<task-notification>agent needs input</task-notification>"

// TestMachineInjectedPrompt is the pure-function table for machineInjectedPrompt:
// no Mapper, no wire, just the six recognised source values plus the empty
// (today-live) case, and the shape-arm edge cases that justify matching the
// WHOLE trimmed prompt rather than a loose substring search.
func TestMachineInjectedPrompt(t *testing.T) {
	tests := []struct {
		name           string
		source         string
		prompt         string
		wantInjected   bool
		wantFromVendor bool
	}{
		// The seven source values a live or forward-compatible payload can carry.
		{"source user", "user", notificationPrompt, false, true}, // vendor wins even over notification-shaped text
		{"source sdk", "sdk", "anything", true, true},
		{"source system", "system", "anything", true, true}, // rule is != "user", not == "system"
		{"source loop_wakeup", "loop_wakeup", "", true, true},
		{"source schedule_wakeup", "schedule_wakeup", "", true, true},
		{"source poll_event", "poll_event", "", true, true},
		{"source empty, ordinary prompt", "", "fix the auth bug please", false, false},

		// Unrecognised vendor value: not evidence either way (risk table: extend
		// promptSources if this needs to change); the shape arm is reserved for
		// source == "".
		{"source unrecognised, notification-shaped prompt", "bogus", notificationPrompt, false, false},

		// Shape arm, only reachable when source == "".
		{"empty source, exact notification", "", notificationPrompt, true, false},
		{"empty source, notification padded with whitespace", "", "\n  " + notificationPrompt + "  \n", true, false},
		{
			"empty source, notification quoted mid-text stays a goal",
			"", "Earlier the tool said: " + notificationPrompt + " -- please continue.",
			false, false,
		},
		{
			"empty source, trailing text after the closing tag stays a goal",
			"", notificationPrompt + " thanks!",
			false, false,
		},
		{
			"empty source, leading text before the opening tag stays a goal",
			"", "FYI: " + notificationPrompt,
			false, false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			injected, fromVendor := machineInjectedPrompt(tc.source, tc.prompt)
			if injected != tc.wantInjected || fromVendor != tc.wantFromVendor {
				t.Errorf("machineInjectedPrompt(%q, %q) = (%v, %v), want (%v, %v)",
					tc.source, tc.prompt, injected, fromVendor, tc.wantInjected, tc.wantFromVendor)
			}
		})
	}
}

// wireBodyFor is the client's exported path: client.New + Client.Emit against
// an in-memory server, decoding the actual bytes signed and sent. This is the
// only level at which "signal_args" exists at all -- DevEvent itself carries
// no such field; client.buildPayload derives governanceEventPayload.SignalArgs
// from ev.Content, unexported, package client -- so an assertion that key is
// absent from "the marshalled JSON" has to run here, not on json.Marshal(ev).
func wireBodyFor(t *testing.T, ev client.DevEvent, clientContentCapture bool) map[string]any {
	t.Helper()
	fc := fakecore.New(t, fakecore.Script{})

	cl, err := client.New(client.Config{
		BaseURL:               fc.URL(),
		APIKey:                fakecore.APIKey(),
		WorkloadPrivateKey:    fakecore.WorkloadPrivateKey(),
		ContentCaptureEnabled: clientContentCapture,
	})
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}
	if _, err := cl.Emit(context.Background(), ev); err != nil {
		t.Fatalf("Emit: %v", err)
	}

	inbox := fc.Inbox()
	if len(inbox) == 0 {
		t.Fatal("nothing was POSTed")
	}
	raw := inbox[len(inbox)-1].Raw

	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("decode wire body: %v\n%s", err, raw)
	}
	return payload
}

// TestWire_MachineInjectedPromptWithholdsSignalArgs is the acceptance-level
// suite: for each fixture, map it through the real Mapper and send it through
// the real Client to an in-memory server, then assert on the literal bytes
// that would have left the machine.
func TestWire_MachineInjectedPromptWithholdsSignalArgs(t *testing.T) {
	const realPrompt = "fix the auth bug please"
	const vendorUserPrompt = "please rotate the leaked credential"
	quotingPrompt := "Earlier the tool said: " + notificationPrompt + " -- please continue."

	tests := []struct {
		name          string
		ev            *HookEvent
		mapperCapture bool
		clientCapture bool

		wantSignalArgsAbsent bool
		wantSignalArgs       map[string]any // asserted only when wantSignalArgsAbsent is false

		wantMeta       map[string]any // subset match: every key here must equal
		wantMetaAbsent []string       // these keys must not be present at all
	}{
		{
			name:                 "task notification, capture on",
			ev:                   &HookEvent{SessionID: "s1", Prompt: notificationPrompt},
			mapperCapture:        true,
			clientCapture:        true,
			wantSignalArgsAbsent: true,
			wantMeta:             map[string]any{"prompt_source": "task_notification", "prompt_source_inferred": true},
		},
		{
			name:                 "vendor source system, capture on",
			ev:                   &HookEvent{SessionID: "s1", Source: "system", Prompt: "irrelevant machine text"},
			mapperCapture:        true,
			clientCapture:        true,
			wantSignalArgsAbsent: true,
			wantMeta:             map[string]any{"prompt_source": "system"},
			wantMetaAbsent:       []string{"prompt_source_inferred"},
		},
		{
			name:           "vendor source user, capture on",
			ev:             &HookEvent{SessionID: "s1", Source: "user", Prompt: vendorUserPrompt},
			mapperCapture:  true,
			clientCapture:  true,
			wantSignalArgs: map[string]any{"prompt": vendorUserPrompt},
			wantMeta:       map[string]any{"prompt_source": "user"},
			wantMetaAbsent: []string{"prompt_source_inferred"},
		},
		{
			name:           "no source, ordinary prompt (byte-identical regression)",
			ev:             &HookEvent{SessionID: "s1", Prompt: realPrompt},
			mapperCapture:  true,
			clientCapture:  true,
			wantSignalArgs: map[string]any{"prompt": realPrompt},
			wantMetaAbsent: []string{"prompt_source", "prompt_source_inferred"},
		},
		{
			name:           "no source, prompt quotes a notification mid-text (false-positive guard)",
			ev:             &HookEvent{SessionID: "s1", Prompt: quotingPrompt},
			mapperCapture:  true,
			clientCapture:  true,
			wantSignalArgs: map[string]any{"prompt": quotingPrompt},
			wantMetaAbsent: []string{"prompt_source", "prompt_source_inferred"},
		},
		{
			name:                 "task notification, content_capture:false at the client (prompt_source survives)",
			ev:                   &HookEvent{SessionID: "s1", Prompt: notificationPrompt},
			mapperCapture:        true,
			clientCapture:        false,
			wantSignalArgsAbsent: true,
			wantMeta:             map[string]any{"prompt_source": "task_notification", "prompt_source_inferred": true},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := testMapper()
			m.CaptureContent = tc.mapperCapture
			ev, ok := m.Map(HookUserPromptSubmit, tc.ev)
			if !ok {
				t.Fatal("Map ok=false")
			}

			payload := wireBodyFor(t, ev, tc.clientCapture)

			if got := payload["event_type"]; got != "SignalReceived" {
				t.Errorf("event_type = %v, want SignalReceived", got)
			}
			if got := payload["signal_name"]; got != "prompt_submitted" {
				t.Errorf("signal_name = %v, want prompt_submitted", got)
			}

			sa, present := payload["signal_args"]
			switch {
			case tc.wantSignalArgsAbsent:
				if present {
					t.Errorf("signal_args must be ABSENT from the wire, got %v", sa)
				}
			default:
				if !present {
					t.Fatalf("signal_args must be present, got none. Full payload: %+v", payload)
				}
				got, ok := sa.(map[string]any)
				if !ok {
					t.Fatalf("signal_args is not an object: %T %v", sa, sa)
				}
				if len(got) != len(tc.wantSignalArgs) {
					t.Errorf("signal_args = %v, want %v", got, tc.wantSignalArgs)
				}
				for k, v := range tc.wantSignalArgs {
					if got[k] != v {
						t.Errorf("signal_args[%q] = %v, want %v", k, got[k], v)
					}
				}
			}

			meta, _ := payload["metadata"].(map[string]any)
			for k, v := range tc.wantMeta {
				if meta[k] != v {
					t.Errorf("metadata[%q] = %v, want %v", k, meta[k], v)
				}
			}
			for _, k := range tc.wantMetaAbsent {
				if v, present := meta[k]; present {
					t.Errorf("metadata[%q] must be ABSENT, got %v", k, v)
				}
			}
		})
	}
}
