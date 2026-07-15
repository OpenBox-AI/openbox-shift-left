// Package devinit orchestrates `openbox dev init --provider <tool>`
// (STORY-SL-2): register a developer agent, capture its once-shown credentials
// into the OS secret store, and delegate the tool's native config to the
// provider installer. Governance is ambient thereafter.
//
// Invariants enforced here:
//   - INV-1: the obx_ key and Ed25519 seed go only to the secret store; they
//     are never printed, logged, or written to a config file. Output shows the
//     secret-store *reference* (service+account), never the value.
//   - INV-4: agents are organization-scoped (the backend derives the org from
//     the caller credential; the secret-store accounts are namespaced by org).
//   - INV-7: developer agents use the shared did:aip namespace (the backend
//     mints the DID via the same uuidv5 scheme as runtime agents).
//
// Safety properties:
//   - --dry-run performs NO network and NO filesystem/secret-store writes.
//   - re-init is idempotent: if credentials already exist locally for this
//     (org, provider), registration is skipped entirely.
//   - partial failure is reported: if a step fails after the agent is created,
//     the output names the registered agent id and how to resume.
package devinit

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"strings"
	"unicode/utf8"

	"github.com/openbox-ai/openbox-shift-left/cli/internal/aivss"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/backend"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/secret"
	"github.com/openbox-ai/openbox-shift-left/provider"
)

const developerAgentType = "developer" // free-form agent_type (S6; no migration)

// Registrar is the control-plane surface devinit needs (backend.Client
// implements it). Kept minimal so tests inject a fake.
type Registrar interface {
	Create(ctx context.Context, req backend.CreateAgentRequest) (*backend.Registration, error)
	FindByName(ctx context.Context, name string) (*backend.AgentSummary, error)
}

// Options are the user-facing knobs for `dev init`.
type Options struct {
	Provider       string // claude-code|codex|cursor
	BackendURL     string // openbox-backend control-plane base (persisted for `dev sync`/staleness, STORY-E6-S8)
	Org            string // organization namespace for naming + secret accounts
	AgentName      string // override; default derived from provider+user+host
	Icon           string // non-empty string required by the backend DTO
	Description    string
	DryRun         bool
	Force          bool // register a fresh agent even if one exists remotely
	ManagedEnable  bool // org-wide force-enable substrate (NFR-5); Phase-1 opt-in
	InstallGitHook bool // STORY-SL-5: enable ambient commit-trailer hook install (off by default)
}

// Deps are the injected collaborators (all faked in tests).
type Deps struct {
	Registrar Registrar
	Store     secret.Store
	Installer provider.Installer
	Out       io.Writer
}

// Result summarizes what happened, for the caller to render / pick an exit code.
type Result struct {
	AgentID          string
	DID              string
	AgentName        string
	Reused           bool // creds already present locally; no registration done
	Registered       bool // a new agent was created this run
	ConfigApplied    bool // provider installer ran
	ConfigManualOnly bool // adapter not built; manual config was printed
}

// secret-store account layout, namespaced by org (INV-4) then provider.
func (o Options) accounts() (service, apiKey, privKey, did string) {
	ns := o.Org
	if ns == "" {
		ns = "local"
	}
	base := ns + "/" + o.Provider + "/"
	return secret.Service, base + "api_key", base + "private_key", base + "did"
}

// defaultAgentName derives a stable, per-developer name so re-init finds the
// same agent and different developers in one org do not collide.
func defaultAgentName(provider string) string {
	u := "user"
	if cu, err := user.Current(); err == nil && cu.Username != "" {
		u = cu.Username
	}
	h, err := os.Hostname()
	if err != nil || h == "" {
		h = "host"
	}
	name := fmt.Sprintf("openbox-dev-%s-%s@%s", provider, u, h)
	return truncate(name, 255)
}

