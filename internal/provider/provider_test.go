package provider

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestSupportedIsSortedAndComplete(t *testing.T) {
	got := Supported()
	want := []string{"claude-code", "codex", "cursor"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("Supported() = %v, want %v", got, want)
	}
}

func TestStubIsUnavailableAndDescribesManualConfig(t *testing.T) {
	ref := CredentialRef{DID: "did:aip:abc"}
	s := Stub{
		ProviderName: Codex,
		Manual: func(r CredentialRef) string {
			return "manual: did=" + r.DID
		},
	}

	if s.Name() != Codex {
		t.Errorf("Name = %q", s.Name())
	}
	if s.Available() {
		t.Error("a stub must report Available()==false")
	}
	if err := s.Install(ref); !errors.Is(err, ErrNotBuilt) {
		t.Fatalf("Install = %v, want ErrNotBuilt", err)
	}
	plan := s.Plan(ref)
	if !strings.Contains(plan, "did:aip:abc") {
		t.Errorf("plan does not name the DID it would install for:\n%s", plan)
	}
}

// TestCredentialRefCarriesOnlySafeFields a CredentialRef must never be able to
// carry a raw secret value (INV-1).
func TestCredentialRefCarriesOnlySafeFields(t *testing.T) {
	allowed := map[string]bool{
		"DID": true, "BaseURL": true, "ContentCapture": true, "InstallGitHook": true,
		"AgentID": true, "BackendURL": true, "ProjectDir": true,
		"Enforce": true, "Tier2": true, "Findings": true,
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

// TestStubPlanFallsBackWhenManualIsNil a Stub built without a Manual func must
// not panic on Plan (the observe/ --dry-run path); it falls back to a generic
// message naming the provider.
func TestStubPlanFallsBackWhenManualIsNil(t *testing.T) {
	s := Stub{ProviderName: Cursor} // Manual left nil
	plan := s.Plan(CredentialRef{DID: "did:aip:x"})
	if !strings.Contains(plan, "cursor") {
		t.Errorf("nil-Manual fallback should name the provider, got %q", plan)
	}
}
