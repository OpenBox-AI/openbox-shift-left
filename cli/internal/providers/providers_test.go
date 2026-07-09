package providers

import (
	"errors"
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/provider"
)

func TestClaudeCodeIsRealAndOthersAreStubs(t *testing.T) {
	inst, err := Lookup("claude-code")
	if err != nil {
		t.Fatalf("Lookup(claude-code): %v", err)
	}
	if !inst.Available() {
		t.Error("claude-code must be a real installer (SL4-WIRE-1), not a stub")
	}
	if inst.Name() != provider.ClaudeCode {
		t.Errorf("Name = %q", inst.Name())
	}

	for _, name := range []string{"codex", "cursor"} {
		inst, err := Lookup(name)
		if err != nil {
			t.Fatalf("Lookup(%q): %v", name, err)
		}
		if inst.Available() {
			t.Errorf("%q should still be a stub until its adapter ships", name)
		}
	}
}

func TestLookupUnknown(t *testing.T) {
	_, err := Lookup("emacs")
	if !errors.Is(err, provider.ErrUnknown) {
		t.Fatalf("Lookup unknown = %v, want ErrUnknown", err)
	}
	if !strings.Contains(err.Error(), "claude-code") {
		t.Errorf("unknown-provider error should list supported names: %v", err)
	}
}

func TestStubPlanReferencesSecretStoreNeverASecret(t *testing.T) {
	inst, _ := Lookup("codex")
	ref := provider.CredentialRef{
		SecretService:     "ai.openbox.dev",
		APIKeyAccount:     "acme/codex/api_key",
		PrivateKeyAccount: "acme/codex/private_key",
		DID:               "did:aip:abc",
	}
	if err := inst.Install(ref); !errors.Is(err, provider.ErrNotBuilt) {
		t.Fatalf("Install = %v, want ErrNotBuilt", err)
	}
	plan := inst.Plan(ref)
	if !strings.Contains(plan, "ai.openbox.dev") || !strings.Contains(plan, "did:aip:abc") {
		t.Errorf("plan missing secret-store reference:\n%s", plan)
	}
	if strings.Contains(plan, "obx_") {
		t.Errorf("stub plan leaked a credential value:\n%s", plan)
	}
}
