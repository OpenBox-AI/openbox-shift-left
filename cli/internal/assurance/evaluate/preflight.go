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
)

var (
	environmentNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	agentIDPattern         = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[1-8][0-9a-fA-F]{3}-[89abAB][0-9a-fA-F]{3}-[0-9a-fA-F]{12}$`)
	credentialNamePattern  = regexp.MustCompile(`(?i)(^|_)(api_?key|access_?key|token|secret|password|passwd|credential|private_?key|signing_?key|bearer|auth)($|_)`)
	ansiPattern            = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)
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
	input            Input
	evaluationID     string
	sandboxName      string
	registryName     string
	output           string
	image            dockerImage
	argv             []string
	environment      map[string]string
	environmentNames []string
	applicationRoot  string
	openShellGateway string
	openShellDriver  string
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
	result := &prepared{
		input: input, evaluationID: evaluationID,
		sandboxName:  "obx-eval-" + identifier[:10],
		registryName: "obx-eval-registry-" + identifier,
		output:       output,
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
	result.environment, err = effectiveEnvironment(image.Config.Env, fileEnvironment, evaluationID, input.OpenBoxAgent)
	if err != nil {
		return nil, err
	}
	result.environmentNames = environmentInventory(result.environment)

	if _, err := inspectImage(ctx, dependencies.Commands, RegistryImage); err != nil {
		return nil, fmt.Errorf("project evaluate: required registry image is not preloaded: %w", err)
	}
	if err := preflightOpenShell(ctx, dependencies, result); err != nil {
		return nil, err
	}
	if err := preflightLocalServices(ctx, dependencies); err != nil {
		return nil, err
	}
	return result, nil
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

func parseEnvironmentFile(filename string) (map[string]string, error) {
	content, err := readEnvironmentFileNoFollow(filename)
	if err != nil {
		return nil, err
	}
	return parseEnvironment(content)
}

func parseEnvironment(content []byte) (map[string]string, error) {
	if len(content) > 64<<10 {
		return nil, errors.New("project evaluate: environment file exceeds 64 KiB")
	}
	if !utf8.Valid(content) || bytes.IndexByte(content, 0) >= 0 {
		return nil, errors.New("project evaluate: environment file must be NUL-free UTF-8")
	}
	values := make(map[string]string)
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
		if _, reserved := reservedEnvironment[name]; reserved {
			return nil, fmt.Errorf("project evaluate: environment line %d sets reserved key %s", index+1, name)
		}
		if credentialNamePattern.MatchString(name) {
			return nil, fmt.Errorf("project evaluate: environment line %d uses credential-looking key %s", index+1, name)
		}
		if _, duplicate := values[name]; duplicate {
			return nil, fmt.Errorf("project evaluate: duplicate environment key %s", name)
		}
		if len(value) > 4<<10 || strings.ContainsAny(value, "\x00\r\n") {
			return nil, fmt.Errorf("project evaluate: invalid value for environment key %s", name)
		}
		if err := validateEnvironmentURL(value); err != nil {
			return nil, fmt.Errorf("project evaluate: environment key %s: %w", name, err)
		}
		values[name] = value
		if len(values) > 64 {
			return nil, errors.New("project evaluate: environment file exceeds 64 entries")
		}
	}
	return values, nil
}

func effectiveEnvironment(imageEntries []string, overrides map[string]string, evaluationID, agentID string) (map[string]string, error) {
	values := make(map[string]string)
	for _, entry := range imageEntries {
		name, value, found := strings.Cut(entry, "=")
		if !found || !environmentNamePattern.MatchString(name) || len(value) > 4<<10 || strings.ContainsAny(value, "\x00\r\n") {
			return nil, errors.New("project evaluate: image Config.Env is malformed")
		}
		if _, reserved := reservedEnvironment[name]; reserved {
			if credentialNamePattern.MatchString(name) && value != "" && value != "unused" {
				return nil, fmt.Errorf("project evaluate: image embeds credential-looking environment key %s", name)
			}
			continue
		}
		if credentialNamePattern.MatchString(name) {
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
	for name, value := range overrides {
		values[name] = value
	}
	values["OPENBOX_EVALUATION_ID"] = evaluationID
	values["OPENBOX_AGENT_ID"] = agentID
	values["OPENAI_BASE_URL"] = reservedEnvironment["OPENAI_BASE_URL"]
	values["OPENAI_API_KEY"] = reservedEnvironment["OPENAI_API_KEY"]
	values["OPENAI_MODEL"] = reservedEnvironment["OPENAI_MODEL"]
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

func preflightOpenShell(ctx context.Context, dependencies Dependencies, result *prepared) error {
	version, err := dependencies.Commands.Run(ctx, Command{Name: "openshell", Args: []string{"--version"}})
	if err != nil || strings.TrimSpace(string(version.Stdout)) != "openshell "+OpenShellVersion {
		return fmt.Errorf("project evaluate: OpenShell CLI must be exactly %s", OpenShellVersion)
	}
	statusResult, err := dependencies.Commands.Run(ctx, Command{Name: "openshell", Args: []string{"status", "-o", "json"}})
	if err != nil {
		return errors.New("project evaluate: active OpenShell Gateway is unavailable")
	}
	var status struct {
		Status         string `json:"status"`
		Version        string `json:"version"`
		Server         string `json:"server"`
		Authentication struct {
			Status   string `json:"status"`
			Provider string `json:"provider"`
		} `json:"authentication"`
	}
	if json.Unmarshal(statusResult.Stdout, &status) != nil || status.Status != "connected" ||
		status.Version != OpenShellVersion || status.Authentication.Status != "authenticated" ||
		status.Authentication.Provider != "mTLS transport" || !strings.HasPrefix(status.Server, "https://localhost:") {
		return errors.New("project evaluate: active local mTLS OpenShell Gateway failed preflight")
	}
	infoResult, err := dependencies.Commands.Run(ctx, Command{Name: "openshell", Args: []string{"gateway", "info", "-o", "json"}})
	if err != nil {
		return errors.New("project evaluate: cannot inspect OpenShell Gateway drivers")
	}
	var info struct {
		Status         string `json:"status"`
		Version        string `json:"version"`
		ComputeDrivers []struct {
			Name         string `json:"name"`
			Capabilities struct {
				DriverName    string `json:"driver_name"`
				DriverVersion string `json:"driver_version"`
			} `json:"capabilities"`
		} `json:"compute_drivers"`
	}
	if json.Unmarshal(infoResult.Stdout, &info) != nil || info.Status != "healthy" || info.Version != OpenShellVersion || len(info.ComputeDrivers) != 1 ||
		info.ComputeDrivers[0].Name != "vm" || info.ComputeDrivers[0].Capabilities.DriverName != "openshell-driver-vm" ||
		info.ComputeDrivers[0].Capabilities.DriverVersion != OpenShellVersion {
		return errors.New("project evaluate: OpenShell Gateway/VM driver tuple must be exactly 0.0.111")
	}
	result.openShellGateway = info.Version
	result.openShellDriver = info.ComputeDrivers[0].Capabilities.DriverVersion

	provider, err := dependencies.Commands.Run(ctx, Command{Name: "openshell", Args: []string{"provider", "get", OpenBoxProvider}})
	providerText := ansiPattern.ReplaceAllString(string(append(provider.Stdout, provider.Stderr...)), "")
	if err != nil || outputField(providerText, "Name") != OpenBoxProvider ||
		outputField(providerText, "Type") != "openbox-local" ||
		outputField(providerText, "Credential keys") != "OPENBOX_API_KEY" || outputField(providerText, "Config keys") != "<none>" {
		return errors.New("project evaluate: obx-openbox-local must exist with only OPENBOX_API_KEY and no config")
	}
	modelProvider, err := dependencies.Commands.Run(ctx, Command{Name: "openshell", Args: []string{"provider", "get", InferenceProvider}})
	modelProviderText := ansiPattern.ReplaceAllString(string(append(modelProvider.Stdout, modelProvider.Stderr...)), "")
	if err != nil || outputField(modelProviderText, "Name") != InferenceProvider ||
		outputField(modelProviderText, "Type") != "openai" ||
		outputField(modelProviderText, "Credential keys") != "OPENAI_API_KEY" ||
		outputField(modelProviderText, "Config keys") != "OPENAI_BASE_URL" {
		return errors.New("project evaluate: local Ollama OpenShell provider is not preconfigured")
	}
	inference, err := dependencies.Commands.Run(ctx, Command{Name: "openshell", Args: []string{"inference", "get"}})
	inferenceText := strings.ToLower(ansiPattern.ReplaceAllString(string(append(inference.Stdout, inference.Stderr...)), ""))
	if err != nil || !strings.Contains(inferenceText, InferenceProvider) || !strings.Contains(inferenceText, strings.ToLower(InferenceModel)) {
		return errors.New("project evaluate: OpenShell inference route must already select local Ollama granite4.1:3b")
	}
	list, err := dependencies.Commands.Run(ctx, Command{Name: "openshell", Args: []string{
		"sandbox", "list", "--selector", "ai.openbox.evaluation-id=" + result.evaluationID, "-o", "json",
	}})
	if err != nil || !jsonArrayEmpty(list.Stdout) {
		return errors.New("project evaluate: generated evaluation label is already in use")
	}
	return nil
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

func preflightLocalServices(ctx context.Context, dependencies Dependencies) error {
	for name, endpoint := range map[string]string{"Core": coreURL + "/", "backend": backendHealthURL} {
		response, err := get(ctx, dependencies.HTTP, endpoint)
		if err != nil || response == nil || response.StatusCode < 200 || response.StatusCode >= 300 {
			if response != nil {
				response.Body.Close()
			}
			return fmt.Errorf("project evaluate: local OpenBox %s health endpoint is unavailable", name)
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
		response.Body.Close()
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
	found := false
	for _, model := range tags.Models {
		if model.Name == InferenceModel && "sha256:"+model.Digest == InferenceModelDigest {
			found = true
		}
	}
	if err != nil || !found {
		return errors.New("project evaluate: local Ollama granite4.1:3b digest does not match the accepted digest")
	}
	loaded, err := dependencies.Commands.Run(ctx, Command{Name: "ollama", Args: []string{"ps"}})
	if err != nil || strings.Contains(string(loaded.Stdout), InferenceModel) {
		return errors.New("project evaluate: granite4.1:3b must not already be loaded")
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
