package provider

import (
	"errors"
	"strings"
	"testing"
)

func TestLookupKnownAndUnknown(t *testing.T) {
	for _, name := range []string{"claude-code", "codex", "cursor"} {
		inst, err := Lookup(name)
		if err != nil {
			t.Fatalf("Lookup(%q): %v", name, err)
		}
		if inst.Available() {
			t.Errorf("%q should be unavailable until its adapter is built", name)
		}
	}
	_, err := Lookup("emacs")
	if !errors.Is(err, ErrUnknown) {
		t.Fatalf("Lookup unknown = %v, want ErrUnknown", err)
	}
}

func TestStubInstallReturnsNotBuiltAndPlanReferencesSecretStore(t *testing.T) {
	inst, _ := Lookup("claude-code")
	ref := CredentialRef{
		SecretService:     "ai.openbox.dev",
		APIKeyAccount:     "acme/claude-code/api_key",
		PrivateKeyAccount: "acme/claude-code/private_key",
		DID:               "did:aip:abc",
	}
	if err := inst.Install(ref); !errors.Is(err, ErrNotBuilt) {
		t.Fatalf("Install = %v, want ErrNotBuilt", err)
	}
	plan := inst.Plan(ref)
	// Plan must point at the secret store, never carry a secret value.
	if !strings.Contains(plan, "ai.openbox.dev") || !strings.Contains(plan, "did:aip:abc") {
		t.Errorf("plan missing secret-store reference:\n%s", plan)
	}
}
