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
	"crypto/rand"
	"crypto/rsa"
	"encoding/hex"
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
	// Findings persists the findings-loop posture into the dev config, so no
	// runtime env var is needed.
	Findings *bool
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
	// a real implementation is supplied) means "discard nothing":
	// register calls it exactly the way a real implementation would, so
	// wiring one in is a Deps assignment, not a change to this file.
	DiscardLegacySpool func(tool string) (int, error)
	// Suffix generates the 6-lowercase-hex-character suffix appended to a name
	// already taken in the org (randomSuffix when nil): crypto/rand once per
	// call, so a test can pin the sequence rather than pattern-match a random
	// result.
	Suffix func() (string, error)
}

// Result summarizes what happened, for the caller to render / pick an exit
// code.
type Result struct {
	AgentID   string
	AgentName string
	// IdentityMethod is devconfig.IdentityMethodKeycloakWorkload on every
	// successful result (reused or freshly registered): the v3 binary never
	// resolves or registers anything else. Empty only when Run returned an
	// error before an identity was confirmed.
	IdentityMethod string
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

// randomSuffix is Deps.Suffix's production implementation: 6 lowercase hex
// characters from crypto/rand, appended to a name already taken in the org.
func randomSuffix() (string, error) {
	b := make([]byte, 3)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate a name suffix: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// suffixedName appends "-<suffix>" to name, trimming name rather than the
// suffix when the whole would exceed the backend's 255-byte limit: cutting the
// suffix off would register the taken name again.
func suffixedName(name, suffix string) string {
	return truncate(name, 255-len(suffix)-1) + "-" + suffix
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
		Findings:   o.Findings,
	}
	res := &Result{AgentName: name}

	stored, readErr := readLocalCredentials()
	fromFile := readErr == nil && stored.apiKey != "" && stored.workloadKey != ""
	// A store holding v3 keys beside a leftover legacy marker (a stored
	// developer_did, or a seed) is one the resolver refuses, so reusing it would
	// print "Reusing" over a tool whose hooks send nothing. It registers instead.
	if ls, lerr := devconfig.LegacyStoreFor(o.Provider); lerr == nil && ls.Legacy {
		fromFile = false
	}
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
	if fromFile || devconfig.EnvIdentityPresent() {
		agentID := devconfig.ResolveAgentID()
		res.Reused = true
		res.AgentID = agentID
		res.IdentityMethod = devconfig.IdentityMethodKeycloakWorkload
		ref.AgentID = agentID
		// Recorded on reuse too: a store provisioned through the environment
		// reaches dev.json only through this install, and doctor reads the
		// method to recognise a v3 identity.
		ref.IdentityMethod = devconfig.IdentityMethodKeycloakWorkload
		where := credentialFileLabel()
		if !fromFile {
			where = "the environment"
		}
		fmt.Fprintf(d.Out, "Reusing the existing %s agent; nothing was registered.\n", o.Provider)
		fmt.Fprintf(d.Out, "  %-12s %s\n", "agent id", agentIDOrNone(agentID))
		// Reuse is a file test, so a key that was revoked server-side still looks
		// like a complete store and this run never goes online to find out. There
		// is no in-place key rotation: deleting the file and re-running registers
		// a new agent (suffixed, since this one keeps its name), and a reader who
		// is not told will expect the old agent id to survive. Which tool this
		// store belongs to, and which identity wins, is `openbox doctor`'s.
		fmt.Fprintf(d.Out, "  %-12s %s\n", "credentials", where)
		if fromFile {
			fmt.Fprintf(d.Out, "  %-12s deleting it and re-running registers a new agent; this agent id is not kept\n", "")
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

	// One line, printed once, right before the run that will actually replace
	// the store: LegacyStoreFor is file-only, so a legacy store here means
	// this tool's identity predates v3 and hooks have been sending nothing.
	if ls, lerr := devconfig.LegacyStoreFor(o.Provider); lerr == nil && ls.Legacy {
		fmt.Fprintln(d.Out, "this tool holds a legacy (pre-IAMv3) identity that hooks can no longer use; "+
			"registering a workload agent replaces it")
	}

	suffixFn := d.Suffix
	if suffixFn == nil {
		suffixFn = randomSuffix
	}

	// Unconditional now. The escape hatch was --force, which registered a
	// second, differently-named agent; a taken name now gets a fresh suffix
	// instead of a halt, since a lost store can no longer be repaired by
	// pasting a DID + seed that no longer exist.
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

	registerName := name
	if existing != nil {
		suffix, serr := suffixFn()
		if serr != nil {
			return res, ref, fmt.Errorf("an agent named %q already exists (id %s), and generating a "+
				"replacement suffix failed: %w", name, existing.ID, serr)
		}
		registerName = suffixedName(name, suffix)
		res.AgentName = registerName
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
		AgentName:   registerName,
		AgentType:   developerAgentType,
		Icon:        icon,
		Description: o.Description,
		Tags:        []string{"openbox-shift-left", "developer-runtime"},
		AivssConfig: profile,
		Config: map[string]any{
			"managed_enable": o.ManagedEnable, // substrate only; not activated yet
		},
		IdentityVerification: &backend.IdentityVerification{
			Method:     devconfig.IdentityMethodKeycloakWorkload,
			Mode:       "generate",
			SourceType: "openbox",
			PublicJWK:  jwkAsMap(jwk),
		},
	}
	reg, err := d.Registrar.Create(ctx, req)
	if err != nil && backend.IsDuplicateNameConflict(err) {
		// The race this covers: another process (or another `init` on another
		// machine for the same tool+user+host) claimed registerName between
		// FindByName and this call. One retry under a fresh suffix, then give
		// up rather than loop.
		suffix, serr := suffixFn()
		if serr == nil {
			registerName = suffixedName(name, suffix)
			res.AgentName = registerName
			req.AgentName = registerName
			reg, err = d.Registrar.Create(ctx, req)
		}
	}
	if err != nil {
		if conflict := backend.ClassifyCreateConflict(err); conflict != backend.None && conflict != backend.Other409 {
			return res, ref, fmt.Errorf("%s (%w)", translatedConflictMessage(conflict), err)
		}
		var apiErr *backend.APIError
		if errors.As(err, &apiErr) {
			return res, ref, fmt.Errorf("HALT: agent/create rejected registration (%w). "+
				"Verify agent_type=%q and the default aivss_config are accepted by this OpenBox org",
				apiErr, developerAgentType)
		}
		return res, ref, fmt.Errorf("agent/create failed: %w", err)
	}
	res.AgentID, res.Registered = reg.AgentID, true
	res.AgentName = reg.AgentName
	// Printed only once Create has succeeded, under the name that succeeded:
	// before it, a failed create (or the duplicate-name retry) would leave
	// stdout claiming a registration that did not happen.
	if existing != nil {
		fmt.Fprintf(d.Out, "an agent named %q (id %s) already exists in this org; registered this "+
			"machine's %s agent as %q\n", existing.AgentName, existing.ID, o.Provider, registerName)
		fmt.Fprintln(d.Out, "  if this machine still holds that agent's workload key, re-run and answer "+
			"yes when it offers to adopt it instead")
	}
	res.IdentityKid = reg.Identity.Kid
	ref.AgentID = reg.AgentID // persisted to dev.json for `dev sync`/staleness

	if reg.Identity.Method != devconfig.IdentityMethodKeycloakWorkload || reg.APIKey == "" {
		return res, ref, fmt.Errorf(
			"agent registered (id %s) but the response did not confirm a %s identity or a token; "+
				"cannot store runtime credentials; rotate the key or re-provision the identity",
			reg.AgentID, devconfig.IdentityMethodKeycloakWorkload)
	}
	// A mismatch here means the backend persisted a different key than the one
	// this machine just proved possession of: every hook signing with priv
	// would fail exchange from the first call onward, silently (fail-open), so
	// this must refuse before a single byte of it is written to disk.
	if reg.Identity.Kid != jwk.Kid {
		return res, ref, fmt.Errorf(
			"agent registered (id %s) but the backend's key id %q does not match the one this machine "+
				"submitted (%q); every hook would fail to authenticate. Refusing to store credentials; "+
				"rotate the key and re-run `openbox init --provider %s`",
			reg.AgentID, reg.Identity.Kid, jwk.Kid, o.Provider)
	}
	ref.IdentityMethod = devconfig.IdentityMethodKeycloakWorkload
	res.IdentityMethod = devconfig.IdentityMethodKeycloakWorkload

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
		return res, ref, resumeErr(o, reg, "write credentials to "+credentialFileLabel(), err)
	}

	if wasLegacy && d.DiscardLegacySpool != nil {
		n, derr := d.DiscardLegacySpool(o.Provider)
		switch {
		case derr != nil:
			// Not fatal: the new store is written. The queued events stay where
			// they are, and no flusher can send them, since every flusher
			// resolves credentials that no longer name the old identity.
			fmt.Fprintf(d.Out, "could not discard events queued under the previous identity: %v\n", derr)
		case n > 0:
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
		fmt.Fprintln(d.Out, "Managed force-enable substrate recorded (verified, not activated; force-enable is opt-in).")
	}

	return res, ref, nil
}

// translatedConflictMessage renders the three known create-time 409s in terms
// a developer can act on. The aivss HALT wording never appears for these:
// each names what an OpenBox admin (or a re-run) must do instead.
func translatedConflictMessage(c backend.CreateConflict) string {
	switch c {
	case backend.IdPNotInitialized:
		return "your org's identity provider is not initialized; ask your OpenBox admin to initialize it " +
			"(operator: `bootstrap-legacy-did-authority --apply`), then re-run"
	case backend.ExternalIdP:
		return "your org issues agent identities from an external provider (Okta/Entra); shift-left " +
			"supports only OpenBox-generated workload identities"
	case backend.IdPChangedRetry:
		return "the org's identity provider changed during registration; re-run"
	default:
		return ""
	}
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

// resumeErr reports a write failure after a successful Create. That agent is
// now orphaned server-side: its API key and the signing key generated for it
// were never written anywhere on this machine and cannot be recovered, so the
// only remedy is a fresh registration, not a retry of this one.
func resumeErr(o Options, reg *backend.Registration, step string, err error) error {
	return fmt.Errorf("agent registered (id %s, name %q) but failed to %s: %w.\n"+
		"  Re-run `openbox init --provider %s`; it registers a fresh agent (suffixed if this name "+
		"is still taken) and tries again.\n"+
		"  The orphaned agent %s %q holds no usable key and may be removed from the org.",
		reg.AgentID, reg.AgentName, step, err, o.Provider, reg.AgentID, reg.AgentName)
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
