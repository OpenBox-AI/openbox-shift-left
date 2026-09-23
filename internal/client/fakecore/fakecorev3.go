package fakecore

// fakecorev3.go is the workload-identity half of the fake control plane: a
// fake Keycloak (bootstrap doc + token exchange) and the governance/validate
// routes, alongside the route dispatch and shared plumbing in fakecore.go.
// It stays on the same import wall (guard_test.go's
// TestFakecoreKeepsItsImportWall): the RS256 assertion is verified with
// crypto/rsa directly here, a second implementation of the same check
// internal/client/workloadauth performs, and the attribution DID derivation
// is restated rather than imported from devconfig -- the wall permits no
// other repo-local import besides memhttptest. A verifier built by calling
// into the code under test would agree with it by construction, including
// when both are wrong.
import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// v3ClientAssertionType is the fixed RFC 7523 value the token exchange
// requires, restated from workloadauth's own copy for the reason given above.
const v3ClientAssertionType = "urn:ietf:params:oauth:client-assertion-type:jwt-bearer"

// v3AttributionAIPNamespace restates devconfig.AttributionAIPNamespace
// (verified against core openbox_did.go:30 and backend aip-namespace.ts).
// TestFakecoreAttributionDIDMatchesTheVerifiedVector pins
// the same vector devconfig's own TestAttributionDIDMatchesCoreDerivation
// asserts, so a drift here is caught without importing devconfig.
const v3AttributionAIPNamespace = "b6e4a1d3-7c02-4e8a-9d1f-5a3b7c2d8e0f"

var (
	v3IdentityOnce sync.Once
	v3PrivKey      *rsa.PrivateKey
	v3APIKeyStr    string
	v3AgentIDStr   string
	v3ClientIDStr  string
	v3ActVersion   string
)

// ensureV3Identity mints the one RSA key, API key and agent id this process
// uses for every fake-core v3 server, so a client under test can read
// WorkloadPrivateKey/APIKey/AgentID before any Server exists.
func ensureV3Identity() {
	v3IdentityOnce.Do(func() {
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			panic("fakecore: generate v3 RSA key: " + err.Error())
		}
		v3PrivKey = key

		apiKeyBytes := make([]byte, 24)
		if _, err := rand.Read(apiKeyBytes); err != nil {
			panic("fakecore: generate v3 API key: " + err.Error())
		}
		v3APIKeyStr = "obx_test_v3_" + base64.RawURLEncoding.EncodeToString(apiKeyBytes)
		v3AgentIDStr = uuid.NewString()
		v3ClientIDStr = uuid.NewString()
		v3ActVersion = uuid.NewString()
	})
}

// WorkloadPrivateKey is the process-wide v3 RSA key, PKCS#8 PEM-encoded --
// the form Config.WorkloadPrivateKey accepts.
func WorkloadPrivateKey() string {
	ensureV3Identity()
	der, err := x509.MarshalPKCS8PrivateKey(v3PrivKey)
	if err != nil {
		panic("fakecore: marshal v3 private key: " + err.Error())
	}
	block := &pem.Block{Type: "PRIVATE KEY", Bytes: der}
	return string(pem.EncodeToMemory(block))
}

// APIKey is the process-wide v3 obx_ API key every fake-core v3 server
// accepts.
func APIKey() string {
	ensureV3Identity()
	return v3APIKeyStr
}

// AgentID is the process-wide v3 workload agent id (a canonical UUID).
func AgentID() string {
	ensureV3Identity()
	return v3AgentIDStr
}

// attributionDIDFor restates devconfig.AttributionDIDFor's derivation; see
// the package doc comment for why this is a restatement rather than an
// import.
func attributionDIDFor(agentID string) (string, error) {
	agentUUID, err := uuid.Parse(agentID)
	if err != nil {
		return "", fmt.Errorf("fakecore: agent id %q is not a UUID: %w", agentID, err)
	}
	ns, err := uuid.Parse(v3AttributionAIPNamespace)
	if err != nil {
		return "", fmt.Errorf("fakecore: parse namespace: %w", err)
	}
	return "did:aip:" + uuid.NewSHA1(ns, []byte(agentUUID.String())).String(), nil
}

