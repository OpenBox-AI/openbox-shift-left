package evaluate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/sandboxclient"
)

var (
	environmentNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	agentIDPattern         = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[1-8][0-9a-fA-F]{3}-[89abAB][0-9a-fA-F]{3}-[0-9a-fA-F]{12}$`)
	credentialNamePattern  = regexp.MustCompile(`(?i)(^|_)(api_?key|access_?key|token|secret|password|passwd|credential|private_?key|signing_?key|bearer|auth)($|_)`)
)

type dockerImage struct {
	ID           string   `json:"Id"`
	Architecture string   `json:"Architecture"`
	OS           string   `json:"Os"`
	RepoTags     []string `json:"RepoTags"`
	Config       struct {
		User       string            `json:"User"`
		Env        []string          `json:"Env"`
		Entrypoint []string          `json:"Entrypoint"`
		Cmd        []string          `json:"Cmd"`
		WorkingDir string            `json:"WorkingDir"`
		Labels     map[string]string `json:"Labels"`
	} `json:"Config"`
}

type prepared struct {
	input             Input
	evaluationID      string
	sandboxName       string
	registryName      string
	image             dockerImage
	argv              []string
	environment       map[string]string
	applicationRoot   string
	sandboxService    string
	sandboxCapability string
	connector         connector
	// declared is the project's own `.env.sandbox`, kept for the secret NAMES
	// the run must warn about. Values are never printed.
	declared *projectEnvironment
}

func prepare(ctx context.Context, input Input, dependencies Dependencies) (*prepared, error) {
	if dependencies.GOOS != "darwin" || dependencies.GOARCH != "arm64" {
		return nil, fmt.Errorf("project evaluate: unsupported platform %s/%s; requires darwin/arm64", dependencies.GOOS, dependencies.GOARCH)
	}
	if dependencies.Commands == nil || dependencies.Clock == nil || dependencies.Random == nil ||
		dependencies.Listen == nil || dependencies.HTTP == nil {
		return nil, errors.New("project evaluate: incomplete dependencies")
	}
	if err := validateInputStrings(input); err != nil {
		return nil, err
	}
	fileEnvironment, err := parseEnvironmentFile(input.EnvFile)
	if err != nil {
		return nil, err
	}
	identifier, err := randomIdentifier(dependencies.Random)
	if err != nil {
		return nil, fmt.Errorf("project evaluate: generate evaluation identity: %w", err)
	}
	evaluationID := "ev-" + identifier
	resolved, err := resolveConnector(input)
	if err != nil {
		return nil, err
	}
	result := &prepared{
		input: input, evaluationID: evaluationID,
		sandboxName:  "obx-eval-" + identifier[:10],
		registryName: "obx-eval-registry-" + identifier,
		connector:    resolved,
	}

	image, err := inspectImage(ctx, dependencies.Commands, input.Image)
	if err != nil {
		return nil, err
	}
	argv, applicationRoot, err := validateImage(image)
	if err != nil {
		return nil, err
	}
	result.image, result.argv, result.applicationRoot = image, argv, applicationRoot
	result.declared = fileEnvironment
	result.environment, err = effectiveEnvironment(image.Config.Env, fileEnvironment, evaluationID, input.OpenBoxAgent)
	if err != nil {
		return nil, err
	}

	if _, err := inspectImage(ctx, dependencies.Commands, RegistryImage); err != nil {
		return nil, fmt.Errorf("project evaluate: required registry image is not preloaded: %w", err)
	}
	if err := preflightSandbox(dependencies, result); err != nil {
		return nil, err
	}
	if err := preflightLocalServices(ctx, dependencies, resolved); err != nil {
		return nil, err
	}
	return result, nil
}

func validateInputStrings(input Input) error {
	for name, value := range map[string]string{
		"--image": input.Image, "--env-file": input.EnvFile,
		"--openbox-agent": input.OpenBoxAgent,
	} {
		if strings.TrimSpace(value) == "" || strings.ContainsAny(value, "\x00\r\n") {
			return fmt.Errorf("project evaluate: %s must not be empty or contain control characters", name)
		}
	}
	if strings.HasPrefix(input.Image, "-") {
		return errors.New("project evaluate: --image must not begin with '-'")
	}
	if strings.Contains(input.Image, "://") || strings.ContainsAny(input.Image, " \t") ||
		(strings.Contains(input.Image, "@") && !regexp.MustCompile(`@sha256:[0-9a-f]{64}$`).MatchString(input.Image)) {
		return errors.New("project evaluate: --image must be a credential-free local Docker reference or image ID")
	}
	if !agentIDPattern.MatchString(input.OpenBoxAgent) {
		return errors.New("project evaluate: --openbox-agent must be a UUID")
	}
	// Required before anything is mutated: a run whose evaluation cannot be
	// requested wastes the developer's time.
	if strings.TrimSpace(input.ControlToken) == "" {
		return errors.New("project evaluate: OPENBOX_CONTROL_TOKEN is required to request the security evaluation (the key needs permission evaluate:agent_security)")
	}
	if strings.ContainsAny(input.ControlToken, "\x00\r\n") {
		return errors.New("project evaluate: OPENBOX_CONTROL_TOKEN must not contain control characters")
	}
	return nil
}

func inspectImage(ctx context.Context, runner CommandRunner, reference string) (dockerImage, error) {
	result, err := runner.Run(ctx, Command{Name: "docker", Args: []string{"image", "inspect", reference}})
	if err != nil {
		return dockerImage{}, fmt.Errorf("project evaluate: resolve local image %q: %w", reference, err)
	}
	var images []dockerImage
	if err := json.Unmarshal(result.Stdout, &images); err != nil || len(images) != 1 {
		if err == nil {
			err = fmt.Errorf("got %d images", len(images))
		}
		return dockerImage{}, fmt.Errorf("project evaluate: decode local image %q: %w", reference, err)
	}
	if images[0].ID == "" {
		return dockerImage{}, errors.New("project evaluate: local image has no immutable ID")
	}
	return images[0], nil
}

func validateImage(image dockerImage) ([]string, string, error) {
	if image.OS != "linux" || image.Architecture != "arm64" {
		return nil, "", fmt.Errorf("project evaluate: image platform is %s/%s, requires linux/arm64", image.OS, image.Architecture)
	}
	if image.Config.Labels[ContractLabel] != ContractVersion {
		return nil, "", fmt.Errorf("project evaluate: image must declare %s=%s", ContractLabel, ContractVersion)
	}
	for name := range image.Config.Labels {
		if name != ContractLabel && strings.HasPrefix(name, "ai.openbox.project-evaluation.") {
			return nil, "", fmt.Errorf("project evaluate: obsolete project-evaluation label %q is not allowed", name)
		}
	}
	if image.Config.User != "1000" && image.Config.User != "1000:1000" {
		return nil, "", errors.New("project evaluate: image Config.User must be 1000 or 1000:1000")
	}
	argv := append(append([]string(nil), image.Config.Entrypoint...), image.Config.Cmd...)
	if len(argv) == 0 {
		return nil, "", errors.New("project evaluate: image Entrypoint + Cmd is empty")
	}
	for _, argument := range argv {
		if argument == "" || strings.ContainsRune(argument, 0) {
			return nil, "", errors.New("project evaluate: image command contains an empty or NUL argument")
		}
	}
	if !path.IsAbs(argv[0]) || path.Clean(argv[0]) != argv[0] {
		return nil, "", errors.New("project evaluate: first resolved OCI argv element must be a clean absolute executable")
	}
	applicationRoot := path.Dir(argv[0])
	for _, argument := range argv[1:] {
		if looksLikeRelativeApplicationPath(argument) {
			return nil, "", errors.New("project evaluate: application and script paths in the OCI command must be absolute")
		}
		if !path.IsAbs(argument) {
			continue
		}
		clean := path.Clean(argument)
		if clean != argument {
			return nil, "", errors.New("project evaluate: absolute command paths must be clean")
		}
		parts := strings.Split(strings.TrimPrefix(clean, "/"), "/")
		if len(parts) > 0 && parts[0] != "" {
			applicationRoot = "/" + parts[0]
			break
		}
	}
	return argv, applicationRoot, nil
}

func looksLikeRelativeApplicationPath(argument string) bool {
	if argument == "" || strings.HasPrefix(argument, "-") || path.IsAbs(argument) {
		return false
	}
	switch strings.ToLower(path.Ext(argument)) {
	case ".js", ".mjs", ".cjs", ".ts", ".py", ".sh", ".rb", ".jar":
		return true
	}
	return strings.Contains(argument, "/")
}

func parseEnvironmentFile(filename string) (*projectEnvironment, error) {
	content, err := readEnvironmentFileNoFollow(filename)
	if err != nil {
		return nil, err
	}
	return parseEnvironment(content)
}

// parseEnvironment reads `.env.sandbox` — the project's sandbox declaration.
//
// This is deliberately NOT the developer's own `.env` or `.env.local`. Those
// describe how the project runs on their machine and routinely hold real
// credentials; reading them would import every one of those into a sandbox run
// by default. The sandbox file is separate so that what crosses this boundary
// is a thing the developer wrote down on purpose.
func parseEnvironment(content []byte) (*projectEnvironment, error) {
	if len(content) > 64<<10 {
		return nil, errors.New("project evaluate: environment file exceeds 64 KiB")
	}
	if !utf8.Valid(content) || bytes.IndexByte(content, 0) >= 0 {
		return nil, errors.New("project evaluate: environment file must be NUL-free UTF-8")
	}
	environment := newProjectEnvironment()
	lines := strings.Split(string(content), "\n")
	for index, raw := range lines {
		line := strings.TrimSuffix(raw, "\r")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(strings.TrimSpace(line), "export ") {
			return nil, fmt.Errorf("project evaluate: environment line %d uses forbidden export syntax", index+1)
		}
		name, value, found := strings.Cut(line, "=")
		if !found || !environmentNamePattern.MatchString(name) {
			return nil, fmt.Errorf("project evaluate: invalid environment line %d", index+1)
		}
		if len(value) > 4<<10 || strings.ContainsAny(value, "\x00\r\n") {
			return nil, fmt.Errorf("project evaluate: invalid value for environment key %s", name)
		}
		if _, reserved := reservedEnvironment[name]; reserved {
			return nil, fmt.Errorf("project evaluate: environment line %d sets connector-reserved key %s", index+1, name)
		}
		if err := environment.classify(name, value, index+1); err != nil {
			return nil, err
		}
		if environment.entries() > 64 {
			return nil, errors.New("project evaluate: environment file exceeds 64 entries")
		}
	}
	// Only non-secret values are URL-checked. A credential is not a URL, and
	// inspecting one here would mean reading it for a reason other than passing
	// it on — which is the one thing this code should never do.
	for name, value := range environment.public {
		if err := validateEnvironmentURL(value); err != nil {
			return nil, fmt.Errorf("project evaluate: environment key %s: %w", name, err)
		}
	}
	return environment, nil
}

// effectiveEnvironment merges the image's own Config.Env with the project's
// declarations into the one map the guest receives.
//
// The evaluator does not inject model routing: `OPENAI_*` and friends are the
// project's to declare. Only the connector values are the evaluator's.
//
// Public and secret declarations land in the same map, because they travel the
// same way. What differs is what the run WARNS about, which
// `plaintextCredentialWarnings` handles from the names.
func effectiveEnvironment(imageEntries []string, declared *projectEnvironment, evaluationID, agentID string) (map[string]string, error) {
	values := make(map[string]string)
	for _, entry := range imageEntries {
		name, value, found := strings.Cut(entry, "=")
		if !found || !environmentNamePattern.MatchString(name) || len(value) > 4<<10 || strings.ContainsAny(value, "\x00\r\n") {
			return nil, errors.New("project evaluate: image Config.Env is malformed")
		}
		if _, reserved := reservedEnvironment[name]; reserved {
			// The connector supplies these, so the image's value is discarded
			// either way. A real credential baked into a layer is still refused
			// rather than quietly dropped: the layer is what gets shared, and a
			// secret in one outlives any single run.
			if credentialNamePattern.MatchString(name) && value != "" && value != "unused" {
				return nil, fmt.Errorf("project evaluate: image embeds credential-looking environment key %s", name)
			}
			continue
		}
		// The same rule for a non-reserved name. A project that needs a real
		// credential declares it in `.env.sandbox`, where the run can disclose
		// it; baking one into the image hides it from the evidence entirely.
		if credentialNamePattern.MatchString(name) && value != "" && value != "unused" {
			return nil, fmt.Errorf("project evaluate: image embeds credential-looking environment key %s", name)
		}
		if _, duplicate := values[name]; duplicate {
			return nil, fmt.Errorf("project evaluate: image has duplicate environment key %s", name)
		}
		if err := validateEnvironmentURL(value); err != nil {
			return nil, fmt.Errorf("project evaluate: image environment key %s: %w", name, err)
		}
		values[name] = value
	}
	// The project's declarations win over anything the image baked in.
	for name, value := range declared.public {
		values[name] = value
	}
	for name, value := range declared.secret {
		values[name] = value
	}
	values["OPENBOX_EVALUATION_ID"] = evaluationID
	values["OPENBOX_AGENT_ID"] = agentID
	return values, nil
}

func validateEnvironmentURL(value string) error {
	if !strings.Contains(value, "://") {
		return nil
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme == "" || parsed.Hostname() == "" || parsed.User != nil {
		return errors.New("contains an invalid URL")
	}
	host := strings.ToLower(parsed.Hostname())
	if host == "localhost" || host == "127.0.0.1" || host == "::1" ||
		host == "host.openshell.internal" || host == "inference.local" {
		return nil
	}
	return errors.New("contains a non-local URL")
}

// preflightSandbox negotiates with the sandbox service instead of interrogating
// OpenShell.
//
// What this replaced was seven `openshell` CLI invocations whose answers were
// parsed out of human-readable output — version strings compared literally,
// provider fields scraped after stripping ANSI escapes, a driver tuple matched
// by equality. Every one of those was this repo asserting facts about a
// component it does not own. The service owns them now, and the only question
// left is the one a caller may legitimately ask: will you run this for me.
//
// A deployment that does not advertise the capability is not runnable. There is
// no fallback to the CLI, by design: two paths to the same sandbox is how the
// contracts drifted in the first place.
func preflightSandbox(dependencies Dependencies, result *prepared) error {
	if dependencies.Sandbox == nil {
		return errors.New("project evaluate: no sandbox service is configured; run `obs provision` and retry")
	}
	capabilities, err := dependencies.Sandbox.Capabilities(sandboxNegotiationTimeout)
	if err != nil {
		return fmt.Errorf("project evaluate: sandbox service is unreachable: %w", err)
	}
	for _, capability := range capabilities {
		if capability == sandboxclient.ProjectRunCapability {
			result.sandboxService = dependencies.SandboxTemplateFromConfig
			result.sandboxCapability = capability
			return nil
		}
	}
	return fmt.Errorf(
		"project evaluate: sandbox service does not offer %s (offers %v); provision with OPENBOX_PROJECT_RUN_V2=1",
		sandboxclient.ProjectRunCapability, capabilities)
}

func preflightLocalServices(ctx context.Context, dependencies Dependencies, resolved connector) error {
	for name, endpoint := range map[string]string{
		"Core":    resolved.coreURL + "/",
		"backend": resolved.backendURL + "/health",
	} {
		response, err := get(ctx, dependencies.HTTP, endpoint)
		if err != nil || response == nil || response.StatusCode < 200 || response.StatusCode >= 300 {
			if response != nil {
				response.Body.Close()
			}
			return fmt.Errorf("project evaluate: OpenBox %s health endpoint at %s is unavailable", name, endpoint)
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
		response.Body.Close()
	}
	return nil
}

func get(ctx context.Context, client HTTPDoer, endpoint string) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("accept", "application/json")
	return client.Do(request)
}

func randomIdentifier(reader io.Reader) (string, error) {
	var value [12]byte
	if _, err := io.ReadFull(reader, value[:]); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", value[:]), nil
}
