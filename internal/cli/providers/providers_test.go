package providers

import (
	"errors"
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/provider"
)

// TestEverySupportedProviderResolvesToARealInstaller. There is no not-built
// state left in the SPI: a name either has an adapter or is refused as unknown,
// so a stub that reported success while installing nothing cannot exist.
func TestEverySupportedProviderResolvesToARealInstaller(t *testing.T) {
	for _, name := range provider.Supported() {
		inst, err := Lookup(name)
		if err != nil {
			t.Fatalf("Lookup(%q) is in Supported() but does not resolve: %v", name, err)
		}
		if string(inst.Name()) != name {
			t.Errorf("Lookup(%q).Name() = %q", name, inst.Name())
		}
	}
}

// TestCursorIsNowAnUnknownProvider. The stub advertised a provider whose
// adapter had not shipped, and `init` exited non-zero for it after printing
// manual instructions. Refusing the name outright is the honest answer, and it
// travels through the same path as any typo.
func TestCursorIsNowAnUnknownProvider(t *testing.T) {
	_, err := Lookup("cursor")
	if !errors.Is(err, provider.ErrUnknown) {
		t.Fatalf("Lookup(cursor) = %v, want ErrUnknown", err)
	}
	if !strings.Contains(err.Error(), "claude-code, codex") {
		t.Errorf("the refusal should list what IS supported: %v", err)
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
