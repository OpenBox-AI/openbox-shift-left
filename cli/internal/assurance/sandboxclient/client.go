// Package sandboxclient speaks the OpenBox Sandbox service protocol.
//
// It exists so the assurance lane stops driving OpenShell itself. The lane used
// to shell out to the `openshell` CLI and parse its stdout — including scraping
// human-readable fields after stripping ANSI escapes — which put a second owner
// on the OpenShell contract, pinned to a different version than the sandbox
// service, with no test holding the two together. `kb/sandbox.md` records the
// rule this restores: Shift Left must not shell out to the OpenShell CLI in the
// supported path, and must see only typed operations.
//
// Stdlib only, deliberately: crypto/tls and encoding/json are all this needs,
// and this repo's dependency budget is spent (ADR-0015, ADR-0020).
package sandboxclient

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	// protocolVersion is the v1 lifecycle. projectRunProtocolVersion is the
	// separate version that carries a caller-chosen workload; a v1 service
	// rejects it at the envelope boundary rather than reinterpreting it.
	protocolVersion           = 1
	projectRunProtocolVersion = 2
	projectRunMediaType       = "application/vnd.openbox.project-run.v2+json"

	// ProjectRunCapability is the negotiated name that gates every v2
	// operation. Absent from Capabilities means unavailable, and a caller must
	// not attempt the operations it gates.
	ProjectRunCapability = "project_run_v2"

	maxRequestFrameBytes  = 2 << 20
	maxResponseFrameBytes = 4 << 20
)

// Config is the complete boundary contract, as published in agent.env.
type Config struct {
	Endpoint   string
	ServerName string
	CAPath     string
	CertPath   string
	KeyPath    string

	// AssetBundle identifies the SERVICE, not the workload. v1 compares it
	// byte-for-byte and refuses a mismatch, so it is echoed verbatim and never
	// constructed.
	AssetBundle AssetBundle
}

type AssetBundle struct {
	RuntimeContractVersion int            `json:"runtime_contract_version"`
	AdapterBuildSHA256     string         `json:"adapter_build_sha256"`
	Template               string         `json:"template"`
	Policy                 PolicyIdentity `json:"policy"`
	CompatibilityID        string         `json:"compatibility_id"`
}

type PolicyIdentity struct {
	ID      string `json:"id"`
	Version uint64 `json:"version"`
	SHA256  string `json:"sha256"`
}

type PolicyDocument struct {
	MediaType string `json:"media_type"`
	Base64    string `json:"document_base64"`
}

// OutputLimits bounds what one execution may return.
type OutputLimits struct {
	StdoutBytes   uint64 `json:"stdout_bytes"`
	StderrBytes   uint64 `json:"stderr_bytes"`
	CombinedBytes uint64 `json:"combined_bytes"`
	ChunkBytes    uint64 `json:"chunk_bytes"`
}

// LoadConfig reads the boundary contract written by `obs provision`.
//
// The file is the authority for every one of these values. Deriving any of them
// — guessing the endpoint, recomputing a digest — would mean this client and
// the service could disagree about which deployment they are talking to, which
// is exactly what the asset-bundle check exists to catch.
func LoadConfig(path string) (Config, error) {
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return Config{}, fmt.Errorf("sandboxclient: resolve home: %w", err)
		}
		path = filepath.Join(home, ".config", "openbox-sandbox", "agent.env")
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("sandboxclient: read %s: %w", path, err)
	}
	values := map[string]string{}
	for _, line := range strings.Split(string(content), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, value, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		values[strings.TrimSpace(name)] = strings.TrimSpace(value)
	}
	version, err := strconv.ParseUint(values["OPENBOX_SANDBOX_POLICY_VERSION"], 10, 64)
	if err != nil {
		return Config{}, errors.New("sandboxclient: agent.env has no usable OPENBOX_SANDBOX_POLICY_VERSION")
	}
	config := Config{
		Endpoint:   values["OPENBOX_SANDBOX_ENDPOINT"],
		ServerName: values["OPENBOX_SANDBOX_SERVER_NAME"],
		CAPath:     values["OPENBOX_SANDBOX_CA"],
		CertPath:   values["OPENBOX_SANDBOX_CERT"],
		KeyPath:    values["OPENBOX_SANDBOX_KEY"],
		AssetBundle: AssetBundle{
			// Not published in agent.env; the service's own config pins it at 1.
			RuntimeContractVersion: 1,
			AdapterBuildSHA256:     values["OPENBOX_SANDBOX_ADAPTER_SHA"],
			Template:               values["OPENBOX_SANDBOX_TEMPLATE"],
			Policy: PolicyIdentity{
				ID:      values["OPENBOX_SANDBOX_POLICY_ID"],
				Version: version,
				SHA256:  values["OPENBOX_SANDBOX_POLICY_SHA256"],
			},
			CompatibilityID: values["OPENBOX_SANDBOX_COMPAT_ID"],
		},
	}
	for name, value := range map[string]string{
		"OPENBOX_SANDBOX_ENDPOINT": config.Endpoint,
		"OPENBOX_SANDBOX_CA":       config.CAPath,
		"OPENBOX_SANDBOX_CERT":     config.CertPath,
		"OPENBOX_SANDBOX_KEY":      config.KeyPath,
	} {
		if value == "" {
			return Config{}, fmt.Errorf("sandboxclient: agent.env is missing %s", name)
		}
	}
	return config, nil
}