// AttributionDID is the derived did:aip: label for AgentID(), matching
// devconfig.AttributionDIDFor.
func AttributionDID() string {
	ensureV3Identity()
	did, err := attributionDIDFor(v3AgentIDStr)
	if err != nil {
		panic("fakecore: " + err.Error())
	}
	return did
}

// BootstrapHits counts every GET to the v3 bootstrap route.
func (f *Server) BootstrapHits() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.v3BootstrapHits
}

// ExchangeHits counts every POST to the fake Keycloak token endpoint.
func (f *Server) ExchangeHits() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.v3ExchangeHits
}

// V3EvaluateAttempts counts every request that reached the v3 evaluate route,
// accepted or not -- unlike Inbox, which holds only the accepted ones. A test
// proving "no resend" needs this: a held 401 is never in the inbox, but it
// must still count as one attempt.
func (f *Server) V3EvaluateAttempts() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.v3EvaluateAttempts
}

// TokenEndpointDown makes the next bootstrap document's token_endpoint point
// at an unregistered address, so the client's exchange call fails at the
// transport level (a real connection refused) rather than with a scripted
// HTTP status -- the one path that exercises Transient classification on a
// genuine network fault.
func (f *Server) TokenEndpointDown() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.v3TokenEndpointDown = true
}

// Revoke makes every subsequent v3 governance/validate call answer the flat
// 401 core sends for a rejected identity, regardless of whether the caller's
// workload token is otherwise valid and unexpired. Bootstrap and the token
// exchange are unaffected: this models an agent deactivated after a token was
// already issued.
func (f *Server) Revoke() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.v3Revoked = true
}

// Unrevoke reverses Revoke.
func (f *Server) Unrevoke() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.v3Revoked = false
}

// SetBootstrapFailure scripts the next (and every subsequent, until
// ClearBootstrapFailure) bootstrap call to answer status with a
// {"reason_code": reasonCode} body, instead of the canonical document.
func (f *Server) SetBootstrapFailure(status int, reasonCode string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.v3BootstrapFailStatus = status
	f.v3BootstrapFailReason = reasonCode
}

// ClearBootstrapFailure reverses SetBootstrapFailure.
func (f *Server) ClearBootstrapFailure() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.v3BootstrapFailStatus = 0
	f.v3BootstrapFailReason = ""
}

// SetExchangeFailure scripts the next (and every subsequent, until
// ClearExchangeFailure) token exchange to answer status with a Keycloak-shaped
// {"error": errCode, "error_description": ...} body, instead of verifying the
// assertion.
func (f *Server) SetExchangeFailure(status int, errCode string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.v3ExchangeFailStatus = status
	f.v3ExchangeFailError = errCode
}

// ClearExchangeFailure reverses SetExchangeFailure.
func (f *Server) ClearExchangeFailure() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.v3ExchangeFailStatus = 0
	f.v3ExchangeFailError = ""
}

// serveV3Bootstrap answers GET /api/v3/auth/bootstrap: API-key-only, no
// workload header, no body (mirroring workloadauth.Bootstrap's request
// shape).
func (f *Server) serveV3Bootstrap(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.v3BootstrapHits++
	failStatus := f.v3BootstrapFailStatus
	failReason := f.v3BootstrapFailReason
	down := f.v3TokenEndpointDown
	f.mu.Unlock()

	if got := r.Header.Get("Authorization"); got != "Bearer "+APIKey() {
		f.writeJSONV3(w, http.StatusUnauthorized, map[string]any{"reason_code": "invalid_api_key"})
		return
	}
	if failStatus != 0 {
		f.writeJSONV3(w, failStatus, map[string]any{"reason_code": failReason})
		return
	}

	issuer := f.URL() + "/realms/fake"
	if down {
		// A loopback address memhttptest never registers: a real dial that
		// gets a real connection-refused, so the client's exchange call fails
		// at the transport level rather than through a scripted status. The
		// document stays internally consistent (token_endpoint ==
		// issuer+suffix), or workloadauth's own strict validation would
		// reject it as malformed before the client ever tries to dial it.
		issuer = "http://127.0.0.1:1"
	}
	tokenEndpoint := issuer + "/protocol/openid-connect/token"

	doc := map[string]any{
		"bootstrap_version":  3,
		"contract_version":   3,
		"token_endpoint":     tokenEndpoint,
		"issuer":             issuer,
		"audience":           v3ClientIDStr,
		"client_id":          v3ClientIDStr,
		"service_account_id": v3AgentIDStr,
		"activation_version": v3ActVersion,
		"identity_source":    "openbox",
		"kid":                "fake-kid-" + v3AgentIDStr,
	}
	f.writeJSONV3(w, http.StatusOK, doc)
}

