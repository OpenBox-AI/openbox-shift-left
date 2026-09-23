package devconfig

import (
	"strings"
	"testing"
)

// TestAttributionDIDMatchesCoreDerivation pins the namespace (core
// openbox_did.go, backend aip-namespace.ts) and one literal agent_id -> did
// vector, computed once with uuid.NewSHA1 since neither repo carries a
// literal vector. This is the only derivation in the repo, so this is the
// only place it has to be pinned.
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
