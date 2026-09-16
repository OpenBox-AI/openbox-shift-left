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
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/sandboxclient"
)

var (
	environmentNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	agentIDPattern         = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[1-8][0-9a-fA-F]{3}-[89abAB][0-9a-fA-F]{3}-[0-9a-fA-F]{12}$`)
	credentialNamePattern  = regexp.MustCompile(`(?i)(^|_)(api_?key|access_?key|token|secret|password|passwd|credential|private_?key|signing_?key|bearer|auth)($|_)`)
	ansiPattern            = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)
	digestPattern          = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
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
	output            string
	image             dockerImage
	argv              []string
	environment       map[string]string
	environmentNames  []string
	applicationRoot   string
	sandboxService    string
	sandboxCapability string
	connector         connector
	// placeholders are credential-shaped names carrying deliberate non-secrets.
	placeholders map[string]string
	// declared is the project's own `.env.sandbox`, kept for the secret names
	// and the model-route setting. Its secret VALUES are never recorded.
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
	output, err := filepath.Abs(input.Output)
	if err != nil {
		return nil, fmt.Errorf("project evaluate: resolve output: %w", err)
	}
	resolvedParent, err := filepath.EvalSymlinks(filepath.Dir(output))
	if err != nil {
		return nil, fmt.Errorf("project evaluate: resolve output parent: %w", err)
	}
	output = filepath.Join(resolvedParent, filepath.Base(output))
	if _, err := os.Lstat(output); err == nil {
		return nil, errors.New("project evaluate: output already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("project evaluate: inspect output: %w", err)
	}
	if info, err := os.Lstat(filepath.Dir(output)); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		if err != nil {
			return nil, fmt.Errorf("project evaluate: inspect output parent: %w", err)
		}
		return nil, errors.New("project evaluate: output parent must be a real directory")
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
		output:       output,
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
	result.environment, result.placeholders, err = effectiveEnvironment(image.Config.Env, fileEnvironment, evaluationID, input.OpenBoxAgent)
	if err != nil {
		return nil, err
	}
	result.environmentNames = environmentInventory(result.environment, result.placeholders)

	if _, err := inspectImage(ctx, dependencies.Commands, RegistryImage); err != nil {
		return nil, fmt.Errorf("project evaluate: required registry image is not preloaded: %w", err)
	}
	if err := preflightSandbox(dependencies, result); err != nil {
		return nil, err
	}
	if err := preflightLocalServices(ctx, dependencies, resolved); err != nil {
		return nil, err
	}
	if err := preflightModelRoute(ctx, dependencies, result); err != nil {
		return nil, err
	}
	if err := refuseUnconsumedSecrets(result.declared); err != nil {
		return nil, err
	}
	return result, nil
}

// refuseUnconsumedSecrets fails a run that declares a secret this lane cannot
// yet deliver.
//
// The parser understands OPENBOX_SANDBOX_SECRET_ and nothing consumes it. The
// tempting alternative — accept the declaration and carry on — would start the
// workload with the variable simply absent, which surfaces as whatever that
// project does when its own credential is missing, arbitrarily far from the
// cause. A run that cannot honour a declaration must say so before it runs
// anything.
//
// Delivering one is not a matter of passing the value through: a credential
// belongs to an OpenShell provider bound to an endpoint in this policy, and an
// unbound credential key makes the gateway fail closed and revoke every other
// credential with it. Until that provisioning path exists, this is a refusal
// rather than a gap the caller has to notice.
func refuseUnconsumedSecrets(declared *projectEnvironment) error {
	names := declared.secretNames()
	if len(names) == 0 {
		return nil
	}
	return fmt.Errorf(
		"project evaluate: %s declarations are not deliverable yet (%s); "+
			"a credential must be an OpenShell provider bound to an endpoint in this policy, "+
			"and this lane cannot provision one",
		secretPrefix, strings.Join(names, ", "))
}

func validateInputStrings(input Input) error {
	for name, value := range map[string]string{
		"--image": input.Image, "--env-file": input.EnvFile,
		"--openbox-agent": input.OpenBoxAgent, "--output": input.Output,
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
		consumed, err := environment.setting(name, value, index+1)
		if err != nil {
			return nil, err
		}
		if consumed {
			continue
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
	// A secret's value never reaches the guest, so only the non-secret channels
	// are URL-checked. Checking a secret's value would mean reading it here for
	// a reason that is not passing it on.
	for _, values := range []map[string]string{environment.public, environment.placeholder} {
		for name, value := range values {
			if err := validateEnvironmentURL(value); err != nil {
				return nil, fmt.Errorf("project evaluate: environment key %s: %w", name, err)
			}
		}
	}
	return environment, nil
}

// effectiveEnvironment merges the image's own Config.Env with the project's
// declarations, keeping the placeholder channel separate the whole way.
//
// The evaluator no longer injects model routing. `OPENAI_BASE_URL`,
// `OPENAI_API_KEY` and `OPENAI_MODEL` used to be forced here to a local Ollama
// serving one pinned model, which made every project that lane could evaluate
// the same project. They are the project's to declare now; only the five
// connector values below are still the evaluator's.
func effectiveEnvironment(imageEntries []string, declared *projectEnvironment, evaluationID, agentID string) (map[string]string, map[string]string, error) {
	values := make(map[string]string)
	placeholders := make(map[string]string)
	for _, entry := range imageEntries {
		name, value, found := strings.Cut(entry, "=")
		if !found || !environmentNamePattern.MatchString(name) || len(value) > 4<<10 || strings.ContainsAny(value, "\x00\r\n") {
			return nil, nil, errors.New("project evaluate: image Config.Env is malformed")
		}
		if _, reserved := reservedEnvironment[name]; reserved {
			// The connector supplies these, so the image's value is discarded
			// either way — but a real credential baked into a layer is still
			// refused rather than quietly dropped, because the layer is what
			// gets shared and the developer should hear about it.
			if credentialNamePattern.MatchString(name) && value != "" && value != "unused" {
				return nil, nil, fmt.Errorf("project evaluate: image embeds credential-looking environment key %s", name)
			}
			continue
		}
		if credentialNamePattern.MatchString(name) {
			// An image may embed a credential-shaped name only as the empty or
			// "unused" stand-in its SDK insists on — which is a placeholder, and
			// is routed as one. Any other value is a credential baked into a
			// layer, which is refused for the same reason it always was.
			if value != "" && value != "unused" {
				return nil, nil, fmt.Errorf("project evaluate: image embeds credential-looking environment key %s", name)
			}
			placeholders[name] = value
			continue
		}
		if _, duplicate := values[name]; duplicate {
			return nil, nil, fmt.Errorf("project evaluate: image has duplicate environment key %s", name)
		}
		if err := validateEnvironmentURL(value); err != nil {
			return nil, nil, fmt.Errorf("project evaluate: image environment key %s: %w", name, err)
		}
		values[name] = value
	}
	// The project's declarations win over anything the image baked in.
	for name, value := range declared.public {
		delete(placeholders, name)
		values[name] = value
	}
	for name, value := range declared.placeholder {
		delete(values, name)
		placeholders[name] = value
	}
	values["OPENBOX_EVALUATION_ID"] = evaluationID
	values["OPENBOX_AGENT_ID"] = agentID
	return values, placeholders, nil
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

func outputField(content, name string) string {
	for _, line := range strings.Split(content, "\n") {
		field, value, found := strings.Cut(strings.TrimSpace(line), ":")
		if found && field == name {
			return strings.TrimSpace(value)
		}
	}
	return ""
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

// preflightModelRoute proves the local Ollama serving inference.local has the
// declared model, and has it cold.
//
// It runs only for OPENBOX_SANDBOX_MODEL_ROUTE=local-ollama. A project pointed
// at OpenAI, Gemini, OpenRouter or a remote Ollama is served by something this
// host cannot see: there is no tag list to read and no process to prove cold,
// and running these checks anyway would assert a local fact about a remote
// service. That route is recorded as unproven instead, which is the honest
// answer and not a weaker one.
//
// The digest pin is likewise conditional. It used to be a compile-time constant
// for one model; now it holds only when the project states one.
func preflightModelRoute(ctx context.Context, dependencies Dependencies, result *prepared) error {
	model := result.environment["OPENAI_MODEL"]
	if model == "" {
		return fmt.Errorf("project evaluate: the project must declare %sOPENAI_MODEL", publicPrefix)
	}
	// Required for every route, because the v1 effects schema requires a
	// sha256 model_digest and offers no way to say a route publishes none.
	// Demanding the project state it is the honest version of that constraint;
	// synthesising one here would be the dishonest version.
	if result.declared.modelDigest == "" {
		return fmt.Errorf("project evaluate: the project must declare %s", modelDigestSetting)
	}
	if result.declared.modelRoute != ModelRouteLocalOllama {
		return nil
	}
	response, err := get(ctx, dependencies.HTTP, ollamaTagsURL)
	if err != nil || response == nil || response.StatusCode != http.StatusOK {
		if response != nil {
			response.Body.Close()
		}
		return errors.New("project evaluate: local Ollama tags endpoint is unavailable")
	}
	var tags struct {
		Models []struct {
			Name   string `json:"name"`
			Digest string `json:"digest"`
		} `json:"models"`
	}
	err = json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&tags)
	response.Body.Close()
	if err != nil {
		return errors.New("project evaluate: local Ollama tag list is unreadable")
	}
	found := false
	for _, candidate := range tags.Models {
		if candidate.Name != model {
			continue
		}
		if result.declared.modelDigest != "" && "sha256:"+candidate.Digest != result.declared.modelDigest {
			return fmt.Errorf("project evaluate: local Ollama %s does not match the declared %s", model, modelDigestSetting)
		}
		found = true
	}
	if !found {
		return fmt.Errorf("project evaluate: local Ollama does not serve %s", model)
	}
	loaded, err := dependencies.Commands.Run(ctx, Command{Name: "ollama", Args: []string{"ps"}})
	if err != nil || strings.Contains(string(loaded.Stdout), model) {
		return fmt.Errorf("project evaluate: %s must not already be loaded", model)
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

func jsonArrayEmpty(content []byte) bool {
	var values []json.RawMessage
	return json.Unmarshal(content, &values) == nil && len(values) == 0
}

func randomIdentifier(reader io.Reader) (string, error) {
	var value [12]byte
	if _, err := io.ReadFull(reader, value[:]); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", value[:]), nil
}
