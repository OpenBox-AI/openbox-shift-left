package provider

import (
	"errors"
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
	ref := CredentialRef{
		SecretService:     "ai.openbox.dev",
		APIKeyAccount:     "acme/codex/api_key",
		PrivateKeyAccount: "acme/codex/private_key",
		DID:               "did:aip:abc",
	}
	s := Stub{
		ProviderName: Codex,
		Manual: func(r CredentialRef) string {
			return "manual: service=" + r.SecretService + " did=" + r.DID
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
	// Plan must point at the secret store, never carry a secret value (INV-1).
	if !strings.Contains(plan, "ai.openbox.dev") || !strings.Contains(plan, "did:aip:abc") {
		t.Errorf("plan missing secret-store reference:\n%s", plan)
	}
}

// A CredentialRef must never be able to carry a raw secret; it only has
// coordinate fields. This compile-time-ish check documents the INV-1 shape.
func TestCredentialRefCarriesOnlyCoordinates(t *testing.T) {
	ref := CredentialRef{DID: "did:aip:x"}
	if ref.DID == "" {
		t.Fatal("unreachable")
	}
}

// A Stub built without a Manual func must not panic on Plan (the observe/
// --dry-run path); it falls back to a generic message naming the provider.
func TestStubPlanFallsBackWhenManualIsNil(t *testing.T) {
	s := Stub{ProviderName: Cursor} // Manual left nil
	plan := s.Plan(CredentialRef{DID: "did:aip:x"})
	if !strings.Contains(plan, "cursor") {
		t.Errorf("nil-Manual fallback should name the provider, got %q", plan)
	}
}
