package activation

import (
	"reflect"
	"strings"
	"testing"
)

// TestNobodyIsElectedOnAnUnroutedMachine is the invariant stated in the
// direction that cannot corrupt data.
func TestNobodyIsElectedOnAnUnroutedMachine(t *testing.T) {
	e := electionFrom(map[string]string{"CORP_TOKEN_PATH": "/etc/corp/token"})
	if e.Elected != "" {
		t.Fatalf("elected %q on a machine with no lane routed", e.Elected)
	}
	if e.Reason == "" {
		t.Error("doctor is the only place an automatic election is visible; it must always have something to print")
	}
}

func TestElectionPrecedenceIsInPathFirst(t *testing.T) {
	transport := map[string]string{"HTTPS_PROXY": "http://127.0.0.1:8790"}
	gateway := map[string]string{"ANTHROPIC_BASE_URL": "http://127.0.0.1:8788"}
	telemetry := map[string]string{
		"CLAUDE_CODE_ENABLE_TELEMETRY":     "1",
		"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT": "http://127.0.0.1:8789/v1/logs",
	}

	for _, tc := range []struct {
		name string
		env  []map[string]string
		want Lane
	}{
		{"transport and telemetry", []map[string]string{transport, telemetry}, LaneTransport},
		{"gateway and telemetry", []map[string]string{telemetry, gateway}, LaneGateway},
		{"telemetry alone", []map[string]string{telemetry}, LaneTelemetry},
		{"transport alone", []map[string]string{transport}, LaneTransport},
		// A loopback base URL is reached directly (NO_PROXY carries 127.0.0.1, which
		// this package writes), so the call never reaches the relay and naming
		// transport would attribute every turn to a lane that saw none.
		{"all three routed", []map[string]string{telemetry, gateway, transport}, LaneGateway},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := electionFrom(merge(tc.env...)).Elected; got != tc.want {
				t.Fatalf("elected %q, want %q", got, tc.want)
			}
		})
	}
}

// TestABaseURLTakesTransportOutOfThePath is the correction to a pure
// precedence ranking, and it protects attribution rather than the count.
func TestABaseURLTakesTransportOutOfThePath(t *testing.T) {
	proxy := "http://127.0.0.1:8790"
	telemetry := map[string]string{
		"CLAUDE_CODE_ENABLE_TELEMETRY":     "1",
		"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT": "http://127.0.0.1:8789/v1/logs",
	}

	e := electionFrom(merge(telemetry, map[string]string{
		"HTTPS_PROXY": proxy, "ANTHROPIC_BASE_URL": "http://127.0.0.1:8788",
	}))
	if e.Elected != LaneGateway {
		t.Errorf("elected %q with a loopback base URL and a proxy; the call never reaches the relay", e.Elected)
	}

	e = electionFrom(merge(telemetry, map[string]string{
		"HTTPS_PROXY": proxy, "ANTHROPIC_BASE_URL": "https://llm-proxy.corp.internal",
	}))
	if e.Elected != LaneTelemetry {
		t.Errorf("elected %q with the tool pointed at an org relay; no in-path lane can see that call", e.Elected)
	}
}

// TestTheElectionNamesWhatItOutranked. An automatic precedence the developer
// cannot see is the "configured but not in force" shape that decision promised
// would always be detectable, so the reason has to name the losers.
func TestTheElectionNamesWhatItOutranked(t *testing.T) {
	e := electionFrom(map[string]string{
		"HTTPS_PROXY":                      "http://127.0.0.1:8790",
		"CLAUDE_CODE_ENABLE_TELEMETRY":     "1",
		"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT": "http://127.0.0.1:8789/v1/logs",
	})
	if e.Elected != LaneTransport {
		t.Fatalf("elected %q", e.Elected)
	}
	if len(e.Routed) != 2 {
		t.Fatalf("Routed = %v, want both routed lanes", e.Routed)
	}
	if !strings.Contains(e.Reason, string(LaneTelemetry)) {
		t.Errorf("the reason does not name the lane that lost: %q", e.Reason)
	}
}

