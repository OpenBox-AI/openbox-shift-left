package provider

import (
	"reflect"
	"strings"
	"testing"
)

func TestSupportedIsSortedAndComplete(t *testing.T) {
	got := Supported()
	want := []string{"claude-code", "codex", "muse"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("Supported() = %v, want %v", got, want)
	}
}

// TestCredentialRefCarriesOnlySafeFields a CredentialRef must never be able to
// carry a raw secret value (INV-1).
func TestCredentialRefCarriesOnlySafeFields(t *testing.T) {
	allowed := map[string]bool{
		"BaseURL": true, "ContentCapture": true, "InstallGitHook": true,
		"AgentID": true, "BackendURL": true, "ProjectDir": true,
		"Enforce": true, "Tier2": true, "Findings": true,
		// IdentityMethod is "keycloak_workload" or "", a store-shape marker, not
		// a credential (devconfig.IdentityMethodKeycloakWorkload).
		"IdentityMethod": true,
	}
	rt := reflect.TypeOf(CredentialRef{})
	for i := 0; i < rt.NumField(); i++ {
		name := rt.Field(i).Name
		if !allowed[name] {
			t.Errorf("CredentialRef gained field %q; if it carries a credential value it breaks INV-1; "+
				"if it is genuinely non-secret, add it to this allowlist deliberately", name)
		}
		for _, banned := range []string{"APIKey", "PrivateKey", "Secret", "Token", "Password", "Seed"} {
			if strings.Contains(name, banned) {
				t.Errorf("CredentialRef field %q looks like a credential; installers must never receive one", name)
			}
		}
	}
}
