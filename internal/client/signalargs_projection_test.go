package client

import "testing"

// TestSignalArgsProjectionCoversEveryClass is the completeness guard the
// projection exists to make possible.
//
// The v1.8 shape was a three-case switch over event types, so a class added to
// the mapper stayed invisible to every policy engine until someone remembered
// to add a fourth case. Nothing failed when they did not: the class shipped,
// emitted metadata, and matched no rule. This test iterates AllEventTypes
// rather than a hand-written list, so a new signal class that does not project
// fails here on the day it is added.
//
// Checked per class, never in aggregate: a count would pass while one class
// carried nothing.
func TestSignalArgsProjectionCoversEveryClass(t *testing.T) {
	// A representative structural map. The keys are deliberately not any class's
	// real ones — the projection has no per-class knowledge, and a test that used
	// the real keys would be asserting the mapper, not the projection.
	meta := func() map[string]any {
		return map[string]any{
			"structural_a": "value-a",
			"structural_b": float64(7),
			"structural_c": true,
		}
	}

	signals := 0
	for _, et := range AllEventTypes {
		wire, name, err := wireTypeFor(et)
		if err != nil {
			t.Errorf("wireTypeFor(%s): %v", et, err)
			continue
		}
		if wire != wireSignalReceived {
			continue
		}
		signals++

		ev := DevEvent{
			EventID: "ev-proj-" + string(et), EventType: et, SessionID: "s", DeveloperDID: "did:aip:x",
			Timestamp: "2026-09-08T00:00:00Z", Tool: Tool{Name: "claude-code", Kind: ToolShell},
			Metadata: meta(),
		}

		// prompt_submitted is the one class that deliberately does not project:
		// its signal_args IS the goal core creates the session from.
		if et == EventPromptSubmitted {
			ev.Content = &Content{Prompt: "the goal"}
			args := signalArgs(t, ev)
			if len(args) != 1 || args["prompt"] != "the goal" {
				t.Errorf("%s (%s): signal_args = %v, want exactly {\"prompt\": …}", et, name, args)
			}
			continue
		}

		args := signalArgs(t, ev)
		if args == nil {
			t.Errorf("%s (%s): signal_args absent; a policy engine cannot match this class at all", et, name)
			continue
		}
		for k, want := range meta() {
			if args[k] != want {
				t.Errorf("%s (%s): signal_args[%q] = %v (%T), want %v (%T)",
					et, name, k, args[k], args[k], want, want)
			}
		}
	}

	if signals != 27 {
		t.Errorf("iterated %d signal classes, want 27; AllEventTypes and the projection have diverged", signals)
	}
}

// TestSignalArgsProjectionDoesNotUseCoreGoalKeys guards the mechanism behind
// the rollout-ordering hazard (core's goal gate must be running first).
//
// Core tries ["prompt","message","input","text","content"]
// in order and treats the first hit as the goal text. On a core WITHOUT the
// source-and-name gate, a projected key with one of those names becomes the
// session goal. No mapper writes one today; this test is what makes that a
// checked property rather than a remembered one.
//
// It does not assert those names are absent from the whole payload — `message`
// is a legitimate metadata key on a commit — only that no signal class emits
// one structurally, unprompted.
//
// This half can only see signalDetailKeyFor's outputs. The larger surface is the
// adapters' metadata keys, which this package cannot enumerate (they import it,
// not the reverse), so the guard is a pair: see
// claude-code's TestNoSignalMetadataKeyIsACoreGoalKey and the git action's
// TestDeployMetadataCarriesNoCoreGoalKey, which cover every live signal
// producer. Codex emits no signals today; when it does, it needs its own.
func TestSignalArgsProjectionDoesNotUseCoreGoalKeys(t *testing.T) {
	goalKeys := []string{"prompt", "message", "input", "text", "content"}

	for _, et := range AllEventTypes {
		wire, name, err := wireTypeFor(et)
		if err != nil || wire != wireSignalReceived || et == EventPromptSubmitted {
			continue
		}
		if k := signalDetailKeyFor(et); k != "" {
			for _, gk := range goalKeys {
				if k == gk {
					t.Errorf("%s (%s): signalDetailKeyFor returns %q, which core reads as goal text",
						et, name, k)
				}
			}
		}
	}
}
