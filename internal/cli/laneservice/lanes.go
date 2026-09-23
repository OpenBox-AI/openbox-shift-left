package laneservice

import (
	"strconv"
	"strings"
)

var grace = strconv.Itoa(StopTimeout) + "s"

// Two spellings, because the platforms do not share a convention: a reverse-
// DNS launchd label and a hyphenated systemd unit.
const (
	GatewayLabel       = "ai.openbox.gateway"
	GatewaySystemdName = "openbox-gateway"
)

// Gateway is the loopback base-URL relay.
func Gateway(addr, upstream, settingsPath string, verbose bool) Spec {
	args := []Arg{
		Literal("gateway"),
		Literal("--addr"), Value(addr),
		Literal("--upstream"), Value(upstream),
		Literal("--shutdown-grace"), Literal(grace),
	}
	args = withSettings(args, settingsPath)
	return Spec{
		Label:              GatewayLabel,
		SystemdName:        GatewaySystemdName,
		DisplayName:        "OpenBox local gateway",
		ServiceDescription: "Relays model calls through OpenBox for governance.",
		UnitDescription:    "OpenBox local gateway (model-call governance)",
		LogFile:            "gateway.log",
		Args:               withVerbose(args, verbose),
	}
}

// Telemetry is the local OTLP receiver (that decision `:otel:`).
func Telemetry(addr, settingsPath string, verbose bool) Spec {
	args := []Arg{
		Literal("telemetry"),
		Literal("--addr"), Value(addr),
		Literal("--shutdown-grace"), Literal(grace),
	}
	args = withSettings(args, settingsPath)
	return Spec{
		Label:              "ai.openbox.telemetry",
		SystemdName:        "openbox-telemetry",
		DisplayName:        "OpenBox telemetry receiver",
		ServiceDescription: "Receives the developer tool's own OTLP exports for governance.",
		UnitDescription:    "OpenBox telemetry receiver (model-call observation)",
		LogFile:            "telemetry.log",
		Args:               withVerbose(args, verbose),
	}
}

// Transport is the in-path CONNECT/TLS relay (that decision `:proxy:`).
func Transport(addr, settingsPath string, verbose bool) Spec {
	args := []Arg{
		Literal("transport"),
		Literal("--addr"), Value(addr),
		Literal("--shutdown-grace"), Literal(grace),
	}
	args = withSettings(args, settingsPath)
	return Spec{
		Label:              "ai.openbox.transport",
		SystemdName:        "openbox-transport",
		DisplayName:        "OpenBox transport relay",
		ServiceDescription: "Relays model calls in-path through OpenBox for governance.",
		UnitDescription:    "OpenBox transport relay (in-path model-call observation)",
		LogFile:            "transport.log",
		Args:               withVerbose(args, verbose),
	}
}

// VerboseFlag is the one spelling, referenced by every Spec above and by the
// test that holds them together.
const VerboseFlag = "--verbose"

// SettingsFlag carries the settings path INTO the unit at install time, because
// a daemon cannot re-derive it: launchd gives it no HOME.
const SettingsFlag = "--settings"

// CodexSettingsFlag carries Codex's config.toml path into the unit, the
// second settings surface the telemetry lane needs once it serves two tools
// from one receiver. Empty is omitted, the same as SettingsFlag: a machine
// with no Codex install yet carries none.
const CodexSettingsFlag = "--codex-settings"

func withSettings(args []Arg, settingsPath string) []Arg {
	if settingsPath == "" {
		return args
	}
	return append(args, Literal(SettingsFlag), Value(settingsPath))
}

// WithCodexSettings returns a copy of s whose Args carry Codex's config.toml
// path, appended after whatever WithEnv/withVerbose already built. A caller
// building a lane for a machine that has not configured Codex yet passes ""
// and gets s back unchanged, matching SettingsFlag's own empty-is-omitted rule.
func (s Spec) WithCodexSettings(configPath string) Spec {
	if configPath == "" {
		return s
	}
	args := make([]Arg, len(s.Args), len(s.Args)+2)
	copy(args, s.Args)
	s.Args = append(args, Literal(CodexSettingsFlag), Value(configPath))
	return s
}

// ProvidersFlag carries the transport lane's provider union into the unit,
// mirroring transport.Config.Providers' own nil/empty distinction: a caller
// passing nil gets s back with its Args untouched (today's default, unit
// args byte-identical for every existing test), and one passing a non-nil,
// possibly zero-length, slice gets an explicit --providers value -- "" for
// zero providers, so a machine that uninstalled every governed tool starts a
// lane that intercepts nothing rather than falling back to claude-code.
const ProvidersFlag = "--providers"

// WithProviders returns a copy of s whose unit carries the provider union as
// a comma-separated value, appended after whatever WithEnv/withVerbose
// already built -- the same shape WithCodexSettings uses for its own
// optional trailing flag.
func (s Spec) WithProviders(providers []string) Spec {
	if providers == nil {
		return s
	}
	args := make([]Arg, len(s.Args), len(s.Args)+2)
	copy(args, s.Args)
	s.Args = append(args, Literal(ProvidersFlag), Value(strings.Join(providers, ",")))
	return s
}

func withVerbose(args []Arg, verbose bool) []Arg {
	if !verbose {
		return args
	}
	return append(args, Literal(VerboseFlag))
}
