package client

import "testing"

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

// TestNoSignalCarriesSignalArgs is a governance test, not a shape test: a
// SignalReceived with non-empty signal_args re-anchors the session goal in
// core. 24 of 27 signal classes must carry none — only PromptSubmitted,
// CommitCreated and Deploy populate buildSignalArgs by design. Each case
// builds a payload with Content and Metadata populated, so a case that
// passes only because nothing was ever set proves nothing.
func TestNoSignalCarriesSignalArgs(t *testing.T) {
	exempt := map[EventType]bool{
		EventPromptSubmitted: true,
		EventCommitCreated:   true,
		EventDeploy:          true,
	}
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
		raw := decodeRaw(t, ev)
		if v, present := raw["signal_args"]; present {
			t.Errorf("%s: signal_args present on the wire: %v", et, v)
		}
	}
	if checked != 24 {
		t.Errorf("checked %d non-exempt signal classes, want 24 (27 signal classes minus the 3 exempt)", checked)
	}
}
