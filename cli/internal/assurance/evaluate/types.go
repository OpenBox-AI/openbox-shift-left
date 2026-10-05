// Package evaluate runs one self-starting local OCI image as the main process
// of an OpenBox Sandbox project run, then asks the OpenBox backend to evaluate
// that run (ADR-0023). The backend owns evidence, analysis and the report; this
// package keeps no record of the run beyond what it prints.
//
// It does not drive OpenShell. The sandbox service owns that contract, its
// version pin and its security floor; this package asks one capability question
// and treats a refusal as not_runnable.
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
	ContractLabel   = "ai.openbox.project-evaluation.contract"
	ContractVersion = "v1"
	// DefaultOpenBoxProvider is the OpenShell provider carrying OPENBOX_API_KEY
	// for a local-stack connector. It is a default, not a pin: a UAT or
	// production connector is a different Core, a different runtime key, and
	// therefore a different provider object on the gateway.
	DefaultOpenBoxProvider = "obx-openbox-local"
	// pushRegistryHost is where the run-owned registry writer binds INSIDE the
	// container engine's own network namespace, which is the only address
	// `docker push` can reach on a Docker Desktop host: the daemon runs in a VM,
	// so a published host port is not its loopback.
	pushRegistryHost       = "127.0.0.1:5000"
	RegistryImage          = "registry:2.8.3@sha256:a3d8aaa63ed8681a604f1dea0aa03f100d5895b6a58ace528858a7b332415373"
	maxCaptureBytes  int64 = 8 << 20

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
)

// reservedEnvironment is what the CONNECTOR owns and a project may not set:
// the four values that describe the OpenBox connector and the run itself, which
// no project can know. Model routing is the project's own to declare.
var reservedEnvironment = map[string]string{
	"OPENBOX_EVALUATION_ID": "",
	"OPENBOX_AGENT_ID":      "",
	"OPENBOX_URL":           "",
	"OPENBOX_API_KEY":       "provider-supplied",
}

// Input is the complete public input contract.
type Input struct {
	Image        string
	EnvFile      string
	OpenBoxAgent string
	// Wait polls the requested security evaluation to a terminal status and
	// prints a summary. Without it Run returns right after the request.
	Wait bool
	// Connector coordinates. Empty means the local-stack default, which is a
	// convenience for development and NOT an assumption the lane makes: a run
	// against UAT or production supplies its own, and everything downstream —
	// the relay target, the health preflight, the URL allowlist, the provider
	// carrying OPENBOX_API_KEY, the evaluation request — follows from these.
	CoreURL         string
	BackendURL      string
	OpenBoxProvider string
	// ControlToken authenticates the one evaluation request after the run. It is
	// required up front and is never printed or placed in an error.
	ControlToken string
	// Stdout receives the request line and the --wait summary; Stderr receives
	// warnings. Nil discards.
	Stdout io.Writer
	Stderr io.Writer
}

// connector is the resolved OpenBox environment for one run.
//
// Resolution happens once, in prepare, so no later step can disagree about
// which Core a run was pointed at.
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
		if _, err := parseBaseURL(name, value); err != nil {
			return connector{}, err
		}
	}
	return resolved, nil
}

// parseBaseURL is the one strictness every OpenBox coordinate gets: absolute
// http or https, no userinfo. The evaluation request additionally refuses a
// query or fragment, because it appends its own path.
func parseBaseURL(name, value string) (*url.URL, error) {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil {
		return nil, fmt.Errorf("project evaluate: %s URL is not an absolute credential-free URL", name)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("project evaluate: %s URL scheme %q is not http or https", name, parsed.Scheme)
	}
	return parsed, nil
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
	// BackendHTTP carries the evaluation request and its polling. Separate from
	// HTTP because it must never follow a redirect (the control token is a custom
	// header, which Go does not strip across hosts) and has its own budgets.
	BackendHTTP HTTPDoer
	GOOS        string
	GOARCH      string
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
		BackendHTTP: newBackendHTTPClient(),
		GOOS:        runtime.GOOS,
		GOARCH:      runtime.GOARCH,
	}
}

// Result names the run and, once requested, the backend's security evaluation.
// Evaluation is nil when the run failed before a request was made.
type Result struct {
	// EvaluationID is the run identity the SDK tagged its events with, which is
	// the run_id the backend is asked to evaluate.
	EvaluationID string
	Evaluation   *Evaluation
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
