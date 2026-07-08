package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/cli/internal/backend"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/devinit"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/secret"
)

// fakeReg implements devinit.Registrar for the command-wiring tests.
type fakeReg struct {
	reg    *backend.Registration
	create int
}

func (f *fakeReg) Create(context.Context, backend.CreateAgentRequest) (*backend.Registration, error) {
	f.create++
	return f.reg, nil
}
func (f *fakeReg) FindByName(context.Context, string) (*backend.AgentSummary, error) {
	return nil, nil
}

// testApp builds an app with in-memory writers and seams that fail loudly if a
// path touches the environment/keychain/network when it should not.
func testApp(env map[string]string) (*app, *bytes.Buffer, *bytes.Buffer) {
	var out, errb bytes.Buffer
	a := &app{
		stdout: &out,
		stderr: &errb,
		getenv: func(k string) string { return env[k] },
		detectStore: func() (secret.Store, error) {
			return nil, errors.New("detectStore should not be called in this path")
		},
		newRegistrar: func(_, _, _ string) devinit.Registrar {
			panic("newRegistrar should not be called in this path")
		},
	}
	return a, &out, &errb
}

func TestVersion(t *testing.T) {
	a, out, _ := testApp(nil)
	if code := a.run([]string{"version"}); code != exitOK {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(out.String(), "openbox ") {
		t.Errorf("version output = %q", out.String())
	}
}

func TestDryRunIsOfflineAndNeedsNoToken(t *testing.T) {
	a, out, _ := testApp(nil) // empty env; detect/registrar seams panic if touched
	code := a.run([]string{"dev", "init", "--provider", "claude-code", "--dry-run", "--org", "acme"})
	if code != exitOK {
		t.Fatalf("dry-run exit = %d", code)
	}
	if !strings.Contains(out.String(), "DRY RUN") {
		t.Errorf("missing DRY RUN banner: %q", out.String())
	}
}

func TestMissingTokenIsINV1Guard(t *testing.T) {
	a, _, errb := testApp(nil)
	code := a.run([]string{"dev", "init", "--provider", "claude-code", "--backend-url", "https://x"})
	if code != exitError {
		t.Fatalf("exit = %d, want %d", code, exitError)
	}
	if !strings.Contains(errb.String(), "OPENBOX_CONTROL_TOKEN") || !strings.Contains(errb.String(), "INV-1") {
		t.Errorf("expected INV-1 token guard, got %q", errb.String())
	}
}

func TestNoSecretStoreHalts(t *testing.T) {
	a, _, errb := testApp(map[string]string{"OPENBOX_CONTROL_TOKEN": "obx_key_x", "OPENBOX_BACKEND_URL": "https://x"})
	a.detectStore = func() (secret.Store, error) { return nil, secret.ErrNoStore }
	code := a.run([]string{"dev", "init", "--provider", "claude-code"})
	if code != exitError {
		t.Fatalf("exit = %d, want %d", code, exitError)
	}
	if !strings.Contains(errb.String(), "HALT") {
		t.Errorf("expected HALT on no secret store, got %q", errb.String())
	}
}

func TestConfigManualOnlyExitsTwo(t *testing.T) {
	a, _, errb := testApp(map[string]string{"OPENBOX_CONTROL_TOKEN": "obx_key_x", "OPENBOX_BACKEND_URL": "https://x"})
	store := secret.NewMemStore()
	a.detectStore = func() (secret.Store, error) { return store, nil }
	a.newRegistrar = func(_, _, _ string) devinit.Registrar {
		return &fakeReg{reg: &backend.Registration{AgentID: "a", DID: "did:aip:x", APIKey: "obx_test_k", PrivateKey: "seed"}}
	}
	// claude-code adapter is not built -> registered but config-manual -> exit 2.
	code := a.run([]string{"dev", "init", "--provider", "claude-code", "--org", "acme"})
	if code != exitConfigOnly {
		t.Fatalf("exit = %d, want %d", code, exitConfigOnly)
	}
	if !strings.Contains(errb.String(), "note:") {
		t.Errorf("expected a note on partial success, got %q", errb.String())
	}
	// Credentials must have landed in the store.
	if store.Len() != 3 {
		t.Errorf("stored %d secrets, want 3", store.Len())
	}
}

func TestUnknownProviderAndMissingProvider(t *testing.T) {
	a, _, _ := testApp(nil)
	if code := a.run([]string{"dev", "init", "--provider", "vim", "--dry-run"}); code != exitError {
		t.Errorf("unknown provider exit = %d", code)
	}
	if code := a.run([]string{"dev", "init", "--dry-run"}); code != exitError {
		t.Errorf("missing provider exit = %d", code)
	}
}
