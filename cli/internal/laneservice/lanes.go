package laneservice

import "strconv"

// lanes.go declares the three supervised daemons.
//
// One file, so that a reader can see at a glance that they differ only in label,
// argv, log file and wording — and so that a fourth lane is a Spec rather than a
// package. Every flag named below is checked against the command's real flag set
// by TestSpecsUseFlagsThatExist: a unit that passes a flag the binary does not
// define fails to start on every boot, forever, with the failure visible only in
// a log the supervisor sends to /dev/null unless the plist says otherwise.

// grace renders the --shutdown-grace value that must match StopTimeout.
var grace = strconv.Itoa(StopTimeout) + "s"

// The gateway's supervisor identity, exported so cli/internal/gatewayservice can
// keep its own long-standing constants without a second literal to drift from.
// Two spellings, because the platforms do not share a convention: a reverse-DNS
// launchd label and a hyphenated systemd unit.
const (
	GatewayLabel       = "ai.openbox.gateway"
	GatewaySystemdName = "openbox-gateway"
)

// Gateway is the loopback base-URL relay (ADR-0021).
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
		ServiceDescription: "Relays model calls through OpenBox for governance (ADR-0021).",
		UnitDescription:    "OpenBox local gateway (model-call governance)",
		LogFile:            "gateway.log",
		Args:               withVerbose(args, verbose),
	}
}

// Telemetry is the local OTLP receiver (ADR-0022 `:otel:`).
//
// It carries no --elected flag. The producer election is DERIVED from where the
// tool's settings route model calls, so baking a decision into the unit's argv
// would create a second answer that drifts the moment another lane is installed
// or removed without rewriting this unit — and the drift is silent in the
// direction that loses all model-call evidence.
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
		ServiceDescription: "Receives the developer tool's own OTLP exports for governance (ADR-0022).",
		UnitDescription:    "OpenBox telemetry receiver (model-call observation)",
		LogFile:            "telemetry.log",
		Args:               withVerbose(args, verbose),
	}
}

// Transport is the in-path CONNECT/TLS relay (ADR-0022 `:proxy:`).
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
		ServiceDescription: "Relays model calls in-path through OpenBox for governance (ADR-0022).",
		UnitDescription:    "OpenBox transport relay (in-path model-call observation)",
		LogFile:            "transport.log",
		Args:               withVerbose(args, verbose),
	}
}

// VerboseFlag is the one spelling, referenced by every Spec above and by the
// test that holds them together. Two platforms drifting on whether a supervised
// daemon logs would be invisible until someone tried to debug the one that does
// not.
const VerboseFlag = "--verbose"

// withVerbose appends the flag when asked.
//
// --verbose belongs in the UNIT, not only on a hand-started daemon. The daemon
// owns the port, so without this the flag is reachable only by stopping the
// supervised job and racing it for the port — which is exactly how a developer
// ends up unable to answer "is anything flowing through this at all?".
func withVerbose(args []Arg, verbose bool) []Arg {
	if !verbose {
		return args
	}
	return append(args, Literal(VerboseFlag))
}
