package muse

import (
	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/client"
)

// Credentials is the resolved runtime identity for the hook binary.
type Credentials struct {
	BaseURL string
	APIKey  string
	// DID is the in-memory attribution label, derived from AgentID; never itself
	// a store value.
	DID                   string
	AgentID               string
	WorkloadPrivateKey    string
	TokenCachePath        string
	ContentCaptureEnabled bool
}

// Identity is the non-secret projection used by the Mapper.
func (c Credentials) Identity() Identity { return Identity{DeveloperDID: c.DID} }

// NewClient builds the workload-authenticated transport from the resolved
// credentials.
func (c Credentials) NewClient(logger client.Logger) (*client.Client, error) {
	return client.New(client.Config{
		BaseURL:               c.BaseURL,
		APIKey:                c.APIKey,
		WorkloadPrivateKey:    c.WorkloadPrivateKey,
		TokenCachePath:        c.TokenCachePath,
		ContentCaptureEnabled: c.ContentCaptureEnabled,
		Logger:                logger,
		// Every hook client gets exactly one attempt at the wire: a retry here
		// would race DrainSession's own single-attempt accounting.
		MaxRetries: &zeroRetries,
	})
}

// zeroRetries makes MaxRetries: 0 addressable; client.Config.MaxRetries is a
// *int precisely so an explicit zero is expressible.
var zeroRetries = 0

// ResolveIdentity resolves only the developer DID; no secret-store access
// (INV-1: zero secret I/O on the hot path).
func ResolveIdentity() (Identity, error) {
	did, err := devconfig.ResolveDID()
	if err != nil {
		return Identity{}, err
	}
	return Identity{DeveloperDID: did}, nil
}

// ResolveCredentials assembles Credentials via the shared resolver: secrets from
// the environment then this tool's ~/.openbox/<tool>/.env, coordinates from the
// environment then dev.json.
func ResolveCredentials() (Credentials, error) {
	dc, err := devconfig.ResolveCredentials()
	if err != nil {
		return Credentials{}, err
	}
	return Credentials{
		BaseURL:               dc.BaseURL,
		APIKey:                dc.APIKey,
		DID:                   dc.DID,
		AgentID:               dc.AgentID,
		WorkloadPrivateKey:    dc.WorkloadPrivateKey,
		TokenCachePath:        dc.TokenCachePath,
		ContentCaptureEnabled: dc.ContentCaptureEnabled,
	}, nil
}
