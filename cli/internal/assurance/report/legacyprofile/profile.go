// Package legacyprofile parses the frozen project-run-profile v1 contract only
// while validating historical audit packs. It has no execution authority.
package legacyprofile

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/artifact"
)

var errUnavailable = errors.New("run profile: unavailable")

const (
	maxProfileBytes  = 262_144
	maxProfileDepth  = 32
	maxTemplateBytes = 65_536
	maxTemplateDepth = 16

	profileAPIVersion = "openbox.project-run-profile/v1"
	profileKind       = "ProjectRunProfile"
	mastraDescriptor  = "@openbox-ai/openbox-mastra-sdk@1.0.0+@openbox-ai/openbox-sdk-ts@1.0.1+@mastra/core@1.8.0"

	OllamaRelayDescriptorID       = "ollama.chat.granite4.1-3b.6fd349357287"
	OllamaRelayProvider           = "ollama"
	OllamaRelayModel              = "granite4.1:3b"
	OllamaRelayModelDigest        = "6fd349357287c7ffc9e38189a93b48ea175d24fc566b38f09cfc564fb7f303eb"
	OllamaRelayServerVersion      = "0.31.1"
	OllamaRelayModelFormat        = "gguf"
	OllamaRelayModelFamily        = "granite"
	OllamaRelayModelParameterSize = "3.4B"
	OllamaRelayModelQuantization  = "Q4_K_M"
	OllamaRelayDestination        = "http://127.0.0.1:11434"
	OllamaRelayPath               = "/api/chat"
	OllamaRelayURLEnvironment     = "MODEL_BASE_URL"
	OllamaRelayBearerEnvironment  = "MODEL_API_KEY"
)

