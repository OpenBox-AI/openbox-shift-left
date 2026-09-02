// Package evaluate runs one self-starting local OCI image in the pinned local
// OpenShell development topology and seals its observation on success.
package evaluate

import (
	"context"
	"io"
	"net"
	"net/http"
	"runtime"
	"time"
)

const (
	Schema                     = "ai.openbox.project-execution/v1"
	ContractLabel              = "ai.openbox.project-evaluation.contract"
	ContractVersion            = "v1"
	OpenShellVersion           = "0.0.111"
	OpenBoxProvider            = "obx-openbox-local"
	InferenceProvider          = "openai-compatible-provider"
	InferenceModel             = "granite4.1:3b"
	InferenceModelDigest       = "sha256:6fd349357287c7ffc9e38189a93b48ea175d24fc566b38f09cfc564fb7f303eb"
	RegistryImage              = "registry:2.8.3@sha256:a3d8aaa63ed8681a604f1dea0aa03f100d5895b6a58ace528858a7b332415373"
	coreURL                    = "http://127.0.0.1:8086"
	backendHealthURL           = "http://127.0.0.1:3000/health"
	ollamaTagsURL              = "http://127.0.0.1:11434/api/tags"
	ollamaGenerateURL          = "http://127.0.0.1:11434/api/generate"
	maxCaptureBytes      int64 = 8 << 20
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
	Image               string
	EnvFile             string
	OpenBoxAgent        string
	Output              string
	BackendURL          string
	ControlToken        string
	ObservationRequired bool
	ProxyConfigured     bool
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

// Dependencies contains every effectful evaluator seam in one value.
type Dependencies struct {
	Commands CommandRunner
	Clock    Clock
	Random   io.Reader
	Listen   func(network, address string) (net.Listener, error)
	HTTP     HTTPDoer
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
