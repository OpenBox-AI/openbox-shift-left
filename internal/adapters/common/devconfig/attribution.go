package devconfig

import (
	"fmt"

	"github.com/google/uuid"
)

// AttributionAIPNamespace is the fixed uuid5 namespace the OpenBox platform
// derives an attribution label from; it matches core
// internal/services/identity/openbox_did.go and backend
// src/modules/did/aip-namespace.ts. This is the repo's only copy of the
// derivation: git-action calls AttributionDIDFor rather than keeping its own.
const AttributionAIPNamespace = "b6e4a1d3-7c02-4e8a-9d1f-5a3b7c2d8e0f"

// AttributionDIDFor derives the in-memory attribution label for a v3
// keycloak_workload agent: did:aip:<uuidv5(namespace, agentID)>. This is
// derived at read time, never stored: a workload agent's DID at the
// backend is NULL, dev.json never holds this value, and it is never sent as a
// header. A non-UUID agent id is refused rather than hashed anyway, since the
// result attributes every governance event the agent produces.
func AttributionDIDFor(agentID string) (string, error) {
	if _, err := uuid.Parse(agentID); err != nil {
		return "", fmt.Errorf("attribution DID: agent id %q is not a UUID: %w", agentID, err)
	}
	ns, err := uuid.Parse(AttributionAIPNamespace)
	if err != nil {
		return "", fmt.Errorf("attribution DID: parse namespace: %w", err)
	}
	// Hashed as the ORIGINAL agent id string's utf8 bytes, as core and the
	// backend do, not a re-serialized/normalized form of it.
	return "did:aip:" + uuid.NewSHA1(ns, []byte(agentID)).String(), nil
}