// serveV3Token answers the fake Keycloak token endpoint: verifies the RS256
// client assertion independently (crypto/rsa, not golang-jwt) and its claims,
// then issues an opaque bearer token this server remembers as valid.
func (f *Server) serveV3Token(w http.ResponseWriter, r *http.Request, raw []byte) {
	f.mu.Lock()
	f.v3ExchangeHits++
	failStatus := f.v3ExchangeFailStatus
	failErr := f.v3ExchangeFailError
	f.mu.Unlock()

	if failStatus != 0 {
		f.writeJSONV3(w, failStatus, map[string]any{"error": failErr, "error_description": "scripted failure"})
		return
	}

	form, err := url.ParseQuery(string(raw))
	if err != nil {
		f.writeJSONV3(w, http.StatusBadRequest, map[string]any{"error": "invalid_request", "error_description": err.Error()})
		return
	}
	if form.Get("grant_type") != "client_credentials" {
		f.writeJSONV3(w, http.StatusBadRequest, map[string]any{"error": "unsupported_grant_type"})
		return
	}
	if form.Get("client_assertion_type") != v3ClientAssertionType {
		f.writeJSONV3(w, http.StatusBadRequest, map[string]any{"error": "invalid_client", "error_description": "unsupported client_assertion_type"})
		return
	}

	wantAudience := f.URL() + "/realms/fake" + "/protocol/openid-connect/token"
	if err := verifyV3AssertionIndependently(form.Get("client_assertion"), &v3PrivKey.PublicKey, v3ClientIDStr, wantAudience, time.Now()); err != nil {
		f.writeJSONV3(w, http.StatusBadRequest, map[string]any{"error": "invalid_client", "error_description": err.Error()})
		return
	}

	tokenBytes := make([]byte, 24)
	if _, err := rand.Read(tokenBytes); err != nil {
		f.writeJSONV3(w, http.StatusInternalServerError, map[string]any{"error": "server_error"})
		return
	}
	token := "fake-workload-token-" + base64.RawURLEncoding.EncodeToString(tokenBytes)

	f.mu.Lock()
	if f.v3IssuedTokens == nil {
		f.v3IssuedTokens = map[string]bool{}
	}
	f.v3IssuedTokens[token] = true
	f.mu.Unlock()

	f.writeJSONV3(w, http.StatusOK, map[string]any{
		"access_token": token,
		"token_type":   "Bearer",
		"expires_in":   300,
	})
}

// v3AuthOK checks the runtime-route envelope: the obx_ API key on
// Authorization (unlike v1, where that header never carries the key alone)
// and a workload token this server itself issued and has not revoked.
func (f *Server) v3AuthOK(r *http.Request) bool {
	if r.Header.Get("Authorization") != "Bearer "+APIKey() {
		return false
	}
	tok := r.Header.Get("X-OpenBox-Workload-Token")
	if tok == "" {
		return false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.v3Revoked {
		return false
	}
	return f.v3IssuedTokens[tok]
}

// writeV3Unauthorized answers the flat v3 401 core sends for evaluate,
// approval and validate: no reason_code, ever.
func (f *Server) writeV3Unauthorized(w http.ResponseWriter) {
	f.writeJSONV3(w, http.StatusUnauthorized, map[string]any{"code": 401, "message": "invalid token or agent identity"})
}

func (f *Server) serveV3Evaluate(w http.ResponseWriter, r *http.Request, raw []byte) {
	f.mu.Lock()
	f.v3EvaluateAttempts++
	f.mu.Unlock()

	if !f.v3AuthOK(r) {
		f.writeV3Unauthorized(w)
		return
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		f.reject(w, http.StatusBadRequest, "body is not a JSON object")
		return
	}
	rec := Received{Headers: r.Header.Clone(), Raw: raw, Body: body}
	if reasons := checkWireShape(body); len(reasons) > 0 {
		f.reject(w, http.StatusBadRequest, "wire shape: "+strings.Join(reasons, "; "))
		return
	}

	f.mu.Lock()
	if f.outage {
		f.scripted++
		f.mu.Unlock()
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	status, verdict := f.script.answer(rec.ToolUseID())
	if status >= 200 && status < 300 {
		f.inbox = append(f.inbox, rec)
	} else {
		f.scripted++
	}
	delay := f.script.Delay
	f.mu.Unlock()

	// Same knob as the v1 route (fakecore.go's serveEvaluate): a scenario
	// scripting a slow control plane to exercise a caller's own budget timeout
	// must get the same behavior regardless of which mode the client under
	// test speaks.
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, verdict)
}

func (f *Server) serveV3Approval(w http.ResponseWriter, r *http.Request, raw []byte) {
	if !f.v3AuthOK(r) {
		f.writeV3Unauthorized(w)
		return
	}
	var body map[string]any
	_ = json.Unmarshal(raw, &body)

	f.mu.Lock()
	f.approvalPolls++
	fn := f.approval
	f.mu.Unlock()

	if fn == nil {
		f.reject(w, http.StatusNotFound, "approval polled but no handler is installed")
		return
	}
	status, resp := fn(Received{Headers: r.Header.Clone(), Raw: raw, Body: body})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, resp)
}

