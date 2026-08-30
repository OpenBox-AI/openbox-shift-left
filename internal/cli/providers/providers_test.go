package providers

import (
	"errors"
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/provider"
)

func TestBuiltProvidersAreRealAndCursorIsStub(t *testing.T) {
	// claude-code (SL4-WIRE-1) and codex (STORY-SL7-A) are real installers.
	for name, want := range map[string]provider.Name{
		"claude-code": provider.ClaudeCode,
		"codex":       provider.Codex,
	} {
		inst, err := Lookup(name)
		if err != nil {
			t.Fatalf("Lookup(%q): %v", name, err)
		}
		if !inst.Available() {
			t.Errorf("%q must be a real installer, not a stub", name)
		}
		if inst.Name() != want {
			t.Errorf("%q Name = %q", name, inst.Name())
		}
	}

	// cursor stays a stub until SL-8 builds its adapter.
	inst, err := Lookup("cursor")
	if err != nil {
		t.Fatalf("Lookup(cursor): %v", err)
	}
	if inst.Available() {
		t.Error("cursor should still be a stub until its adapter ships")
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

func TestStubPlanNamesTheIdentityNeverASecret(t *testing.T) {
	inst, _ := Lookup("cursor")
	ref := provider.CredentialRef{DID: "did:aip:abc"}
	if err := inst.Install(ref); !errors.Is(err, provider.ErrNotBuilt) {
		t.Fatalf("Install = %v, want ErrNotBuilt", err)
	}
	plan := inst.Plan(ref)
	// The plan names the identity and where credentials come from — never a
	// secret-store location, since that decision deleted the store.
	if !strings.Contains(plan, "did:aip:abc") || !strings.Contains(plan, ".env") {
		t.Errorf("plan should name the DID and the credential file:\n%s", plan)
	}
	if strings.Contains(plan, "obx_") {
		t.Errorf("stub plan leaked a credential value:\n%s", plan)
	}
}

// STORY-SL7-A AC-2: the codex installer resolves the running engine into its
// hook commands and its plan surfaces the /hooks trust step (never a secret).
func TestCodexInstallerPlanSurfacesTrustStep(t *testing.T) {
	inst, err := Lookup("codex")
	if err != nil {
		t.Fatal(err)
	}
	plan := inst.Plan(provider.CredentialRef{DID: "did:aip:abc"})
	for _, want := range []string{"/hooks", "hook codex", "did:aip:abc"} {
		if !strings.Contains(plan, want) {
			t.Errorf("codex plan missing %q:\n%s", want, plan)
		}
	}
	if strings.Contains(plan, "obx_") {
		t.Errorf("codex plan leaked a credential value:\n%s", plan)
	}
}
