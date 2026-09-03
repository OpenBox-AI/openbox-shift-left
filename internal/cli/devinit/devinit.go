// Package devinit registers a developer agent, captures its once-shown
// credentials into ~/.openbox/.env, and delegates the tool's native config to
// the provider installer. Invariants enforced here: - INV-1: the obx_ key and
// signing key are written only to the credential file and never printed,
// logged, or placed on an argv. Output shows the file path, never a value.
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

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/aivss"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/backend"
	"github.com/openbox-ai/openbox-shift-left/internal/provider"
)

const developerAgentType = "developer" // free-form agent_type; no migration

// Registrar is the control-plane surface devinit needs (backend.Client
// implements it).
type Registrar interface {
	Create(ctx context.Context, req backend.CreateAgentRequest) (*backend.Registration, error)
	FindByName(ctx context.Context, name string) (*backend.AgentSummary, error)
}

// Options are the user-facing knobs for `init`.
type Options struct {
	Provider   string // claude-code|codex|cursor
	BackendURL string // openbox-backend control-plane base (persisted for `dev sync`/staleness)
	// BaseURL is the openbox-core data-plane base; where events are emitted and
	// where `dev verify` authenticates.
	BaseURL        string
	AgentName      string // override; default derived from user+host
	Icon           string // non-empty string required by the backend DTO
	Description    string
	DryRun         bool
	ManagedEnable  bool // org-wide force-enable substrate; opt-in
	InstallGitHook bool // enable ambient commit-trailer hook install (off by default)
	// ProjectDir selects project hook scope, which is `openbox init`'s default :
	// the adapter merges its hook block into <dir>/.claude/settings.local.json,
	// so sessions in that project are governed and sessions elsewhere are not.
	ProjectDir string
	// Enforce turns enforce mode on or off and persists it (plus its companions,
	// Findings) into the dev config, so no runtime env var is needed (that
	// decision for the mechanism).
	Enforce  *bool
	Findings *bool
}

// Deps are the injected collaborators (all faked in tests).
type Deps struct {
	Registrar Registrar
	Installer provider.Installer
	Out       io.Writer
}

// Result summarizes what happened, for the caller to render / pick an exit
// code.
type Result struct {
	AgentID          string
	DID              string
	AgentName        string
	Reused           bool // creds already present locally; no registration done
	Registered       bool // a new agent was created this run
	ConfigApplied    bool // provider installer ran
	ConfigManualOnly bool // adapter not built; manual config was printed
}

func defaultAgentName() string {
	u := "user"
	if cu, err := user.Current(); err == nil && cu.Username != "" {
		u = cu.Username
	}
	h, err := os.Hostname()
	if err != nil || h == "" {
		h = "host"
	}
	name := fmt.Sprintf("openbox-dev-%s@%s", u, h)
	return truncate(name, 255)
}

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

// Register is the credential half: register a developer agent (or recognize
// that this machine already has credentials) and write the two secrets to
// ~/.openbox/.env.
func Register(ctx context.Context, o Options, d Deps) (*Result, provider.CredentialRef, error) {
	return register(ctx, o, d)
}

// Run executes the onboarding flow.
func Run(ctx context.Context, o Options, d Deps) (*Result, error) {
	// Checked before registration so a missing flag fails immediately rather than
	// after creating an agent that then cannot be installed for.
	if o.Provider == "" {
		return nil, errors.New("--provider is required (one of: " + strings.Join(provider.Supported(), ", ") + ")")
	}
	res, ref, err := register(ctx, o, d)
	if err != nil || res == nil {
		return res, err
	}
	if o.DryRun {
		return res, nil
	}
	return res, applyConfig(o, d, ref, res)
}

