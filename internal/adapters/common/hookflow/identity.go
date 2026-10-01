package hookflow

import (
	"log"
	"strings"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/client"
	"github.com/openbox-ai/openbox-shift-left/internal/provider"
)

// Identity is the developer-agent identity an adapter emits under. Only the
// DID is needed to build events; the key material lives in the client, never
// here (INV-1).
type Identity struct {
	DeveloperDID string // did:aip:<uuid>
}

// Credentials is the resolved runtime identity for the hook binary, shared by
// every adapter: the store is per tool, the shape is not.
type Credentials struct {
	BaseURL string
	APIKey  string
	// DID is the in-memory attribution label, derived from AgentID; never
	// itself a store value.
	DID                   string
	AgentID               string
	WorkloadPrivateKey    string
	TokenCachePath        string
	ContentCaptureEnabled bool
}

// Identity is the non-secret projection used by an adapter's Mapper.
func (c Credentials) Identity() Identity { return Identity{DeveloperDID: c.DID} }

// NewClient builds the v3 workload-authenticated transport from the resolved
// credentials.
func (c Credentials) NewClient(logger client.Logger) (*client.Client, error) {
	return client.New(client.Config{
		BaseURL:               c.BaseURL,
		APIKey:                c.APIKey,
		WorkloadPrivateKey:    c.WorkloadPrivateKey,
		TokenCachePath:        c.TokenCachePath,
		ContentCaptureEnabled: c.ContentCaptureEnabled,
		Logger:                logger,
		// Every hook client -- the flusher's, the gate's own escalation --
		// gets exactly one attempt at the wire too: a retry here would race
		// DrainSession's own single-attempt accounting. The git action's
		// client is unrelated and keeps the library default.
		MaxRetries: &zeroRetries,
	})
}

// zeroRetries makes MaxRetries: 0 addressable; client.Config.MaxRetries is a
// *int precisely so an explicit zero is expressible (unset is the library
// default, defaultMaxRetries).
var zeroRetries = 0

// ResolveCredentials assembles Credentials through the shared resolver:
// secrets from the environment then this tool's ~/.openbox/<tool>/.env,
// coordinates from the environment then dev.json. It returns an error (never a
// panic) when identity is incomplete; the caller logs it fail-open.
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

// ResolveIdentity resolves only the developer DID (env, then config file); no
// secret-store access (INV-1: zero secret I/O on the hot path).
func ResolveIdentity() (Identity, error) {
	did, err := devconfig.ResolveDID()
	if err != nil {
		return Identity{}, err
	}
	return Identity{DeveloperDID: did}, nil
}

// NewEvaluator is the one bounded /evaluate round trip every adapter's gated
// events make: ceiling is the provider's declared hook-kill limit, and the
// transport is built from the resolved Credentials on each evaluation.
func NewEvaluator(ceiling provider.HookCeiling) Evaluator {
	return Evaluator{
		Ceiling: ceiling,
		NewClient: func(logger *log.Logger) (Governor, error) {
			creds, err := ResolveCredentials()
			if err != nil {
				return nil, err
			}
			return creds.NewClient(logger)
		},
	}
}

// HasDeveloperDID reports whether the identity carries a did:aip DID, the only
// shape an event may be emitted under.
func (id Identity) HasDeveloperDID() bool { return strings.HasPrefix(id.DeveloperDID, "did:aip:") }
