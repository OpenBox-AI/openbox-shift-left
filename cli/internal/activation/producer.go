package activation

import (
	"net"
	"net/url"
	"strings"
)

// producer.go decides which lane emits a model-call turn event, and it is a
// CORRECTNESS INVARIANT rather than a preference.
//
// ── WHY A DOUBLE IS WORSE THAN A MISS ────────────────────────────────────────
//
// The three lanes each describe the same model call, and their activity_ids sit
// in deliberately DISJOINT namespaces (`:gateway:`, `:proxy:`, `:otel:`) so that
// core's dedupe cannot absorb one lane's event as a duplicate of another's —
// which would silently delete half the evidence. The cost of that protection is
// this: when two lanes both emit, nothing collides, core stores both, and every
// token count and cost figure downstream doubles with no error anywhere.
//
// A missed emission is loud by comparison: each daemon says at startup that it
// is not elected, and `openbox doctor` names the elected lane and the reason. So
// every ambiguous case here resolves toward emitting LESS.
//
// ── WHY THE ELECTION IS DERIVED, NOT STORED ──────────────────────────────────
//
// The input is the tool's own env block: the one place that decides where a
// model call actually goes. A lane observes a call only if the client is routed
// to it, so "who is routed" and "who can emit" are the same question, and asking
// the routing directly means there is nothing to keep in sync.
//
// A persisted `elected` field would be a second store of derivable state, and
// its drift is the silent direction: remove the transport lane without rewriting
// the field and telemetry stays quiet forever, so the machine reports no model
// calls at all while looking perfectly configured. That is the failure mode this
// repo has shipped before under other names (two DID stores, two hook engines),
// and the fix each time was to have one source, not two that agree at install.
//
// Consequence worth stating rather than hiding: the settings env is read by the
// tool at SESSION START, so a session already running when a lane is installed
// keeps producing from the lane it started with. The count stays right — one
// producer per call — but the lane is the old one until that session ends.

// lanePrecedence is ADR-0022's ruling, highest assurance first.
//
// In-path relays see the real bytes and could refuse a call; telemetry is the
// governed tool reporting on itself and can be suppressed by the very thing it
// observes. So the strongest ROUTED lane wins automatically, without the
// developer having to hold the ordering in their head — which is exactly why
// doctor has to print the winner and the reason. An automatic precedence nobody
// can see is the "configured but not in force" shape ADR-0021 promised would
// always be detectable.
var lanePrecedence = []Lane{LaneTransport, LaneGateway, LaneTelemetry}

// Election is the resolved answer plus the evidence for it.
type Election struct {
	// Elected is the lane that may emit model-call turns. "" means none, which
	// is the correct answer on a machine with no lane routed.
	Elected Lane
	// Routed is every lane the tool's env POINTS AT, in precedence order. It is
	// what `openbox doctor` reports as "configured".
	Routed []Lane
	// Candidates is the subset of Routed that can actually SEE the call. The two
	// differ in exactly one state — a base URL takes the relay out of the path —
	// and keeping them apart is what stops doctor reporting a configured lane as
	// unconfigured just because it cannot win.
	Candidates []Lane
	// Reason is one sentence for `openbox doctor`.
	Reason string
}

// ResolveElection reads the tool's settings file and decides.
//
// Deliberately tolerant of an unreadable or absent file: that is the state of a
// machine with nothing installed, and it elects nobody.
func ResolveElection(settingsPath string) Election {
	return electionFrom(CurrentEnv(settingsPath))
}

