package muse

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEveryFixtureParses(t *testing.T) {
	files, _ := filepath.Glob(filepath.Join("testdata", "*.json"))
	if len(files) < 19 {
		t.Fatalf("found %d fixtures, want the 19 hook payloads", len(files))
	}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		ev, err := ParseHookEvent(strings.NewReader(string(raw)))
		if err != nil {
			t.Errorf("%s: %v", f, err)
			continue
		}
		if ev.SessionID == "" || ev.HookEventName == "" {
			t.Errorf("%s: session_id/hook_event_name not read: %+v", f, ev)
		}
		if _, err := ParseHookName(ev.HookEventName); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
}

// Unknown keys are tolerated, and a value of a type nothing depends on reads as
// empty instead of failing a payload a gated event would then be denied for.
func TestParseIsTolerant(t *testing.T) {
	ev, err := ParseHookEvent(strings.NewReader(`{
		"hook_event_name":"PreLLMCall","session_id":"s","brand_new_key":{"a":[1,2]},
		"turn_id":7,"cwd":null,"model":{"id":"x"},"request_id":42,"attempt":"3","step":1.0,
		"messages":"not a list","tools":[{"name":"Bash"},"Read",7],"message_count":"2","tool_count":null,
		"options":[1],"usage":"n/a","error":{"type":"rate_limit"}}`))
	if err != nil {
		t.Fatalf("a type surprise must not fail the payload: %v", err)
	}
	if ev.SessionID != "s" || ev.TurnID != "7" || ev.RequestID != "42" || ev.Cwd != "" || ev.Model != "" {
		t.Errorf("fields = %+v", ev)
	}
	if n, ok := intOf(ev.Attempt); !ok || n != 3 {
		t.Errorf("a quoted attempt reads as %d, %v", n, ok)
	}
	if n, ok := intOf(ev.Step); !ok || n != 1 {
		t.Errorf("a float step reads as %d, %v", n, ok)
	}
	if len(ev.Messages) != 0 || len(ev.Tools) != 3 {
		t.Errorf("messages=%d tools=%d", len(ev.Messages), len(ev.Tools))
	}
}

func TestParseRefusesWhatIsNotAnObject(t *testing.T) {
	for _, in := range []string{"", "[1]", "null", "\"x\"", "{not json"} {
		if _, err := ParseHookEvent(strings.NewReader(in)); err == nil {
			t.Errorf("%q parsed", in)
		}
	}
}

func TestHookClasses(t *testing.T) {
	gated := map[HookName]bool{HookUserPromptSubmit: true, HookPreToolUse: true, HookPermissionRequest: true, HookPreLLMCall: true}
	silent := map[HookName]bool{HookSubagentStop: true, HookPreCompact: true, HookPostCompact: true, HookNotification: true, HookPostToolBatch: true, HookInterrupt: true}
	for h := range hookNames {
		if h.Gated() != gated[h] {
			t.Errorf("%s: Gated() = %v", h, h.Gated())
		}
		if h.Observed() == silent[h] {
			t.Errorf("%s: Observed() = %v", h, h.Observed())
		}
	}
	if len(hookNames) != 18 {
		t.Errorf("Muse documents 18 hook events, the adapter knows %d", len(hookNames))
	}
}