// truncate limits s to at most maxBytes bytes without splitting a multibyte
// rune (the backend DTO caps agent_name at 255; usernames/hostnames may be
// non-ASCII, so a byte slice could otherwise produce invalid UTF-8).
func truncate(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// Run executes the onboarding flow. It returns a non-nil error when the command
// should exit non-zero; Result is still populated for reporting on partial
// success (e.g. registered but config-manual-only).
func Run(ctx context.Context, o Options, d Deps) (*Result, error) {
	if o.Provider == "" {
		return nil, errors.New("--provider is required (one of: " + strings.Join(provider.Supported(), ", ") + ")")
	}
	profile := aivss.DefaultDeveloperProfile()
	if field, ok := profile.Validate(); !ok {
		return nil, fmt.Errorf("default aivss profile field %s out of range (build bug)", field)
	}
	name := o.AgentName
	if name == "" {
		name = defaultAgentName(o.Provider)
	}
	icon := o.Icon
	if icon == "" {
		icon = "🧑‍💻"
	}
	service, apiKeyAcct, privKeyAcct, didAcct := o.accounts()
	ref := provider.CredentialRef{
		SecretService:     service,
		APIKeyAccount:     apiKeyAcct,
		PrivateKeyAccount: privKeyAcct,
		InstallGitHook:    o.InstallGitHook,
		// STORY-E6-S8: persist the control-plane base so `dev sync`/staleness can
		// reach the policy read without re-supplying OPENBOX_BACKEND_URL. The agent id
		// is set below on the register path (the reuse path preserves a prior value —
		// installer.writeConfig).
		BackendURL: o.BackendURL,
	}
	res := &Result{AgentName: name}

	// --- dry-run: describe, write nothing, touch no network ------------------
	if o.DryRun {
		return res, planDryRun(o, d, name, icon, profile, ref)
	}

	// --- idempotency: local secret store is the source of truth --------------
	// The obx_ key and Ed25519 seed are shown by agent/create exactly once, so a
	// prior successful init is only recoverable locally. If both are present, we
	// reuse them and skip registration entirely.
	if existingKey, err := d.Store.Get(service, apiKeyAcct); err == nil && existingKey != "" {
		if _, err := d.Store.Get(service, privKeyAcct); err == nil {
			did, _ := d.Store.Get(service, didAcct)
			ref.DID = did
			res.Reused = true
			res.DID = did
			fmt.Fprintf(d.Out, "Already initialized for org=%s provider=%s — reusing stored credentials (DID %s).\n",
				nsOrLocal(o.Org), o.Provider, did)
			return res, applyConfig(o, d, ref, res)
		}
	}

	// --- remote duplicate detection (avoid a 400, never duplicate silently) --
	// A lookup FAILURE must not fall through to Create: we could neither confirm
	// nor rule out a duplicate, so idempotent detection would be silently bypassed
	// (F1). Surface it and let the user retry or --force past the check.
	if !o.Force {
		existing, err := d.Registrar.FindByName(ctx, name)
		if err != nil {
			return res, fmt.Errorf(
				"could not check for an existing agent named %q (agent/list failed: %w); "+
					"re-run when the OpenBox org is reachable, or pass --force to skip the check",
				name, err)
		}
		if existing != nil {
			res.AgentID, res.DID = existing.ID, existing.DID
			return res, fmt.Errorf(
				"a developer agent named %q already exists in this org (id %s, DID %s) but no local credentials are stored. "+
					"Its API key and signing key are shown only once and cannot be re-retrieved. "+
					"Delete that agent and re-run, or pass --force to register a new distinctly-named agent.",
				name, existing.ID, existing.DID)
		}
	} else {
		// Forcing a new registration still needs a free name; a lookup failure
		// here is surfaced rather than proceeding to a confusing 400 (F4).
		free, err := freeName(ctx, d, name)
		if err != nil {
			return res, fmt.Errorf("could not find a free agent name for --force (agent/list failed: %w)", err)
		}
		name = free
		res.AgentName = name
	}

	// --- register ------------------------------------------------------------
	req := backend.CreateAgentRequest{
		AgentName:   name,
		AgentType:   developerAgentType,
		Icon:        icon,
		Description: o.Description,
		Tags:        []string{"openbox-shift-left", "developer-runtime", o.Provider},
		AivssConfig: profile,
		Config: map[string]any{
			"provider":       o.Provider,
			"managed_enable": o.ManagedEnable, // substrate only; not activated in Phase 1
		},
	}
	reg, err := d.Registrar.Create(ctx, req)
	if err != nil {
		var apiErr *backend.APIError
		if errors.As(err, &apiErr) {
			// Stop condition: exact 4xx (agent_type / aivss_config rejection) → HALT.
			return res, fmt.Errorf("HALT: agent/create rejected registration (%w). "+
				"Verify agent_type=%q and the default aivss_config are accepted by this OpenBox org",
				apiErr, developerAgentType)
		}
		return res, fmt.Errorf("agent/create failed: %w", err)
	}
	res.AgentID, res.DID, res.Registered = reg.AgentID, reg.DID, true
	ref.DID = reg.DID
	ref.AgentID = reg.AgentID // STORY-E6-S8: persisted to dev.json for `dev sync`/staleness

	if reg.APIKey == "" || reg.PrivateKey == "" {
		return res, fmt.Errorf(
			"agent registered (id %s, DID %s) but the response did not include %s — "+
				"cannot store runtime credentials; rotate the key or re-provision the identity",
			reg.AgentID, reg.DID, missingCreds(reg))
	}

	// --- capture credentials into the OS secret store (INV-1) ----------------
	// Order: secrets first, DID last. On any failure after the agent exists, the
	// error names the registered agent id so the operator can resume.
	if err := d.Store.Set(service, apiKeyAcct, reg.APIKey); err != nil {
		return res, resumeErr(reg, "store API key", err)
	}
	if err := d.Store.Set(service, privKeyAcct, reg.PrivateKey); err != nil {
		return res, resumeErr(reg, "store private key", err)
	}
	if err := d.Store.Set(service, didAcct, reg.DID); err != nil {
		return res, resumeErr(reg, "store DID", err)
	}

	fmt.Fprintf(d.Out, "Registered developer agent %q\n  id:    %s\n  DID:   %s\n  tier:  %s (trust %s)\n",
		reg.AgentName, reg.AgentID, reg.DID, reg.Tier, reg.TrustScore)
	fmt.Fprintf(d.Out, "Credentials stored in %s (service %q); values are not printed (INV-1).\n",
		d.Store.Name(), service)
	if o.ManagedEnable {
		fmt.Fprintln(d.Out, "Managed force-enable substrate recorded (verified, not activated — Phase-1 pilot is opt-in).")
	}

	return res, applyConfig(o, d, ref, res)
}

// applyConfig delegates provider config writing. When the adapter isn't built,
// it prints the manual config and returns an error so the command exits
// non-zero — but the agent + credentials are already durably in place.
func applyConfig(o Options, d Deps, ref provider.CredentialRef, res *Result) error {
	if !d.Installer.Available() {
		res.ConfigManualOnly = true
		fmt.Fprintf(d.Out, "\nProvider config not applied — %s\n\n%s\n", o.Provider, d.Installer.Plan(ref))
		return fmt.Errorf("provider %q config not applied: adapter not built yet (see manual config above)", o.Provider)
	}
	if err := d.Installer.Install(ref); err != nil {
		return fmt.Errorf("agent ready but writing %s config failed: %w", o.Provider, err)
	}
	res.ConfigApplied = true
	fmt.Fprintf(d.Out, "Wrote %s native config (references the secret store, no secrets inline).\n", o.Provider)
	return nil
}

// planDryRun prints the full plan and asserts no writes occur.
func planDryRun(o Options, d Deps, name, icon string, profile aivss.Config, ref provider.CredentialRef) error {
	out := d.Out
	fmt.Fprintf(out, "DRY RUN — no network calls, no secret-store or filesystem writes.\n\n")
	fmt.Fprintf(out, "Would register developer agent:\n")
	fmt.Fprintf(out, "  provider:    %s\n", o.Provider)
	fmt.Fprintf(out, "  agent_name:  %s\n", name)
	fmt.Fprintf(out, "  agent_type:  %s\n", developerAgentType)
	fmt.Fprintf(out, "  icon:        %s\n", icon)
	fmt.Fprintf(out, "  aivss_config: base_security/ai_specific/impact (accepted developer posture; server computes score/tier)\n")
	fmt.Fprintf(out, "  managed_enable: %t (substrate only; not activated in Phase 1)\n", o.ManagedEnable)
	fmt.Fprintf(out, "  install_git_hook: %t (STORY-SL-5 ambient commit-trailer hook; off by default — modifies .git/hooks)\n", o.InstallGitHook)
	svc, apiAcct, privAcct, didAcct := o.accounts()
	fmt.Fprintf(out, "\nWould store credentials in the OS secret store (service %q):\n", svc)
	fmt.Fprintf(out, "  %s  (obx_ API key)\n  %s  (Ed25519 seed)\n  %s  (DID)\n", apiAcct, privAcct, didAcct)
	fmt.Fprintf(out, "\nProvider config:\n%s\n", d.Installer.Plan(ref))
	return nil
}

// freeName returns a name not already taken remotely, appending -2, -3, ... when
// forcing a new registration. It returns an error if the remote lookup fails (so
// the caller does not proceed to a confusing 400) or if no free name is found in
// a bounded number of attempts.
func freeName(ctx context.Context, d Deps, name string) (string, error) {
	for i := 1; i < 100; i++ {
		candidate := name
		if i > 1 {
			candidate = fmt.Sprintf("%s-%d", name, i)
		}
		existing, err := d.Registrar.FindByName(ctx, candidate)
		if err != nil {
			return "", err
		}
		if existing == nil {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("no free name near %q after 99 attempts", name)
}

func resumeErr(reg *backend.Registration, step string, err error) error {
	return fmt.Errorf("agent registered (id %s, DID %s) but failed to %s: %w — "+
		"the API key and signing key were shown only once; rotate the key and re-run, or complete the step manually",
		reg.AgentID, reg.DID, step, err)
}

func missingCreds(reg *backend.Registration) string {
	switch {
	case reg.APIKey == "" && reg.PrivateKey == "":
		return "an API key or a signing key"
	case reg.APIKey == "":
		return "an API key"
	default:
		return "a signing key"
	}
}

func nsOrLocal(org string) string {
	if org == "" {
		return "local"
	}
	return org
}
