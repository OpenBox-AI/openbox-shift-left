package activation

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/openbox-ai/openbox-shift-left/internal/cli/atomicfile"
	"github.com/openbox-ai/openbox-shift-left/internal/transport"

	"github.com/pelletier/go-toml/v2"
)

// CodexOtelRead is Codex's routed-detection read: config.toml, never
// settings.json's env block, and never CLAUDE_CODE_ENABLE_TELEMETRY. It
// mirrors SettingsRead's shape (same three problem states: unresolved path,
// not absolute, unreadable) so ResolveCodexElection can report exactly the
// way ResolveElection does, without the two ever being able to disagree on
// what "could not be read" means.
type CodexOtelRead struct {
	// Endpoint is the loopback-or-not OTLP/HTTP endpoint Codex's own [otel]
	// block points at, "" when absent.
	Endpoint string
	// ModelProvider, OpenAIBaseURL and ProviderBaseURLs are the model-host
	// inputs: which provider Codex sends model calls to, and where.
	ModelProvider    string
	OpenAIBaseURL    string
	ProviderBaseURLs map[string]string
	Path             string
	NotAbsolute      bool
	Missing          bool
	Err              error
}

func (r CodexOtelRead) Problem() string {
	switch {
	case r.Path == "":
		return "Codex's config.toml path could not be resolved at all, so no lane can be elected"
	case r.NotAbsolute:
		return fmt.Sprintf("Codex's config.toml path %q is not absolute, so it resolves against "+
			"whatever working directory this process has; a daemon's is not the developer's home", r.Path)
	case r.Err != nil:
		return fmt.Sprintf("Codex's config.toml at %s could not be read: %v", r.Path, r.Err)
	}
	return ""
}

func (r CodexOtelRead) Readable() bool { return r.Problem() == "" }

// codexOtelDoc binds only the one path this reader needs; every other key in
// config.toml, at any depth, is unread and therefore untouched by this
// package -- the writer's own ownership model (internal/adapters/codex) is
// what owns writing it.
type codexOtelDoc struct {
	ModelProvider  string `toml:"model_provider"`
	OpenAIBaseURL  string `toml:"openai_base_url"`
	ModelProviders map[string]struct {
		BaseURL string `toml:"base_url"`
	} `toml:"model_providers"`
	Otel struct {
		Exporter struct {
			OTLPHTTP struct {
				Endpoint string `toml:"endpoint"`
			} `toml:"otlp-http"`
		} `toml:"exporter"`
	} `toml:"otel"`
}

// ReadCodexOtelEndpoint reads the OTLP/HTTP endpoint Codex's own [otel] block
// names, or reports why it could not. An absent file elects nobody, quietly
// -- the same rule ReadSettingsEnv holds for a Claude Code settings.json that
// does not exist yet.
func ReadCodexOtelEndpoint(path string) CodexOtelRead {
	out := CodexOtelRead{Path: path}
	if path == "" {
		return out
	}
	if !filepath.IsAbs(path) {
		out.NotAbsolute = true
		return out
	}
	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
	case os.IsNotExist(err):
		out.Missing = true
		return out
	default:
		out.Err = err
		return out
	}
	var doc codexOtelDoc
	if err := toml.Unmarshal(raw, &doc); err != nil {
		out.Err = err
		return out
	}
	out.Endpoint = doc.Otel.Exporter.OTLPHTTP.Endpoint
	out.ModelProvider = doc.ModelProvider
	out.OpenAIBaseURL = doc.OpenAIBaseURL
	if len(doc.ModelProviders) > 0 {
		out.ProviderBaseURLs = make(map[string]string, len(doc.ModelProviders))
		for id, p := range doc.ModelProviders {
			out.ProviderBaseURLs[id] = p.BaseURL
		}
	}
	return out
}

// codexProviderOpenAI is Codex's built-in provider id, the one a config with
// no model_provider uses.
const codexProviderOpenAI = "openai"

// codexName is the provider name the activation record and the host table
// both use for Codex.
const codexName = "codex"

// CodexProxyStatus is whether the proxy arm of Codex's election is routed,
// and the one sentence saying why not. It is what ResolveCodexElection and
// `openbox doctor` both read, so they cannot disagree.
//
// The arm is routed only when BOTH hold:
//
//  1. the system-PAC activation record is committed (not Pending), lists
//     Codex, and Codex's effective model host is one the relay intercepts;
//  2. the relay has observed a Codex model call since that activation was
//     committed (see MarkCodexProxyObserved).
//
// The second is what keeps a Codex that ignores the PAC, or does not trust
// the relay's CA, from silencing its telemetry: until the relay has actually
// seen Codex, the telemetry lane stays the producer.
type CodexProxyStatus struct {
	Routed bool
	// Committed is condition 1 alone; Observed is condition 2 alone, read only
	// once Committed holds.
	Committed bool
	Observed  bool
	// Reason is empty when Routed, else why the arm is not.
	Reason string
}

// ResolveCodexProxy evaluates the proxy arm. An empty pacRecordPath is an
// older unit that was never given one: not routed, and quietly so.
func ResolveCodexProxy(configPath, pacRecordPath, observedMarkerPath string) CodexProxyStatus {
	return codexProxyStatus(ReadCodexOtelEndpoint(configPath), pacRecordPath, observedMarkerPath)
}

