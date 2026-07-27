package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// AuthValidatePath is openbox-core's read-only preflight route
// (GET /api/v1/auth/validate). Exported so a caller that only wants to
// preview the request — `openbox dev verify --dry-run` — can render the plan
// without constructing a Client or touching the network. The same string is
// the canonical-string PATH component core reconstructs.
const AuthValidatePath = "/api/v1/auth/validate"

// Validate performs a read-only preflight against the configured core: a
// signed GET /api/v1/auth/validate. It confirms the data-plane path works
// end-to-end — the obx_ key is accepted and the Ed25519 signing round-trip
// verifies, so a signing_required=true agent is rejected without a valid
// signature exactly as /evaluate would be. It reuses the identical auth
// headers and AIP signing as Emit (INV-1: key only in Authorization, seed
// only in ed25519.Sign).
//
// Unlike Emit, Validate is not fail-open: it is a diagnostic the operator
// ran on purpose, so a failure is returned, never swallowed. A 2xx yields
// nil; any other outcome yields an error whose message is a mapped,
// actionable diagnostic (reason category + fix hint) — never a secret,
// nonce, or signature (INV-1). The bounded http.Client timeout keeps a
// dead/slow core a clear failure, not a hang. A single attempt (no retry):
// a preflight wants immediate feedback, and re-sending a fully-prepared
// request risks a spurious nonce_replayed.
func (c *Client) Validate(ctx context.Context) error {
	// Sign the empty-body GET exactly as core reconstructs it: canonical
	// GET\n<path>\n<ts>\n<nonce>\n<sha256("")>. signer.sign is method-agnostic
	// and a nil body hashes to the empty-string SHA (== core's sha256(nil)).
	sig, err := c.signer.sign(http.MethodGet, AuthValidatePath, nil, c.now())
	if err != nil {
		return fmt.Errorf("sign validate request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+AuthValidatePath, nil)
	if err != nil {
		return err
	}
	// Identical auth surface to Emit's attempt(). No Content-Type: the GET carries
	// no body, and core hashes a nil body for this route.
	req.Header.Set("Accept", "application/json")
	req.Header.Set(headerAuthorization, "Bearer "+c.apiKey)
	req.Header.Set(headerSDKVersion, sdkVersion)
	req.Header.Set(headerUserAgent, "OpenBox-SDK/"+sdkVersion)
	req.Header.Set(headerAgentDID, c.signer.did)
	req.Header.Set(headerAgentTS, sig.timestamp)
	req.Header.Set(headerAgentNonce, sig.nonce)
	req.Header.Set(headerAgentSig, sig.sig)
	req.Header.Set(headerBodySHA256, sig.bodySHA)

	resp, err := c.http.Do(req)
	if err != nil {
		// Transport failure (dead core, DNS, TLS, timeout). Reported verbatim; it
		// carries no secret (the key lives only in the Authorization header, never
		// in a URL or error). Named so the ✗ line points at the core it tried.
		return fmt.Errorf("could not reach core at %s: %w", c.baseURL, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	// Reuse the reason→guidance map (diagnose) so the ✗ names the likely fix
	// — e.g. a 500 "verifier unavailable" → set signing_required=false.
	return &ValidateError{Status: resp.StatusCode, Diagnostic: diagnose(resp.StatusCode, string(body))}
}

// ValidateError is a non-2xx /auth/validate outcome. Status is the HTTP
// status; Diagnostic is the mapped, actionable line (reason category + fix
// hint, never a secret). A transport failure is a plain wrapped error, not
// this type, so a caller can tell "core said no" from "couldn't reach core"
// via errors.As.
type ValidateError struct {
	Status     int
	Diagnostic string
}

func (e *ValidateError) Error() string {
	return "auth/validate returned HTTP " + itoa(e.Status) + ": " + e.Diagnostic
}

// AsValidateError reports the *ValidateError in err's chain, if any. It is a thin
// convenience over errors.As for callers that want the HTTP status/diagnostic.
func AsValidateError(err error) (*ValidateError, bool) {
	var ve *ValidateError
	if errors.As(err, &ve) {
		return ve, true
	}
	return nil, false
}
