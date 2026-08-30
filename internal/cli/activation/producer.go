package activation

import (
	"net"
	"net/url"
	"strings"
)

var lanePrecedence = []Lane{LaneTransport, LaneGateway, LaneTelemetry}

// Election is the resolved answer plus the evidence for it.
type Election struct {
	// Elected is the lane that may emit model-call turns.
	Elected Lane
	// Routed is every lane the tool's env points AT, in precedence order.
	Routed []Lane
	// Candidates is the subset of Routed that can actually SEE the call. The two
	// differ in exactly one state; a base URL takes the relay out of the path;
	// and keeping them apart is what stops doctor reporting a configured lane as
	// unconfigured just because it cannot win.
	Candidates []Lane
	// Reason is one sentence for `openbox doctor`.
	Reason string
}

// ResolveElection reads the tool's settings file and decides. Deliberately
// tolerant of an unreadable or absent file: that is the state of a machine
// with nothing installed, and it elects nobody.
func ResolveElection(settingsPath string) Election {
	return electionFrom(CurrentEnv(settingsPath))
}

// electionFrom one classifier, two callers; the lesson from the duplicate-
// hook-engine repair, where the check and the fix were built on the same
// function on purpose.
func electionFrom(env map[string]string) Election {
	var routed []Lane
	for _, lane := range lanePrecedence {
		if laneIsRouted(lane, env) {
			routed = append(routed, lane)
		}
	}
	candidates := candidateLanes(routed, env)
	e := Election{Routed: routed, Candidates: candidates}

	switch {
	case len(routed) == 0:
		e.Reason = "no lane is routed in the tool's settings, so no model-call turns are emitted"
		return e
	case len(candidates) == 0:
		e.Reason = "the tool's base URL sends model calls somewhere no installed lane can observe"
		return e
	}

	e.Elected = candidates[0]
	if len(routed) == 1 {
		e.Reason = "the only routed lane"
		return e
	}

	var outranked, notInPath []string
	for _, lane := range routed {
		switch {
		case lane == e.Elected:
		case containsLane(candidates, lane):
			outranked = append(outranked, string(lane))
		default:
			notInPath = append(notInPath, string(lane))
		}
	}
	var parts []string
	if len(outranked) > 0 {
		parts = append(parts, "outranks "+strings.Join(outranked, ", ")+
			" (in-path relays observe real bytes; telemetry is the tool reporting on itself)")
	}
	if len(notInPath) > 0 {
		parts = append(parts, strings.Join(notInPath, ", ")+
			" is routed but cannot see the call; the base URL sends it elsewhere")
	}
	e.Reason = strings.Join(parts, "; ")
	return e
}

func containsLane(lanes []Lane, want Lane) bool {
	for _, l := range lanes {
		if l == want {
			return true
		}
	}
	return false
}

// candidateLanes so with both lanes routed the call goes straight to the
// gateway, transport records nothing, and naming transport as the producer
// would attribute every turn to a lane that never saw one.
func candidateLanes(routed []Lane, env map[string]string) []Lane {
	baseURL := env["ANTHROPIC_BASE_URL"]
	var out []Lane
	for _, lane := range routed {
		if lane == LaneTransport && baseURL != "" {
			continue
		}
		out = append(out, lane)
	}
	return out
}

func laneIsRouted(lane Lane, env map[string]string) bool {
	switch lane {
	case LaneTransport:
		return isLoopbackURL(env["HTTPS_PROXY"]) || isLoopbackURL(env["HTTP_PROXY"])
	case LaneGateway:
		return isLoopbackURL(env["ANTHROPIC_BASE_URL"])
	case LaneTelemetry:
		if !isTruthy(env["CLAUDE_CODE_ENABLE_TELEMETRY"]) {
			return false
		}
		// Both halves are required, or a machine exporting to a corporate OTel
		// endpoint would elect a receiver that never sees a record.
		for _, key := range otelEndpointKeys {
			if isLoopbackURL(env[key]) {
				return true
			}
		}
		return false
	default:
		return false
	}
}

// isLoopbackURL this one answers "is a lane of ours routed here" for several,
// and the two are kept apart deliberately: merging them would make a change to
// either question silently change the other.
func isLoopbackURL(raw string) bool {
	if raw == "" {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return false
	}
	host := u.Hostname()
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return strings.EqualFold(host, "localhost")
}

func isTruthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