func codexProxyStatus(cfg CodexOtelRead, pacRecordPath, observedMarkerPath string) CodexProxyStatus {
	if pacRecordPath == "" {
		return CodexProxyStatus{Reason: "this daemon was given no system PAC record path (an older install); " +
			"re-run `openbox init --provider codex`"}
	}
	entry, err := LoadSystemEntryAt(pacRecordPath)
	switch {
	case err != nil:
		return CodexProxyStatus{Reason: fmt.Sprintf("the system PAC record could not be read: %v", err)}
	case entry == nil:
		return CodexProxyStatus{Reason: "no system PAC is activated for Codex"}
	case entry.Pending:
		return CodexProxyStatus{Reason: "the system PAC activation is still pending (an interrupted or running install)"}
	case !entry.PACActivated:
		return CodexProxyStatus{Reason: "the system PAC is not activated"}
	case !slices.Contains(entry.Providers, codexName):
		return CodexProxyStatus{Reason: "the activated system PAC does not list Codex"}
	}
	if reason := codexHostProblem(cfg); reason != "" {
		return CodexProxyStatus{Reason: reason}
	}
	st := CodexProxyStatus{Committed: true}
	if entry.stamp() == "" || readMarker(observedMarkerPath) != entry.stamp() {
		st.Reason = "relay has not yet seen a Codex request since the system PAC was activated"
		return st
	}
	st.Observed = true
	st.Routed = true
	return st
}

// codexHostProblem is "" when Codex's effective model host is one the relay
// intercepts for Codex, else why it is not. With no model_provider, or the
// built-in one and no base URL override, Codex talks to api.openai.com or
// chatgpt.com, both Codex rows.
func codexHostProblem(cfg CodexOtelRead) string {
	id := cfg.ModelProvider
	if id == "" {
		id = codexProviderOpenAI
	}
	base := cfg.OpenAIBaseURL
	if id != codexProviderOpenAI {
		base = cfg.ProviderBaseURLs[id]
		if base == "" {
			return fmt.Sprintf("Codex's model provider %q names no base_url, so its model host is unknown", id)
		}
	}
	if base == "" {
		return ""
	}
	u, err := url.Parse(base)
	if err != nil || u.Hostname() == "" {
		return fmt.Sprintf("Codex's model base URL %q has no host", base)
	}
	if !slices.Contains(transport.CandidatesForHost(u.Hostname()), codexName) {
		return fmt.Sprintf("Codex's model host %s is not a host the relay intercepts for Codex", u.Hostname())
	}
	return ""
}

// stamp is what the evidence marker stores: the activation's nanosecond id,
// or its commit time for a record written before that id existed.
func (e *SystemEntry) stamp() string {
	if e.ActivationID != "" {
		return e.ActivationID
	}
	return e.ActivatedAt
}

func readMarker(path string) string {
	if path == "" {
		return ""
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

// MarkCodexProxyObserved records, durably, that the relay has attributed a
// request to Codex under the CURRENT committed activation. It writes the
// activation's own commit time into markerPath, so a re-activation (a new
// commit time) invalidates the old evidence by itself and nothing has to
// remember to delete it. Nothing is written unless the record is committed
// and lists Codex: a request seen while an install is half-applied is not
// evidence about the finished one. Idempotent, and best-effort in spirit: an
// error costs only the election staying with telemetry.
func MarkCodexProxyObserved(pacRecordPath, markerPath string) (bool, error) {
	if pacRecordPath == "" || markerPath == "" {
		return false, nil
	}
	entry, err := LoadSystemEntryAt(pacRecordPath)
	if err != nil {
		return false, err
	}
	if entry == nil || entry.Pending || !entry.PACActivated || entry.stamp() == "" ||
		!slices.Contains(entry.Providers, codexName) {
		return false, nil
	}
	if readMarker(markerPath) == entry.stamp() {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(markerPath), 0o700); err != nil {
		return false, err
	}
	if err := atomicfile.Write(markerPath, []byte(entry.stamp()+"\n"), 0o600); err != nil {
		return false, err
	}
	return true, nil
}

// ResolveCodexElection is ResolveElection's Codex counterpart, reading
// different native surfaces (Codex's TOML, and the system-PAC activation
// record) for the two lanes Codex has: telemetry (its [otel] block points at
// loopback) and transport (CodexProxyStatus). It is a NEW function, not an
// edit to ResolveElection/electionFrom/laneIsRouted -- those are untouched,
// which is what makes the Claude Code arm's answer for every input provably
// identical (see codexelection_test.go's before/after table).
//
// In-path relays observe real bytes, so transport outranks telemetry: when
// both are routed the relay is elected and telemetry stands by. Both daemons
// resolve this from the same three paths, carried in their units, so exactly
// one of them records a given call.
//
// Loopback is the discriminator for telemetry, exactly as it is for every
// other lane: electing a producer that cannot see the call would silence the
// one that can.
func ResolveCodexElection(configPath, pacRecordPath, observedMarkerPath string) Election {
	read := ReadCodexOtelEndpoint(configPath)
	if problem := read.Problem(); problem != "" {
		return Election{SettingsProblem: problem, Reason: problem + ", so no lane can be elected"}
	}
	proxy := codexProxyStatus(read, pacRecordPath, observedMarkerPath)
	var routed []Lane
	if proxy.Routed {
		routed = append(routed, LaneTransport)
	}
	if isLoopbackURL(read.Endpoint) {
		routed = append(routed, LaneTelemetry)
	}
	e := Election{Routed: routed, Candidates: routed}
	switch len(routed) {
	case 0:
		e.Reason = "no lane is routed in Codex's config.toml, so no model-call turns are emitted"
		return e
	case 1:
		e.Elected = routed[0]
		e.Reason = "the only routed lane"
		return e
	}
	e.Elected = LaneTransport
	e.Reason = "the relay observes real bytes, so it outranks telemetry, which stands by"
	return e
}
