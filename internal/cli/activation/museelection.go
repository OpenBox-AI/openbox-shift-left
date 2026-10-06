package activation

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// Muse has exactly one model-call lane, telemetry: its own export. The proxy
// lane is impossible (Muse ignores the system proxy and rejects the relay's
// certificate), so there is nothing to outrank and nothing to stand by for.
// What the election still decides is whether the export is pointed at this
// receiver at all, because a receiver that elected itself for an export that
// goes to Meta's own destinations would claim to record calls it never sees.

// museTelemetryDoc binds only the one object this reader needs; every other
// key in settings.json is unread here, and the writer (internal/adapters/muse)
// owns touching it.
type museTelemetryDoc struct {
	Telemetry json.RawMessage `json:"telemetry"`
}

type museTelemetryObject struct {
	Enabled     *bool  `json:"enabled"`
	Destination string `json:"destination"`
	Endpoint    string `json:"endpoint"`
}

// museDestinationExternal is the value of telemetry.destination that sends
// Muse's export to telemetry.endpoint instead of Meta's own destinations.
const museDestinationExternal = "external"

// ResolveMuseElection reads Muse's settings.json and decides, the same way
// ResolveElection and ResolveCodexElection do for their tools. receiverAddr is
// the host:port of the receiver asking (its own --addr): the endpoint must
// point there, not merely at some loopback port. An empty receiverAddr checks
// loopback alone.
//
// An absent file elects nobody, quietly; a file that cannot be read says so in
// SettingsProblem, which is not the same as no lane being routed.
func ResolveMuseElection(settingsPath, receiverAddr string) Election {
	problem := func(msg string) Election {
		return Election{SettingsProblem: msg, Reason: msg + ", so no lane can be elected"}
	}
	notRouted := func(format string, args ...any) Election {
		return Election{Reason: fmt.Sprintf(format, args...)}
	}
	switch {
	case settingsPath == "":
		return problem("Muse's settings.json path could not be resolved at all")
	case !filepath.IsAbs(settingsPath):
		return problem(fmt.Sprintf("Muse's settings.json path %q is not absolute, so it resolves against "+
			"whatever working directory this process has; a daemon's is not the developer's home", settingsPath))
	}
	raw, err := os.ReadFile(settingsPath)
	switch {
	case os.IsNotExist(err):
		return notRouted("Muse's settings.json does not exist, so Muse exports nothing to this receiver")
	case err != nil:
		return problem(fmt.Sprintf("Muse's settings.json at %s could not be read: %v", settingsPath, err))
	}
	var doc museTelemetryDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return problem(fmt.Sprintf("Muse's settings.json at %s could not be read: %v", settingsPath, err))
	}
	var t museTelemetryObject
	if len(doc.Telemetry) == 0 || json.Unmarshal(doc.Telemetry, &t) != nil {
		return notRouted("Muse's settings.json has no telemetry block, so Muse exports to its own destinations")
	}
	switch {
	case t.Enabled == nil || !*t.Enabled:
		return notRouted("Muse's telemetry.enabled is not true, so Muse exports nothing")
	case t.Destination != museDestinationExternal:
		return notRouted("Muse's telemetry.destination is %q, not %q, so its export goes to its own "+
			"destinations and not to telemetry.endpoint", t.Destination, museDestinationExternal)
	case !isLoopbackURL(t.Endpoint):
		return notRouted("Muse's telemetry.endpoint %q is not a loopback URL, so the export does not reach "+
			"this machine's receiver", t.Endpoint)
	case !endpointIsReceiver(t.Endpoint, receiverAddr):
		return notRouted("Muse's telemetry.endpoint %q is not this receiver (%s)", t.Endpoint, receiverAddr)
	}
	return Election{
		Elected:    LaneTelemetry,
		Routed:     []Lane{LaneTelemetry},
		Candidates: []Lane{LaneTelemetry},
		Reason:     "the only lane Muse has; its own export is pointed at this receiver",
	}
}

// endpointIsReceiver compares ports: the host is already known to be loopback
// on both sides, and localhost and 127.0.0.1 are the same machine.
func endpointIsReceiver(endpoint, receiverAddr string) bool {
	if receiverAddr == "" {
		return true
	}
	_, want, err := net.SplitHostPort(receiverAddr)
	if err != nil {
		return false
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return false
	}
	got := u.Port()
	if got == "" {
		got = map[string]string{"http": "80", "https": "443"}[strings.ToLower(u.Scheme)]
	}
	return got == want
}
