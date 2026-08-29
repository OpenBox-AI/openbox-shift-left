package activation

import (
	"sort"
	"strings"
)

// keys.go declares what each lane writes into the tool's env block.
//
// ── THE SEAM THIS FILE IS ────────────────────────────────────────────────────
//
// Every test around activation asserts JSON that WE wrote. The consumer —
// Claude Code — reads these names and silently ignores the ones it does not
// recognize, exactly as core's SpanData silently dropped `http_status` because
// the field it wanted was spelled `http_status_code`. A misspelled key here, a
// wrong endpoint path, or a value in the wrong unit yields a fully green suite
// and a lane that never receives a single record — which OD4 would then report
// as telemetry SILENCE, i.e. as a finding against the developer.
//
// So the names and values below are copied verbatim from the set that produced
// the 4366-record corpus in the sibling lab repo (openbox-logger run
// 20260827T063932Z-225cac, `src/openbox_logger/settings.py` TELEMETRY_ENV), not
// derived from documentation or from memory. TestTelemetryKeysAreTheProvenSet
// pins them as a literal list so a rename is a decision rather than a typo, and
// live confirmation against a running client is phase 13's job — stated here so
// nobody reads a green unit suite as evidence that the tool is exporting.

// otelEndpointKeys are the three signal endpoints. Named separately because the
// election reads them to decide whether telemetry points at OUR receiver or at
// somebody else's collector.
var otelEndpointKeys = []string{
	"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT",
	"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT",
	"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT",
}

// TelemetryKeys is what the telemetry lane writes, for a receiver at addr.
//
// OTEL_LOG_RAW_API_BODIES is deliberately ABSENT. The logger sets it, and it is
// what makes the client dump every raw request and response body to disk. This
// product reads none of them: phase 10 deferred body ingestion pending the
// confinement-root decision (a body_ref is a filesystem path handed to an
// unauthenticated loopback listener, and containment has to land in the same
// commit as the first read). Writing the key would therefore create a directory
// of unredacted prompts and completions on the developer's disk that nothing
// consumes — a liability with no corresponding evidence. It arrives with the
// reader, not before it.
func TelemetryKeys(addr string) map[string]string {
	base := "http://" + addr
	return map[string]string{
		"CLAUDE_CODE_ENABLE_TELEMETRY": "1",
		// OD3: this is a BETA surface, ridden deliberately. It is the switch that
		// turns the `api_request` records phase 10 maps from absent to present,
		// and the accepted failure mode is that a client update renames or drops
		// it — which shows up as silence, which OD4 makes a finding.
		"CLAUDE_CODE_ENHANCED_TELEMETRY_BETA": "1",
		"OTEL_LOGS_EXPORTER":                  "otlp",
		"OTEL_TRACES_EXPORTER":                "otlp",
		"OTEL_METRICS_EXPORTER":               "otlp",
		// http/protobuf, not grpc: measured, not assumed — this is the protocol
		// that produced the corpus phase 10's mapper was built against. The
		// receiver accepts both encodings, so a client that switched would still
		// be received; pinning the one we have evidence for is the honest default.
		"OTEL_EXPORTER_OTLP_PROTOCOL":         "http/protobuf",
		"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT":    base + "/v1/logs",
		"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT":  base + "/v1/traces",
		"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT": base + "/v1/metrics",
		// Milliseconds, bare. A unit suffix here is silently rejected.
		"OTEL_LOGS_EXPORT_INTERVAL": "1000",
		// The three content switches. They are what make this lane carry the same
		// posture the hook path already does; the `content_capture` key still
		// gates what LEAVES this machine, so turning these on does not widen
		// egress on its own — it widens what the local receiver can see.
		"OTEL_LOG_USER_PROMPTS": "1",
		"OTEL_LOG_TOOL_DETAILS": "1",
		"OTEL_LOG_TOOL_CONTENT": "1",
	}
}

// TransportKeys is what the transport lane writes, for a relay at addr with its
// CA certificate at caPath.
//
// current is the settings env as it stands, and it is a parameter rather than a
// read inside because of NO_PROXY alone: an org's existing list must be MERGED,
// not replaced. Removal would put the original back, but between install and
// removal a replaced NO_PROXY sends traffic through our relay that the org had
// deliberately excluded — a live breakage the activation record cannot undo
// while it is happening.
func TransportKeys(addr, caPath string, current map[string]string) map[string]string {
	proxy := "http://" + addr
	return map[string]string{
		"HTTP_PROXY":  proxy,
		"HTTPS_PROXY": proxy,
		// Loopback must never go through the relay: the relay's own upstream leg,
		// and every other local service the tool talks to, would otherwise dial
		// back into it.
		"NO_PROXY": mergeNoProxy(current["NO_PROXY"], "localhost", "127.0.0.1", "::1"),
		// Node reads its own trust store, not the system one, so the CA has to be
		// named explicitly or every intercepted handshake fails — which the
		// developer experiences as the provider being down.
		"NODE_EXTRA_CA_CERTS": caPath,
		// Both stores: the bundled roots keep working for every host the relay
		// tunnels without interception, while the system store is what carries
		// our CA on the hosts it does intercept.
		"CLAUDE_CODE_CERT_STORE": "bundled,system",
	}
}

// mergeNoProxy adds required entries to an existing NO_PROXY list, preserving
// order and dropping nothing.
func mergeNoProxy(existing string, required ...string) string {
	var entries []string
	seen := map[string]bool{}
	for _, part := range strings.Split(strings.ReplaceAll(existing, " ", ","), ",") {
		part = strings.TrimSpace(part)
		if part == "" || seen[part] {
			continue
		}
		seen[part] = true
		entries = append(entries, part)
	}
	for _, want := range required {
		if !seen[want] {
			seen[want] = true
			entries = append(entries, want)
		}
	}
	return strings.Join(entries, ",")
}

// KeyNames lists a key set's names, sorted — for the install plan and for
// `--remove-all`'s report of exactly what it is touching.
func KeyNames(keys map[string]string) []string {
	names := make([]string, 0, len(keys))
	for name := range keys {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