// Client performs one operation per connection.
//
// That is the service's shape, not a simplification: it reads exactly one
// request frame and writes exactly one response frame per TLS session, so a
// pooled connection would be a protocol error rather than an optimization.
type Client struct {
	config Config
	tls    *tls.Config
}

func New(config Config) (*Client, error) {
	certificate, err := tls.LoadX509KeyPair(config.CertPath, config.KeyPath)
	if err != nil {
		return nil, fmt.Errorf("sandboxclient: load client identity: %w", err)
	}
	authority, err := os.ReadFile(config.CAPath)
	if err != nil {
		return nil, fmt.Errorf("sandboxclient: read CA: %w", err)
	}
	pool := newCertPool()
	if !pool.AppendCertsFromPEM(authority) {
		return nil, errors.New("sandboxclient: CA file held no certificate")
	}
	serverName := config.ServerName
	if serverName == "" {
		serverName = "localhost"
	}
	return &Client{
		config: config,
		tls: &tls.Config{
			Certificates: []tls.Certificate{certificate},
			RootCAs:      pool,
			ServerName:   serverName,
			MinVersion:   tls.VersionTLS13,
		},
	}, nil
}

// Capabilities negotiates before anything is mutated.
//
// A caller checks here rather than discovering support from a failed run.
func (client *Client) Capabilities(timeout time.Duration) ([]string, error) {
	var envelope struct {
		Response struct {
			Response         string   `json:"response"`
			ProtocolVersions []uint16 `json:"protocol_versions"`
			Capabilities     []string `json:"capabilities"`
		} `json:"response"`
	}
	request := map[string]any{
		"protocol_version": protocolVersion,
		"operation_id":     mustOperationID(),
		"asset_bundle":     client.config.AssetBundle,
		"request":          map[string]any{"operation": "capabilities"},
	}
	if err := client.roundTrip(request, &envelope, timeout); err != nil {
		return nil, err
	}
	if envelope.Response.Response != "capabilities" {
		return nil, fmt.Errorf("sandboxclient: negotiation answered %q", envelope.Response.Response)
	}
	return envelope.Response.Capabilities, nil
}

// SupportsProjectRun reports whether this deployment will accept v2 at all.
func (client *Client) SupportsProjectRun(timeout time.Duration) (bool, error) {
	capabilities, err := client.Capabilities(timeout)
	if err != nil {
		return false, err
	}
	for _, capability := range capabilities {
		if capability == ProjectRunCapability {
			return true, nil
		}
	}
	return false, nil
}

// ProjectRunSpec is the closed run envelope.
//
// Environment carries no secrets — the service refuses a credential-shaped name
// outright. Providers are NAMES: the gateway resolves the credential, so no
// secret value is representable here.
type ProjectRunSpec struct {
	RunID          string            `json:"run_id"`
	Template       string            `json:"template"`
	PolicyDocument PolicyDocument    `json:"policy_document"`
	ExpectedPolicy PolicyIdentity    `json:"expected_policy"`
	Environment    map[string]string `json:"environment"`
	Providers      []string          `json:"providers"`
}

// ProjectRunResult is one terminal execution, with the isolation evidence the
// service observed. The evidence is typed, which is the whole point: the lane's
// previous source for this was gateway log text.
type ProjectRunResult struct {
	ExitCode        int             `json:"exit_code"`
	Stdout          []byte          `json:"stdout"`
	Stderr          []byte          `json:"stderr"`
	Timeout         string          `json:"timeout"`
	SandboxEvidence json.RawMessage `json:"sandbox_evidence"`
}

func (client *Client) projectRun(operation map[string]any, timeout time.Duration) (map[string]json.RawMessage, string, error) {
	request := map[string]any{
		"protocol_version": projectRunProtocolVersion,
		"media_type":       projectRunMediaType,
		"operation_id":     mustOperationID(),
		"request":          operation,
	}
	var envelope struct {
		Response json.RawMessage `json:"response"`
	}
	if err := client.roundTrip(request, &envelope, timeout); err != nil {
		return nil, "", err
	}
	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal(envelope.Response, &fields); err != nil {
		return nil, "", fmt.Errorf("sandboxclient: decode response: %w", err)
	}
	var kind string
	if raw, ok := fields["response"]; ok {
		_ = json.Unmarshal(raw, &kind)
	}
	return fields, kind, nil
}

