package muse

import (
	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
)

// Credentials is the resolved runtime identity for the hook binary; the shape
// is shared by every adapter (hookflow.Credentials).
type Credentials = hookflow.Credentials

// ResolveIdentity resolves only the developer DID; no secret-store access
// (INV-1: zero secret I/O on the hot path).
func ResolveIdentity() (Identity, error) { return hookflow.ResolveIdentity() }

// ResolveCredentials assembles Credentials via the shared resolver: secrets
// from the environment then this tool's ~/.openbox/<tool>/.env, coordinates
// from the environment then dev.json.
func ResolveCredentials() (Credentials, error) { return hookflow.ResolveCredentials() }
