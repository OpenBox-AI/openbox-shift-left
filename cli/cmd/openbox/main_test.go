package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
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
	// codex adapter is not built -> registered but config-manual -> exit 2.
	code := a.run([]string{"dev", "init", "--provider", "codex", "--org", "acme"})
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

// TestClaudeCodeInstallsForRealExitsZero proves the SL4-WIRE-1 front door: the
// CLI registers the real claudecode.Installer (not the SL-2 stub), so
// `dev init --provider claude-code` materializes the plugin bundle + dev config
// and exits 0. HOME and OPENBOX_CONFIG are redirected to temp dirs (the
// installer reads them directly) so nothing lands in the developer's real home.
func TestClaudeCodeInstallsForRealExitsZero(t *testing.T) {
	home := t.TempDir()
	cfgPath := filepath.Join(t.TempDir(), "openbox", "dev.json")
	t.Setenv("HOME", home)
	t.Setenv("OPENBOX_CONFIG", cfgPath)

	a, out, errb := testApp(map[string]string{"OPENBOX_CONTROL_TOKEN": "obx_key_x", "OPENBOX_BACKEND_URL": "https://x"})
	store := secret.NewMemStore()
	a.detectStore = func() (secret.Store, error) { return store, nil }
	a.newRegistrar = func(_, _, _ string) devinit.Registrar {
		return &fakeReg{reg: &backend.Registration{AgentID: "a", DID: "did:aip:x", APIKey: "obx_test_k", PrivateKey: "c2VlZA=="}}
	}

	code := a.run([]string{"dev", "init", "--provider", "claude-code", "--org", "acme"})
	if code != exitOK {
		t.Fatalf("exit = %d, want %d; stderr=%q", code, exitOK, errb.String())
	}
	if !strings.Contains(out.String(), "Wrote claude-code native config") {
		t.Errorf("expected a config-applied message, got %q", out.String())
	}

	// Bundle + dev config materialized under the redirected HOME / config path.
	if _, err := os.Stat(filepath.Join(home, ".claude", "plugins", "openbox-observe", ".claude-plugin", "plugin.json")); err != nil {
		t.Errorf("plugin bundle not materialized: %v", err)
	}
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read dev config: %v", err)
	}
	// INV-1: the written config carries coordinates, never the secret values.
	if strings.Contains(string(raw), "obx_test_k") || strings.Contains(string(raw), "c2VlZA==") {
		t.Errorf("dev config leaked a secret value:\n%s", raw)
	}
	if store.Len() != 3 {
		t.Errorf("stored %d secrets, want 3", store.Len())
	}
}

// A claude-code --dry-run must render the real installer's plan but write
// NOTHING to disk (no bundle, no config), even though claude-code is now a real
// installer. HOME/OPENBOX_CONFIG are redirected so any accidental write would
// land — and be caught — under temp dirs.
func TestClaudeCodeDryRunWritesNothing(t *testing.T) {
	home := t.TempDir()
	cfgPath := filepath.Join(t.TempDir(), "openbox", "dev.json")
	t.Setenv("HOME", home)
	t.Setenv("OPENBOX_CONFIG", cfgPath)

	a, out, _ := testApp(nil) // no token/store/registrar: dry-run must stay offline
	code := a.run([]string{"dev", "init", "--provider", "claude-code", "--dry-run", "--org", "acme"})
	if code != exitOK {
		t.Fatalf("dry-run exit = %d", code)
	}
	// The plan came from the REAL installer (observe-only plugin), not the stub.
	if !strings.Contains(out.String(), "OpenBox Claude Code plugin (observe-only") {
		t.Errorf("dry-run did not render the real installer plan:\n%s", out.String())
	}
	// Nothing was written.
	if _, err := os.Stat(filepath.Join(home, ".claude")); !os.IsNotExist(err) {
		t.Errorf("dry-run created a plugin dir under HOME (err=%v)", err)
	}
	if _, err := os.Stat(cfgPath); !os.IsNotExist(err) {
		t.Errorf("dry-run wrote the dev config (err=%v)", err)
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
