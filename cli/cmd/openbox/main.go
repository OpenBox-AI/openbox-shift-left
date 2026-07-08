// Command openbox is the developer-runtime governance CLI (STORY-SL-2).
//
// The front door is `openbox dev init --provider <tool>`: it registers a
// developer agent with OpenBox, captures the agent's credentials into the OS
// secret store, and delegates the tool's native config to that provider's
// adapter installer. After that, governance is ambient — no further UI.
//
// OD17: single static Go binary; no cgo. The OS secret store is reached by
// shelling out to the platform tool (libsecret / macOS security).
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/openbox-ai/openbox-shift-left/cli/internal/backend"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/devinit"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/provider"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/secret"
)

// version is overridable at build time via -ldflags "-X main.version=...".
var version = "0.1.0-dev"

// Exit codes: 0 success, 1 error, 2 registered-but-config-manual (partial).
const (
	exitOK         = 0
	exitError      = 1
	exitConfigOnly = 2
)

// app holds the CLI's external dependencies behind seams so the command wiring
// — including the INV-1 credential guards and the HALT-on-no-store path — is
// testable without touching the real environment, OS keychain, or network.
type app struct {
	stdout, stderr io.Writer
	getenv         func(string) string
	detectStore    func() (secret.Store, error)
	newRegistrar   func(baseURL, credential, clientID string) devinit.Registrar
}

func defaultApp() *app {
	return &app{
		stdout:       os.Stdout,
		stderr:       os.Stderr,
		getenv:       os.Getenv,
		detectStore:  secret.Detect,
		newRegistrar: func(u, c, id string) devinit.Registrar { return backend.New(u, c, id) },
	}
}

func main() { os.Exit(defaultApp().run(os.Args[1:])) }

func (a *app) errorf(format string, args ...any) int {
	fmt.Fprintf(a.stderr, "error: "+format+"\n", args...)
	return exitError
}

func (a *app) run(args []string) int {
	if len(args) == 0 {
		a.usage()
		return exitError
	}
	switch args[0] {
	case "dev":
		return a.runDev(args[1:])
	case "version", "--version", "-v":
		fmt.Fprintln(a.stdout, "openbox "+version)
		return exitOK
	case "help", "--help", "-h":
		a.usage()
		return exitOK
	default:
		a.usage()
		return a.errorf("unknown command %q", args[0])
	}
}

func (a *app) runDev(args []string) int {
	if len(args) == 0 || args[0] != "init" {
		return a.errorf("usage: openbox dev init --provider <claude-code|codex|cursor> [flags]")
	}
	return a.runDevInit(args[1:])
}

func (a *app) runDevInit(args []string) int {
	fs := flag.NewFlagSet("openbox dev init", flag.ContinueOnError)
	fs.SetOutput(a.stderr)
	var o devinit.Options
	var backendURL, clientID string
	fs.StringVar(&o.Provider, "provider", "", "developer tool: claude-code|codex|cursor (required)")
	fs.StringVar(&o.Org, "org", a.env("OPENBOX_ORG", ""), "organization namespace for credential storage")
	fs.StringVar(&o.AgentName, "agent-name", "", "override the derived agent name")
	fs.StringVar(&o.Icon, "icon", "", "agent icon string (backend requires non-empty; defaults to an emoji)")
	fs.StringVar(&o.Description, "description", "OpenBox developer-runtime agent", "agent description")
	fs.BoolVar(&o.DryRun, "dry-run", false, "print the plan; make no network or secret-store writes")
	fs.BoolVar(&o.Force, "force", false, "register a new distinctly-named agent even if one exists remotely")
	fs.BoolVar(&o.ManagedEnable, "managed-enable", false, "record the org force-enable substrate (Phase-1: verified, not activated)")
	fs.StringVar(&backendURL, "backend-url", a.env("OPENBOX_BACKEND_URL", ""), "openbox-backend control-plane base URL")
	fs.StringVar(&clientID, "client-id", a.env("OPENBOX_CLIENT", "openbox-cli"), "value for the x-openbox-client header (Keycloak JWT path)")
	if err := fs.Parse(args); err != nil {
		return exitError
	}
	if o.Provider == "" {
		return a.errorf("--provider is required (one of: claude-code, codex, cursor)")
	}

	inst, err := provider.Lookup(o.Provider)
	if err != nil {
		return a.errorf("%v", err)
	}

	d := devinit.Deps{Installer: inst, Out: a.stdout}

	// Dry-run is fully offline: no secret store, no backend, no credential.
	if o.DryRun {
		if _, err := devinit.Run(context.Background(), o, d); err != nil {
			return a.errorf("%v", err)
		}
		return exitOK
	}

	// Credential comes from the environment, never a flag/argv (INV-1).
	credential := a.getenv("OPENBOX_CONTROL_TOKEN")
	if credential == "" {
		return a.errorf("set OPENBOX_CONTROL_TOKEN (Keycloak JWT or obx_key_ org key) in the environment; " +
			"it is never accepted as a flag so it cannot leak via argv/shell history (INV-1)")
	}
	if backendURL == "" {
		return a.errorf("set --backend-url or OPENBOX_BACKEND_URL to the openbox-backend base URL")
	}

	store, err := a.detectStore()
	if err != nil {
		// Stop condition: no OS secret store → HALT, never write plaintext (INV-1).
		return a.errorf("HALT: %v — refusing to store credentials in plaintext. "+
			"Install libsecret (secret-tool) on Linux or use macOS", err)
	}
	d.Store = store
	d.Registrar = a.newRegistrar(backendURL, credential, clientID)

	res, runErr := devinit.Run(context.Background(), o, d)
	// A registered-but-config-manual result is a real partial success worth a
	// distinct exit code so scripts can tell it apart from a hard failure.
	if runErr != nil && res != nil && res.ConfigManualOnly {
		fmt.Fprintln(a.stderr, "note: "+runErr.Error())
		return exitConfigOnly
	}
	if runErr != nil {
		return a.errorf("%v", runErr)
	}
	return exitOK
}

func (a *app) env(key, def string) string {
	if v := a.getenv(key); v != "" {
		return v
	}
	return def
}

func (a *app) usage() {
	fmt.Fprint(a.stderr, `openbox — OpenBox developer-runtime governance CLI

Usage:
  openbox dev init --provider <claude-code|codex|cursor> [flags]
  openbox version

Environment:
  OPENBOX_CONTROL_TOKEN   control-plane credential (Keycloak JWT or obx_key_ org key)
  OPENBOX_BACKEND_URL     openbox-backend base URL
  OPENBOX_ORG             organization namespace for credential storage

Run 'openbox dev init -h' for flags.
`)
}
