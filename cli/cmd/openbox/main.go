// Command openbox is the developer-runtime governance CLI.
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
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"

	claudecode "github.com/openbox-ai/openbox-shift-left/adapters/claude-code"
	codex "github.com/openbox-ai/openbox-shift-left/adapters/codex"
	obgit "github.com/openbox-ai/openbox-shift-left/adapters/common/git"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/backend"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/devinit"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/providers"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/secret"
	"github.com/openbox-ai/openbox-shift-left/client"
	"github.com/openbox-ai/openbox-shift-left/decision"

	"github.com/openbox-ai/openbox-shift-left/adapters/common/devconfig"
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
	stdout, stderr  io.Writer
	stdin           io.Reader
	getenv          func(string) string
	openStore       func(kind string) (secret.Store, error)
	newRegistrar    func(baseURL, credential, clientID string) devinit.Registrar
	newPolicyReader func(baseURL, credential, clientID string) policyReader
}

// policyReader is the control-plane read `dev sync` + the `dev init`
// last-step need. backend.Client implements it; a fake injects in tests.
type policyReader interface {
	GetCurrentPolicy(ctx context.Context, agentID string) (*backend.Policy, error)
}

func defaultApp() *app {
	return &app{
		stdout:          os.Stdout,
		stderr:          os.Stderr,
		stdin:           os.Stdin,
		getenv:          os.Getenv,
		openStore:       secret.Open,
		newRegistrar:    func(u, c, id string) devinit.Registrar { return backend.New(u, c, id) },
		newPolicyReader: func(u, c, id string) policyReader { return backend.New(u, c, id) },
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
	case "hook":
		return a.runHook(args[1:])
	case "managed":
		return a.runManaged(args[1:])
	case "doctor":
		return a.runDoctor(args[1:])
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
	if len(args) == 0 {
		return a.errorf("usage: openbox dev <init|verify> [flags]")
	}
	switch args[0] {
	case "init":
		return a.runDevInit(args[1:])
	case "verify":
		return a.runDevVerify(args[1:])
	case "sync":
		return a.runDevSync(args[1:])
	default:
		return a.errorf("usage: openbox dev <init|verify|sync> [flags]")
	}
}

// runDevSync fetches this agent's current org policy from the control plane
// and writes it as the local policy bundle + pin (ADR-0005): the pull half
// of the pull-at-init + session-start-staleness distribution model. It reads
// the org control-plane credential from OPENBOX_CONTROL_TOKEN (never a
// flag — INV-1), resolves the agent id + backend URL persisted by `dev
// init` (env overrides), fetches, translates config.policy_builder into a
// builder bundle (or a fail-open-local bundle for raw rego / a no-policy
// allow bundle), writes it 0600, and clears any fail-closed stale markers so
// the enforce gate proceeds. It never prints the org key or rego text
// (INV-1). On any auth/fetch failure it exits non-zero with a mapped hint
// and leaves the last-good bundle untouched.
func (a *app) runDevSync(args []string) int {
	fs := flag.NewFlagSet("openbox dev sync", flag.ContinueOnError)
	fs.SetOutput(a.stderr)
	var bundlePath, clientID string
	fs.StringVar(&bundlePath, "bundle", a.env("OPENBOX_SIDECAR_BUNDLE", ""), "local policy bundle to write (default: $XDG_CONFIG_HOME/openbox/policy-bundle.json)")
	fs.StringVar(&clientID, "client-id", a.env("OPENBOX_CLIENT", "openbox-cli"), "value for the x-openbox-client header (Keycloak JWT path)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitError
	}

	token := a.getenv("OPENBOX_CONTROL_TOKEN")
	if token == "" {
		return a.errorf("set OPENBOX_CONTROL_TOKEN (Keycloak JWT or obx_key_ org key) in the environment; " +
			"it is never accepted as a flag so it cannot leak via argv/shell history (INV-1)")
	}
	backendURL := claudecode.ResolveBackendURL()
	if backendURL == "" {
		return a.errorf("no backend URL configured — set OPENBOX_BACKEND_URL or re-run `openbox dev init --backend-url <url>`")
	}
	agentID := claudecode.ResolveAgentID()
	if agentID == "" {
		return a.errorf("no agent id configured — run `openbox dev init --provider <tool>` first (it persists the agent id), or set OPENBOX_AGENT_ID")
	}
	if bundlePath == "" {
		bundlePath = claudecode.ResolveBundlePath()
	}

	if err := a.syncPolicyBundle(context.Background(), backendURL, token, clientID, agentID, bundlePath, a.stdout); err != nil {
		return a.errorf("%v", err)
	}
	return exitOK
}

// syncPolicyBundle performs the fetch → translate → write → clear-markers flow,
// shared by `dev sync` and the `dev init` last step. It returns a mapped error on
// failure (the caller decides exit code / warn); it never prints a secret.
func (a *app) syncPolicyBundle(ctx context.Context, backendURL, token, clientID, agentID, bundlePath string, out io.Writer) error {
	reader := a.newPolicyReader(backendURL, token, clientID)
	pol, err := reader.GetCurrentPolicy(ctx, agentID)
	if err != nil {
		return mapPolicyReadError(err)
	}

	bundle, note, err := translateBundle(pol)
	if err != nil {
		return err
	}
	// Verify before replacing the last-good bundle (E8-S6). A bundle that fails
	// verification is refused outright rather than written and then distrusted at
	// load: writing it would discard a policy that DID verify in favour of one
	// that did not, which is the wrong direction on every axis.
	integrity, integrityNote, err := verifySyncedBundle(bundlePath, bundle)
	if err != nil {
		return err
	}
	if err := writeBundleFile(bundlePath, bundle); err != nil {
		return fmt.Errorf("write policy bundle: %w", err)
	}
	// Pin the epoch ONLY from a verified signature — the same gate
	// NewInProcessDecider applies. Bundle.Epoch() reads the signed payload without
	// checking the signature, so pinning from it unconditionally would let anyone
	// who can answer the policy fetch set the floor: a bundle claiming
	// policy_epoch = MaxInt64 makes every genuinely-signed bundle afterwards verify
	// as IntegrityEpochRollback, the decider then refuses to load any of them, and
	// enforcement is fail-open from then on. The floor only ever advances
	// (WriteEpochPin), so there is no way back short of deleting the pin file.
	// Reachable today via the no-key path, which is the default until
	// org_signing_pubkey is populated.
	if integrity == decision.IntegrityVerified {
		if epoch := bundle.Epoch(); epoch > 0 {
			decision.WriteEpochPin(bundlePath, epoch)
		}
	}
	// A fresh, re-pinned bundle clears any fail-closed staleness block so
	// the PreToolUse enforce gate proceeds again.
	_ = claudecode.ClearAllStaleMarkers()

	// Non-secret summary only: the policy id + pin, never the rego or org key (INV-1).
	if pol == nil {
		fmt.Fprintf(out, "Synced policy bundle → %s (no current policy for this agent — allow/no-policy bundle).\n", bundlePath)
	} else {
		fmt.Fprintf(out, "Synced policy bundle → %s (policy %s, updated_at %s).\n", bundlePath, pol.ID, orUnset(pol.UpdatedAt))
	}
	if note != "" {
		fmt.Fprintln(out, note)
	}
	if integrityNote != "" {
		fmt.Fprintln(out, integrityNote)
	}
	return nil
}

// verifySyncedBundle checks a freshly fetched bundle's signature before it is
// allowed to replace the last-good one. It returns the verification outcome plus a
// non-secret note to print, or an error that aborts the sync.
//
// The outcome is returned rather than collapsed to ok/not-ok because the caller
// needs it: only IntegrityVerified may advance the epoch pin, since every other
// outcome means the payload carrying that epoch was never authenticated.
//
// An unsigned bundle is accepted with a note, not an error: backends that do not
// sign yet must keep working, and pretending otherwise would make the feature
// undeployable. A bundle that carries a signature and fails to verify is a
// different matter — that is either tampering in transit or a key mismatch, and
// continuing would mean trusting content we just proved untrustworthy.
func verifySyncedBundle(bundlePath string, b *decision.Bundle) (decision.Integrity, string, error) {
	if b == nil || b.Signed == nil {
		return decision.IntegrityUnsigned, "note: this policy is unsigned — the local bundle cannot be integrity-checked " +
			"(a local edit would not be detectable). Signing is served by newer backends (E8-S6).", nil
	}
	pubKeyB64, keyID := devconfig.ResolveOrgSigningKey()
	pub := decision.DecodePublicKey(pubKeyB64)
	if pub == nil {
		return decision.IntegrityNoKey, "note: this policy is signed but no org signing key is pinned, so the signature " +
			"could not be checked and its epoch is not pinned. It is installed and will be ENFORCED UNVERIFIED " +
			"(a local edit would not be detectable). Pin org_signing_pubkey in dev.json to enable verification.", nil
	}
	_, integrity := b.VerifyIntegrity(decision.VerifyOptions{
		PublicKey: pub,
		MinEpoch:  decision.ReadEpochPin(bundlePath),
	})
	if integrity == decision.IntegrityVerified {
		return integrity, fmt.Sprintf("Policy signature verified (key %s, epoch %d).", orUnset(keyID), b.Epoch()), nil
	}
	return integrity, "", fmt.Errorf("refusing to install policy bundle: signature check failed (%s); "+
		"the previous bundle is unchanged", integrity)
}

// translateBundle maps a fetched *backend.Policy into a *decision.Bundle + an
// optional non-secret note to print. nil policy → an empty allow bundle;
// config.policy_builder → a builder bundle; raw rego with no builder → a
// fail-open-local bundle + a warning (ADR-0005 §Decision-2).
func translateBundle(pol *backend.Policy) (*decision.Bundle, string, error) {
	if pol == nil {
		return &decision.Bundle{Version: "no-policy"}, "", nil
	}
	pin := pol.ID + "@" + pol.UpdatedAt
	signed := signatureBlock(pol)
	if len(pol.PolicyBuilder) > 0 {
		var cfg decision.PolicyBuilderConfig
		if err := json.Unmarshal(pol.PolicyBuilder, &cfg); err != nil {
			return nil, "", fmt.Errorf("parse policy_builder config: %w", err)
		}
		return &decision.Bundle{
			Version:       pin,
			PolicyID:      pol.ID,
			UpdatedAt:     pol.UpdatedAt,
			PolicyBuilder: &cfg,
			Signed:        signed,
		}, "", nil
	}
	if pol.HasRawRego {
		note := "warning: this policy is hand-written raw rego with no builder config — it cannot be evaluated locally " +
			"and the decider will serve it fail-open (allow) locally; enforcement for it relies on the async /evaluate audit (ADR-0005)."
		return &decision.Bundle{
			Version:            pin,
			PolicyID:           pol.ID,
			UpdatedAt:          pol.UpdatedAt,
			RawRegoUnlocalized: true,
			Signed:             signed,
		}, note, nil
	}
	// A policy with neither builder config nor rego → treat as no-op allow, pinned.
	return &decision.Bundle{Version: pin, PolicyID: pol.ID, UpdatedAt: pol.UpdatedAt, Signed: signed}, "", nil
}

// signatureBlock converts the backend's signature block for the bundle file. The
// signed bytes are carried verbatim: re-serializing them here would risk not
// reproducing what the backend signed, and the whole point is that the decider
// verifies the signer's own bytes.
func signatureBlock(pol *backend.Policy) *decision.SignedPolicy {
	if pol == nil || pol.Signed == nil {
		return nil
	}
	return &decision.SignedPolicy{
		KeyID:        pol.Signed.KeyID,
		Algorithm:    pol.Signed.Algorithm,
		CanonicalB64: pol.Signed.CanonicalB64,
		SigB64:       pol.Signed.SigB64,
	}
}

// writeBundleFile marshals the bundle and writes it 0600 (owner-only),
// creating the parent dir 0700. It round-trips through decision.ParseBundle
// first so a malformed/deny-by-default bundle is rejected before it
// replaces the last-good file (never write a bundle the daemon would refuse
// to load).
//
// The write is atomic: a temp file in the same dir (so rename is atomic on
// one filesystem) written 0600, then os.Rename over the target. A crash
// mid-write can never leave the daemon (or the session-start staleness
// read) a truncated/half-parsed bundle — it sees either the old file or the
// whole new one.
func writeBundleFile(path string, b *decision.Bundle) error {
	raw, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return err
	}
	if _, err := decision.ParseBundle(raw); err != nil {
		return fmt.Errorf("refusing to write an invalid bundle: %w", err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".policy-bundle-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(raw, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// mapPolicyReadError turns a control-plane read failure into an actionable,
// secret-free hint. It surfaces the exact 4xx cause without echoing any
// credential.
func mapPolicyReadError(err error) error {
	var apiErr *backend.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.StatusCode {
		case 401:
			return fmt.Errorf("policy read rejected (HTTP 401): the control-plane credential is invalid or expired — check OPENBOX_CONTROL_TOKEN")
		case 403:
			return fmt.Errorf("policy read forbidden (HTTP 403): the credential lacks the read:agent_policy permission for this org")
		case 404:
			return fmt.Errorf("policy read not found (HTTP 404): the agent id may be wrong for this org — re-check `openbox dev init`")
		default:
			return fmt.Errorf("policy read failed (HTTP %d)", apiErr.StatusCode)
		}
	}
	return fmt.Errorf("policy read failed: %w", err)
}

func orUnset(s string) string {
	if s == "" {
		return "(unset)"
	}
	return s
}

// runDevVerify is the read-only data-plane preflight: `openbox dev verify`
// resolves the finished dev.json + creds (reusing the adapter's resolvers),
// then a signed GET /api/v1/auth/validate against the actual core it emits
// to confirms the obx_ key + Ed25519 signing round-trip work. It is
// strictly read-only — no mint, no config write, no secret printed (INV-1)
// — and prints one ✓ line or a ✗ with the mapped fix hint.
func (a *app) runDevVerify(args []string) int {
	fs := flag.NewFlagSet("openbox dev verify", flag.ContinueOnError)
	fs.SetOutput(a.stderr)
	var dryRun bool
	fs.BoolVar(&dryRun, "dry-run", false, "print the plan (method, path, base_url, DID); make no network call")
	fs.BoolVar(&dryRun, "print-plan", false, "alias for --dry-run")
	if err := fs.Parse(args); err != nil {
		return exitError
	}

	// --dry-run is fully offline: resolve only the NON-SECRET coordinates (no
	// keychain read, no network) and print what the real run would call.
	if dryRun {
		baseURL, did := claudecode.ResolveCoordinates()
		fmt.Fprintln(a.stdout, "DRY RUN — openbox dev verify would call (no network, no secret access):")
		fmt.Fprintf(a.stdout, "  request:  GET %s%s\n", baseURL, client.AuthValidatePath)
		fmt.Fprintf(a.stdout, "  base_url: %s\n", baseURL)
		fmt.Fprintf(a.stdout, "  did:      %s\n", displayOrUnset(did))
		return exitOK
	}

	// Reuse the adapter's resolvers (dev.json + OS keychain / file backend / env).
	// A missing identity means onboarding hasn't run — say so, don't half-proceed.
	creds, err := claudecode.ResolveCredentials()
	if err != nil {
		return a.errorf("cannot verify — %v.\n"+
			"  Run `openbox dev init --provider <claude-code|codex|cursor>` first, then retry.", err)
	}

	// client.New enforces the INV-1 TLS guard (refuses plaintext http:// to a
	// non-loopback core) and validates the identity shape before any network I/O.
	c, err := client.New(client.Config{
		BaseURL: creds.BaseURL,
		APIKey:  creds.APIKey,
		DID:     creds.DID,
		SeedB64: creds.SeedB64,
		// No Logger: Validate returns its diagnostic directly (not fail-open).
	})
	if err != nil {
		return a.errorf("%v", err)
	}

	if err := c.Validate(context.Background()); err != nil {
		// ✗ to stderr; the message is the mapped reason + fix hint. It never
		// contains the key/seed/nonce/signature (INV-1) — only status + guidance.
		fmt.Fprintf(a.stderr, "✗ %v\n", err)
		return exitError
	}
	// ✓ names only the DID + base_url (INV-1: no secret in output).
	fmt.Fprintf(a.stdout, "✓ verified: %s @ %s\n", creds.DID, creds.BaseURL)
	return exitOK
}

// displayOrUnset renders an empty coordinate as an actionable placeholder for the
// dry-run plan rather than a blank line.
func displayOrUnset(s string) string {
	if s == "" {
		return "(not configured — run `openbox dev init`)"
	}
	return s
}

// runHook is the unified observe-only hook entrypoint: `openbox hook
// <provider> <event>`. The plugin's hooks.json invokes it for every Claude
// Code hook.
//
// INV-3 (the reason this does not go through errorf/usage): the hook path
// must always return exitOK — a non-zero exit blocks the tool call. In
// observe mode it also writes nothing to stdout (on
// SessionStart/UserPromptSubmit an exit-0 hook's stdout is injected into the
// model's context). So every diagnostic (bad args, unknown provider, and
// everything inside RunHook) goes to stderr, and we unconditionally return
// 0. Folding the hook into the multi-command binary must not leak
// cobra/flag/usage/banner text to stdout.
//
// Enforce mode (opt-in) is the sole stdout writer: RunHook may emit a
// Claude Code PreToolUse permissionDecision (deny/ask) to a.stdout — still
// exit 0 (the decision travels in the JSON, not the exit code), still
// tighten-only. The permissionDecision JSON is the only structured stdout
// this path ever produces.
func (a *app) runHook(args []string) (code int) {
	// Belt-and-suspenders for INV-3: default to exitOK and swallow any panic that
	// escapes RunHook's own recover, so the hook path can NEVER return non-zero
	// (which would block the tool call).
	code = exitOK
	defer func() { _ = recover() }()
	logger := log.New(a.stderr, "openbox hook: ", 0)
	if len(args) < 1 {
		logger.Printf("usage: openbox hook <provider> <event...>  (providers: claude-code, codex, git)")
		return exitOK
	}
	switch args[0] {
	case "claude-code":
		if len(args) < 2 {
			logger.Printf("usage: openbox hook claude-code <event>")
			return exitOK
		}
		claudecode.RunHook(args[1], a.stdin, a.stdout, logger)
	case "codex":
		// The Codex observe engine — identical safety contract (recover-all,
		// diagnostics to stderr, caller exits 0 always; this leg never
		// writes stdout — Codex parses hook stdout as output JSON).
		if len(args) < 2 {
			logger.Printf("usage: openbox hook codex <event>")
			return exitOK
		}
		codex.RunHook(args[1], a.stdin, a.stdout, logger)
	case "git":
		// The git hook re-invokes this binary as `openbox hook git
		// prepare-commit-msg` (OD17 — folds the standalone openbox-git-hook
		// in).
		//
		// Supply the attestation signing context (E8-S10). The git module never
		// touches the secret store itself, so the engine injects a resolver;
		// returning ok=false leaves the commit unattested and the lineage
		// inferred, which is the pre-E8 behaviour.
		obgit.SetAttestContext(attestContext)
		obgit.RunHook(args[1:], []string{"hook", "git", "prepare-commit-msg"}, logger.Printf)
	default:
		logger.Printf("unknown hook provider %q (supported: claude-code, codex, git)", args[0])
	}
	return exitOK
}

func (a *app) runDevInit(args []string) int {
	fs := flag.NewFlagSet("openbox dev init", flag.ContinueOnError)
	fs.SetOutput(a.stderr)
	var o devinit.Options
	var backendURL, clientID, secretBackend string
	fs.StringVar(&o.Provider, "provider", "", "developer tool: claude-code|codex|cursor (required)")
	fs.StringVar(&o.Org, "org", a.env("OPENBOX_ORG", ""), "organization namespace for credential storage")
	fs.StringVar(&o.AgentName, "agent-name", "", "override the derived agent name")
	fs.StringVar(&o.Icon, "icon", "", "agent icon string (backend requires non-empty; defaults to an emoji)")
	fs.StringVar(&o.Description, "description", "OpenBox developer-runtime agent", "agent description")
	fs.BoolVar(&o.DryRun, "dry-run", false, "print the plan; make no network or secret-store writes")
	fs.BoolVar(&o.Force, "force", false, "register a new distinctly-named agent even if one exists remotely")
	fs.BoolVar(&o.ManagedEnable, "managed-enable", false, "record the org force-enable substrate (Phase-1: verified, not activated)")
	fs.BoolVar(&o.InstallGitHook, "install-git-hook", false, "enable ambient install of the commit-trailer hook into repos on session start (off by default — it modifies .git/hooks)")
	var enforce bool
	fs.BoolVar(&enforce, "enforce", false, "turn on ENFORCE mode and persist it to dev.json (ADR-0006): the PreToolUse hook blocks/asks/redacts in-process — no daemon, no runtime env. Also enables Tier-2 sync escalation + the Tier-3 findings loop. Off by default = observe-only.")
	fs.StringVar(&backendURL, "backend-url", a.env("OPENBOX_BACKEND_URL", ""), "openbox-backend control-plane base URL")
	fs.StringVar(&clientID, "client-id", a.env("OPENBOX_CLIENT", "openbox-cli"), "value for the x-openbox-client header (Keycloak JWT path)")
	fs.StringVar(&secretBackend, "secret-backend", a.env("OPENBOX_SECRET_BACKEND", "auto"), "credential store: auto|os (OS keychain, default) or file (opt-in 0600 plaintext file, for machines with no OS keyring)")
	if err := fs.Parse(args); err != nil {
		return exitError
	}
	if o.Provider == "" {
		return a.errorf("--provider is required (one of: claude-code, codex, cursor)")
	}
	o.BackendURL = backendURL // persist the control-plane base for `dev sync`/staleness
	// ADR-0006: `--enforce` is the one-flag enforce posture. It turns on
	// enforce and its sensible companions (Tier-2 sync escalation + the
	// Tier-3 findings loop) and persists all three to dev.json, so the
	// runtime hook needs no env var. Granular per-toggle tuning remains
	// available via the dev.json fields / OPENBOX_* env overrides.
	// Fail-open stays the default failure policy — enforce never implies
	// fail-closed.
	if enforce {
		t := true
		o.Enforce = true
		o.Tier2 = &t
		o.Findings = &t
	}

	inst, err := providers.Lookup(o.Provider)
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

	store, err := a.openStore(secretBackend)
	if err != nil {
		if errors.Is(err, secret.ErrNoStore) {
			// Stop condition: no OS keychain and the operator did not opt
			// into an alternative → HALT, never silently write plaintext
			// (INV-1).
			return a.errorf("HALT: %v — refusing to store credentials in plaintext by default.\n"+
				"  Fix by EITHER:\n"+
				"  • installing an OS keyring — Linux: `secret-tool` (e.g. apt install libsecret-tools) with a running\n"+
				"    keyring daemon (gnome-keyring/kwallet); macOS has one built in; then re-run; OR\n"+
				"  • opting into the file backend for this machine (0600 plaintext, no keyring needed):\n"+
				"      openbox dev init --provider %s --secret-backend file   (or OPENBOX_SECRET_BACKEND=file)",
				err, o.Provider)
		}
		return a.errorf("%v", err)
	}
	// The file backend trades away at-rest encryption; make that explicit and
	// tell the operator how to point the adapter hook at the same store.
	if secretBackend == "file" {
		fmt.Fprintf(a.stderr,
			"warning: using the file secret backend — credentials are stored PLAINTEXT (0600) at %s.\n"+
				"         Prefer an OS keyring where available. The Claude Code hook reads this store when you set\n"+
				"         OPENBOX_SECRET_FILE=%s in its environment.\n",
			secret.DefaultFilePath(), secret.DefaultFilePath())
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

	// Codex hash-trusts non-managed hooks — until the user trusts the
	// freshly-written entries via /hooks inside Codex, they do not run.
	// Surface that as the explicit next step (re-install re-hashes, so it
	// applies to re-inits too).
	if o.Provider == "codex" && res != nil && res.ConfigApplied {
		fmt.Fprintln(a.stdout, "Next step: open Codex and run /hooks to review and TRUST the new OpenBox hooks — they do not run until trusted (Codex hash-trusts non-managed hooks; re-running dev init re-hashes them).")
	}

	// `dev init`'s last step best-effort pulls the agent's current policy
	// into the local bundle so enforce mode has a policy on first run. It
	// is best-effort — a fetch failure warns (stderr) and does not fail
	// init (the agent is already registered and configured; the user can
	// re-run `openbox dev sync`). The agent id was persisted by the
	// installer; resolve it back out.
	if agentID := claudecode.ResolveAgentID(); agentID != "" && a.newPolicyReader != nil {
		bundlePath := claudecode.ResolveBundlePath()
		if err := a.syncPolicyBundle(context.Background(), backendURL, credential, clientID, agentID, bundlePath, a.stdout); err != nil {
			fmt.Fprintf(a.stderr, "note: initial policy sync skipped (%v); run `openbox dev sync` when ready.\n", err)
		}
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
  openbox dev init --provider <claude-code|codex|cursor> [--enforce] [flags]
  openbox dev verify [--dry-run]
  openbox dev sync [--bundle <path>]
  openbox managed install --provider <claude-code,codex> [--dry-run] [--force]
  openbox doctor
  openbox version

Environment (needed only at 'dev init' time):
  OPENBOX_CONTROL_TOKEN   control-plane credential (Keycloak JWT or obx_key_ org key)
  OPENBOX_BACKEND_URL     openbox-backend base URL (or --backend-url)
  OPENBOX_ORG             organization namespace for credential storage

After 'dev init' governance is ambient — no daemon to run and no runtime env to set.
Run 'openbox dev init -h' for flags.
`)
}
