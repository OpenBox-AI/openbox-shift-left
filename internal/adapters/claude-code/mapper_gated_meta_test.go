package claudecode

import (
	"strings"
	"testing"
)

// gatedMetaCases are the two fields this adapter decoded and deliberately left
// unwired. Each is free text, so each is gated and redacted; each rides
// metadata rather than content.signal_detail because its class already claims
// that single-string carrier for a different field.
var gatedMetaCases = []struct {
	name       string
	hook       HookName
	ev         func(text string) *HookEvent
	key        string // the new metadata key
	siblingKey string // the content.signal_detail key it must not displace
	sibling    string // that sibling's text in the fixture
}{
	{
		name: "Notification.title",
		hook: HookNotification,
		ev: func(text string) *HookEvent {
			return &HookEvent{
				SessionID: "s", NotificationType: "idle_prompt",
				Message: "please respond", Title: text,
			}
		},
		key: "notification_title", siblingKey: "notification_message", sibling: "please respond",
	},
	{
		name: "TaskCreated.task_description",
		hook: HookTaskCreated,
		ev: func(text string) *HookEvent {
			return &HookEvent{
				SessionID: "s", TaskID: "tid1", TaskSubject: "investigate the flake",
				TaskDescription: text,
			}
		},
		key: "task_description", siblingKey: "task_subject", sibling: "investigate the flake",
	},
	{
		name: "TaskCompleted.task_description",
		hook: HookTaskCompleted,
		ev: func(text string) *HookEvent {
			return &HookEvent{
				SessionID: "s", TaskID: "tid1", TaskSubject: "investigate the flake",
				TaskDescription: text,
			}
		},
		key: "task_description", siblingKey: "task_subject", sibling: "investigate the flake",
	},
}

// TestGatedContentMetaIsBoundAndGated. Bound at all is the point -- both fields
// sat decoded and unread, so the struct could hold them and nothing could ever
// see them.
func TestGatedContentMetaIsBoundAndGated(t *testing.T) {
	const text = "GATED-META-SENTINEL: the flake only reproduces under -race"

	for _, tc := range gatedMetaCases {
		t.Run(tc.name, func(t *testing.T) {
			on := testMapper()
			on.CaptureContent = true
			got, ok := on.Map(tc.hook, tc.ev(text))
			if !ok {
				t.Fatalf("Map ok=false")
			}
			if got.Metadata[tc.key] != text {
				t.Errorf("metadata[%q] = %v, want the free text", tc.key, got.Metadata[tc.key])
			}
			// R4: it must not displace the class's existing content field.
			if got.Content == nil || got.Content.SignalDetail != tc.sibling {
				t.Errorf("content.signal_detail = %+v, want %q; the new key displaced it",
					got.Content, tc.sibling)
			}

			off := testMapper()
			off.CaptureContent = false
			got, ok = off.Map(tc.hook, tc.ev(text))
			if !ok {
				t.Fatalf("Map ok=false with capture off")
			}
			if v, present := got.Metadata[tc.key]; present {
				t.Errorf("metadata[%q] = %v with capture OFF; the primary gate did not hold", tc.key, v)
			}
		})
	}
}

// TestGatedContentMetaIsRedactedBeforeAttachment. Redact-then-attach is the
// only in-transit control there is, and the ordering is the control.
func TestGatedContentMetaIsRedactedBeforeAttachment(t *testing.T) {
	const secret = "sk-live-REDACT-ME"

	for _, tc := range gatedMetaCases {
		t.Run(tc.name, func(t *testing.T) {
			m := testMapper()
			m.CaptureContent = true
			m.RedactContent = func(s string) string {
				return strings.ReplaceAll(s, secret, "[REDACTED]")
			}
			got, ok := m.Map(tc.hook, tc.ev("token is "+secret))
			if !ok {
				t.Fatalf("Map ok=false")
			}
			v, _ := got.Metadata[tc.key].(string)
			if strings.Contains(v, secret) {
				t.Errorf("metadata[%q] = %q; the secret was attached before redaction", tc.key, v)
			}
			if !strings.Contains(v, "[REDACTED]") {
				t.Errorf("metadata[%q] = %q, want the redacted marker", tc.key, v)
			}
		})
	}
}
