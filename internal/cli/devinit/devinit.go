// Package devinit registers a developer agent, captures its once-shown
// credentials into that tool's ~/.openbox/<tool>/.env, and delegates the
// tool's native config to the provider installer. One store per governed tool:
// the reuse decision reads that tool's own file and never falls back to the
// org-level one. Invariants enforced here: - INV-1: the obx_ key and
// signing key are written only to the credential file and never printed,
// logged, or placed on an argv. Output shows the file path, never a value.
package devinit

import (
	"context"
	"crypto/rsa"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"runtime"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/aivss"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/backend"
	"github.com/openbox-ai/openbox-shift-left/internal/client/workloadauth"
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
	// where the control-plane check authenticates.
	BaseURL        string
	AgentName      string // override; default derived from user+host
	Icon           string // non-empty string required by the backend DTO
	Description    string
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
	// AssumeExistingStore is set by the caller when it has already written a
	// usable store THIS run, outside the file/env shapes register's own reuse
	// check reads -- the one case today is a successful interactive adopt
	// (cmd/openbox/adopt.go), which still writes the pre-v3 DID+seed shape.
	// Without this, a freshly adopted store falls through register's v3 reuse
	// check (adopt writes no workload key) into a fresh registration with no
	// registrar wired, discarding what the operator just pasted. Adopt's own
	// prompts and messages are unchanged here; phase 05 owns their v3-native
	// redesign.
	AssumeExistingStore bool
}

// Deps are the injected collaborators (all faked in tests).
type Deps struct {
	Registrar Registrar
	Installer provider.Installer
	Out       io.Writer
	// GenerateKey creates the RSA key a fresh registration proves possession of
	// via its public JWK. workloadauth.GenerateKey when nil; a test injects a
	// fake to avoid paying for RSA keygen on every case, or to drive an error.
	GenerateKey func() (*rsa.PrivateKey, error)
	// DiscardLegacySpool discards one tool's spooled events queued under the
	// legacy identity this run's write is about to replace, and reports how
	// many it removed. nil (every fixture here, and cmd/openbox's wiring until
	// a later phase supplies a real implementation) means "discard nothing":
	// register calls it exactly the way a real implementation would, so
	// wiring one in is a Deps assignment, not a change to this file.
	DiscardLegacySpool func(tool string) (int, error)
}

// Result summarizes what happened, for the caller to render / pick an exit
// code.
type Result struct {
	AgentID   string
	DID       string // the derived attribution label (D1), best-effort; "" if AgentID itself is not a UUID
	AgentName string
	// IdentityKid is the registered keycloak_workload key's RFC 7638
	// thumbprint, printed in place of a DID for a fresh registration.
	IdentityKid   string
	Reused        bool // creds already present locally; no registration done
	Registered    bool // a new agent was created this run
	ConfigApplied bool // provider installer ran
}

// defaultAgentName names the agent after the tool it governs and the machine
// it runs on. The tool half is what makes it a default at all now: a machine
// holds one agent per governed tool, two agents cannot share a name in an org,
// and a name that omitted the tool would collide with itself on the second
// `init`.
//
// The host half reads scutil on darwin because os.Hostname() there returns
// whatever DHCP most recently handed out -- it changes with the network, and
// produced agent names carrying a stale IP address. Any failure falls back to
// os.Hostname(): this is only a default, Options.AgentName overrides it, and
// nothing about registration may block on an exec.
func defaultAgentName(tool string) string {
	u := "user"
	if cu, err := user.Current(); err == nil && cu.Username != "" {
		u = cu.Username
	}
	name := fmt.Sprintf("%s-%s@%s", tool, u, localHostName())
	return truncate(name, 255)
}

func localHostName() string {
	if runtime.GOOS == "darwin" {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if out, err := exec.CommandContext(ctx, "scutil", "--get", "LocalHostName").Output(); err == nil {
			if h := strings.TrimSpace(string(out)); h != "" {
				return h
			}
		}
	}
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "host"
	}
	return h
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
	return res, applyConfig(o, d, ref, res)
}