// Begin starts one run from the caller's own image.
func (client *Client) Begin(spec ProjectRunSpec, deadline time.Duration) (runID string, token string, err error) {
	fields, kind, err := client.projectRun(map[string]any{
		"operation":   "begin_project_run",
		"spec":        spec,
		"deadline_ms": deadline.Milliseconds(),
	}, deadline+networkGrace)
	if err != nil {
		return "", "", err
	}
	if kind != "begun" {
		return "", "", failureFor("begin", kind, fields)
	}
	return stringField(fields, "run_id"), stringField(fields, "lifecycle_token"), nil
}

// WaitReady blocks until the workload and its policy are both attested.
func (client *Client) WaitReady(runID, token string, expected PolicyIdentity, deadline time.Duration) (string, error) {
	fields, kind, err := client.projectRun(map[string]any{
		"operation":       "wait_ready",
		"run_id":          runID,
		"lifecycle_token": token,
		"expected_policy": expected,
		"deadline_ms":     deadline.Milliseconds(),
	}, deadline+networkGrace)
	if err != nil {
		return "", err
	}
	if kind != "ready" {
		return "", failureFor("wait_ready", kind, fields)
	}
	// The lifecycle token ROTATES at readiness; the created-stage token is not
	// accepted afterwards.
	return stringField(fields, "lifecycle_token"), nil
}

// Exec runs one command, two-phase.
//
// Prepare-then-commit is what makes an ambiguous dispatch un-retryable, so both
// halves stay on the caller's path rather than being hidden behind one call.
func (client *Client) Exec(runID, token string, argv []string, commandTimeout uint16, limits OutputLimits, deadline time.Duration) (*ProjectRunResult, error) {
	fields, kind, err := client.projectRun(map[string]any{
		"operation":       "prepare_exec",
		"run_id":          runID,
		"lifecycle_token": token,
		"request": map[string]any{
			"argv":          argv,
			"timeout":       commandTimeout,
			"output_limits": limits,
		},
		"deadline_ms": deadline.Milliseconds(),
	}, deadline+networkGrace)
	if err != nil {
		return nil, err
	}
	if kind != "exec_prepared" {
		return nil, failureFor("prepare_exec", kind, fields)
	}
	prepareToken := stringField(fields, "prepare_token")

	fields, kind, err = client.projectRun(map[string]any{
		"operation":     "commit_exec",
		"run_id":        runID,
		"prepare_token": prepareToken,
		"deadline_ms":   deadline.Milliseconds(),
	}, deadline+networkGrace)
	if err != nil {
		return nil, err
	}
	if kind != "executed" {
		return nil, failureFor("commit_exec", kind, fields)
	}
	var result ProjectRunResult
	if err := json.Unmarshal(fields["result"], &result); err != nil {
		return nil, fmt.Errorf("sandboxclient: decode execution result: %w", err)
	}
	return &result, nil
}

// Delete requests deletion, and WaitDeleted proves terminal absence.
//
// Separate because an accepted delete is not absence: the service owns the
// difference and the caller must ask for the proof.
func (client *Client) Delete(runID string, deadline time.Duration) error {
	_, kind, err := client.projectRun(map[string]any{
		"operation":   "delete",
		"target":      map[string]any{"request_id": runID},
		"deadline_ms": deadline.Milliseconds(),
	}, deadline+networkGrace)
	if err != nil {
		return err
	}
	if kind != "deleted" {
		return fmt.Errorf("sandboxclient: delete answered %q", kind)
	}
	return nil
}

func (client *Client) WaitDeleted(runID string, deadline time.Duration) error {
	_, kind, err := client.projectRun(map[string]any{
		"operation":   "wait_deleted",
		"target":      map[string]any{"request_id": runID},
		"deadline_ms": deadline.Milliseconds(),
	}, deadline+networkGrace)
	if err != nil {
		return err
	}
	if kind != "terminally_absent" {
		return fmt.Errorf("sandboxclient: wait_deleted answered %q", kind)
	}
	return nil
}

// networkGrace is the margin between the service-side deadline and this
// client's own I/O deadline, so the service's own typed timeout answer wins
// over a local socket timeout that would say less.
const networkGrace = 10 * time.Second

