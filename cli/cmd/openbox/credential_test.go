package main

import (
	"strings"
	"testing"
)

// The mistake this exists for: the dashboard shows an agent's runtime key on the
// agent page, so that is what people paste. It is minted BY onboarding, and the
// control plane answers a bare 401 — indistinguishable from an expired key, a
// missing permission, or a broken install.
func TestControlTokenProblemNamesTheWrongKey(t *testing.T) {
	agentKey := "obx_test_8ab210d6acd7847f3135e4c4a14349fe"
	problem := controlTokenProblem(agentKey)
	if problem == "" {
		t.Fatal("an agent runtime key was accepted as a control credential")
	}
	for _, want := range []string{"AGENT RUNTIME key", "Organization", "obx_key_", "getting-started"} {
		if !strings.Contains(problem, want) {
			t.Errorf("the message does not mention %q:\n%s", want, problem)
		}
	}
	// INV-1: recognisable, not disclosable.
	if strings.Contains(problem, agentKey) {
		t.Error("the message echoes the whole credential")
	}
	if !strings.Contains(problem, "obx_test_8ab") {
		t.Errorf("the message does not show the public prefix, so a user cannot tell which key it means:\n%s", problem)
	}
}

func TestControlTokenProblemAcceptsRealCredentials(t *testing.T) {
	for _, ok := range []string{
		"",                         // unset is handled by the caller's own message
		"obx_key_abcdef0123456789", // an org key
		"header.payload.signature", // a Keycloak JWT
	} {
		if problem := controlTokenProblem(ok); problem != "" {
			t.Errorf("controlTokenProblem(%q) rejected a usable credential: %s", ok, problem)
		}
	}
	if controlTokenProblem("not-a-credential") == "" {
		t.Error("an unrecognisable value was accepted silently")
	}
}

// The other silent one: a self-hosted control plane with the default data plane.
// Registration succeeds, the config looks right, and `dev verify` then returns 401
// from a URL the operator never chose.
func TestSelfHostedWithoutDataPlaneWarns(t *testing.T) {
	warn := []string{
		"http://localhost:3000",
		"http://127.0.0.1:3000",
		"https://openbox.internal",
		"https://10.0.0.5:3000",
		"https://172.16.4.4",
		"https://192.168.1.10:3000",
		"http://openbox-backend:3000", // single-label service name
	}
	for _, u := range warn {
		if !selfHostedWithoutDataPlane(u, "") {
			t.Errorf("no warning for a self-hosted backend %q", u)
		}
		if selfHostedWithoutDataPlane(u, "http://localhost:8086") {
			t.Errorf("warned even though --base-url was given (%q)", u)
		}
	}
	for _, u := range []string{"https://api.openbox.ai", "https://openbox.example.com", "", "not a url"} {
		if selfHostedWithoutDataPlane(u, "") {
			t.Errorf("warned for a hosted or unparseable backend %q", u)
		}
	}
}