func register(ctx context.Context, o Options, d Deps) (*Result, provider.CredentialRef, error) {
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
		BackendURL:     o.BackendURL,
		BaseURL:        o.BaseURL,
		// Empty = global scope, whose activation is a managed-settings deployment
		// this command cannot perform.
		ProjectDir: o.ProjectDir,
		Enforce:    o.Enforce,
		Findings:   o.Findings,
	}
	res := &Result{AgentName: name}

	stored, readErr := readLocalCredentials()
	fromFile := readErr == nil && stored.apiKey != "" && stored.workloadKey != ""
	// The environment counts, and has to, because the caller's gate already
	// accepted it: a machine provisioned entirely through exported variables
	// would otherwise reach the registration branch with no registrar wired and
	// be told it had hit a build bug. An exported identity also outranks every
	// store at runtime, so minting an agent here would create one that is never
	// used -- one per ephemeral runner. A legacy store (a stored developer_did,
	// or a seed under the current or a deprecated name) never satisfies either
	// check: EnvIdentityPresent and readLocalCredentials both ask the v3
	// question, so a legacy store falls through to registration below, same as
	// no store at all.
	if fromFile || devconfig.EnvIdentityPresent() || o.AssumeExistingStore {
		agentID := devconfig.ResolveAgentID()
		res.Reused = true
		res.AgentID = agentID
		ref.AgentID = agentID
		if did, err := devconfig.AttributionDIDFor(agentID); err == nil {
			res.DID = did
		}
		where := credentialFileLabel()
		if !fromFile && !o.AssumeExistingStore {
			where = "the environment"
		}
		fmt.Fprintf(d.Out, "Reusing the existing %s agent; nothing was registered.\n", o.Provider)
		fmt.Fprintf(d.Out, "  %-12s %s\n", "agent id", agentIDOrNone(agentID))
		// Reuse is a file test, so a key that was revoked server-side still looks
		// like a complete store and this run never goes online to find out. That
		// makes deleting the file the whole rotation procedure, and a reader who
		// is not told will look for a flag that does not exist. Which tool this
		// store belongs to, and which identity wins, is `openbox doctor`'s.
		fmt.Fprintf(d.Out, "  %-12s %s\n", "credentials", where)
		if fromFile {
			fmt.Fprintf(d.Out, "  %-12s delete it and re-run to rotate the key, keeping this agent id\n", "")
		}
		return res, ref, nil
	}

	// Reaching here means the tool has no usable store, so this run must
	// register -- and the caller wires a Registrar only on that branch. A nil
	// one is a wiring defect, and naming it beats dereferencing it: the panic
	// this replaces surfaced as a stack trace from a hook path where every
	// other failure is a sentence.
	if d.Registrar == nil {
		return res, ref, fmt.Errorf(
			"no registrar wired: init reached the registration branch for %s with no control-plane "+
				"client. This is a build or wiring bug, not a configuration problem", o.Provider)
	}

	// Unconditional now. The escape hatch was --force, which registered a
	// second, differently-named agent; without it there is exactly one recovery
	// and the message has to say what it costs rather than name a flag.
	existing, err := d.Registrar.FindByName(ctx, name)
	if err != nil {
		// A rejected credential is not an outage, and saying so sends the
		// operator to wait for connectivity they already have. The backend
		// answered; what it rejected is the organization token, and by far the
		// most common reason is that the token belongs to a different
		// deployment than the backend URL being used -- `auth` takes both and
		// validates neither against each other. The URL comes with the error,
		// so the message can name the host the token was actually offered to.
		var apiErr *backend.APIError
		if errors.As(err, &apiErr) && (apiErr.StatusCode == http.StatusUnauthorized ||
			apiErr.StatusCode == http.StatusForbidden) {
			return res, ref, fmt.Errorf(
				"the OpenBox organization rejected this machine's credential (agent/list failed: %w).\n"+
					"  The backend answered, so this is not an outage; the token was refused by it.\n"+
					"  Most often the token and the backend URL belong to different deployments:\n"+
					"  check that the organization token came from the dashboard of the backend named\n"+
					"  above, then re-run `openbox auth` to correct either, or export "+
					"%s for one run.",
				err, devconfig.EnvControlToken)
		}
		return res, ref, fmt.Errorf(
			"could not check for an existing agent named %q (agent/list failed: %w); "+
				"re-run when the OpenBox org is reachable",
			name, err)
	}
	if existing != nil {
		res.AgentID, res.DID = existing.ID, existing.DID
		return res, ref, fmt.Errorf(
			"a developer agent named %q already exists in this org (id %s, DID %s) but %s holds "+
				"no credentials for it on this machine.\n"+
				"  Most likely this machine registered it before, and its store was deleted or moved; "+
				"the other cause is a second machine reporting the same user and host name.\n"+
				"  Its API key and signing key were shown once, at registration, and are not stored "+
				"server-side -- so if they are lost, that agent's identity cannot be recovered at all.\n"+
				"  If you still have them, re-run `openbox init --provider %s` and answer yes when it "+
				"offers to adopt an existing agent; it takes the id %s, the DID, the key and the seed.\n"+
				"  If you do not: delete %q in the dashboard and re-run. That mints a NEW DID, "+
				"so work attributed to the old one stays attached to the old one.",
			name, existing.ID, existing.DID, o.Provider, o.Provider, existing.ID, name)
	}

	genKey := d.GenerateKey
	if genKey == nil {
		genKey = workloadauth.GenerateKey
	}
	priv, err := genKey()
	if err != nil {
		return res, ref, fmt.Errorf("generate workload signing key: %w", err)
	}
	jwk, err := workloadauth.PublicJWK(&priv.PublicKey)
	if err != nil {
		return res, ref, fmt.Errorf("build public jwk: %w", err)
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
		IdentityVerification: &backend.IdentityVerification{
			Method:     devconfig.IdentityMethodKeycloakWorkload,
			Mode:       "generate",
			SourceType: "openbox",
			PublicJWK:  jwkAsMap(jwk),
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
	res.AgentID, res.Registered = reg.AgentID, true
	res.IdentityKid = reg.Identity.Kid
	ref.AgentID = reg.AgentID // persisted to dev.json for `dev sync`/staleness; ref.DID stays unset (D1: never stored)
	ref.IdentityMethod = devconfig.IdentityMethodKeycloakWorkload

	if reg.Identity.Method != devconfig.IdentityMethodKeycloakWorkload || reg.APIKey == "" {
		return res, ref, fmt.Errorf(
			"agent registered (id %s) but the response did not confirm a %s identity or a token; "+
				"cannot store runtime credentials; rotate the key or re-provision the identity",
			reg.AgentID, devconfig.IdentityMethodKeycloakWorkload)
	}

	workloadKeyB64, err := workloadauth.EncodePrivateKey(priv)
	if err != nil {
		return res, ref, fmt.Errorf("encode workload signing key: %w", err)
	}

	// Captured before the write: WriteWorkloadIdentity clears the legacy
	// markers this reads, so reading it after would always answer false.
	wasLegacy := false
	if ls, lerr := devconfig.LegacyStoreFor(o.Provider); lerr == nil {
		wasLegacy = ls.Legacy
	}

	if err := devconfig.WriteWorkloadIdentity(o.Provider, devconfig.WorkloadIdentity{
		AgentID:       reg.AgentID,
		APIKey:        reg.APIKey,
		PrivateKeyB64: workloadKeyB64,
	}); err != nil {
		return res, ref, resumeErr(reg, "write credentials to "+credentialFileLabel(), err)
	}

	if wasLegacy && d.DiscardLegacySpool != nil {
		if n, derr := d.DiscardLegacySpool(o.Provider); derr == nil && n > 0 {
			fmt.Fprintf(d.Out, "discarded %d events queued under the previous identity\n", n)
		}
	}

	fmt.Fprintf(d.Out, "Registered developer agent %q\n", reg.AgentName)
	fmt.Fprintf(d.Out, "  %-12s %s\n", "id", reg.AgentID)
	fmt.Fprintf(d.Out, "  %-12s %s\n", "identity", devconfig.IdentityMethodKeycloakWorkload+" (kid "+reg.Identity.Kid+")")
	if tier := tierLabel(reg.Tier, reg.TrustScore); tier != "" {
		fmt.Fprintf(d.Out, "  %-12s %s\n", "tier", tier)
	}
	fmt.Fprintf(d.Out, "  %-12s %s (0600; values never printed, INV-1)\n",
		"credentials", credentialFileLabel())
	if o.ManagedEnable {
		fmt.Fprintln(d.Out, "Managed force-enable substrate recorded (verified, not activated; Phase-1 pilot is opt-in).")
	}

	return res, ref, nil
}

// applyConfig every recognized provider has a built adapter, so there is no
// not-built branch left: a name either resolves to an installer or was refused
// as unknown before reaching here.
func applyConfig(o Options, d Deps, ref provider.CredentialRef, res *Result) error {
	if err := d.Installer.Install(ref); err != nil {
		return fmt.Errorf("agent ready but writing %s config failed: %w", o.Provider, err)
	}
	res.ConfigApplied = true
	fmt.Fprintf(d.Out, "  %-12s Wrote %s native config; the hook reads the .env above\n",
		"config", o.Provider)
	return nil
}

// tierLabel renders the pair only when the control plane actually sent one. An
// unscored agent arrives with both halves empty, and TrustScore comes through
// fmt.Sprint of an any, so "absent" reads as the literal "<nil>" -- which is
// what "tier:   (trust <nil>)" was.
func tierLabel(tier, trust string) string {
	scored := trust != "" && trust != "<nil>"
	switch {
	case tier != "" && scored:
		return tier + " (trust " + trust + ")"
	case tier != "":
		return tier
	case scored:
		return "trust " + trust
	default:
		return ""
	}
}

func resumeErr(reg *backend.Registration, step string, err error) error {
	return fmt.Errorf("agent registered (id %s, DID %s) but failed to %s: %w; "+
		"the API key and signing key were shown only once; rotate the key and re-run, or complete the step manually",
		reg.AgentID, reg.DID, step, err)
}

// jwkAsMap projects a workloadauth.JWK onto the map[string]string shape
// backend.IdentityVerification.PublicJWK carries over the wire.
func jwkAsMap(j workloadauth.JWK) map[string]string {
	return map[string]string{
		"kty": j.Kty,
		"n":   j.N,
		"e":   j.E,
		"kid": j.Kid,
		"alg": j.Alg,
		"use": j.Use,
	}
}
