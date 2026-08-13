// Package devinit registers a developer agent, captures its once-shown
// credentials into ~/.openbox/.env, and delegates the tool's native config to
// the provider installer.
//
// Nothing needs to run afterwards — but "ambient" describes the MECHANISM, not the
// COVERAGE: a default install governs one project directory (ADR-0016, and see
// Options.ProjectDir). Conflating the two is the overstatement this product exists
// to prevent.
//
// Invariants enforced here:
//   - INV-1: the obx_ key and signing key are written only to the credential
//     file and never printed, logged, or placed on an argv. Output shows the
//     file path, never a value. ADR-0015 narrowed what INV-1 claims — that file
//     is plaintext — but not the no-print/no-argv part enforced here.
//   - INV-4: agents are organization-scoped (the backend derives the org from
//     the caller credential).
//   - INV-7: developer agents use the shared did:aip namespace (the backend
//     mints the DID via the same uuidv5 scheme as runtime agents).
//
// Safety properties:
//   - --dry-run performs NO network and NO filesystem writes.
//   - re-registration is skipped when credentials already exist locally.
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

	"github.com/openbox-ai/openbox-shift-left/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/aivss"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/backend"
	"github.com/openbox-ai/openbox-shift-left/provider"
)

const developerAgentType = "developer" // free-form agent_type; no migration

// Registrar is the control-plane surface devinit needs (backend.Client
// implements it). Kept minimal so tests inject a fake.
type Registrar interface {
	Create(ctx context.Context, req backend.CreateAgentRequest) (*backend.Registration, error)
	FindByName(ctx context.Context, name string) (*backend.AgentSummary, error)
}

// Options are the user-facing knobs for `init`.
type Options struct {
	Provider   string // claude-code|codex|cursor
	BackendURL string // openbox-backend control-plane base (persisted for `dev sync`/staleness)
	// BaseURL is the openbox-core DATA-PLANE base — where events are emitted
	// and where `dev verify` authenticates. Empty keeps the adapter default
	// (the SaaS core), which is right for a SaaS install and wrong for every
	// self-hosted one: the backend's registration reply carries no data-plane
	// URL, so without this a local deployment onboards successfully and then
	// signs every request at core.openbox.ai (a 401 that reads as a broken
	// install).
	BaseURL     string
	Org         string // organization namespace for naming + secret accounts
	AgentName   string // override; default derived from provider+user+host
	Icon        string // non-empty string required by the backend DTO
	Description string
	DryRun      bool
	Force       bool // register a fresh agent even if one exists remotely
	// EnvFile overrides where credentials are written (`auth --env-file`). Empty
	// uses devconfig.EnvFilePath(). It is threaded through rather than resolved at
	// the write site because registration is the ONE path that mints credentials:
	// ignoring the override here wrote a newly-created agent's once-shown key to
	// the default location while reporting the custom one.
	EnvFile        string
	ManagedEnable  bool // org-wide force-enable substrate; opt-in
	InstallGitHook bool // enable ambient commit-trailer hook install (off by default)
	// ProjectDir selects PROJECT hook scope, which is `openbox init`'s default
	// (ADR-0016): the adapter merges its hook block into
	// <dir>/.claude/settings.local.json, so sessions in that project are governed
	// and sessions elsewhere are not. Empty means GLOBAL scope — the bundle is
	// still installed, but activation waits on a managed-settings deployment.
	ProjectDir string
	// Enforce turns enforce mode on or off and persists it (plus its companions,
	// Findings) into the dev config, so no runtime env var is needed
	// (ADR-0006 for the mechanism). Enforce now defaults ON (ADR-0016) — it is
	// resolved from the ABSENCE of the field, not written by every run.
	//
	// All three are *bool so nil means "this run did not say", leaving whatever is
	// on disk untouched. That is load-bearing in both directions: a plain `init`
	// re-run must not drop a developer out of enforce mode, AND must not silently
	// re-enable it for someone who chose --enforce=false.
	Enforce  *bool
	Findings *bool
}

