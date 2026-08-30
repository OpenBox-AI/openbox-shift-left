package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
)

// AuthValidatePath is openbox-core's read-only preflight route (GET
// /api/v1/auth/validate).
const AuthValidatePath = "/api/v1/auth/validate"

// Validate performs a read-only preflight against the configured core: a
// signed GET /api/v1/auth/validate. Unlike Emit, Validate is not fail-open: it
// is a diagnostic the operator ran on purpose, so a failure is returned, never
// swallowed.
func (c *Client) Validate(ctx context.Context) error {
	sig, err := c.signer.sign(http.MethodGet, AuthValidatePath, nil, c.now())
	if err != nil {
		return fmt.Errorf("sign validate request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+AuthValidatePath, nil)
	if err != nil {
		return err
	}
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
		// Reported verbatim; it carries no secret (the key lives only in the
		// Authorization header, never in a URL or error).
		return fmt.Errorf("could not reach core at %s: %w", c.baseURL, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	return &ValidateError{Status: resp.StatusCode, Diagnostic: diagnose(resp.StatusCode, string(body))}
}

// ValidateError is a non-2xx /auth/validate outcome. Status is the HTTP
// status; Diagnostic is the mapped, actionable line (reason category + fix
// hint, never a secret).
type ValidateError struct {
	Status     int
	Diagnostic string
}

func (e *ValidateError) Error() string {
	return "auth/validate returned HTTP " + strconv.Itoa(e.Status) + ": " + e.Diagnostic
}

// AsValidateError reports the *ValidateError in err's chain, if any.
func AsValidateError(err error) (*ValidateError, bool) {
	var ve *ValidateError
	if errors.As(err, &ve) {
		return ve, true
	}
	return nil, false
}