func register(ctx context.Context, o Options, d Deps) (*Result, provider.CredentialRef, error) {
	profile := aivss.DefaultDeveloperProfile()
	if field, ok := profile.Validate(); !ok {
		return nil, provider.CredentialRef{}, fmt.Errorf("default aivss profile field %s out of range (build bug)", field)
	}
	name := o.AgentName
	if name == "" {
		name = defaultAgentName()
	}
	icon := o.Icon
	if icon == "" {
		icon = "🧑‍💻"
	}
	ref := provider.CredentialRef{
		InstallGitHook: o.InstallGitHook,
		BackendURL:     o.BackendURL,
		BaseURL:        o.BaseURL,
		// Empty = global scope, whose activation is a managed-settings deployment
		// this command cannot perform.
		ProjectDir: o.ProjectDir,
		Enforce:    o.Enforce,
		Findings:   o.Findings,
	}
	res := &Result{AgentName: name}

	if o.DryRun {
		return res, ref, planDryRun(o, d, name, icon, profile, ref)
	}

	if existing, err := readLocalCredentials(); err == nil && existing.apiKey != "" && existing.privateKey != "" {
		did := devconfig.ResolveDIDOrEmpty()
		ref.DID = did
		res.Reused = true
		res.DID = did
		// Losing it disables that check silently.
		res.AgentID = devconfig.ResolveAgentID()
		ref.AgentID = res.AgentID
		fmt.Fprintf(d.Out, "This machine already has credentials in %s; reusing them (DID %s).\n",
			credentialFileLabel(), didOrNone(did))
		fmt.Fprintf(d.Out, "  Nothing was registered. A machine holds ONE agent identity: if these belong to a\n")
		fmt.Fprintf(d.Out, "  different org or tool than you intended, `openbox auth` overwrites them, and\n")
		fmt.Fprintf(d.Out, "  `openbox doctor` shows which identity is in effect.\n")
		return res, ref, nil
	}

	// Unconditional now. The escape hatch was --force, which registered a
	// second, differently-named agent; without it there is exactly one recovery
	// and the message has to say what it costs rather than name a flag.
	existing, err := d.Registrar.FindByName(ctx, name)
	if err != nil {
		return res, ref, fmt.Errorf(
			"could not check for an existing agent named %q (agent/list failed: %w); "+
				"re-run when the OpenBox org is reachable",
			name, err)
	}
	if existing != nil {
		res.AgentID, res.DID = existing.ID, existing.DID
		return res, ref, fmt.Errorf(
			"a developer agent named %q already exists in this org (id %s, DID %s) but this machine holds "+
				"no credentials for it.\n"+
				"  Its API key and signing key were shown once, at registration, and are not stored "+
				"server-side -- so if they are lost, that agent's identity cannot be recovered at all.\n"+
				"  If you still have them, re-run `openbox auth` and give the agent id %s.\n"+
				"  If you do not: delete that agent in the dashboard and re-run. That mints a NEW DID, "+
				"so work attributed to the old one stays attached to the old one.",
			name, existing.ID, existing.DID, existing.ID)
	}

	req := backend.CreateAgentRequest{
		AgentName:   name,
		AgentType:   developerAgentType,
		Icon:        icon,
		Description: o.Description,
		Tags:        []string{"openbox-shift-left", "developer-runtime"},
		AivssConfig: profile,
		Config: map[string]any{
			"managed_enable": o.ManagedEnable, // substrate only; not activated in Phase 1
		},
	}
	reg, err := d.Registrar.Create(ctx, req)
	if err != nil {
		var apiErr *backend.APIError
		if errors.As(err, &apiErr) {
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
			"agent registered (id %s, DID %s) but the response did not include %s; "+
				"cannot store runtime credentials; rotate the key or re-provision the identity",
			reg.AgentID, reg.DID, missingCreds(reg))
	}

	if err := writeLocalCredentials(reg.APIKey, reg.PrivateKey); err != nil {
		return res, ref, resumeErr(reg, "write credentials to "+credentialFileLabel(), err)
	}

	fmt.Fprintf(d.Out, "Registered developer agent %q\n  id:    %s\n  DID:   %s\n  tier:  %s (trust %s)\n",
		reg.AgentName, reg.AgentID, reg.DID, reg.Tier, reg.TrustScore)
	fmt.Fprintf(d.Out, "Credentials written to %s (0600); values are not printed (INV-1).\n",
		credentialFileLabel())
	if o.ManagedEnable {
		fmt.Fprintln(d.Out, "Managed force-enable substrate recorded (verified, not activated; Phase-1 pilot is opt-in).")
	}

	return res, ref, nil
}

func applyConfig(o Options, d Deps, ref provider.CredentialRef, res *Result) error {
	if !d.Installer.Available() {
		res.ConfigManualOnly = true
		fmt.Fprintf(d.Out, "\nProvider config not applied; %s\n\n%s\n", o.Provider, d.Installer.Plan(ref))
		return fmt.Errorf("provider %q config not applied: adapter not built yet (see manual config above)", o.Provider)
	}
	if err := d.Installer.Install(ref); err != nil {
		return fmt.Errorf("agent ready but writing %s config failed: %w", o.Provider, err)
	}
	res.ConfigApplied = true
	fmt.Fprintf(d.Out, "Wrote %s native config (no secrets inline; the hook reads %s at runtime).\n",
		o.Provider, credentialFileLabel())
	return nil
}

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

func planDryRun(o Options, d Deps, name, icon string, profile aivss.Config, ref provider.CredentialRef) error {
	out := d.Out
	fmt.Fprintf(out, "DRY RUN; no network calls, no secret-store or filesystem writes.\n\n")
	fmt.Fprintf(out, "Would register developer agent:\n")
	fmt.Fprintf(out, "  provider:    %s\n", o.Provider)
	fmt.Fprintf(out, "  agent_name:  %s\n", name)
	fmt.Fprintf(out, "  agent_type:  %s\n", developerAgentType)
	fmt.Fprintf(out, "  icon:        %s\n", icon)
	fmt.Fprintf(out, "  aivss_config: base_security/ai_specific/impact (accepted developer posture; server computes score/tier)\n")
	fmt.Fprintf(out, "  managed_enable: %t (substrate only; not activated in Phase 1)\n", o.ManagedEnable)
	fmt.Fprintf(out, "  install_git_hook: %t (ambient commit-trailer hook; off by default; modifies .git/hooks)\n", o.InstallGitHook)
	fmt.Fprintf(out, " enforce: %s (that decision: ON by default; inert until your org publishes a policy, and\n", describePosture(o.Enforce))
	fmt.Fprintf(out, "           fail-open regardless. --enforce=false opts out and persists. Enforce also\n")
	fmt.Fprintf(out, "           carries tier2 + findings, all persisted to dev.json; no runtime env)\n")
	fmt.Fprintf(out, "\nWould write credentials to %s (0600, plaintext):\n", credentialFileLabel())
	fmt.Fprintf(out, "  %s  (obx_ API key)\n  %s  (Ed25519 signing key)\n", devconfig.EnvAPIKeyDirect, devconfig.EnvAgentPrivateKey)
	fmt.Fprintf(out, "\nProvider config:\n%s\n", d.Installer.Plan(ref))
	return nil
}

func resumeErr(reg *backend.Registration, step string, err error) error {
	return fmt.Errorf("agent registered (id %s, DID %s) but failed to %s: %w; "+
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
