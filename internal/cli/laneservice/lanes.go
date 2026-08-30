package laneservice

import "strconv"

var grace = strconv.Itoa(StopTimeout) + "s"

// Two spellings, because the platforms do not share a convention: a reverse-
// DNS launchd label and a hyphenated systemd unit.
const (
	GatewayLabel       = "ai.openbox.gateway"
	GatewaySystemdName = "openbox-gateway"
)

// Gateway is the loopback base-URL relay.
func Gateway(addr, upstream string, verbose bool) Spec {
	args := []Arg{
		Literal("gateway"),
		Literal("--addr"), Value(addr),
		Literal("--upstream"), Value(upstream),
		Literal("--shutdown-grace"), Literal(grace),
	}
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
func Telemetry(addr string, verbose bool) Spec {
	args := []Arg{
		Literal("telemetry"),
		Literal("--addr"), Value(addr),
		Literal("--shutdown-grace"), Literal(grace),
	}
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
func Transport(addr string, verbose bool) Spec {
	args := []Arg{
		Literal("transport"),
		Literal("--addr"), Value(addr),
		Literal("--shutdown-grace"), Literal(grace),
	}
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

func withVerbose(args []Arg, verbose bool) []Arg {
	if !verbose {
		return args
	}
	return append(args, Literal(VerboseFlag))
}
