// Package evaluate runs one self-starting local OCI image in the pinned local
// OpenShell development topology and seals its observation on success.
package evaluate

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"runtime"
	"strings"
	"time"

	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/sandboxclient"
)

const (
	Schema          = "ai.openbox.project-execution/v1"
	ContractLabel   = "ai.openbox.project-evaluation.contract"
	ContractVersion = "v1"
	// DefaultOpenBoxProvider is the OpenShell provider carrying OPENBOX_API_KEY
	// for a local-stack connector. It is a default, not a pin: a UAT or
	// production connector is a different Core, a different runtime key, and
	// therefore a different provider object on the gateway.
	DefaultOpenBoxProvider = "obx-openbox-local"
	InferenceProvider      = "openai-compatible-provider"
	InferenceModel         = "granite4.1:3b"
	InferenceModelDigest   = "sha256:6fd349357287c7ffc9e38189a93b48ea175d24fc566b38f09cfc564fb7f303eb"
	// pushRegistryHost is where the run-owned registry writer binds INSIDE the
	// container engine's own network namespace, which is the only address
	// `docker push` can reach on a Docker Desktop host: the daemon runs in a VM,
	// so a published host port is not its loopback.
	pushRegistryHost        = "127.0.0.1:5000"
	RegistryImage           = "registry:2.8.3@sha256:a3d8aaa63ed8681a604f1dea0aa03f100d5895b6a58ace528858a7b332415373"
	ollamaTagsURL           = "http://127.0.0.1:11434/api/tags"
	ollamaGenerateURL       = "http://127.0.0.1:11434/api/generate"
	maxCaptureBytes   int64 = 8 << 20

	// sandboxNegotiationTimeout bounds the one read-only question asked before
	// anything is mutated. Short on purpose: an unreachable service is a
	// not_runnable answer, not something to wait out.
	sandboxNegotiationTimeout = 10 * time.Second
	// sandboxBeginDeadline covers image preparation, which on a cold cache
	// includes the gateway pulling and converting the image.
	sandboxBeginDeadline = 5 * time.Minute
	sandboxReadyDeadline = 5 * time.Minute
	sandboxExecDeadline  = 4 * time.Minute
	sandboxDeleteTimeout = 60 * time.Second
	// sandboxCommandTimeout is the guest-side ceiling, in seconds, and is the
	// same 180s budget the CLI path allowed the image command.
	sandboxCommandTimeout uint16 = 180
)

var reservedEnvironment = map[string]string{
	"OPENBOX_EVALUATION_ID": "",
	"OPENBOX_AGENT_ID":      "",
	"OPENBOX_URL":           "",
	"OPENBOX_API_KEY":       "provider-supplied",
	"OPENBOX_SAFE_SINK_URL": "",
	"OPENAI_BASE_URL":       "https://inference.local/v1",
	"OPENAI_API_KEY":        "unused",
	"OPENAI_MODEL":          InferenceModel,
}

// Input is the complete public input contract.
type Input struct {
	Image        string
	EnvFile      string
	OpenBoxAgent string
	Output       string
	// Connector coordinates. Empty means the local-stack default, which is a
	// convenience for development and NOT an assumption the lane makes: a run
	// against UAT or production supplies its own, and everything downstream —
	// the relay target, the health preflight, the URL allowlist, the provider
	// carrying OPENBOX_API_KEY — follows from these three.
	CoreURL         string
	BackendURL      string
	OpenBoxProvider string
	ControlToken    string

	ObservationRequired bool
	ProxyConfigured     bool
}

// connector is the resolved OpenBox environment for one run.
//
// Resolution happens once, in prepare, so no later step can disagree about
// which Core a run was pointed at. The record carries it for the same reason:
// an observation pack that does not say which environment produced it is not
// evidence of anything in particular.
type connector struct {
	coreURL         string
	backendURL      string
	openBoxProvider string
}

const (
	localCoreURL    = "http://127.0.0.1:8086"
	localBackendURL = "http://127.0.0.1:3000"
)

