package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/adapters/common/devconfig"
)

const rotateTestDID = "did:aip:3f2504e0-4f89-11d3-9a0c-0305e82c3301"

// rotateBackend serves both rotation endpoints, recording which were called.
func rotateBackend(t *testing.T, keyBody, identityBody string, keyStatus, identityStatus int) (*httptest.Server, *[]string) {
	t.Helper()
	var called []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/rotate-api-key"):
			called = append(called, "key")
			w.WriteHeader(keyStatus)
			_, _ = w.Write([]byte(keyBody))
		case strings.HasSuffix(r.URL.Path, "/identity/rotate"):
			called = append(called, "identity")
			w.WriteHeader(identityStatus)
			_, _ = w.Write([]byte(identityBody))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &called
}

func rotateApp(t *testing.T, srvURL string) (*app, string) {
	t.Helper()
	a, _, home := rotateAppFull(t, srvURL)
	return a, home
}

func rotateAppWithErr(t *testing.T, srvURL string) (*app, string, *bytes.Buffer) {
	t.Helper()
	a, errb, home := rotateAppFull(t, srvURL)
	return a, home, errb
}

func rotateAppFull(t *testing.T, srvURL string) (*app, *bytes.Buffer, string) {
	t.Helper()
	home := isolateHome(t)
	a, _, errb := testApp(map[string]string{
		devconfig.EnvControlToken: "obx_key_" + strings.Repeat("f", 48),
		devconfig.EnvBackendURL:   srvURL,
	})
	return a, errb, home
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// The happy path: a new key and signing key land on disk, and the DID is
// unchanged — which is what makes rotation the recovery that KEEPS the agent.
func TestRotateWritesNewCredentialsAndPreservesTheDID(t *testing.T) {
	newSeed := make([]byte, 32)
	newSeed[0] = 7
	newSeedB64 := base64.StdEncoding.EncodeToString(newSeed)
	srv, called := rotateBackend(t,
		`{"token":"obx_rotated_key"}`,
		`{"did":"`+rotateTestDID+`","privateKey":"`+newSeedB64+`"}`,
		http.StatusOK, http.StatusOK)

	a, home := rotateApp(t, srv.URL)
	// A machine whose credentials are stale but whose coordinates are known.
	if err := devconfig.WriteConfig(filepath.Join(home, "dev.json"), devconfig.Update{
		DID: rotateTestDID, AgentID: "agent-1", BackendURL: srv.URL,
	}); err != nil {
		t.Fatal(err)
	}

	if code := a.run([]string{"auth", "--rotate", "--yes"}); code != exitOK {
		t.Fatalf("exit = %d", code)
	}
	if len(*called) != 2 || (*called)[0] != "key" || (*called)[1] != "identity" {
		// Key first: a failure between the two then leaves a working signing
		// identity with a dead key, which --rotate can retry.
		t.Errorf("call order = %v, want [key identity]", *called)
	}
	kv := readEnvFile(t, home)
	if kv[devconfig.EnvAPIKeyDirect] != "obx_rotated_key" {
		t.Errorf("api key = %q, want the rotated value", kv[devconfig.EnvAPIKeyDirect])
	}
	if kv[devconfig.EnvAgentPrivateKey] != newSeedB64 {
		t.Errorf("signing key = %q, want the rotated value", kv[devconfig.EnvAgentPrivateKey])
	}
	if cfg := readDevJSON(t, home); cfg.DID != rotateTestDID {
		t.Errorf("DID = %q, want it preserved as %q", cfg.DID, rotateTestDID)
	}
}

// No implicit rotation, ever: without --rotate no rotate endpoint is called.
func TestNoRotateFlagCallsNoRotateEndpoint(t *testing.T) {
	srv, called := rotateBackend(t, `{"token":"x"}`, `{"did":"d","privateKey":"k"}`, http.StatusOK, http.StatusOK)
	a, _ := rotateApp(t, srv.URL)
	a.stdin = strings.NewReader("obx_k\n" + testSeedB64 + "\n")
	if code := a.run([]string{"auth", "--api-key-stdin", "--private-key-stdin", "--yes", "--did", rotateTestDID}); code != exitOK {
		t.Fatalf("exit = %d", code)
	}
	if len(*called) != 0 {
		t.Errorf("rotation endpoints were called without --rotate: %v", *called)
	}
}

// A 2xx that omits privateKey must produce NO write: a half-valid credential pair
// on disk looks configured and fails at flush time.
func TestRotateWritesNothingWhenTheIdentityReplyIsUnusable(t *testing.T) {
	srv, _ := rotateBackend(t,
		`{"token":"obx_rotated_key"}`,
		`{"did":"`+rotateTestDID+`","key_id":"k"}`, // no privateKey
		http.StatusOK, http.StatusOK)

	a, home := rotateApp(t, srv.URL)
	envPath := filepath.Join(home, ".env")
	if err := devconfig.WriteEnvFile(envPath, map[string]string{
		devconfig.EnvAPIKeyDirect:    "obx_original",
		devconfig.EnvAgentPrivateKey: testSeedB64,
	}); err != nil {
		t.Fatal(err)
	}
	if err := devconfig.WriteConfig(filepath.Join(home, "dev.json"), devconfig.Update{
		DID: rotateTestDID, AgentID: "agent-1", BackendURL: srv.URL,
	}); err != nil {
		t.Fatal(err)
	}

	if code := a.run([]string{"auth", "--rotate", "--yes"}); code != exitError {
		t.Fatalf("exit = %d, want an error", code)
	}
	kv := readEnvFile(t, home)
	if kv[devconfig.EnvAPIKeyDirect] != "obx_original" || kv[devconfig.EnvAgentPrivateKey] != testSeedB64 {
		t.Errorf("the credential file was modified despite the failure: %v", kv)
	}
}

// The API key rotation already succeeded by then, so the error must say so —
// otherwise the user retries and cannot understand why the old key stopped
// working "before" anything happened.
func TestRotateSaysTheKeyIsAlreadyInvalidWhenIdentityFails(t *testing.T) {
	srv, _ := rotateBackend(t,
		`{"token":"obx_rotated_key"}`,
		`{"message":"Agent identity has not been provisioned"}`,
		http.StatusOK, http.StatusNotFound)

	a, home, errb := rotateAppWithErr(t, srv.URL)
	if err := devconfig.WriteConfig(filepath.Join(home, "dev.json"), devconfig.Update{
		DID: rotateTestDID, AgentID: "agent-1", BackendURL: srv.URL,
	}); err != nil {
		t.Fatal(err)
	}
	if code := a.run([]string{"auth", "--rotate", "--yes"}); code != exitError {
		t.Fatalf("exit = %d, want an error", code)
	}
	s := errb.String()
	if !strings.Contains(s, "already succeeded") {
		t.Errorf("error should say the key rotation already happened:\n%s", s)
	}
	if !strings.Contains(s, "no signing identity provisioned") {
		t.Errorf("error should carry the not-provisioned distinction:\n%s", s)
	}
}

// Rotation needs an agent id: there is nothing to rotate without one, and the
// error must distinguish it from "register a new agent".
func TestRotateWithoutAnAgentIDExplainsWhatToDo(t *testing.T) {
	srv, called := rotateBackend(t, `{}`, `{}`, http.StatusOK, http.StatusOK)
	a, _, errb := rotateAppWithErr(t, srv.URL)
	if code := a.run([]string{"auth", "--rotate", "--yes"}); code != exitError {
		t.Fatalf("exit = %d, want an error with no agent id", code)
	}
	if !strings.Contains(errb.String(), "agent id") {
		t.Errorf("error should name the missing agent id:\n%s", errb.String())
	}
	if len(*called) != 0 {
		t.Errorf("no endpoint should be called without an agent id: %v", *called)
	}
}

// Rotation is destructive server-side, so it needs an org key and must say which
// kind when it does not have one.
func TestRotateWithoutAnOrgKeyRefuses(t *testing.T) {
	srv, called := rotateBackend(t, `{}`, `{}`, http.StatusOK, http.StatusOK)
	home := isolateHome(t)
	if err := devconfig.WriteConfig(filepath.Join(home, "dev.json"), devconfig.Update{
		DID: rotateTestDID, AgentID: "agent-1", BackendURL: srv.URL,
	}); err != nil {
		t.Fatal(err)
	}
	a, _, errb := testApp(map[string]string{devconfig.EnvBackendURL: srv.URL})
	if code := a.run([]string{"auth", "--rotate", "--yes"}); code != exitError {
		t.Fatalf("exit = %d, want an error", code)
	}
	if !strings.Contains(errb.String(), devconfig.EnvControlToken) {
		t.Errorf("error should name the credential it needs:\n%s", errb.String())
	}
	// The alternative — register a fresh agent — must be offered, because plenty
	// of developers have no org key.
	if !strings.Contains(errb.String(), "register a fresh agent") {
		t.Errorf("error should offer the no-org-key alternative:\n%s", errb.String())
	}
	if len(*called) != 0 {
		t.Errorf("no endpoint should be called without a credential: %v", *called)
	}
}

// A rotation that returned a DIFFERENT DID must be refused: the DID is derived
// from the agent id, so it cannot change, and silently adopting a new one would
// re-attribute this machine's history.
func TestRotateRefusesAChangedDID(t *testing.T) {
	srv, _ := rotateBackend(t,
		`{"token":"obx_rotated_key"}`,
		`{"did":"did:aip:99999999-9999-9999-9999-999999999999","privateKey":"`+testSeedB64+`"}`,
		http.StatusOK, http.StatusOK)

	home := isolateHome(t)
	if err := devconfig.WriteConfig(filepath.Join(home, "dev.json"), devconfig.Update{
		DID: rotateTestDID, AgentID: "agent-1", BackendURL: srv.URL,
	}); err != nil {
		t.Fatal(err)
	}
	a, _, errb := testApp(map[string]string{
		devconfig.EnvControlToken: "obx_key_" + strings.Repeat("f", 48),
		devconfig.EnvBackendURL:   srv.URL,
	})
	if code := a.run([]string{"auth", "--rotate", "--yes"}); code != exitError {
		t.Fatalf("exit = %d, want a refusal", code)
	}
	if !strings.Contains(errb.String(), "must preserve the DID") {
		t.Errorf("error should explain the DID invariant:\n%s", errb.String())
	}
	// Nothing written.
	if _, err := devconfig.ParseEnvFile(filepath.Join(home, ".env")); err != nil {
		t.Fatal(err)
	}
	if kv := readEnvFile(t, home); len(kv) != 0 {
		t.Errorf("credentials were written despite the refusal: %v", kv)
	}
}

// An org key returned in the agent's runtime slot must be refused: it would give
// the hook org-wide authority.
func TestRotateRefusesAnOrgKeyInTheRuntimeSlot(t *testing.T) {
	srv, _ := rotateBackend(t,
		`{"token":"obx_key_`+strings.Repeat("f", 48)+`"}`,
		`{"did":"`+rotateTestDID+`","privateKey":"`+testSeedB64+`"}`,
		http.StatusOK, http.StatusOK)

	home := isolateHome(t)
	if err := devconfig.WriteConfig(filepath.Join(home, "dev.json"), devconfig.Update{
		DID: rotateTestDID, AgentID: "agent-1", BackendURL: srv.URL,
	}); err != nil {
		t.Fatal(err)
	}
	a, _, errb := testApp(map[string]string{
		devconfig.EnvControlToken: "obx_key_" + strings.Repeat("f", 48),
		devconfig.EnvBackendURL:   srv.URL,
	})
	if code := a.run([]string{"auth", "--rotate", "--yes"}); code != exitError {
		t.Fatalf("exit = %d, want a refusal", code)
	}
	if !strings.Contains(errb.String(), "ORGANIZATION key") {
		t.Errorf("error should identify the wrong credential type:\n%s", errb.String())
	}
	if kv := readEnvFile(t, home); len(kv) != 0 {
		t.Errorf("credentials were written despite the refusal: %v", kv)
	}
}

// Rotation must not silently rewrite posture, exactly as an ordinary auth run
// must not.
func TestRotateLeavesPostureAlone(t *testing.T) {
	newSeed := make([]byte, 32)
	newSeed[1] = 3
	srv, _ := rotateBackend(t,
		`{"token":"obx_rotated"}`,
		`{"did":"`+rotateTestDID+`","privateKey":"`+base64.StdEncoding.EncodeToString(newSeed)+`"}`,
		http.StatusOK, http.StatusOK)

	home := isolateHome(t)
	devPath := filepath.Join(home, "dev.json")
	tr := true
	if err := devconfig.WriteConfig(devPath, devconfig.Update{
		DID: rotateTestDID, AgentID: "agent-1", BackendURL: srv.URL,
		Enforce: &tr, Tier2: &tr, Findings: &tr,
	}); err != nil {
		t.Fatal(err)
	}
	var before map[string]any
	raw := mustReadFile(t, devPath)
	if err := json.Unmarshal(raw, &before); err != nil {
		t.Fatal(err)
	}

	a, _, errb := testApp(map[string]string{
		devconfig.EnvControlToken: "obx_key_" + strings.Repeat("f", 48),
		devconfig.EnvBackendURL:   srv.URL,
	})
	if code := a.run([]string{"auth", "--rotate", "--yes"}); code != exitOK {
		t.Fatalf("exit = %d; stderr=%q", code, errb.String())
	}
	var after map[string]any
	if err := json.Unmarshal(mustReadFile(t, devPath), &after); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"enforce", "tier2", "findings"} {
		if before[field] != after[field] {
			t.Errorf("posture field %q changed across a rotation: %v → %v", field, before[field], after[field])
		}
	}
}
