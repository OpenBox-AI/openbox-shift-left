package activation

import (
	"fmt"
	"os"
	"path/filepath"

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
	Endpoint    string
	Path        string
	NotAbsolute bool
	Missing     bool
	Err         error
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
	return out
}

// ResolveCodexElection is ResolveElection's Codex counterpart, reading a
// different native surface (TOML, not a JSON settings env block) for the one
// lane Codex has today: telemetry. It is a NEW function, not an edit to
// ResolveElection/electionFrom/laneIsRouted -- those are byte-for-byte
// unchanged by this file, which is what makes the Claude Code arm's answer
// for every input provably identical (see codexelection_test.go's
// before/after table).
//
// Loopback is the discriminator, exactly as it is for every other lane:
// electing a producer that cannot see the call would silence the one that
// can.
func ResolveCodexElection(configPath string) Election {
	read := ReadCodexOtelEndpoint(configPath)
	if problem := read.Problem(); problem != "" {
		return Election{SettingsProblem: problem, Reason: problem + ", so no lane can be elected"}
	}
	if !isLoopbackURL(read.Endpoint) {
		return Election{Reason: "no lane is routed in Codex's config.toml, so no model-call turns are emitted"}
	}
	return Election{
		Elected:    LaneTelemetry,
		Routed:     []Lane{LaneTelemetry},
		Candidates: []Lane{LaneTelemetry},
		Reason:     "the only routed lane",
	}
}