func resolveConnector(input Input) (connector, error) {
	resolved := connector{
		coreURL:         firstNonEmpty(strings.TrimRight(input.CoreURL, "/"), localCoreURL),
		backendURL:      firstNonEmpty(strings.TrimRight(input.BackendURL, "/"), localBackendURL),
		openBoxProvider: firstNonEmpty(input.OpenBoxProvider, DefaultOpenBoxProvider),
	}
	for name, value := range map[string]string{"Core": resolved.coreURL, "backend": resolved.backendURL} {
		parsed, err := url.Parse(value)
		if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil {
			return connector{}, fmt.Errorf("project evaluate: %s URL is not an absolute credential-free URL", name)
		}
		if parsed.Scheme != "http" && parsed.Scheme != "https" {
			return connector{}, fmt.Errorf("project evaluate: %s URL scheme %q is not http or https", name, parsed.Scheme)
		}
	}
	return resolved, nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

// Command describes one direct executable invocation. Args never pass through
// a shell. Env contains only explicit non-secret additions; nil inherits the
// evaluator's host environment.
type Command struct {
	Name string
	Args []string
	Env  []string
}

type CommandResult struct {
	Stdout          []byte
	Stderr          []byte
	StdoutTruncated bool
	StderrTruncated bool
	ExitCode        int
}

type Process interface {
	Wait() CommandResult
}

type CommandRunner interface {
	Run(context.Context, Command) (CommandResult, error)
	Start(context.Context, Command) (Process, error)
}

type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

type Clock interface {
	Now() time.Time
	Sleep(context.Context, time.Duration) error
}

// SandboxClient is the evaluation lane's only path to a sandbox.
//
// It replaces the `openshell` CLI outright. The lane used to shell out and
// parse stdout — including scraping human-readable provider fields after
// stripping ANSI escapes — which made this repo a second owner of the OpenShell
// contract, pinned to a different version than the sandbox service and with no
// test holding the two together. kb/sandbox.md already forbade that: Shift Left
// must not shell out to the OpenShell CLI in the supported path, and must see
// only typed operations.
type SandboxClient interface {
	Capabilities(timeout time.Duration) ([]string, error)
	Begin(spec sandboxclient.ProjectRunSpec, deadline time.Duration) (string, string, error)
	WaitReady(runID, token string, expected sandboxclient.PolicyIdentity, deadline time.Duration) (string, error)
	WaitCompleted(runID, token string, deadline time.Duration) (*sandboxclient.ProjectRunCompleted, error)
	Delete(runID string, deadline time.Duration) error
	WaitDeleted(runID string, deadline time.Duration) error
}

// Dependencies contains every effectful evaluator seam in one value.
type Dependencies struct {
	// Sandbox is the run boundary. Nil means the lane cannot execute, which is
	// a preflight failure rather than a silent fallback to anything else.
	Sandbox SandboxClient
	// SandboxTemplateFromConfig records which deployment answered, for the run
	// record. It is the service's identity, never the workload's.
	SandboxTemplateFromConfig string
	Commands                  CommandRunner
	Clock                     Clock
	Random                    io.Reader
	Listen                    func(network, address string) (net.Listener, error)
	HTTP                      HTTPDoer
	// InferenceHTTP carries the longer model-load budget. The preflight requires
	// the model to be UNLOADED, so every load is cold: a 2 GB model read from
	// cold storage exceeds HTTP's short budget while a warm reload takes about a
	// second, which turns the shared client into an availability coin flip.
	// Kept separate because HTTP is also the Core relay client (relay.go), where
	// a longer per-request budget would change how long a stalled Core holds a
	// relayed SDK call. Nil falls back to HTTP.
	InferenceHTTP HTTPDoer
	BackendHTTP   *http.Client
	GOOS          string
	GOARCH        string
}

// inferenceHTTP returns the model-load client, falling back to the shared one.
func (d Dependencies) inferenceHTTP() HTTPDoer {
	if d.InferenceHTTP != nil {
		return d.InferenceHTTP
	}
	return d.HTTP
}

// SystemDependencies wires the real seams. The sandbox client is attached
// separately by SystemSandbox, because it needs a provisioned service and its
// absence must surface as a preflight failure rather than a nil panic.
func SystemDependencies() Dependencies {
	return Dependencies{
		Commands: systemCommandRunner{},
		Clock:    realClock{},
		Random:   systemRandomReader{},
		Listen:   net.Listen,
		HTTP: &http.Client{
			Timeout: 10 * time.Second,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		InferenceHTTP: &http.Client{
			Timeout: 30 * time.Second,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		BackendHTTP: newObservationHTTPClient(),
		GOOS:        runtime.GOOS,
		GOARCH:      runtime.GOARCH,
	}
}

// Result names the diagnostic or sealed observation directory without exposing
// captured data.
type Result struct {
	EvaluationID string
	Output       string
	Succeeded    bool
}

// SystemSandbox attaches the real sandbox client, reading the boundary contract
// that `obs provision` published.
func SystemSandbox(dependencies Dependencies, agentEnvPath string) (Dependencies, error) {
	config, err := sandboxclient.LoadConfig(agentEnvPath)
	if err != nil {
		return dependencies, err
	}
	client, err := sandboxclient.New(config)
	if err != nil {
		return dependencies, err
	}
	dependencies.Sandbox = client
	dependencies.SandboxTemplateFromConfig = config.AssetBundle.Template
	return dependencies, nil
}
