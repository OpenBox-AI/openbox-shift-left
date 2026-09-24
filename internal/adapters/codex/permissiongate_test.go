package codex

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
)

func permissionEvent(session string) string {
	return `{"hook_event_name":"PermissionRequest","session_id":"` + session + `","cwd":"/r",` +
		`"model":"gpt-5.6-sol","permission_mode":"default","turn_id":"t1",` +
		`"tool_name":"apply_patch","tool_input":{"command":"*** Begin Patch\n*** Add File: /etc/x\n+y\n*** End Patch"},` +
		`"transcript_path":null}`
}

// TestPermissionGate_NeverLoosens is a security control, not a style rule.
//
// PermissionRequest is the ONE lever in the entire Codex surface that can
// loosen: `behavior:"allow"` skips the human approval prompt outright. A
// governance product that emitted it would be an approval bypass. The other
// three forbidden keys are reserved fields Codex documents as failing closed,
// and emitting any of them invalidates the output, which on this surface means
// the call is no longer governed at all.
//
// The assertion is on raw bytes across every verdict the gate can hand the
// contract, because a structural check would miss a key added by a later edit.
func TestPermissionGate_NeverLoosens(t *testing.T) {
	forbidden := []string{`"allow"`, `"updatedInput"`, `"updatedPermissions"`, `"interrupt"`,
		`"continue"`, `"stopReason"`, `"suppressOutput"`}

	sawDeny := false
	for _, dec := range []string{"", "allow", "deny", "block", hookflow.DecisionHalt} {
		line, applied := permissionContract.Render(dec, "a reason", json.RawMessage(`{"command":"x"}`))
		if len(line) == 0 {
			if applied != "" {
				t.Errorf("decision %q: wrote nothing but reported %q applied", dec, applied)
			}
			continue // proceed → the human is still asked; that is the safe default
		}
		s := string(line)
		for _, f := range forbidden {
			if strings.Contains(s, f) {
				t.Errorf("decision %q: PermissionRequest output contains %s, which either loosens the "+
					"approval or fails the output closed: %s", dec, f, s)
			}
		}
		if applied != codexDecisionDeny {
			t.Errorf("decision %q: the only outcome this surface may apply is deny; got %q", dec, applied)
		}
		sawDeny = true

		var o permissionRequestOutput
		if err := json.Unmarshal(line, &o); err != nil {
			t.Fatalf("decision %q: unmarshalable output: %v", dec, err)
		}
		if o.HookSpecificOutput.HookEventName != string(HookPermissionRequest) {
			t.Errorf("decision %q: hookEventName = %q", dec, o.HookSpecificOutput.HookEventName)
		}
		if o.HookSpecificOutput.Decision.Behavior != codexDecisionDeny {
			t.Errorf("decision %q: behavior = %q, want deny", dec, o.HookSpecificOutput.Decision.Behavior)
		}
		if o.HookSpecificOutput.Decision.Message == "" {
			t.Errorf("decision %q: Codex rejects a denial with no message, and a rejected output "+
				"fails the call OPEN: %s", dec, s)
		}
	}
	if !sawDeny {
		t.Fatal("no verdict rendered a denial; the table proves nothing")
	}
}

// TestPermissionGate_EmptyReasonStillCarriesAMessage: an empty policy reason must
// not become an empty message. Codex rejects that output and the rejection fails
// open, so a generic message that is delivered governs where a precise one that
// is discarded does not.
func TestPermissionGate_EmptyReasonStillCarriesAMessage(t *testing.T) {
	line, _ := permissionContract.Render(codexDecisionDeny, "", nil)
	if len(line) == 0 {
		t.Fatal("a deny must render output")
	}
	var o permissionRequestOutput
	if err := json.Unmarshal(line, &o); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if o.HookSpecificOutput.Decision.Message == "" {
		t.Error("an empty reason must still produce a non-empty message")
	}
}

// TestPermissionGate_DeniesEndToEnd drives the real hook path.
func TestPermissionGate_DeniesEndToEnd(t *testing.T) {
	gateEnv(t)
	serveVerdictServer(t, `{"verdict":"block","reason":"writes outside the workspace","policy_id":"p9"}`)

	stdout, _ := runHook(t, "PermissionRequest", permissionEvent("th-perm"))
	if strings.TrimSpace(stdout) == "" {
		t.Fatal("a blocking verdict on an escalation must deny it")
	}
	var o permissionRequestOutput
	if err := json.Unmarshal([]byte(stdout), &o); err != nil {
		t.Fatalf("stdout is not valid PermissionRequest JSON: %v (%q)", err, stdout)
	}
	if o.HookSpecificOutput.Decision.Behavior != codexDecisionDeny {
		t.Errorf("behavior = %q, want deny", o.HookSpecificOutput.Decision.Behavior)
	}
}

// TestPermissionGate_AllowVerdictWritesNothing: when policy does NOT object, the
// adapter stays out of the way and the human still gets asked. Writing an
// approval on the developer's behalf is the bypass this contract exists to
// prevent, so "allow" must be indistinguishable from "silent".
func TestPermissionGate_AllowVerdictWritesNothing(t *testing.T) {
	gateEnv(t)
	serveVerdictServer(t, `{"verdict":"allow"}`)

	stdout, _ := runHook(t, "PermissionRequest", permissionEvent("th-perm-allow"))
	if strings.TrimSpace(stdout) != "" {
		t.Errorf("an allow verdict must write nothing, so the human is still asked; got %q", stdout)
	}
}

// TestPermissionRequest_IsInTheUninstallSweep: hookremove.go iterates the same
// hookedEvents slice the installer writes, so a newly registered event is swept
// for free -- but only if it was added to that slice rather than somewhere else.
// This asserts the coupling rather than trusting it.
func TestPermissionRequest_IsInTheUninstallSweep(t *testing.T) {
	var found bool
	for _, ev := range hookedEvents {
		if ev == HookPermissionRequest {
			found = true
		}
	}
	if !found {
		t.Fatal("PermissionRequest must be in hookedEvents, or `openbox uninstall` leaves an orphan hook")
	}
}

// TestPermissionRequest_IsAGatingTimeout: the escalation can hold for a real
// approval decision, so it must carry the gating ceiling. A hot-hook bound here
// would time out mid-decision and fail the call open.
func TestPermissionRequest_IsAGatingTimeout(t *testing.T) {
	if got := timeoutFor(HookPermissionRequest); got != preToolUseHookTimeoutSec {
		t.Errorf("timeoutFor(PermissionRequest) = %d, want the gating ceiling %d", got, preToolUseHookTimeoutSec)
	}
}