// TestSomebodyElsesRemoteEndpointIsNotOurLane is the discriminator, and it is
// a correctness property rather than a nicety.
func TestSomebodyElsesRemoteEndpointIsNotOurLane(t *testing.T) {
	e := electionFrom(map[string]string{
		"ANTHROPIC_BASE_URL":               "https://llm-proxy.corp.internal",
		"HTTPS_PROXY":                      "http://proxy.corp.internal:3128",
		"CLAUDE_CODE_ENABLE_TELEMETRY":     "1",
		"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT": "https://otel.corp.internal/v1/logs",
	})
	if e.Elected != "" {
		t.Fatalf("elected %q from an env that points at three of someone else's services", e.Elected)
	}
}

// TestTelemetryNeedsBothHalves: the enable switch alone, pointed nowhere, is
// not this lane. A machine exporting to a corporate collector would otherwise
// elect a local receiver that never sees a record.
func TestTelemetryNeedsBothHalves(t *testing.T) {
	if got := electionFrom(map[string]string{"CLAUDE_CODE_ENABLE_TELEMETRY": "1"}).Elected; got != "" {
		t.Errorf("elected %q from the enable switch with no local endpoint", got)
	}
	if got := electionFrom(map[string]string{
		"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT": "http://127.0.0.1:8789/v1/logs",
	}).Elected; got != "" {
		t.Errorf("elected %q from an endpoint with telemetry switched off", got)
	}
}

// TestTheElectionReadsTheSettingsFile crosses the seam between the pure rule
// and the file, so doctor and the daemons cannot be resolving different
// things.
func TestTheElectionReadsTheSettingsFile(t *testing.T) {
	home := t.TempDir()
	if got := ResolveElection(settingsPath(home)).Elected; got != "" {
		t.Fatalf("elected %q with no settings file at all", got)
	}
	activate(t, home, LaneTelemetry, TelemetryKeys("127.0.0.1:8789"))
	if got := ResolveElection(settingsPath(home)).Elected; got != LaneTelemetry {
		t.Fatalf("after activating telemetry the election says %q", got)
	}
	activate(t, home, LaneTransport, TransportKeys("127.0.0.1:8790", "/x/ca.pem", nil))
	if got := ResolveElection(settingsPath(home)).Elected; got != LaneTransport {
		t.Fatalf("after activating transport the election says %q, want the in-path lane", got)
	}
	if _, err := Deactivate(home, settingsPath(home), LaneTransport, false); err != nil {
		t.Fatal(err)
	}
	if got := ResolveElection(settingsPath(home)).Elected; got != LaneTelemetry {
		t.Fatalf("after removing transport the election says %q; a stale election is total evidence loss, not a cosmetic bug", got)
	}
}

// TestTelemetryKeysAreTheProvenSet is the pin for the seam that no other test
// in this repo can see.
func TestTelemetryKeysAreTheProvenSet(t *testing.T) {
	want := []string{
		"CLAUDE_CODE_ENABLE_TELEMETRY",
		"CLAUDE_CODE_ENHANCED_TELEMETRY_BETA",
		"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT",
		"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT",
		"OTEL_EXPORTER_OTLP_PROTOCOL",
		"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT",
		"OTEL_LOGS_EXPORTER",
		"OTEL_LOGS_EXPORT_INTERVAL",
		"OTEL_LOG_TOOL_CONTENT",
		"OTEL_LOG_TOOL_DETAILS",
		"OTEL_LOG_USER_PROMPTS",
		"OTEL_METRICS_EXPORTER",
		"OTEL_TRACES_EXPORTER",
	}
	got := KeyNames(TelemetryKeys("127.0.0.1:8789"))
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("telemetry key set drifted from the set that produced the evidence corpus.\n got %v\nwant %v", got, want)
	}

	keys := TelemetryKeys("127.0.0.1:8789")
	if keys["OTEL_EXPORTER_OTLP_PROTOCOL"] != "http/protobuf" {
		t.Errorf("protocol = %q; the corpus phase 10's mapper was built against is http/protobuf", keys["OTEL_EXPORTER_OTLP_PROTOCOL"])
	}
	if keys["OTEL_EXPORTER_OTLP_LOGS_ENDPOINT"] != "http://127.0.0.1:8789/v1/logs" {
		t.Errorf("logs endpoint = %q; the path is part of the contract, not decoration", keys["OTEL_EXPORTER_OTLP_LOGS_ENDPOINT"])
	}
	if keys["OTEL_LOGS_EXPORT_INTERVAL"] != "1000" {
		t.Errorf("export interval = %q; this value is bare milliseconds and a unit suffix is silently rejected", keys["OTEL_LOGS_EXPORT_INTERVAL"])
	}
	// A green suite must not be how somebody discovers we started writing raw
	// prompt and completion bodies to a directory nothing reads.
	if _, present := keys["OTEL_LOG_RAW_API_BODIES"]; present {
		t.Error("OTEL_LOG_RAW_API_BODIES is set: that makes the client dump raw request and response bodies to disk, and nothing in this product reads them yet (phase 10 deferred body ingestion pending the confinement-root decision)")
	}
}