// electionFrom is the pure half, so the rule can be tested without a filesystem
// and so doctor and the daemons cannot disagree about what "elected" means. One
// classifier, two callers — the lesson from the duplicate-hook-engine repair,
// where the check and the fix were built on the same function on purpose.
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
		// Routed but none in path: the tool is pointed at a base URL that is
		// neither ours nor the provider's, so the relay blind-tunnels it. Say
		// that, rather than reporting the machine as unconfigured.
		e.Reason = "the tool's base URL sends model calls somewhere no installed lane can observe"
		return e
	}

	e.Elected = candidates[0]
	if len(routed) == 1 {
		e.Reason = "the only routed lane"
		return e
	}

	// Two different reasons a lane can lose, and saying the wrong one is worse
	// than saying nothing. It either lost on PRECEDENCE — it could see the call
	// and something stronger also could — or it is not in the path at all, in
	// which case it did not lose a ranking, it was never a candidate.
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
			" is routed but cannot see the call — the base URL sends it elsewhere")
	}
	e.Reason = strings.Join(parts, "; ")
	return e
}

// containsLane is a local membership test; the slices are three long at most.
func containsLane(lanes []Lane, want Lane) bool {
	for _, l := range lanes {
		if l == want {
			return true
		}
	}
	return false
}

// candidateLanes filters the routed set down to the lanes that CAN see the
// call, whatever the precedence says.
//
// ── WHY PRECEDENCE ALONE IS NOT ENOUGH ───────────────────────────────────────
//
// ADR-0022 ranks transport above gateway because an in-path relay observes real
// bytes. That ranking answers "which lane should an org prefer to install". This
// function answers a different question — "which lane will actually see THIS
// machine's model call" — and in one state the two disagree.
//
// A base URL pointing somewhere other than the provider defeats the transport
// lane entirely. The relay intercepts the PROVIDER's host and blind-tunnels every
// other one, and a loopback base URL is not even proxied: `NO_PROXY` carries
// 127.0.0.1, which this package writes. So with both lanes routed the call goes
// straight to the gateway, transport records nothing, and naming transport as the
// producer would attribute every turn to a lane that never saw one.
//
// The COUNT is right either way — exactly one lane emits in all of these states.
// What this protects is the ATTRIBUTION, which is the election's other job:
// `openbox doctor` prints the elected lane, and a confident wrong answer there is
// worse than an uncertain one.
func candidateLanes(routed []Lane, env map[string]string) []Lane {
	baseURL := env["ANTHROPIC_BASE_URL"]
	var out []Lane
	for _, lane := range routed {
		// A base URL set to ANYTHING takes the transport lane out of the path:
		// loopback bypasses the proxy, and any other host is blind-tunnelled
		// because it is not the provider. Both cases are "transport sees
		// nothing", and only the second is at all subtle.
		if lane == LaneTransport && baseURL != "" {
			continue
		}
		out = append(out, lane)
	}
	return out
}

// laneIsRouted reports whether the tool's env sends this lane traffic.
//
// LOOPBACK is the discriminator throughout, because every lane here is a
// per-developer daemon on this machine. An org's own remote relay in
// ANTHROPIC_BASE_URL, or a corporate proxy in HTTPS_PROXY, is therefore not read
// as one of ours — which is the direction that matters: mistaking someone else's
// remote endpoint for an OpenBox lane would elect a producer that does not
// exist and silence the one that does.
//
// The opposite mistake — a developer's own loopback proxy read as our transport
// lane — costs turn events from telemetry and is announced in its log. That is
// the tolerable direction.
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
		// Enabled telemetry pointed at somebody ELSE's collector is not this
		// lane. Both halves are required, or a machine exporting to a corporate
		// OTel endpoint would elect a receiver that never sees a record.
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

// isLoopbackURL reports whether raw addresses this machine.
//
// A sibling of gatewayservice.ourGatewayURL, which answers a different question
// (may we forget a value we displaced) for a single key. This one answers "is a
// lane of ours routed here" for several, and the two are kept apart deliberately:
// merging them would make a change to either question silently change the other.
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

// isTruthy matches the spelling the tool's own docs use for these switches. Kept
// local rather than borrowed from devconfig: that one decides OpenBox posture
// from OpenBox config, this one reads a third party's env values.
func isTruthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
