package client

import (
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

// This file is phase 10's cross-cutting signal-vocabulary suite: it iterates
// the FULL AllEventTypes (33, all 27 signal classes included — the 6
// pre-1.8 signals plus the 21 v1.8 classes phase 04 scoped its own coverage
// to), not just the new classes. Phase 04's contract_v18_test.go already
// covers the 21 new classes in isolation; this file is the closed-vocabulary
// guard that must hold across the whole list, forever, as new classes join.

// TestSignalNamesAreUnique is insight 1's guard: core validates signal_name
// with no allowlist at all (an optional *string), so two classes sharing a
// name merge silently in the stored column and in OPA's input. This test is
// the ONLY thing that would catch that.
func TestSignalNamesAreUnique(t *testing.T) {
	seen := map[string]EventType{}
	count := 0
	for _, et := range AllEventTypes {
		wire, name, err := wireTypeFor(et)
		if err != nil {
			t.Errorf("wireTypeFor(%s): %v", et, err)
			continue
		}
		if wire != wireSignalReceived {
			continue
		}
		count++
		if name == "" {
			t.Errorf("%s rides SignalReceived with an empty signal_name", et)
			continue
		}
		if prior, dup := seen[name]; dup {
			t.Errorf("signal_name %q is claimed by both %s and %s; two classes sharing a name merge "+
				"silently in the stored column and in OPA's input — core has no allowlist to catch this",
				name, prior, et)
			continue
		}
		seen[name] = et
	}
	if count != 27 {
		t.Errorf("found %d signal classes in AllEventTypes, want 27", count)
	}
}

// TestEveryContentKeyIsGated is derived by iteration, never a second
// hand-written list: every non-empty key signalDetailKeyFor can return for
// ANY EventType must be in contentMetadataKeys, or an adapter populating that
// key routes around the content gate entirely.
func TestEveryContentKeyIsGated(t *testing.T) {
	keys := map[string]bool{}
	for _, et := range AllEventTypes {
		k := signalDetailKeyFor(et)
		if k == "" {
			continue
		}
		keys[k] = true
		if !contentMetadataKeys[k] {
			t.Errorf("signalDetailKeyFor(%s) = %q, which is not in contentMetadataKeys", et, k)
		}
	}
	if len(keys) == 0 {
		t.Fatal("signalDetailKeyFor returned no keys at all across AllEventTypes; the test would pass vacuously")
	}
}

// TestMetadataNativeContentKeysAreGated covers the content keys that live in
// metadata natively rather than arriving through signalDetailKeyFor.
//
// TestEveryContentKeyIsGated iterates signalDetailKeyFor, so it cannot see
// these: Content.SignalDetail is a single string and each of their classes
// already claims it for a different field, which is why they ride metadata at
// all. contentMetadataKeys is their only gate, and a key missing from it routes
// straight around content_capture.
func TestMetadataNativeContentKeysAreGated(t *testing.T) {
	for _, k := range []string{
		"notification_title", // Notification.title, alongside notification_message
		"task_description",   // Task{Created,Completed}.task_description, alongside task_subject
	} {
		if !contentMetadataKeys[k] {
			t.Errorf("%q is not in contentMetadataKeys; an adapter writing it egresses free "+
				"text with content capture off", k)
		}
	}
}

// TestContentMetadataKeysAreCappedOnEgress. Content that rides Content.* is
// capped where it is attached; the same text arriving through metadata had no
// bound at all until eventMetadataForEgress applied one. Bounds have owners,
// and capBody owns content egress — so it applies wherever content egresses,
// not only on the carrier it was first written for.
func TestContentMetadataKeysAreCappedOnEgress(t *testing.T) {
	// Multi-byte, so a byte-cap and a rune-cap give visibly different answers.
	long := strings.Repeat("日", maxBodySize+10)
	ev := DevEvent{
		EventID: "ev-cap", EventType: EventConfigChange, SessionID: "s", DeveloperDID: "did:aip:x",
		Timestamp: "2026-09-08T00:00:00Z", Tool: Tool{Name: "claude-code", Kind: ToolShell},
		Metadata: map[string]any{"command": long, "file_path": "/tmp/.env"},
	}
	cut := &cutLog{}
	m := eventMetadataForEgress(ev, cut, "metadata")
	got, _ := m["command"].(string)
	if n := utf8.RuneCountInString(got); n != maxBodySize {
		t.Errorf("a content metadata key egressed at %d runes, want the %d-rune content cap", n, maxBodySize)
	}
	if !utf8.ValidString(got) {
		t.Error("the cap cut a rune in half")
	}
	if p, _ := m["file_path"].(string); p != "/tmp/.env" {
		t.Errorf("a structural key was capped too: %q", p)
	}
	if want := []string{"metadata.command"}; !slices.Equal(cut.sorted(), want) {
		t.Errorf("cut.sorted() = %v, want %v; the structural file_path key must contribute no path", cut.sorted(), want)
	}
}

// TestEverySignalProjectsItsPayload is a governance test, not a shape test:
// signal_args is the only field a policy engine reads on a SignalReceived, so a
// class that carries none is unenforceable. 26 of 27 project their metadata
// there; prompt_submitted is the one exception, because its signal_args IS the
// goal and its shape is a shipped contract (see TestSignalArgs_Prompt_Verbatim).
// Each case builds a payload with Content and Metadata populated, so a case
// that passes only because nothing was ever set proves nothing.
func TestEverySignalProjectsItsPayload(t *testing.T) {
	exempt := map[EventType]bool{EventPromptSubmitted: true}
	checked := 0
	for _, et := range AllEventTypes {
		wire, _, err := wireTypeFor(et)
		if err != nil {
			t.Errorf("wireTypeFor(%s): %v", et, err)
			continue
		}
		if wire != wireSignalReceived || exempt[et] {
			continue
		}
		checked++
		ev := DevEvent{
			EventID: "ev-args-" + string(et), EventType: et, SessionID: "s", DeveloperDID: "did:aip:x",
			Timestamp: "2026-09-07T00:00:00Z", Tool: Tool{Name: "claude-code", Kind: ToolShell},
			Content: &Content{SignalDetail: "some free text", Prompt: "some free text"},
			Metadata: map[string]any{
				"commit_sha": "abc", "repo": "r", "branch": "main",
				"deploy_id": "d", "environment": "prod", "deploy_did": "did:aip:y",
			},
		}
		args := signalArgs(t, ev)
		if args == nil {
			t.Errorf("%s: signal_args absent on the wire; no policy engine can match this class", et)
			continue
		}
		if args["commit_sha"] != "abc" || args["repo"] != "r" || args["branch"] != "main" {
			t.Errorf("%s: signal_args did not carry the structural metadata: %v", et, args)
		}
		// Content.Prompt is prompt_submitted's carrier alone; no other class may
		// pick it up, or core's stringifySignalArgs reads it as a goal.
		if _, leaked := args["prompt"]; leaked {
			t.Errorf("%s: signal_args carries a `prompt` key; core reads that as goal text: %v", et, args)
		}
	}
	if checked != 26 {
		t.Errorf("checked %d non-exempt signal classes, want 26 (27 signal classes minus prompt_submitted)", checked)
	}
}

// TestEveryContentKeyIsGatedInSignalArgs is TestEveryContentKeyIsGated's twin
// for the second destination. Once signal_args carries content, INV-2's
// completeness rule has two surfaces to hold on, and a key gated in one but not
// the other is a gate with a hole in it.
func TestEveryContentKeyIsGatedInSignalArgs(t *testing.T) {
	const canary = "CONTENT-KEY-CANARY"
	checked := 0
	for _, et := range AllEventTypes {
		k := signalDetailKeyFor(et)
		if k == "" {
			continue
		}
		if wire, _, err := wireTypeFor(et); err != nil || wire != wireSignalReceived {
			continue
		}
		checked++
		ev := DevEvent{
			EventID: "ev-gate-" + string(et), EventType: et, SessionID: "s", DeveloperDID: "did:aip:x",
			Timestamp: "2026-09-07T00:00:00Z", Tool: Tool{Name: "claude-code", Kind: ToolShell},
			Content:  &Content{SignalDetail: canary},
			Metadata: map[string]any{"tool_name": "Bash"},
		}
		if args := signalArgs(t, ev); args[k] != canary {
			t.Errorf("%s: signal_args[%q] = %v with capture on, want the free text", et, k, args[k])
		}
		if args := signalArgs(t, stripContent(ev)); args != nil {
			if v, present := args[k]; present {
				t.Errorf("%s: signal_args[%q] = %v survived the content gate", et, k, v)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no signal class had a signalDetailKeyFor key; the test would pass vacuously")
	}
}
