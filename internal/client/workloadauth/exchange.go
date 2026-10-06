package workloadauth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
)

// clientAssertionType is the fixed OAuth client-assertion-type value the
// exchange sends, per RFC 7523.
const clientAssertionType = "urn:ietf:params:oauth:client-assertion-type:jwt-bearer"

// ExchangeResult is the exchange's successful response, already validated:
// a non-empty bearer access token and its raw expires_in seconds.
type ExchangeResult struct {
	AccessToken string
	ExpiresIn   float64
}

// keycloakErrorEnvelope is Keycloak's OAuth error body shape.
type keycloakErrorEnvelope struct {
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

// Exchange performs the POST doc.TokenEndpoint client_credentials exchange,
// signed by assertion. It carries no Authorization header: the assertion is
// the whole proof. The response must show token_type Bearer
// (case-insensitive), a non-empty access_token, and a JSON-number
// expires_in > 30 (never a JSON bool, which json.Unmarshal into float64
// already rejects).
func Exchange(ctx context.Context, hc *http.Client, doc *BootstrapDocument, assertion string) (*ExchangeResult, error) {
	form := url.Values{}
	form.Set("grant_type", "client_credentials")
	form.Set("client_id", doc.ClientID)
	form.Set("client_assertion_type", clientAssertionType)
	form.Set("client_assertion", assertion)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, doc.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, &Error{Stage: StageExchange, Reason: "building request"}
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := hc.Do(req)
	if err != nil {
		return nil, &Error{Stage: StageExchange, Reason: "network fault", Detail: netCause(err), Transient: true}
	}
	defer resp.Body.Close()

	body, err := readBounded(resp.Body)
	if err != nil {
		return nil, &Error{Stage: StageExchange, Status: resp.StatusCode, Reason: "reading response", Transient: isTransientStatus(resp.StatusCode)}
	}

	if resp.StatusCode != http.StatusOK {
		return nil, exchangeError(resp.StatusCode, body)
	}

	var raw struct {
		AccessToken string          `json:"access_token"`
		TokenType   string          `json:"token_type"`
		ExpiresIn   json.RawMessage `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, &Error{Stage: StageExchange, Status: resp.StatusCode, Reason: "malformed token response"}
	}
	if raw.AccessToken == "" {
		return nil, &Error{Stage: StageExchange, Status: resp.StatusCode, Reason: "empty access_token"}
	}
	if !strings.EqualFold(raw.TokenType, "Bearer") {
		return nil, &Error{Stage: StageExchange, Status: resp.StatusCode, Reason: "token_type is not Bearer"}
	}
	expiresIn, ok := parseExpiresIn(raw.ExpiresIn)
	if !ok || expiresIn <= 30 {
		return nil, &Error{Stage: StageExchange, Status: resp.StatusCode, Reason: "expires_in must be a number greater than 30"}
	}

	return &ExchangeResult{AccessToken: raw.AccessToken, ExpiresIn: expiresIn}, nil
}

// parseExpiresIn accepts only a JSON number: unmarshalling a JSON bool into
// float64 already fails, which is exactly the "not a boolean" rule SDK
// behaviour 7 pins.
func parseExpiresIn(raw json.RawMessage) (float64, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err != nil {
		return 0, false
	}
	return f, true
}

// exchangeError classifies a non-200 exchange response: 400/401/403 name the
// host clock, since an expired 60s assertion is the most common cause; 5xx
// and 429 are transient.
func exchangeError(status int, body []byte) *Error {
	var ke keycloakErrorEnvelope
	_ = json.Unmarshal(body, &ke)

	e := &Error{
		Stage:     StageExchange,
		Status:    status,
		Reason:    boundString(ke.Error, maxReasonLen),
		Transient: isTransientStatus(status),
	}
	if ke.ErrorDescription != "" {
		e.Detail = boundString(ke.ErrorDescription, maxDetailLen)
	}
	if status == http.StatusBadRequest || status == http.StatusUnauthorized || status == http.StatusForbidden {
		e.Hint = clockSkewHint
	}
	return e
}