var (
	namePattern        = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[._-][a-z0-9]+)*$`)
	modelPattern       = regexp.MustCompile(`^[A-Za-z0-9._:/-]+$`)
	costPattern        = regexp.MustCompile(`^(?:0|[1-9][0-9]{0,5})\.[0-9]{2,6}$`)
	environmentPattern = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)
)

var templateTokens = map[string]string{
	"fixture.poison.url": "{{fixture.poison.url}}",
	"fixture.sink.url":   "{{fixture.sink.url}}",
	"model.url":          "{{model.url}}",
	"run.marker":         "{{run.marker}}",
	"scenario.id":        "{{scenario.id}}",
}

var bindingNamesBySource = map[string]string{
	"application.listen_host": "APP_HOST",
	"application.listen_port": "APP_PORT",
	"fixture.poison_url":      "POISON_FIXTURE_URL",
	"fixture.sink_url":        "SAFE_SINK_URL",
	"fixture.model_url":       "MODEL_BASE_URL",
	"run.marker":              "SCENARIO_MARKER",
	"scenario.id":             "SCENARIO_ID",
}

var compiledRelayDescriptors = []relayDescriptor{{
	ID:                 OllamaRelayDescriptorID,
	URLEnvironment:     OllamaRelayURLEnvironment,
	BearerEnvironment:  OllamaRelayBearerEnvironment,
	Provider:           OllamaRelayProvider,
	Models:             []string{OllamaRelayModel},
	Destination:        OllamaRelayDestination,
	PathFamily:         OllamaRelayPath,
	Method:             "POST",
	FollowRedirects:    false,
	ServerVersion:      OllamaRelayServerVersion,
	ModelDigest:        OllamaRelayModelDigest,
	ModelFormat:        OllamaRelayModelFormat,
	ModelFamily:        OllamaRelayModelFamily,
	ModelParameterSize: OllamaRelayModelParameterSize,
	ModelQuantization:  OllamaRelayModelQuantization,
	Capabilities:       []string{"completion", "tools"},
	InspectionRoutes: []relayInspectionRoute{
		{Method: "GET", Path: "/api/version"},
		{Method: "GET", Path: "/api/tags"},
	},
}}

type relayInspectionRoute struct {
	Method string
	Path   string
}

type relayDescriptor struct {
	ID                 string
	URLEnvironment     string
	BearerEnvironment  string
	Provider           string
	Models             []string
	Destination        string
	PathFamily         string
	Method             string
	FollowRedirects    bool
	ServerVersion      string
	ModelDigest        string
	ModelFormat        string
	ModelFamily        string
	ModelParameterSize string
	ModelQuantization  string
	Capabilities       []string
	InspectionRoutes   []relayInspectionRoute
}

// Profile is an immutable normalized v1 profile.
type Profile struct {
	raw       rawProfile
	template  any
	canonical []byte
	digest    artifact.ContentDigest
}

func (profile *Profile) CanonicalJSON() []byte {
	if profile == nil {
		return nil
	}
	return slices.Clone(profile.canonical)
}

func (profile *Profile) Digest() artifact.ContentDigest {
	if profile == nil {
		return artifact.ContentDigest{}
	}
	return profile.digest
}

func Parse(content []byte) (*Profile, error) {
	return parse(content, compiledRelayDescriptors)
}

func parse(content []byte, trustedRelays []relayDescriptor) (*Profile, error) {
	if len(content) == 0 || len(content) > maxProfileBytes {
		return nil, errors.New("run profile: invalid byte length")
	}
	depth, err := lexicalJSONDepth(content)
	if err != nil || depth > maxProfileDepth {
		return nil, errors.New("run profile: invalid lexical depth")
	}
	canonical, err := artifact.CanonicalizeJSON(content)
	if err != nil {
		return nil, errors.New("run profile: invalid JSON")
	}
	if err := validateClosedShape(canonical); err != nil {
		return nil, err
	}
	var raw rawProfile
	decoder := json.NewDecoder(bytes.NewReader(canonical))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&raw); err != nil {
		return nil, errors.New("run profile: invalid closed shape")
	}
	if err := validateProfile(&raw, trustedRelays); err != nil {
		return nil, err
	}
	var template any
	templateDecoder := json.NewDecoder(bytes.NewReader(raw.Application.Stimulus.BodyTemplate))
	templateDecoder.UseNumber()
	if err := templateDecoder.Decode(&template); err != nil {
		return nil, errors.New("run profile: invalid body template")
	}
	if err := validateTemplate(template, raw.Application.Stimulus.BodyTemplate); err != nil {
		return nil, err
	}
	if int64(len(raw.Application.Stimulus.BodyTemplate)) > raw.Budgets.MaxRequestBytes {
		return nil, errors.New("run profile: body template exceeds request budget")
	}
	return &Profile{
		raw: raw, template: template, canonical: canonical, digest: artifact.DigestBytes(canonical),
	}, nil
}

func validateProfile(profile *rawProfile, trustedRelays []relayDescriptor) error {
	if profile.APIVersion != profileAPIVersion || profile.Kind != profileKind || !validName(profile.Name) {
		return errors.New("run profile: invalid identity")
	}
	if err := validateApplication(profile); err != nil {
		return err
	}
	if err := validateFixtures(profile, trustedRelays); err != nil {
		return err
	}
	if profile.SDK.Descriptor != mastraDescriptor || len(profile.SDK.RequiredActionClasses) != 1 ||
		profile.SDK.RequiredActionClasses[0] != "recordingTool" {
		return errors.New("run profile: unsupported SDK declaration")
	}
	if err := validateEnvironment(profile); err != nil {
		return err
	}
	if err := validateBudgets(profile); err != nil {
		return err
	}
	if profile.Retention.Mode != "redacted_digests" || profile.Retention.RawContent != "memory_until_finalization" ||
		profile.Retention.Persist != "redacted_projections_lengths_and_sha256" ||
		profile.Retention.OnRedactionFailure != "omit_and_mark_inconclusive" {
		return errors.New("run profile: unsupported retention")
	}
	return nil
}

func validateApplication(profile *rawProfile) error {
	application := profile.Application
	if application.Protocol != "http" || application.Listen.Host != "127.0.0.1" ||
		!validBindingName(application.Listen.PortEnvironment) {
		return errors.New("run profile: invalid application listener")
	}
	readiness := application.Readiness
	if readiness.Method != "GET" || !validHTTPPath(readiness.Path) || readiness.ExpectedStatus < 200 ||
		readiness.ExpectedStatus > 299 || readiness.StartupTimeoutMS < 100 || readiness.StartupTimeoutMS > 120_000 ||
		readiness.IntervalMS < 10 || readiness.IntervalMS > 5_000 {
		return errors.New("run profile: invalid readiness declaration")
	}
	stimulus := application.Stimulus
	if stimulus.Method != "POST" || !validHTTPPath(stimulus.Path) ||
		stimulus.Headers.ContentType != "application/json" || len(stimulus.BodyTemplate) == 0 ||
		stimulus.Completion.Kind != "response" || len(stimulus.Completion.ExpectedStatuses) < 1 ||
		len(stimulus.Completion.ExpectedStatuses) > 8 {
		return errors.New("run profile: invalid stimulus declaration")
	}
	seen := make(map[int]struct{}, len(stimulus.Completion.ExpectedStatuses))
	for _, status := range stimulus.Completion.ExpectedStatuses {
		if status < 200 || status > 599 {
			return errors.New("run profile: invalid completion status")
		}
		if _, exists := seen[status]; exists {
			return errors.New("run profile: duplicate completion status")
		}
		seen[status] = struct{}{}
	}
	return nil
}

func validateFixtures(profile *rawProfile, trustedRelays []relayDescriptor) error {
	fixtures := profile.Fixtures
	if !validBindingName(fixtures.Poison.URLEnvironment) || !validBindingName(fixtures.Sink.URLEnvironment) ||
		!validBindingName(fixtures.Model.URLEnvironment) {
		return errors.New("run profile: invalid fixture binding")
	}
	model := fixtures.Model
	switch model.Mode {
	case "deterministic_local":
		if model.BearerEnvironment != "" || model.Descriptor != "" || model.Provider != "" || model.Model != "" ||
			model.Destination != "" || model.PathFamily != "" || model.Method != "" || model.FollowRedirects != nil ||
			model.DataPosture != "" {
			return errors.New("run profile: invalid deterministic model declaration")
		}
	case "authorized_relay":
		if !validCredentialEnvironment(model.BearerEnvironment) || !validName(model.Descriptor) ||
			!validName(model.Provider) || len(model.Model) < 1 || len(model.Model) > 128 || !modelPattern.MatchString(model.Model) ||
			model.Destination != OllamaRelayDestination ||
			!validHTTPPath(model.PathFamily) || model.Method != "POST" || model.FollowRedirects == nil ||
			*model.FollowRedirects || model.DataPosture != "prompt_and_completion" {
			return errors.New("run profile: invalid relay declaration")
		}
		descriptor, err := trustedRelay(model.Descriptor, trustedRelays)
		if err != nil || !relayMatches(model, descriptor) {
			return errors.New("run profile: untrusted relay declaration")
		}
	default:
		return errors.New("run profile: invalid model mode")
	}
	return nil
}

func validateEnvironment(profile *rawProfile) error {
	environment := profile.Environment
	if len(environment.GeneratedBindings) < 1 || len(environment.GeneratedBindings) > 32 || environment.Static == nil ||
		len(*environment.Static) > 1 {
		return errors.New("run profile: invalid environment declaration")
	}
	names := make(map[string]struct{}, len(environment.GeneratedBindings))
	sources := make(map[string]struct{}, len(environment.GeneratedBindings))
	for _, binding := range environment.GeneratedBindings {
		expected, sourceExists := bindingNamesBySource[binding.Source]
		if !sourceExists || binding.Name != expected {
			return errors.New("run profile: generated binding does not match its source")
		}
		if _, exists := names[binding.Name]; exists {
			return errors.New("run profile: duplicate generated binding name")
		}
		if _, exists := sources[binding.Source]; exists {
			return errors.New("run profile: duplicate generated binding source")
		}
		names[binding.Name] = struct{}{}
		sources[binding.Source] = struct{}{}
	}
	for _, binding := range *environment.Static {
		if binding.Name != "APP_ENV" || binding.Value != "security-test" {
			return errors.New("run profile: unsupported static environment")
		}
		if _, exists := names[binding.Name]; exists {
			return errors.New("run profile: static and generated environment overlap")
		}
	}
	required := []struct {
		name   string
		source string
	}{
		{profile.Application.Listen.PortEnvironment, "application.listen_port"},
		{profile.Fixtures.Poison.URLEnvironment, "fixture.poison_url"},
		{profile.Fixtures.Sink.URLEnvironment, "fixture.sink_url"},
		{profile.Fixtures.Model.URLEnvironment, "fixture.model_url"},
	}
	for _, binding := range required {
		if bindingNamesBySource[binding.source] != binding.name {
			return errors.New("run profile: required generated binding is missing")
		}
		if _, exists := sources[binding.source]; !exists {
			return errors.New("run profile: required generated binding is missing")
		}
	}
	if profile.Fixtures.Model.BearerEnvironment != "" {
		if _, exists := names[profile.Fixtures.Model.BearerEnvironment]; exists {
			return errors.New("run profile: relay bearer must be runner-owned")
		}
		for _, binding := range *environment.Static {
			if binding.Name == profile.Fixtures.Model.BearerEnvironment {
				return errors.New("run profile: relay bearer must be runner-owned")
			}
		}
	}
	return nil
}

func validateBudgets(profile *rawProfile) error {
	budgets := profile.Budgets
	if budgets.MaxProcesses < 1 || budgets.MaxProcesses > 256 || budgets.MaxRequests < 1 || budgets.MaxRequests > 10_000 ||
		budgets.MaxRequestBytes < 1 || budgets.MaxRequestBytes > 16_777_216 || budgets.MaxDurationMS < 100 ||
		budgets.MaxDurationMS > 3_600_000 || budgets.MaxStdoutBytes < 1 || budgets.MaxStdoutBytes > 16_777_216 ||
		budgets.MaxStderrBytes < 1 || budgets.MaxStderrBytes > 16_777_216 || budgets.MaxInputTokens == nil ||
		*budgets.MaxInputTokens < 0 || *budgets.MaxInputTokens > 1_000_000 || budgets.MaxOutputTokens == nil ||
		*budgets.MaxOutputTokens < 0 || *budgets.MaxOutputTokens > 1_000_000 || !costPattern.MatchString(budgets.MaxCostUSD) ||
		budgets.CleanupGraceMS < 100 || budgets.CleanupGraceMS > 30_000 {
		return errors.New("run profile: invalid budget")
	}
	if profile.Application.Readiness.IntervalMS > profile.Application.Readiness.StartupTimeoutMS {
		return errors.New("run profile: readiness interval exceeds startup timeout")
	}
	if profile.Application.Readiness.StartupTimeoutMS+budgets.CleanupGraceMS > budgets.MaxDurationMS {
		return errors.New("run profile: readiness and cleanup exceed total duration")
	}
	zeroCost := strings.Trim(budgets.MaxCostUSD, "0.") == ""
	if profile.Fixtures.Model.Mode == "deterministic_local" && !zeroCost {
		return errors.New("run profile: deterministic model cost must be zero")
	}
	if profile.Fixtures.Model.Mode == "authorized_relay" &&
		(!zeroCost || *budgets.MaxInputTokens < 1 || *budgets.MaxOutputTokens < 1) {
		return errors.New("run profile: local relay token budgets must be positive and monetary cost must be zero")
	}
	return nil
}

func validateTemplate(value any, encoded []byte) error {
	if len(encoded) > maxTemplateBytes {
		return errors.New("run profile: body template exceeds byte limit")
	}
	return inspectTemplate(value, 1)
}

func inspectTemplate(value any, depth int) error {
	if depth > maxTemplateDepth {
		return errors.New("run profile: body template exceeds depth limit")
	}
	switch typed := value.(type) {
	case nil, bool:
		return nil
	case json.Number:
		number, err := strconv.ParseFloat(string(typed), 64)
		if err != nil || number < -9_007_199_254_740_991 || number > 9_007_199_254_740_991 {
			return errors.New("run profile: body template number is outside v1 bounds")
		}
		return nil
	case string:
		if utf8.RuneCountInString(typed) > 65_536 {
			return errors.New("run profile: body template string is too long")
		}
		if strings.ContainsAny(typed, "{}") {
			if _, exists := templateTokens[strings.TrimSuffix(strings.TrimPrefix(typed, "{{"), "}}")]; !exists || !strings.HasPrefix(typed, "{{") || !strings.HasSuffix(typed, "}}") || strings.Count(typed, "{") != 2 || strings.Count(typed, "}") != 2 {
				return errors.New("run profile: invalid template token")
			}
		}
		return nil
	case []any:
		if len(typed) > 1_024 {
			return errors.New("run profile: body template array is too large")
		}
		for _, item := range typed {
			if err := inspectTemplate(item, depth+1); err != nil {
				return err
			}
		}
		return nil
	case map[string]any:
		if len(typed) > 1_024 {
			return errors.New("run profile: body template object is too large")
		}
		for key, item := range typed {
			if strings.ContainsAny(key, "{}") {
				return errors.New("run profile: template syntax is forbidden in object keys")
			}
			if err := inspectTemplate(item, depth+1); err != nil {
				return err
			}
		}
		return nil
	default:
		return errors.New("run profile: unsupported body template value")
	}
}

func trustedRelay(id string, descriptors []relayDescriptor) (relayDescriptor, error) {
	var match relayDescriptor
	found := false
	for _, descriptor := range descriptors {
		if descriptor.ID != id {
			continue
		}
		if found {
			return relayDescriptor{}, errors.New("run profile: duplicate trusted relay descriptor")
		}
		match, found = descriptor, true
	}
	if !found {
		return relayDescriptor{}, errors.New("run profile: relay descriptor is not trusted")
	}
	return match, nil
}

func relayMatches(model rawModel, descriptor relayDescriptor) bool {
	if model.Descriptor != descriptor.ID || model.URLEnvironment != descriptor.URLEnvironment ||
		model.BearerEnvironment != descriptor.BearerEnvironment || model.Provider != descriptor.Provider ||
		model.Destination != descriptor.Destination || model.PathFamily != descriptor.PathFamily ||
		model.Method != descriptor.Method || model.FollowRedirects == nil ||
		*model.FollowRedirects != descriptor.FollowRedirects {
		return false
	}
	return slices.Contains(descriptor.Models, model.Model)
}

func validName(value string) bool {
	return len(value) >= 1 && len(value) <= 64 && namePattern.MatchString(value)
}

func validBindingName(value string) bool {
	for _, expected := range bindingNamesBySource {
		if value == expected {
			return true
		}
	}
	return false
}

func validCredentialEnvironment(value string) bool {
	if len(value) < 1 || len(value) > 64 || !environmentPattern.MatchString(value) || strings.HasPrefix(value, "OPENBOX_") ||
		strings.Contains(value, "PRIVATE") || strings.Contains(value, "SECRET") || strings.Contains(value, "PASSWORD") ||
		(!strings.Contains(value, "TOKEN") && !strings.Contains(value, "KEY") && !strings.Contains(value, "CREDENTIAL")) {
		return false
	}
	for _, exact := range []string{
		"PATH", "HOME", "SHELL", "TMPDIR", "TMP", "TEMP", "BASH_ENV", "ENV", "CDPATH", "GLOBIGNORE", "IFS",
		"NODE_OPTIONS", "NODE_PATH", "PYTHONPATH", "PYTHONHOME", "RUBYOPT", "PERL5OPT", "HTTP_PROXY", "HTTPS_PROXY",
		"ALL_PROXY", "NO_PROXY", "SSL_CERT_FILE", "SSL_CERT_DIR",
	} {
		if value == exact {
			return false
		}
	}
	return !strings.HasPrefix(value, "LD_") && !strings.HasPrefix(value, "DYLD_") &&
		!strings.HasPrefix(value, "GIT_") && !strings.HasPrefix(value, "SSH_")
}

func validHTTPPath(value string) bool {
	if len(value) < 1 || utf8.RuneCountInString(value) > 256 || value[0] != '/' {
		return false
	}
	if value == "/" {
		return true
	}
	parts := strings.Split(value[1:], "/")
	for index, part := range parts {
		if part == "" {
			return index == len(parts)-1
		}
		if part == "." || part == ".." {
			return false
		}
		for _, character := range part {
			if !strings.ContainsRune("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789._~-", character) {
				return false
			}
		}
	}
	return true
}

func validLoopbackServiceURL(value string) bool {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "http" || parsed.Hostname() != "127.0.0.1" || parsed.User != nil ||
		parsed.RawPath != "" || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || strings.Contains(value, "#") || parsed.Port() == "" ||
		(parsed.Path != "" && !validHTTPPath(parsed.Path)) {
		return false
	}
	port, err := strconv.Atoi(parsed.Port())
	return err == nil && port >= 1_024 && port <= 65_535
}

func lexicalJSONDepth(content []byte) (int, error) {
	depth, maximum := 0, 0
	inString, escaped := false, false
	for _, character := range content {
		if inString {
			if escaped {
				escaped = false
			} else if character == '\\' {
				escaped = true
			} else if character == '"' {
				inString = false
			}
			continue
		}
		switch character {
		case '"':
			inString = true
		case '{', '[':
			depth++
			if depth > maximum {
				maximum = depth
			}
			if maximum > maxProfileDepth {
				return maximum, nil
			}
		case '}', ']':
			depth--
			if depth < 0 {
				return 0, errors.New("unbalanced JSON")
			}
		}
	}
	if inString || depth != 0 {
		return 0, errors.New("unbalanced JSON")
	}
	return maximum, nil
}

type rawProfile struct {
	APIVersion  string         `json:"apiVersion"`
	Kind        string         `json:"kind"`
	Name        string         `json:"name"`
	Application rawApplication `json:"application"`
	Fixtures    rawFixtures    `json:"fixtures"`
	SDK         rawSDK         `json:"sdk"`
	Environment rawEnvironment `json:"environment"`
	Budgets     rawBudgets     `json:"budgets"`
	Retention   rawRetention   `json:"retention"`
}

type rawApplication struct {
	Protocol  string       `json:"protocol"`
	Listen    rawListen    `json:"listen"`
	Readiness rawReadiness `json:"readiness"`
	Stimulus  rawStimulus  `json:"stimulus"`
}

type rawListen struct {
	Host            string `json:"host"`
	PortEnvironment string `json:"portEnvironment"`
}

type rawReadiness struct {
	Method           string `json:"method"`
	Path             string `json:"path"`
	ExpectedStatus   int    `json:"expectedStatus"`
	StartupTimeoutMS int64  `json:"startupTimeoutMs"`
	IntervalMS       int64  `json:"intervalMs"`
}

type rawStimulus struct {
	Method       string          `json:"method"`
	Path         string          `json:"path"`
	Headers      rawHeaders      `json:"headers"`
	BodyTemplate json.RawMessage `json:"bodyTemplate"`
	Completion   rawCompletion   `json:"completion"`
}

type rawHeaders struct {
	ContentType string `json:"content-type"`
}

type rawCompletion struct {
	Kind             string `json:"kind"`
	ExpectedStatuses []int  `json:"expectedStatuses"`
}

type rawFixtures struct {
	Poison rawURLBinding `json:"poison"`
	Sink   rawURLBinding `json:"sink"`
	Model  rawModel      `json:"model"`
}

type rawURLBinding struct {
	URLEnvironment string `json:"urlEnvironment"`
}

type rawModel struct {
	Mode              string `json:"mode"`
	URLEnvironment    string `json:"urlEnvironment"`
	BearerEnvironment string `json:"bearerEnvironment"`
	Descriptor        string `json:"descriptor"`
	Provider          string `json:"provider"`
	Model             string `json:"model"`
	Destination       string `json:"destination"`
	PathFamily        string `json:"pathFamily"`
	Method            string `json:"method"`
	FollowRedirects   *bool  `json:"followRedirects"`
	DataPosture       string `json:"dataPosture"`
}

type rawSDK struct {
	Descriptor            string   `json:"descriptor"`
	RequiredActionClasses []string `json:"requiredActionClasses"`
}

type rawEnvironment struct {
	GeneratedBindings []rawGeneratedBinding `json:"generatedBindings"`
	Static            *[]rawStaticBinding   `json:"static"`
}

type rawGeneratedBinding struct {
	Name   string `json:"name"`
	Source string `json:"source"`
}

type rawStaticBinding struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type rawBudgets struct {
	MaxProcesses    int64  `json:"maxProcesses"`
	MaxRequests     int64  `json:"maxRequests"`
	MaxRequestBytes int64  `json:"maxRequestBytes"`
	MaxDurationMS   int64  `json:"maxDurationMs"`
	MaxStdoutBytes  int64  `json:"maxStdoutBytes"`
	MaxStderrBytes  int64  `json:"maxStderrBytes"`
	MaxInputTokens  *int64 `json:"maxInputTokens"`
	MaxOutputTokens *int64 `json:"maxOutputTokens"`
	MaxCostUSD      string `json:"maxCostUsd"`
	CleanupGraceMS  int64  `json:"cleanupGraceMs"`
}

type rawRetention struct {
	Mode               string `json:"mode"`
	RawContent         string `json:"rawContent"`
	Persist            string `json:"persist"`
	OnRedactionFailure string `json:"onRedactionFailure"`
}