func (f *Server) serveV3Validate(w http.ResponseWriter, r *http.Request) {
	if !f.v3AuthOK(r) {
		f.writeV3Unauthorized(w)
		return
	}
	f.writeJSONV3(w, http.StatusOK, map[string]any{
		"valid":      true,
		"active":     true,
		"agent_id":   v3AgentIDStr,
		"agent_name": "fake-workload-agent",
	})
}

func (f *Server) writeJSONV3(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// v3AssertionClaims is the RS256 client assertion's payload, restated from
// workloadauth.BuildAssertion's claim set rather than imported (see the
// package doc comment).
type v3AssertionClaims struct {
	Aud string `json:"aud"`
	Exp int64  `json:"exp"`
	Iat int64  `json:"iat"`
	Iss string `json:"iss"`
	Jti string `json:"jti"`
	Sub string `json:"sub"`
}

type v3AssertionHeader struct {
	Alg string `json:"alg"`
	Kid string `json:"kid"`
	Typ string `json:"typ"`
}

// verifyV3AssertionIndependently checks the RS256 client assertion using
// crypto/rsa directly: header.payload signed over its own bytes, never
// through golang-jwt (the client's own dependency), which is the entire
// point of a fake that shares no code with what it grades.
func verifyV3AssertionIndependently(assertion string, pub *rsa.PublicKey, wantClientID, wantAudience string, now time.Time) error {
	parts := strings.Split(assertion, ".")
	if len(parts) != 3 {
		return fmt.Errorf("assertion is not a 3-part JWT")
	}

	headerRaw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return fmt.Errorf("decode header: %w", err)
	}
	var header v3AssertionHeader
	if err := json.Unmarshal(headerRaw, &header); err != nil {
		return fmt.Errorf("parse header: %w", err)
	}
	if header.Alg != "RS256" {
		return fmt.Errorf("alg %q is not RS256", header.Alg)
	}

	payloadRaw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return fmt.Errorf("decode payload: %w", err)
	}
	var claims v3AssertionClaims
	if err := json.Unmarshal(payloadRaw, &claims); err != nil {
		return fmt.Errorf("parse claims: %w", err)
	}

	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return fmt.Errorf("decode signature: %w", err)
	}

	signedInput := parts[0] + "." + parts[1]
	hashed := sha256.Sum256([]byte(signedInput))
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, hashed[:], sig); err != nil {
		return fmt.Errorf("signature does not verify: %w", err)
	}

	if claims.Iss != wantClientID || claims.Sub != wantClientID {
		return fmt.Errorf("iss/sub do not match the issued client_id")
	}
	if claims.Aud != wantAudience {
		return fmt.Errorf("aud %q does not match the token endpoint", claims.Aud)
	}
	if claims.Jti == "" {
		return fmt.Errorf("jti is empty")
	}
	if claims.Exp-claims.Iat != 60 {
		return fmt.Errorf("assertion lifetime is not exactly 60s")
	}
	if !now.Before(time.Unix(claims.Exp, 0)) {
		return fmt.Errorf("assertion is expired")
	}
	return nil
}
