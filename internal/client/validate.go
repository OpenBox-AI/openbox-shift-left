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

// AuthValidatePath is core's read-only preflight route (GET
// /api/v3/auth/validate).
const AuthValidatePath = "/api/v3/auth/validate"

// ValidateResult is core's /auth/validate answer, once the workload bearer is
// accepted: {valid, active, agent_id, agent_name}. It stays the zero value
// whenever Validate's non-2xx or transport-error paths return --
// ValidateDetailed is the only way to read it, so Validate itself keeps its
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

// ValidateDetailed is Validate plus the parsed result: a non-2xx is a
// *ValidateError (AsValidateError still works) and a token-acquisition
// failure is the wrapped *workloadauth.Error verbatim, so a caller (doctor)
// can name the stage.
//
// It acquires a workload token and GETs /api/v3/auth/validate. A 401 with a
// cached token invalidates the cache, the same rule attempt applies to
// evaluate/approval; either way the failure is reported once, never resent.
func (c *Client) ValidateDetailed(ctx context.Context) (ValidateResult, error) {
	token, fromCache, terr := c.tokens.Token(ctx)
	if terr != nil {
		return ValidateResult{}, fmt.Errorf("client: acquire workload token: %w", terr)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+AuthValidatePath, nil)
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
		return ValidateResult{}, &ValidateError{Status: resp.StatusCode, Diagnostic: diagnose(resp.StatusCode, string(body))}
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
	res := ValidateResult{Valid: wire.Valid, Active: wire.Active, AgentID: wire.AgentID, AgentName: wire.AgentName}
	// A 200 is not proof by itself: core answers valid/active in the body, and
	// an agent deactivated after its token was issued still gets a 200. Doctor
	// and git-action's ownership witness both treat this call as "core vouches
	// for this agent", so a body that does not vouch is a failure here.
	if !res.Valid || !res.Active {
		return res, &ValidateError{Status: resp.StatusCode, Diagnostic: fmt.Sprintf(
			"core reports the agent as valid=%t active=%t; it is deactivated or its identity was revoked",
			res.Valid, res.Active)}
	}
	return res, nil
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
