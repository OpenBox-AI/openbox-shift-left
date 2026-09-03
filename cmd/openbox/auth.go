package main

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"regexp"
	"strings"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/devinit"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/prompt"
	"github.com/openbox-ai/openbox-shift-left/internal/provider"
)

// It installs nothing; `openbox init` does that, and the split is what lets
// init be a command that cannot touch a secret.

type authFields struct {
	backendURL string
	baseURL    string
	agentID    string
	did        string
	apiKey     string
	privateKey string
	register   bool
}

var didPattern = regexp.MustCompile(`^did:aip:[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func (a *app) runAuth(args []string) int {
	fs := a.newFlagSet("openbox auth")
	if code, ok := parseFlags(fs, args); !ok {
		return code
	}
	// No flags at all. Every one of the thirteen was a way to do this without a
	// terminal, and each was also a way to get it wrong: a secret in argv, a
	// confirmation skipped, a credential file written somewhere the hooks do not
	// read. Provisioning without a terminal is still possible and better served
	// by the environment or the two files themselves, which already outrank
	// anything this command writes.
	if fs.NArg() > 0 {
		return a.errorf("`openbox auth` takes no arguments or flags; it prompts. Got %q.\n%s",
			fs.Arg(0), prompt.NonInteractiveHelp)
	}

	a.migrateLegacyConfig()

	envPath, err := devconfig.EnvFilePath()
	if err != nil {
		return a.errorf("%v", err)
	}
	existing, err := devconfig.ParseEnvFile(envPath)
	if err != nil {
		return a.errorf("%v", err)
	}
	cfg, _ := devconfig.Load(devconfig.DefaultConfigPath())

	// Prefilled from what is on the machine, so a re-run to fix one URL keeps
	// the agent id, the DID and both secrets. The agent-id prompt is the one
	// deliberate exception: it never prefills, because a blank answer there is
	// the only way to reach registration at all.
	f := authFields{
		backendURL: firstNonEmptyStr(cfg.BackendURL, devconfig.DefaultBackendURL),
		baseURL:    firstNonEmptyStr(cfg.BaseURL, devconfig.DefaultBaseURL),
		agentID:    cfg.AgentID,
		did:        cfg.DID,
		apiKey:     existing[devconfig.EnvAPIKeyDirect],
		privateKey: existing[devconfig.EnvAgentPrivateKey],
	}

	p, err := a.newPrompt()
	if err != nil {
		return a.errorf("%v", err)
	}
	if f, err = collectAuthFields(p, f); err != nil {
		return a.errorf("%v", err)
	}

	if problem := validateAuthFields(f); problem != "" {
		return a.errorf("%s", problem)
	}

	if f.register {
		res, ref, code := a.registerForAuth(f)
		if code != exitOK {
			return code
		}
		f.agentID, f.did = ref.AgentID, ref.DID
		if f.did == "" {
			f.did = res.DID
		}
	} else if code := a.writeSecrets(envPath, f); code != exitOK {
		// Written unconditionally: there is no confirmation left to skip. Both
		// writers overwrite the two keys they own and merge the rest, and a blank
		// answer at either secret prompt kept the current value, so a re-run
		// cannot erase a credential.
		return code
	}

	if code := a.writeCoordinates(f); code != exitOK {
		return code
	}
	a.warnShadowedByEnv(f, envPath)
	a.printAuthNextSteps()
	return exitOK
}

func collectAuthFields(p prompt.Prompter, f authFields) (authFields, error) {
	var err error
	if f.backendURL, err = p.Line("Backend URL (control plane)", f.backendURL); err != nil {
		return f, err
	}
	if f.baseURL, err = p.Line("Core URL (data plane)", f.baseURL); err != nil {
		return f, err
	}
	answered, err := p.Line("Agent id (blank registers a new agent)", "")
	if err != nil {
		return f, err
	}
	f.agentID = strings.TrimSpace(answered)
	if f.agentID == "" {
		f.register = true
		return f, nil
	}

	if f.did, err = p.Line("Agent DID", f.did); err != nil {
		return f, err
	}
	apiKey, err := p.Secret("API key (obx_…)", f.apiKey != "")
	if err != nil {
		return f, err
	}
	if apiKey != "" {
		f.apiKey = apiKey
	}
	privateKey, err := p.Secret("Signing key (base64)", f.privateKey != "")
	if err != nil {
		return f, err
	}
	if privateKey != "" {
		f.privateKey = privateKey
	}
	return f, nil
}

func validateAuthFields(f authFields) string {
	if f.register {
		return "" // nothing to validate: the server supplies all of it
	}
	if strings.HasPrefix(strings.TrimSpace(f.apiKey), "obx_key_") {
		return fmt.Sprintf("that looks like an ORGANIZATION key (%s…), not this agent's runtime key.\n"+
			"  An obx_key_ key belongs in %s and can create and rotate agents org-wide.\n"+
			"  The agent runtime key starts obx_ (no `key_`) and is shown once on the agent's page\n"+
			"  when it is created. See docs/getting-started.md § Get the right credential.",
			safePrefix(strings.TrimSpace(f.apiKey)), devconfig.EnvControlToken)
	}
	if f.apiKey == "" {
		return fmt.Sprintf("no API key given. Paste this agent's obx_ runtime key, or leave the agent id\n" +
			"  blank to register a new agent and have one issued.")
	}
	if problem := privateKeyProblem(f.privateKey); problem != "" {
		return problem
	}
	if f.did == "" {
		return "no DID given. It is on the agent's page in the dashboard, and looks like\n" +
			"  did:aip:xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx. Leave the agent id blank to register instead."
	}
	if !didPattern.MatchString(strings.TrimSpace(f.did)) {
		return fmt.Sprintf("%q is not a valid DID. Expected did:aip:<uuid>, e.g.\n"+
			"  did:aip:3f2504e0-4f89-11d3-9a0c-0305e82c3301", f.did)
	}
	return ""
}

func privateKeyProblem(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return "no signing key given. It is shown once, when the agent is created; leave the\n" +
			"  agent id blank to register a new agent."
	}
	raw, err := base64.StdEncoding.DecodeString(v)
	if err != nil {
		return "the signing key is not valid base64. Paste the value exactly as OpenBox showed it\n" +
			"  (it is about 44 characters and usually ends in '=')."
	}
	if len(raw) != ed25519.SeedSize {
		return fmt.Sprintf("the signing key decodes to %d bytes; an Ed25519 seed is %d.\n"+
			"  Check you pasted the whole value and not a truncated copy.", len(raw), ed25519.SeedSize)
	}
	return ""
}

// writeSecrets writes only secrets to the credential file. Never a coordinate,
// and never the org control token.
func (a *app) writeSecrets(envPath string, f authFields) int {
	secrets := map[string]string{
		devconfig.EnvAPIKeyDirect:    strings.TrimSpace(f.apiKey),
		devconfig.EnvAgentPrivateKey: strings.TrimSpace(f.privateKey),
	}
	// The control token is never written. It is an org credential with
	// fleet-wide authority -- a much larger exposure than this agent's own seed
	// -- and the only thing that read it from disk was the approver persona.
	// Registration still takes it from the environment, which is where a
	// credential that powerful belongs.
	if err := devconfig.WriteEnvFile(envPath, secrets); err != nil {
		return a.errorf("write credentials: %v", err)
	}
	fmt.Fprintf(a.stdout, "✓ wrote %s (0600; plaintext;)\n", envPath)
	return exitOK
}

// writeCoordinates deliberately not provider.ConfigUpdate: that always sets
// InstallGitHook to a non-nil value (provider/config.go), so routing through
// it would make `auth` write posture.
func (a *app) writeCoordinates(f authFields) int {
	path, err := devconfig.DevConfigWritePath()
	if err != nil {
		return a.errorf("%v", err)
	}
	if err := devconfig.WriteConfig(path, devconfig.Update{
		DID:        strings.TrimSpace(f.did),
		AgentID:    strings.TrimSpace(f.agentID),
		BackendURL: strings.TrimSpace(f.backendURL),
		BaseURL:    strings.TrimSpace(f.baseURL),
	}); err != nil {
		return a.errorf("write dev config: %v", err)
	}
	fmt.Fprintf(a.stdout, "✓ wrote %s  (agent id, DID, URLs; no secrets)\n", path)
	return exitOK
}

// warnShadowedByEnv a real env var beats both files, so writing while one is
// exported produces a config that silently has no effect; the user changes a
// credential, sees success, and observes no change in behaviour.
func (a *app) warnShadowedByEnv(f authFields, envPath string) {
	type shadow struct{ name, file string }
	devPath, _ := devconfig.DevConfigWritePath()
	for _, s := range []shadow{
		{devconfig.EnvAPIKeyDirect, envPath},
		{devconfig.EnvAgentPrivateKey, envPath},
		{devconfig.EnvDID, devPath},
		{devconfig.EnvAgentID, devPath},
		{devconfig.EnvBaseURL, devPath},
		{devconfig.EnvBackendURL, devPath},
	} {
		if a.getenv(s.name) == "" {
			continue
		}
		fmt.Fprintf(a.stderr, "warning: %s is set in this environment, so it overrides what was just written to %s.\n"+
			"         The file is correct; this shell will not use it. Unset the variable to use the file.\n",
			s.name, s.file)
	}
	_ = f
}

func (a *app) printAuthSummary(f authFields, envPath string) {
	devPath, _ := devconfig.DevConfigWritePath()
	fmt.Fprintf(a.stdout, "\nAbout to write:\n")
	fmt.Fprintf(a.stdout, "  %s\n", envPath)
	fmt.Fprintf(a.stdout, "    %-28s %s\n", devconfig.EnvAPIKeyDirect, maskToken(f.apiKey))
	fmt.Fprintf(a.stdout, "    %-28s %s\n", devconfig.EnvAgentPrivateKey, publicKeyFingerprint(f.privateKey))
	fmt.Fprintf(a.stdout, "  %s\n", devPath)
	fmt.Fprintf(a.stdout, "    %-28s %s\n", "agent id", orDefault(f.agentID, "(none)"))
	fmt.Fprintf(a.stdout, "    %-28s %s\n", "DID", orDefault(f.did, "(none)"))
	fmt.Fprintf(a.stdout, "    %-28s %s\n", "backend URL", f.backendURL)
	fmt.Fprintf(a.stdout, "    %-28s %s\n", "core URL", f.baseURL)
	fmt.Fprintln(a.stdout)
}

func maskToken(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return "(none)"
	}
	if len(v) <= 12 {
		return fmt.Sprintf("(%d chars)", len(v))
	}
	return fmt.Sprintf("%s…%s (%d chars)", v[:8], v[len(v)-4:], len(v))
}

// publicKeyFingerprint the seed itself is never hashed or displayed.
func publicKeyFingerprint(seedB64 string) string {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(seedB64))
	if err != nil || len(raw) != ed25519.SeedSize {
		if strings.TrimSpace(seedB64) == "" {
			return "(none)"
		}
		return "(unreadable; validation will reject it)"
	}
	pub := ed25519.NewKeyFromSeed(raw).Public().(ed25519.PublicKey)
	sum := sha256.Sum256(pub)
	return fmt.Sprintf("SHA256:%s (public key)", base64.RawStdEncoding.EncodeToString(sum[:])[:24])
}

// registerForAuth devinit.Register, not devinit.Run: Run also invokes the
// provider installer, and `auth` must never install hooks.
func (a *app) registerForAuth(f authFields) (*devinit.Result, provider.CredentialRef, int) {
	token := a.getenv(devconfig.EnvControlToken)
	if token == "" {
		return nil, provider.CredentialRef{}, a.errorf(
			"registering a new agent needs an organization credential.\n"+
				"  Set %s (an obx_key_ organization key, or a Keycloak JWT) in the environment; it is\n"+
				"  never accepted as a flag so it cannot leak via argv or shell history (INV-1).\n"+
				"  Dashboard → Organization → API Keys, with create:agent + read:agent.\n"+
				"  Already have an agent? Re-run and give its agent id instead of leaving it blank.",
			devconfig.EnvControlToken)
	}
	if problem := controlTokenProblem(token); problem != "" {
		return nil, provider.CredentialRef{}, a.errorf("%s", problem)
	}
	if f.backendURL == "" {
		return nil, provider.CredentialRef{}, a.errorf("no backend URL; answer the backend prompt, or set %s", devconfig.EnvBackendURL)
	}
	if selfHostedWithoutDataPlane(f.backendURL, f.baseURL) {
		fmt.Fprintf(a.stderr,
			"warning: the backend is %s but the core URL is the hosted default (%s).\n"+
				"         If OpenBox is self-hosted, set the core URL to your own openbox-core.\n",
			f.backendURL, devconfig.DefaultBaseURL)
	}

	res, ref, err := devinit.Register(context.Background(), devinit.Options{
		BackendURL:  f.backendURL,
		BaseURL:     f.baseURL,
		Description: "OpenBox developer-runtime agent",
	}, devinit.Deps{
		Registrar: a.newRegistrar(f.backendURL, token, "openbox-cli"),
		Out:       a.stdout,
	})
	if err != nil {
		return res, ref, a.errorf("%v", err)
	}
	return res, ref, exitOK
}

func orDefault(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func (a *app) printAuthNextSteps() {
	fmt.Fprintf(a.stdout, "\nNext: openbox init --provider <%s>\n", strings.Join(provider.Supported(), "|"))
	fmt.Fprintf(a.stdout, "  That installs the hooks and every model-call lane the provider supports. One\n")
	fmt.Fprintf(a.stdout, "  run governs every session on this machine, in any directory; there is no\n")
	fmt.Fprintf(a.stdout, "  scope to choose and nothing to repeat per project.\n")
}
