package main

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
	var (
		icon, description            string
		baseURL, backendURL          string
		did, agentID                 string
		envFile                      string
		rotate, force, yes           bool
		apiKeyStdin, privateKeyStdin bool
		controlTokenStdin            bool
	)
	fs.BoolVar(&rotate, "rotate", false, "re-issue credentials for an agent that already exists remotely, preserving its id and DID")
	fs.BoolVar(&yes, "yes", false, "skip the confirmation prompt (for automation)")
	fs.BoolVar(&apiKeyStdin, "api-key-stdin", false, "read the obx_ API key from the FIRST line of stdin (never a flag value — INV-1)")
	fs.BoolVar(&privateKeyStdin, "private-key-stdin", false, "read the base64 signing key from the NEXT line of stdin")
	fs.BoolVar(&controlTokenStdin, "control-token-stdin", false, "read an approver's obx_key_ control token from the NEXT line of stdin")
	fs.StringVar(&envFile, "env-file", "", "write the credential file here instead of ~/.openbox/.env")
	fs.StringVar(&icon, "icon", "", "agent icon string (defaults to an emoji; the backend requires non-empty)")
	fs.StringVar(&description, "description", "OpenBox developer-runtime agent", "agent description")
	fs.BoolVar(&force, "force", false, "register a new distinctly-named agent even if one exists remotely")
	fs.StringVar(&baseURL, "base-url", a.env(devconfig.EnvBaseURL, ""), "openbox-core DATA-PLANE base URL (where events go)")
	fs.StringVar(&backendURL, "backend-url", a.env(devconfig.EnvBackendURL, ""), "openbox-backend CONTROL-PLANE base URL")
	fs.StringVar(&did, "did", a.env(devconfig.EnvDID, ""), "this agent's DID (did:aip:<uuid>) — non-secret, so a flag value is fine")
	fs.StringVar(&agentID, "agent-id", a.env(devconfig.EnvAgentID, ""), "this agent's backend id — non-secret; blank on an interactive run registers a new agent")
	if code, ok := parseFlags(fs, args); !ok {
		return code
	}

	a.migrateLegacyConfig()

	envPath, err := a.credentialFilePath(envFile)
	if err != nil {
		return a.errorf("%v", err)
	}
	existing, err := devconfig.ParseEnvFile(envPath)
	if err != nil {
		return a.errorf("%v", err)
	}
	cfg, _ := devconfig.Load(devconfig.DefaultConfigPath())

	piped, code := a.readStdinSecrets(apiKeyStdin, privateKeyStdin, controlTokenStdin)
	if code != exitOK {
		return code
	}

	f := authFields{
		backendURL: firstNonEmptyStr(backendURL, cfg.BackendURL, devconfig.DefaultBackendURL),
		baseURL:    firstNonEmptyStr(baseURL, cfg.BaseURL, devconfig.DefaultBaseURL),
		agentID:    firstNonEmptyStr(agentID, cfg.AgentID),
		did:        firstNonEmptyStr(did, cfg.DID),
		apiKey:     existing[devconfig.EnvAPIKeyDirect],
		privateKey: existing[devconfig.EnvAgentPrivateKey],
	}

	interactive := len(piped) == 0 && !yes
	if interactive {
		if err := prompt.RequireTerminal(a.stdinFile()); err != nil {
			return a.errorf("%v", err)
		}
		p := prompt.New(a.stdinFile(), a.stdout)
		if f, err = collectAuthFields(p, f, rotate); err != nil {
			return a.errorf("%v", err)
		}
	} else {
		for k, v := range piped {
			switch k {
			case devconfig.EnvAPIKeyDirect:
				f.apiKey = v
			case devconfig.EnvAgentPrivateKey:
				f.privateKey = v
			}
		}
		f.register = false
	}

	if len(piped) > 0 && !yes {
		return a.errorf("--api-key-stdin / --private-key-stdin need --yes: the secrets arrive on stdin,\n" +
			"  so there is no way left to ask for confirmation on it. Add --yes to accept the write.")
	}

	if rotate {
		return a.runAuthRotate(f, piped, envPath, backendURL, yes)
	}

	if problem := validateAuthFields(f); problem != "" {
		return a.errorf("%s", problem)
	}

	if f.register {
		res, ref, code := a.registerForAuth(f, icon, description, envFile, force)
		if code != exitOK {
			return code
		}
		f.agentID, f.did = ref.AgentID, ref.DID
		if f.did == "" {
			f.did = res.DID
		}
	} else {
		if !yes {
			p := prompt.New(a.stdinFile(), a.stdout)
			a.printAuthSummary(f, envPath)
			ok, err := p.Confirm("Write these credentials?", false)
			if err != nil {
				return a.errorf("%v", err)
			}
			if !ok {
				fmt.Fprintln(a.stdout, "Nothing written.")
				return exitOK
			}
		}
		if code := a.writeSecrets(envPath, f, piped); code != exitOK {
			return code
		}
	}

	if code := a.writeCoordinates(f); code != exitOK {
		return code
	}
	a.warnShadowedByEnv(f, envPath)
	a.printAuthNextSteps()
	return exitOK
}

