package workloadauth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// bootstrapPath is the v3 bootstrap endpoint, relative to the configured base
// URL.
const bootstrapPath = "/api/v3/auth/bootstrap"

// tokenEndpointSuffix is what a canonical bootstrap document's token_endpoint
// must equal issuer (with any trailing slash stripped) plus.
const tokenEndpointSuffix = "/protocol/openid-connect/token"

// maxBodyBytes bounds every response this package reads: a body past this is
// read up to the cap and no further, so a 2 MiB response truncates into
// invalid JSON rather than growing the process without limit.
const maxBodyBytes = 1 << 20 // 1 MiB

// BootstrapDocument is the strictly-validated bootstrap response: the wire
// shape from SDK behaviour 1, unmarshalled and checked before this package
// ever hands it back to a caller.
type BootstrapDocument struct {
	BootstrapVersion  int    `json:"bootstrap_version"`
	ContractVersion   int    `json:"contract_version"`
	TokenEndpoint     string `json:"token_endpoint"`
	Issuer            string `json:"issuer"`
	Audience          string `json:"audience"`
	ClientID          string `json:"client_id"`
	ServiceAccountID  string `json:"service_account_id"`
	ActivationVersion string `json:"activation_version"`
	IdentitySource    string `json:"identity_source"`
	Kid               string `json:"kid"`
}

// Bootstrap performs the GET {baseURL}/api/v3/auth/bootstrap call: API key
// only, no workload header, no body. It returns a strictly-validated document
// or a typed *Error naming the bootstrap stage.
func Bootstrap(ctx context.Context, hc *http.Client, baseURL, apiKey, sdkVersion string) (*BootstrapDocument, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+bootstrapPath, nil)
	if err != nil {
		return nil, &Error{Stage: StageBootstrap, Reason: "building request", Transient: false}
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("X-OpenBox-SDK-Version", sdkVersion)
	req.Header.Set("User-Agent", "OpenBox-SDK/"+sdkVersion)
	req.Header.Set("Accept", "application/json")

	resp, err := hc.Do(req)
	if err != nil {
		return nil, &Error{Stage: StageBootstrap, Reason: "network fault", Detail: netCause(err), Transient: true}
	}
	defer resp.Body.Close()

	body, err := readBounded(resp.Body)
	if err != nil {
		return nil, &Error{Stage: StageBootstrap, Status: resp.StatusCode, Reason: "reading response", Transient: isTransientStatus(resp.StatusCode)}
	}

	if resp.StatusCode != http.StatusOK {
		return nil, &Error{
			Stage:     StageBootstrap,
			Status:    resp.StatusCode,
			Reason:    boundString(bootstrapReasonCode(body), maxReasonLen),
			Transient: isTransientStatus(resp.StatusCode),
		}
	}

	var doc BootstrapDocument
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, &Error{Stage: StageBootstrap, Status: resp.StatusCode, Reason: "malformed bootstrap document"}
	}
	if verr := validateBootstrapDocument(&doc); verr != nil {
		return nil, &Error{Stage: StageBootstrap, Status: resp.StatusCode, Reason: boundString(verr.Error(), maxReasonLen)}
	}
	return &doc, nil
}

// readBounded reads at most maxBodyBytes from r. A larger body is read up to
// the cap and truncated silently -- the caller's JSON parse then fails on the
// cut-off document -- rather than read in full and risk unbounded memory.
func readBounded(r io.Reader) ([]byte, error) {
	return io.ReadAll(io.LimitReader(r, maxBodyBytes))
}

// bootstrapReasonCode extracts the bootstrap error envelope's reason_code, or
// "" for a body that carries none.
func bootstrapReasonCode(body []byte) string {
	var m struct {
		ReasonCode string `json:"reason_code"`
	}
	if json.Unmarshal(body, &m) != nil {
		return ""
	}
	return m.ReasonCode
}

var canonicalUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func isCanonicalUUID(s string) bool {
	return canonicalUUID.MatchString(s)
}

// isLoopbackHost reports whether host (already stripped of any port) is a
// loopback name or address, the one case the bootstrap issuer may use http
// rather than https.
func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// validateBootstrapDocument enforces every strict rule SDK behaviour 1 pins:
// versions, issuer shape, the derived token_endpoint, non-empty identifiers,
// canonical lowercase UUIDs and a known identity_source.
func validateBootstrapDocument(doc *BootstrapDocument) error {
	if doc.BootstrapVersion != 3 {
		return fmt.Errorf("bootstrap_version %d != 3", doc.BootstrapVersion)
	}
	if doc.ContractVersion != 3 {
		return fmt.Errorf("contract_version %d != 3", doc.ContractVersion)
	}

	iss, err := url.Parse(doc.Issuer)
	if err != nil || iss.Host == "" {
		return fmt.Errorf("issuer is not an absolute URL")
	}
	if iss.RawQuery != "" || iss.Fragment != "" || iss.User != nil {
		return fmt.Errorf("issuer must carry no query, fragment or userinfo")
	}
	switch iss.Scheme {
	case "https":
	case "http":
		if !isLoopbackHost(iss.Hostname()) {
			return fmt.Errorf("issuer scheme http is only allowed on loopback")
		}
	default:
		return fmt.Errorf("issuer scheme %q is not https", iss.Scheme)
	}

	wantEndpoint := strings.TrimRight(doc.Issuer, "/") + tokenEndpointSuffix
	if doc.TokenEndpoint != wantEndpoint {
		return fmt.Errorf("token_endpoint does not equal issuer + %q", tokenEndpointSuffix)
	}

	if doc.Audience == "" {
		return fmt.Errorf("audience is empty")
	}
	if doc.ClientID == "" {
		return fmt.Errorf("client_id is empty")
	}
	if doc.Kid == "" {
		return fmt.Errorf("kid is empty")
	}
	if !isCanonicalUUID(doc.ServiceAccountID) {
		return fmt.Errorf("service_account_id is not a canonical lowercase UUID")
	}
	if !isCanonicalUUID(doc.ActivationVersion) {
		return fmt.Errorf("activation_version is not a canonical lowercase UUID")
	}
	switch doc.IdentitySource {
	case "openbox", "okta", "entra":
	default:
		return fmt.Errorf("identity_source %q is not one of openbox, okta, entra", doc.IdentitySource)
	}
	return nil
}