func (client *Client) roundTrip(request any, response any, timeout time.Duration) error {
	payload, err := json.Marshal(request)
	if err != nil {
		return fmt.Errorf("sandboxclient: encode request: %w", err)
	}
	if len(payload) > maxRequestFrameBytes {
		return errors.New("sandboxclient: request frame exceeds the service maximum")
	}
	dialer := &tls.Dialer{Config: client.tls}
	connection, err := dialer.Dial("tcp", client.config.Endpoint)
	if err != nil {
		return fmt.Errorf("sandboxclient: dial %s: %w", client.config.Endpoint, err)
	}
	defer connection.Close()
	if timeout > 0 {
		if err := connection.SetDeadline(time.Now().Add(timeout)); err != nil {
			return fmt.Errorf("sandboxclient: set deadline: %w", err)
		}
	}
	var frame bytes.Buffer
	if err := binary.Write(&frame, binary.BigEndian, uint32(len(payload))); err != nil {
		return fmt.Errorf("sandboxclient: encode frame: %w", err)
	}
	frame.Write(payload)
	if _, err := connection.Write(frame.Bytes()); err != nil {
		return fmt.Errorf("sandboxclient: write request: %w", err)
	}
	reader := bufio.NewReader(connection)
	var length uint32
	if err := binary.Read(reader, binary.BigEndian, &length); err != nil {
		return fmt.Errorf("sandboxclient: read response length: %w", err)
	}
	if length == 0 || int(length) > maxResponseFrameBytes {
		return fmt.Errorf("sandboxclient: response frame length %d is out of range", length)
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(reader, body); err != nil {
		return fmt.Errorf("sandboxclient: read response: %w", err)
	}
	if err := json.Unmarshal(body, response); err != nil {
		return fmt.Errorf("sandboxclient: decode response envelope: %w", err)
	}
	return nil
}

func stringField(fields map[string]json.RawMessage, name string) string {
	var value string
	if raw, ok := fields[name]; ok {
		_ = json.Unmarshal(raw, &value)
	}
	return value
}

// failureFor keeps the service's own typed failure rather than flattening it,
// because the distinction between "not dispatched" and "possibly dispatched"
// decides whether cleanup is owed.
func failureFor(operation, kind string, fields map[string]json.RawMessage) error {
	if raw, ok := fields["failure"]; ok {
		return fmt.Errorf("sandboxclient: %s failed (%s): %s", operation, kind, string(raw))
	}
	return fmt.Errorf("sandboxclient: %s answered %q", operation, kind)
}

func mustOperationID() string {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		panic("sandboxclient: system randomness unavailable: " + err.Error())
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", value[0:4], value[4:6], value[6:8], value[8:10], value[10:16])
}

// newCertPool is split out so the import of crypto/x509 stays local to the one
// place that needs it.
func newCertPool() *x509.CertPool { return x509.NewCertPool() }

// DefaultOutputLimits are the ceilings the evaluation lane uses. Stated rather
// than defaulted server-side, because a caller that does not know its own
// output budget cannot know whether a truncated result is complete.
func DefaultOutputLimits() OutputLimits {
	return OutputLimits{
		StdoutBytes:   8 << 20,
		StderrBytes:   8 << 20,
		CombinedBytes: 12 << 20,
		ChunkBytes:    64 << 10,
	}
}

// NewRunID mints a caller-owned identifier in the service's exact shape.
//
// `sbx-<15 lowercase hex>` is 19 characters, which is the OpenShell gateway's
// MAX_ROUTABLE_NAME_LEN. The limit is the gateway's, not a style choice, and
// exceeding it fails at create rather than here.
func NewRunID() (string, error) {
	var value [8]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("sandboxclient: system randomness unavailable: %w", err)
	}
	return "sbx-" + hex.EncodeToString(value[:])[:15], nil
}

// policyFromFile reads a policy document and derives the identity the service
// will attest, so the two cannot disagree about which bytes were meant.
func policyFromFile(path string) (PolicyDocument, PolicyIdentity, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return PolicyDocument{}, PolicyIdentity{}, fmt.Errorf("sandboxclient: read policy: %w", err)
	}
	digest := sha256.Sum256(content)
	identity := PolicyIdentity{
		ID:      strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)),
		Version: policyVersion(content),
		SHA256:  hex.EncodeToString(digest[:]),
	}
	document := PolicyDocument{
		MediaType: "application/yaml",
		Base64:    base64.StdEncoding.EncodeToString(content),
	}
	return document, identity, nil
}

// policyVersion reads the document's own declared version. The service checks
// it against the identity, so guessing would turn a mismatch into a confusing
// rejection at create time.
func policyVersion(content []byte) uint64 {
	for _, line := range strings.Split(string(content), "\n") {
		if rest, found := strings.CutPrefix(strings.TrimSpace(line), "version:"); found {
			if value, err := strconv.ParseUint(strings.TrimSpace(rest), 10, 64); err == nil {
				return value
			}
		}
	}
	return 0
}
