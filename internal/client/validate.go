package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
)

// AuthValidatePath is openbox-core's read-only preflight route (GET
// /api/v1/auth/validate).
const AuthValidatePath = "/api/v1/auth/validate"

// v3ValidatePath is used instead of AuthValidatePath when
// Config.WorkloadPrivateKey is set (dual-mode, phase 04 slice 4a).
const v3ValidatePath = "/api/v3/auth/validate"

// ValidateResult is core's v3 /auth/validate answer, once the workload bearer
// is accepted: {valid, active, agent_id, agent_name}. It stays the zero value
// in v1 mode and whenever Validate's non-2xx or transport-error paths return
// -- ValidateDetailed is the only way to read it, so Validate itself keeps its
// existing signature and every caller keeps compiling.
type ValidateResult struct {
	Valid     bool
	Active    bool
	AgentID   string
	AgentName string
}

// Validate performs a read-only preflight against the configured core.
// Unlike Emit, Validate is not fail-open: it is a diagnostic the operator ran
// on purpose, so a failure is returned, never swallowed. It discards the
// parsed result; use ValidateDetailed to read it.
func (c *Client) Validate(ctx context.Context) error {
	_, err := c.ValidateDetailed(ctx)
	return err
}

// ValidateDetailed is Validate plus the parsed v3 result. In v1 mode it is
// byte-identical to Validate's old behaviour and ValidateResult is always the
// zero value. In v3 mode a non-2xx is a *ValidateError (AsValidateError still
// works) and a token-acquisition failure is the wrapped *workloadauth.Error
// verbatim, so a caller (doctor) can name the stage.
func (c *Client) ValidateDetailed(ctx context.Context) (ValidateResult, error) {
	if c.v3() {
		return c.validateV3(ctx)
	}
	if err := c.validateV1(ctx); err != nil {
		return ValidateResult{}, err
	}
	return ValidateResult{}, nil
}

// validateV1 is the original signed-GET preflight, unchanged.
func (c *Client) validateV1(ctx context.Context) error {
	sig, err := c.signer.sign(http.MethodGet, AuthValidatePath, nil, c.now())
	if err != nil {
		return fmt.Errorf("sign validate request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+AuthValidatePath, nil)
	if err != nil {
		return err
	}
	c.setSignedHeaders(req.Header, sig)

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

// validateV3 acquires a workload token and GETs /api/v3/auth/validate. A 401
// with a cached token invalidates the cache (D2, same rule attemptV3 applies
// to evaluate/approval); either way the failure is reported once, never
// resent.
func (c *Client) validateV3(ctx context.Context) (ValidateResult, error) {
	token, fromCache, terr := c.tokens.Token(ctx)
	if terr != nil {
		return ValidateResult{}, fmt.Errorf("client: acquire workload token: %w", terr)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+v3ValidatePath, nil)
	if err != nil {
		return ValidateResult{}, err
	}
	c.setWorkloadHeaders(req.Header, token)

	resp, err := c.http.Do(req)
	if err != nil {
		return ValidateResult{}, fmt.Errorf("could not reach core at %s: %w", c.baseURL, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode == http.StatusUnauthorized && fromCache {
		_ = c.tokens.Invalidate()
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return ValidateResult{}, &ValidateError{Status: resp.StatusCode, Diagnostic: diagnoseV3(resp.StatusCode, string(body))}
	}

	var wire struct {
		Valid     bool   `json:"valid"`
		Active    bool   `json:"active"`
		AgentID   string `json:"agent_id"`
		AgentName string `json:"agent_name"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		return ValidateResult{}, fmt.Errorf("client: unparseable v3 validate response: %w", err)
	}
	return ValidateResult{Valid: wire.Valid, Active: wire.Active, AgentID: wire.AgentID, AgentName: wire.AgentName}, nil
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