// Deps are the injected collaborators (all faked in tests).
type Deps struct {
	Registrar Registrar
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

// Register is the credential half: register a developer agent (or recognize that
// this machine already has credentials) and write the two secrets to
// ~/.openbox/.env. It installs nothing and touches no tool config.
//
// It exists as its own entry point because `openbox auth` owns authentication
// while `openbox init` owns setup (ADR-0015/ADR-0016). auth needs exactly the
// behaviour proven here — remote duplicate detection, the HALT-on-4xx stop
// condition, the once-only-credential guard, and the resume error that names the
// registered agent — with no installer running.
//
// The returned CredentialRef carries the coordinates the caller should persist:
// DID and AgentID are set on the register path. Run wraps this and adds the
// provider install.
func Register(ctx context.Context, o Options, d Deps) (*Result, provider.CredentialRef, error) {
	return register(ctx, o, d)
}

// Run executes the onboarding flow. It returns a non-nil error when the command
// should exit non-zero; Result is still populated for reporting on partial
// success (e.g. registered but config-manual-only).
func Run(ctx context.Context, o Options, d Deps) (*Result, error) {
	res, ref, err := register(ctx, o, d)
	if err != nil || res == nil {
		return res, err
	}
	if o.DryRun {
		// planDryRun already printed the plan, including the installer's own
		// section; there is nothing to apply.
		return res, nil
	}
	return res, applyConfig(o, d, ref, res)
}

// register is Run without the install step. See Register.
func register(ctx context.Context, o Options, d Deps) (*Result, provider.CredentialRef, error) {
	if o.Provider == "" {
		return nil, provider.CredentialRef{}, errors.New("--provider is required (one of: " + strings.Join(provider.Supported(), ", ") + ")")
	}
	profile := aivss.DefaultDeveloperProfile()
	if field, ok := profile.Validate(); !ok {
		return nil, provider.CredentialRef{}, fmt.Errorf("default aivss profile field %s out of range (build bug)", field)
	}
	name := o.AgentName
	if name == "" {
		name = defaultAgentName(o.Provider)
	}
	icon := o.Icon
	if icon == "" {
		icon = "🧑‍💻"
	}
	ref := provider.CredentialRef{
		InstallGitHook: o.InstallGitHook,
		// Persist the control-plane base so `dev sync`/staleness can reach
		// the policy read without re-supplying OPENBOX_BACKEND_URL. The
		// agent id is set below on the register path (the reuse path
		// preserves a prior value — installer.writeConfig).
		BackendURL: o.BackendURL,
		// The data-plane base, persisted so the hook and `dev verify` reach the
		// core this install belongs to. Empty ⇒ the adapter default (SaaS).
		BaseURL: o.BaseURL,
		// Project hook scope: govern one project via its
		// .claude/settings.local.json. Empty = global scope, whose activation is
		// a managed-settings deployment this command cannot perform.
		ProjectDir: o.ProjectDir,
		// Persist the enforce posture into dev.json so the runtime hook needs no
		// env var. Enforce defaults ON (ADR-0016).
		Enforce:  o.Enforce,
		Findings: o.Findings,
	}
	res := &Result{AgentName: name}

	// --- dry-run: describe, write nothing, touch no network ------------------
	if o.DryRun {
		return res, ref, planDryRun(o, d, name, icon, profile, ref)
	}

	// --- idempotency: the local credential file is the source of truth -------
	// The obx_ key and signing key are shown by agent/create exactly once, so a
	// prior successful registration is only recoverable locally. If both are
	// present, reuse them and skip registration entirely.
	//
	// The DID comes from dev.json rather than from beside the credentials, which
	// is the ADR-0015 split doing its job: before it, the DID lived in the
	// keychain too and this reuse path read the keychain's copy and wrote it into
	// dev.json — so a stale keychain entry silently reverted a corrected DID on
	// every re-init. With one store per field there is nothing to revert from.
	//
	// ONE IDENTITY PER MACHINE, and the message must not pretend otherwise. The
	// deleted keychain namespaced credentials by `<org>/<provider>`, so this check
	// could ask "are there credentials for THIS org and provider". `.env` holds a
	// single key pair with no namespace, so all this can honestly report is that
	// *some* identity is already configured — it cannot know whether it belongs to
	// the org and provider this run named. Saying "already initialized for
	// org=beta provider=codex" while describing acme/claude-code's credentials
	// would be a false statement about identity in a governance tool, which is
	// worse than the friction of a vaguer message.
	if existing, err := readLocalCredentials(o.EnvFile); err == nil && existing.apiKey != "" && existing.privateKey != "" {
		did := devconfig.ResolveDIDOrEmpty()
		ref.DID = did
		res.Reused = true
		res.DID = did
		fmt.Fprintf(d.Out, "This machine already has credentials in %s — reusing them (DID %s).\n",
			credentialFileLabel(o.EnvFile), didOrNone(did))
		fmt.Fprintf(d.Out, "  Nothing was registered. A machine holds ONE agent identity: if these belong to a\n")
		fmt.Fprintf(d.Out, "  different org or tool than you intended, `openbox auth` overwrites them, and\n")
		fmt.Fprintf(d.Out, "  `openbox doctor` shows which identity is in effect.\n")
		return res, ref, nil
	}

	// --- remote duplicate detection (avoid a 400, never duplicate silently) --
	// A lookup FAILURE must not fall through to Create: we could neither confirm
	// nor rule out a duplicate, so idempotent detection would be silently bypassed
	// (F1). Surface it and let the user retry or --force past the check.
	if !o.Force {
		existing, err := d.Registrar.FindByName(ctx, name)
		if err != nil {
			return res, ref, fmt.Errorf(
				"could not check for an existing agent named %q (agent/list failed: %w); "+
					"re-run when the OpenBox org is reachable, or pass --force to skip the check",
				name, err)
		}
		if existing != nil {
			res.AgentID, res.DID = existing.ID, existing.DID
			return res, ref, fmt.Errorf(
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
			return res, ref, fmt.Errorf("could not find a free agent name for --force (agent/list failed: %w)", err)
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
			return res, ref, fmt.Errorf("HALT: agent/create rejected registration (%w). "+
				"Verify agent_type=%q and the default aivss_config are accepted by this OpenBox org",
				apiErr, developerAgentType)
		}
		return res, ref, fmt.Errorf("agent/create failed: %w", err)
	}
	res.AgentID, res.DID, res.Registered = reg.AgentID, reg.DID, true
	ref.DID = reg.DID
	ref.AgentID = reg.AgentID // persisted to dev.json for `dev sync`/staleness

	if reg.APIKey == "" || reg.PrivateKey == "" {
		return res, ref, fmt.Errorf(
			"agent registered (id %s, DID %s) but the response did not include %s — "+
				"cannot store runtime credentials; rotate the key or re-provision the identity",
			reg.AgentID, reg.DID, missingCreds(reg))
	}

	// --- capture credentials into ~/.openbox/.env (INV-1) --------------------
	// One atomic write for both secrets, so there is no half-written credential
	// pair to reason about. The DID is not written here — it is a coordinate and
	// goes to dev.json via the installer (ADR-0015's one-store-per-field split).
	// On failure after the agent exists, the error names the registered agent id
	// so the operator can resume.
	if err := writeLocalCredentials(o.EnvFile, reg.APIKey, reg.PrivateKey); err != nil {
		return res, ref, resumeErr(reg, "write credentials to "+credentialFileLabel(o.EnvFile), err)
	}

	fmt.Fprintf(d.Out, "Registered developer agent %q\n  id:    %s\n  DID:   %s\n  tier:  %s (trust %s)\n",
		reg.AgentName, reg.AgentID, reg.DID, reg.Tier, reg.TrustScore)
	fmt.Fprintf(d.Out, "Credentials written to %s (0600); values are not printed (INV-1).\n",
		credentialFileLabel(o.EnvFile))
	if o.ManagedEnable {
		fmt.Fprintln(d.Out, "Managed force-enable substrate recorded (verified, not activated — Phase-1 pilot is opt-in).")
	}

	return res, ref, nil
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
	fmt.Fprintf(d.Out, "Wrote %s native config (no secrets inline — the hook reads %s at runtime).\n",
		o.Provider, credentialFileLabel(o.EnvFile))
	return nil
}

// describePosture renders a tri-state posture flag for the plan output, so an
// unspecified setting reads as "left alone" rather than as a chosen false.
func describePosture(v *bool) string {
	switch {
	case v == nil:
		return "unchanged (keeps whatever dev.json already has)"
	case *v:
		return "true"
	default:
		return "false"
	}
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
	fmt.Fprintf(out, "  install_git_hook: %t (ambient commit-trailer hook; off by default — modifies .git/hooks)\n", o.InstallGitHook)
	fmt.Fprintf(out, "  enforce: %s (ADR-0016: ON by default — inert until your org publishes a policy, and\n", describePosture(o.Enforce))
	fmt.Fprintf(out, "           fail-open regardless. --enforce=false opts out and persists. Enforce also\n")
	fmt.Fprintf(out, "           carries tier2 + findings, all persisted to dev.json — no runtime env)\n")
	fmt.Fprintf(out, "\nWould write credentials to %s (0600, plaintext — ADR-0015):\n", credentialFileLabel(o.EnvFile))
	fmt.Fprintf(out, "  %s  (obx_ API key)\n  %s  (Ed25519 signing key)\n", devconfig.EnvAPIKeyDirect, devconfig.EnvAgentPrivateKey)
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
