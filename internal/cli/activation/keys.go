package activation

import (
	"sort"
	"strings"
)

var otelEndpointKeys = []string{
	"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT",
	"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT",
	"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT",
}

// TelemetryKeys is what the telemetry lane writes, for a receiver at addr.
// OTEL_LOG_RAW_API_BODIES is deliberately absent.
func TelemetryKeys(addr string) map[string]string {
	base := "http://" + addr
	return map[string]string{
		"CLAUDE_CODE_ENABLE_TELEMETRY":        "1",
		"CLAUDE_CODE_ENHANCED_TELEMETRY_BETA": "1",
		"OTEL_LOGS_EXPORTER":                  "otlp",
		"OTEL_TRACES_EXPORTER":                "otlp",
		"OTEL_METRICS_EXPORTER":               "otlp",
		"OTEL_EXPORTER_OTLP_PROTOCOL":         "http/protobuf",
		"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT":    base + "/v1/logs",
		"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT":  base + "/v1/traces",
		"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT": base + "/v1/metrics",
		// A unit suffix here is silently rejected.
		"OTEL_LOGS_EXPORT_INTERVAL": "1000",
		"OTEL_LOG_USER_PROMPTS":     "1",
		"OTEL_LOG_TOOL_DETAILS":     "1",
		"OTEL_LOG_TOOL_CONTENT":     "1",
	}
}

// TransportKeys is what the transport lane writes, for a relay at addr with
// its CA certificate at caPath. Current is the settings env as it stands, and
// it is a parameter rather than a read inside because of NO_PROXY alone: an
// org's existing list must be merged, not replaced.
func TransportKeys(addr, caPath string, current map[string]string) map[string]string {
	proxy := "http://" + addr
	return map[string]string{
		"HTTP_PROXY":             proxy,
		"HTTPS_PROXY":            proxy,
		"NO_PROXY":               mergeNoProxy(current["NO_PROXY"], "localhost", "127.0.0.1", "::1"),
		"NODE_EXTRA_CA_CERTS":    caPath,
		"CLAUDE_CODE_CERT_STORE": "bundled,system",
	}
}

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

// KeyNames lists a key set's names, sorted; for the install plan and for
// `--remove-all`'s report of exactly what it is touching.
func KeyNames(keys map[string]string) []string {
	names := make([]string, 0, len(keys))
	for name := range keys {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