func collectAuthFields(p prompt.Prompter, f authFields, rotate bool) (authFields, error) {
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
	if f.agentID == "" && !rotate {
		f.register = true
		return f, nil
	}

	if f.did, err = p.Line("Agent DID", f.did); err != nil {
		return f, err
	}
	if rotate {
		return f, nil
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
			"  agent id blank to register a new agent, or use `openbox auth --rotate` to re-issue."
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

// writeSecrets writes only secrets to the credential file. Never a coordinate.
func (a *app) writeSecrets(envPath string, f authFields, piped map[string]string) int {
	secrets := map[string]string{
		devconfig.EnvAPIKeyDirect:    strings.TrimSpace(f.apiKey),
		devconfig.EnvAgentPrivateKey: strings.TrimSpace(f.privateKey),
	}
	// It is a much larger exposure than the agent seed, so it is never written as
	// a side effect of an ordinary auth run.
	if v := piped[devconfig.EnvControlToken]; v != "" {
		secrets[devconfig.EnvControlToken] = v
	}
	if err := devconfig.WriteEnvFile(envPath, secrets); err != nil {
		return a.errorf("write credentials: %v", err)
	}
	fmt.Fprintf(a.stdout, "✓ wrote %s (0600 — plaintext;)\n", envPath)
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
	fmt.Fprintf(a.stdout, "✓ wrote %s  (agent id, DID, URLs — no secrets)\n", path)
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
		return "(unreadable — validation will reject it)"
	}
	pub := ed25519.NewKeyFromSeed(raw).Public().(ed25519.PublicKey)
	sum := sha256.Sum256(pub)
	return fmt.Sprintf("SHA256:%s (public key)", base64.RawStdEncoding.EncodeToString(sum[:])[:24])
}

func (a *app) readStdinSecrets(apiKey, privateKey, controlToken bool) (map[string]string, int) {
	want := make([]string, 0, 3)
	if apiKey {
		want = append(want, devconfig.EnvAPIKeyDirect)
	}
	if privateKey {
		want = append(want, devconfig.EnvAgentPrivateKey)
	}
	if controlToken {
		want = append(want, devconfig.EnvControlToken)
	}
	if len(want) == 0 {
		return nil, exitOK
	}
	lines, err := readLines(a.stdin, len(want))
	if err != nil {
		return nil, a.errorf("read secrets from stdin: %v (expected %d line(s): %s)",
			err, len(want), strings.Join(want, ", "))
	}
	out := make(map[string]string, len(want))
	for i, k := range want {
		out[k] = lines[i]
	}
	return out, exitOK
}

// registerForAuth devinit.Register, not devinit.Run: Run also invokes the
// provider installer, and `auth` must never install hooks.
func (a *app) registerForAuth(f authFields, icon, description, envFileOverride string, force bool) (*devinit.Result, provider.CredentialRef, int) {
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
		return nil, provider.CredentialRef{}, a.errorf("no backend URL — pass --backend-url or set %s", devconfig.EnvBackendURL)
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
		Icon:        icon,
		Description: description,
		Force:       force,
		EnvFile:     envFileOverride,
	}, devinit.Deps{
		Registrar: a.newRegistrar(f.backendURL, token, "openbox-cli"),
		Out:       a.stdout,
	})
	if err != nil {
		return res, ref, a.errorf("%v", err)
	}
	return res, ref, exitOK
}

func readLines(r io.Reader, n int) ([]string, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	out := make([]string, 0, n)
	for len(out) < n && sc.Scan() {
		out = append(out, strings.TrimRight(sc.Text(), "\r"))
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(out) < n {
		return nil, fmt.Errorf("got %d line(s), want %d", len(out), n)
	}
	return out, nil
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

func (a *app) credentialFilePath(override string) (string, error) {
	override = strings.TrimSpace(override)
	if override == "" {
		return devconfig.EnvFilePath()
	}
	if !filepath.IsAbs(override) {
		return "", fmt.Errorf("--env-file must be an absolute path (got %q): a relative path resolves "+
			"against the current directory, which would write credentials into whatever project you are in", override)
	}
	return filepath.Clean(override), nil
}

func (a *app) stdinFile() *os.File {
	if f, ok := a.stdin.(*os.File); ok {
		return f
	}
	return nil
}

func (a *app) printAuthNextSteps() {
	fmt.Fprintf(a.stdout, "\nNext: openbox init --provider <claude-code|codex|cursor>\n")
	fmt.Fprintf(a.stdout, "  That installs the hooks. By default it governs THIS DIRECTORY only —\n")
	fmt.Fprintf(a.stdout, "  run it in each project you want governed, or use --scope global for a\n")
	fmt.Fprintf(a.stdout, " fleet rollout.\n")
}