// TestTransportKeysMergeTheDevelopersNoProxy. Replacing an org's NO_PROXY
// sends traffic through our relay that they deliberately excluded, and the
// activation record cannot undo that while it is happening; only after
// removal.
func TestTransportKeysMergeTheDevelopersNoProxy(t *testing.T) {
	keys := TransportKeys("127.0.0.1:8790", "/home/dev/.openbox/transport-ca.pem", map[string]string{
		"NO_PROXY": "internal.corp, .corp.internal",
	})
	for _, want := range []string{"internal.corp", ".corp.internal", "localhost", "127.0.0.1", "::1"} {
		if !strings.Contains(keys["NO_PROXY"], want) {
			t.Errorf("NO_PROXY = %q, missing %q", keys["NO_PROXY"], want)
		}
	}
	if got := KeyNames(TransportKeys("127.0.0.1:8790", "/x/ca.pem", nil)); !reflect.DeepEqual(got, []string{
		"CLAUDE_CODE_CERT_STORE", "HTTPS_PROXY", "HTTP_PROXY", "NODE_EXTRA_CA_CERTS", "NO_PROXY",
	}) {
		t.Fatalf("transport key set = %v", got)
	}
	first := TransportKeys("127.0.0.1:8790", "/x/ca.pem", nil)["NO_PROXY"]
	second := TransportKeys("127.0.0.1:8790", "/x/ca.pem", map[string]string{"NO_PROXY": first})["NO_PROXY"]
	if first != second {
		t.Errorf("re-installing grew NO_PROXY: %q -> %q", first, second)
	}
}

func merge(maps ...map[string]string) map[string]string {
	out := map[string]string{}
	for _, m := range maps {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}

// TestLaneKeySetsDoNotOverlap is a structural guard, not a style check.
// Nothing today overlaps; this exists so a key added to one set later cannot
// create that state silently.
func TestLaneKeySetsDoNotOverlap(t *testing.T) {
	telemetry := TelemetryKeys("127.0.0.1:8789")
	transport := TransportKeys("127.0.0.1:8790", "/x/ca.pem", nil)
	const gatewayKey = "ANTHROPIC_BASE_URL"

	for key := range telemetry {
		if _, dup := transport[key]; dup {
			t.Errorf("%q is claimed by both the telemetry and transport lanes", key)
		}
		if key == gatewayKey {
			t.Errorf("the telemetry lane claims the gateway's key %q", key)
		}
	}
	for key := range transport {
		if key == gatewayKey {
			t.Errorf("the transport lane claims the gateway's key %q", key)
		}
	}
}

// TestTheReasonDoesNotClaimAnOutrankingThatDidNotHappen. A lane loses for one
// of two unrelated reasons: it was outranked by something stronger that could
// also see the call, or it is not in the path at all; in which case it did not
// lose a ranking, it was never a candidate.
func TestTheReasonDoesNotClaimAnOutrankingThatDidNotHappen(t *testing.T) {
	e := electionFrom(map[string]string{
		"HTTPS_PROXY":                      "http://127.0.0.1:8790",
		"ANTHROPIC_BASE_URL":               "https://llm-proxy.corp.internal",
		"CLAUDE_CODE_ENABLE_TELEMETRY":     "1",
		"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT": "http://127.0.0.1:8789/v1/logs",
	})
	if e.Elected != LaneTelemetry {
		t.Fatalf("elected %q", e.Elected)
	}
	if strings.Contains(e.Reason, "outranks") {
		t.Errorf("telemetry claims to outrank the relay; it did not; the relay was never in the path: %q", e.Reason)
	}
	if !strings.Contains(e.Reason, "cannot see the call") {
		t.Errorf("the reason does not say why the relay lost: %q", e.Reason)
	}
}
