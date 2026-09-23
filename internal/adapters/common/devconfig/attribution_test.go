package devconfig

import (
	"os/exec"
	"strings"
	"testing"
)

// TestAttributionDIDMatchesCoreDerivation pins the namespace (VERIFIED,
// Validation Session 1: core openbox_did.go:30, backend aip-namespace.ts) and
// one literal agent_id -> did vector, computed once with uuid.NewSHA1 since
// neither repo carries a literal vector today. It then asserts git-action's
// own DIDForAgent (the only other local copy of this derivation) agrees,
// until phase 04 deletes that copy in favour of this one.
//
// The cross-check runs git-action's function out of process
// (testdata/didcheck), not via a direct import: git-action already imports
// devconfig (advisory.go), so importing git-action back from here would be a
// compile cycle.
func TestAttributionDIDMatchesCoreDerivation(t *testing.T) {
	if AttributionAIPNamespace != "b6e4a1d3-7c02-4e8a-9d1f-5a3b7c2d8e0f" {
		t.Fatalf("namespace = %q, want the VERIFIED core/backend value", AttributionAIPNamespace)
	}

	const (
		agentID = "8f2a1c4e-9b3d-4a6f-8c5e-2d7b9a1f3e6c"
		wantDID = "did:aip:63bdd432-3786-5689-9a5c-a9979497de1d"
	)

	got, err := AttributionDIDFor(agentID)
	if err != nil {
		t.Fatalf("AttributionDIDFor(%q): %v", agentID, err)
	}
	if got != wantDID {
		t.Fatalf("AttributionDIDFor(%q) = %q, want the pinned vector %q", agentID, got, wantDID)
	}
	if !strings.HasPrefix(got, "did:aip:") {
		t.Fatalf("AttributionDIDFor(%q) = %q, missing the did:aip: prefix", agentID, got)
	}

	out, err := exec.Command("go", "run", "./testdata/didcheck", agentID).Output()
	if err != nil {
		t.Fatalf("running git-action's DIDForAgent out of process: %v", err)
	}
	if gitActionDID := strings.TrimSpace(string(out)); gitActionDID != got {
		t.Fatalf("devconfig.AttributionDIDFor(%q) = %q, git-action's DIDForAgent = %q; the derivations have drifted",
			agentID, got, gitActionDID)
	}
}

// TestAttributionDIDRefusesNonUUID a malformed agent id must be refused
// rather than hashed anyway, since the result attributes every governance
// event the agent produces.
func TestAttributionDIDRefusesNonUUID(t *testing.T) {
	for _, bad := range []string{"", "not-a-uuid", "12345", "8f2a1c4e-9b3d-4a6f-8c5e", "  "} {
		if _, err := AttributionDIDFor(bad); err == nil {
			t.Errorf("AttributionDIDFor(%q) succeeded; want a refusal for a non-UUID agent id", bad)
		}
	}
}
