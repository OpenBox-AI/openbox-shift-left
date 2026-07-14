package sidecar

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"time"

	"github.com/openbox-ai/openbox-shift-left/client"
)

// DefaultDecisionTimeout is the hard per-call budget the enforce hook allows for
// a local decision (ADR-0002 INV-3b: ~50 ms hook budget; sidecar decision target
// <10 ms). Past it the Client FAILS OPEN. It bounds the worst-case latency added
// to a tool call to this value, then allow.
const DefaultDecisionTimeout = 50 * time.Millisecond

// Client is what the enforce-mode PreToolUse hook (E6-S1) imports to ask the
// resident daemon for a decision. Its ONE job is INV-3b's teeth: get a local
// decision within the hard timeout, and on ANY fault — socket absent, dial
// refused, timeout, malformed reply — return a fail-open ALLOW rather than an
// error. An infra failure NEVER blocks the developer (OD9).
//
// Per-org fail-closed (deny on the same faults) is NOT here: it is E6-S3's
// explicit opt-in policy layered on top. This Client is the fail-OPEN primitive.
type Client struct {
	socketPath string
	timeout    time.Duration
	dialer     net.Dialer
}

// ClientConfig configures a Client.
type ClientConfig struct {
	// SocketPath is the daemon's Unix socket. Empty → DefaultSocketPath().
	SocketPath string
	// Timeout is the hard per-call budget. Zero → DefaultDecisionTimeout.
	Timeout time.Duration
}

// NewClient builds an enforce-hook client. It never fails: an empty/invalid
// socket path simply means every Decide call fails open (the daemon is absent).
func NewClient(cfg ClientConfig) *Client {
	sp := cfg.SocketPath
	if sp == "" {
		sp = DefaultSocketPath()
	}
	to := cfg.Timeout
	if to == 0 {
		to = DefaultDecisionTimeout
	}
	return &Client{socketPath: sp, timeout: to}
}

// Decision is the enforce hook's result: the local Evaluation plus whether it
// came from the daemon or a fail-open fallback. FailOpen==true means the daemon
// could not be reached/answered in time and the Evaluation is a synthesized
// allow — the caller (E6-S1/E6-S2) treats it as "proceed, degrade to observe".
type Decision struct {
	Evaluation client.Evaluation
	// FailOpen reports that NO real daemon verdict was obtained for this call, so
	// telemetry/conformance (E6-S7) — and the E6-S3 failure policy — can distinguish
	// an actual evaluated ALLOW from a degraded allow. It is true in TWO cases,
	// both "OpenBox did not govern this call":
	//   - the Client could not reach/parse the daemon (socket absent, dial refused,
	//     timeout, malformed reply) — see allowFailOpen, Source=sourceFailOpenClient;
	//   - the daemon WAS reached but produced no real verdict (no bundle synced yet,
	//     bad protocol, missing session) — Source=sourceFailOpenNoBundle.
	// Both are the same class to the failure policy: fail-open (default) proceeds,
	// fail-closed (opt-in, E6-S3) denies. Only a resident-evaluator verdict
	// (Source=sourceLocalBundle) is FailOpen=false — see isRealVerdictSource.
	//
	// This is a DELIBERATE shift-left deviation from the reference SDK, which has no
	// "reachable-but-no-verdict" state (an empty/unbundled evaluation there proceeds
	// as ALLOW). The local sidecar CAN be up-but-unbundled ([EXT-opa-bundle]); a
	// fail-closed org must not be silently ungoverned in that state. It is consistent
	// with E6-S3, which already fails a fail-closed org closed on a malformed reply
	// (the adjacent "reachable, no usable verdict" axis).
	FailOpen bool
	// Source echoes the daemon's DecisionResponse.Source, or sourceFailOpenClient
	// when the Client synthesized the allow.
	Source string
	// Stale echoes the daemon's staleness flag (false on a fail-open fallback).
	Stale bool
	// RedactedInput echoes the daemon's guardrail-redacted tool_input (STORY-E6-S4),
	// which the enforce hook applies via Claude Code's `updatedInput`. It is nil on
	// a fail-open fallback and empty unless a redaction-capable evaluator produced
	// one. INV-2: content-bearing, LOCAL-only — see DecisionResponse.RedactedInput.
	RedactedInput json.RawMessage
}

// allowFailOpen is the synthesized degrade-to-observe result. VerdictUnknown (not
// VerdictAllow) records honestly that OpenBox did NOT evaluate this call — the
// enforce mapping (E6-S2) treats Unknown as allow, and telemetry can tell it
// apart from a real ALLOW verdict.
func allowFailOpen(reason string) Decision {
	return Decision{
		Evaluation: client.Evaluation{Verdict: client.VerdictUnknown, Reason: reason},
		FailOpen:   true,
		Source:     sourceFailOpenClient,
	}
}

// Decide asks the daemon for a decision on req, bounded by the Client timeout.
// It NEVER returns an error: every failure path yields a fail-open allow. The
// bounded ctx guarantees the caller is delayed at most ~timeout before the tool
// call proceeds (INV-3b).
func (c *Client) Decide(ctx context.Context, req DecisionRequest) Decision {
	if req.Protocol == 0 {
		req.Protocol = ProtocolVersion
	}

	// Hard-bound the whole exchange (dial + write + read) to the timeout, on top
	// of the caller's ctx. Whichever fires first wins.
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	conn, err := c.dialer.DialContext(ctx, "unix", c.socketPath)
	if err != nil {
		// Socket absent / daemon down / refused → fail open. This is the common,
		// expected case when enforcement is off or the daemon has not been started.
		return allowFailOpen("sidecar unavailable")
	}
	defer conn.Close()

	if dl, ok := ctx.Deadline(); ok {
		// The whole INV-3b worst-case bound rests on this deadline: a blocking
		// socket read does not watch ctx.Done(), so the write/read below are bounded
		// ONLY by the connection deadline. If setting it fails, do NOT proceed with
		// an unbounded read — fail open immediately (G_SEC F2).
		if err := conn.SetDeadline(dl); err != nil {
			return allowFailOpen("sidecar deadline unset")
		}
	}

	body, err := json.Marshal(req)
	if err != nil {
		return allowFailOpen("request marshal failed")
	}
	body = append(body, '\n')
	if _, err := conn.Write(body); err != nil {
		return allowFailOpen("sidecar write failed")
	}

	// Read one bounded response line. A hung daemon trips the deadline → fail open
	// within the bound.
	r := bufio.NewReader(conn)
	line, err := readBounded(r, defaultMaxRequestBytes)
	if err != nil {
		return allowFailOpen("sidecar read failed or timed out")
	}
	var resp DecisionResponse
	if err := json.Unmarshal(line, &resp); err != nil {
		return allowFailOpen("sidecar response malformed")
	}
	// A WELL-FORMED reply is not necessarily a real verdict: the daemon reports a
	// no-verdict outcome (cold start / no bundle, bad protocol, missing session)
	// with Source=sourceFailOpenNoBundle. Mark those FailOpen so the E6-S3 failure
	// policy engages (fail-open proceeds; fail-closed denies) — closing the E6-S3
	// G_SEC INFO-1 hole where a reachable-but-unbundled daemon left a fail-closed
	// org silently ungoverned. Only sourceLocalBundle is a real verdict.
	return Decision{
		Evaluation:    resp.Evaluation,
		FailOpen:      !isRealVerdictSource(resp.Source),
		Source:        resp.Source,
		Stale:         resp.Stale,
		RedactedInput: resp.RedactedInput, // LOCAL-only; applied via CC updatedInput (E6-S4)
	}
}

// isRealVerdictSource reports whether a well-formed DecisionResponse.Source
// denotes a REAL evaluated verdict (as opposed to a degraded no-verdict reply the
// failure policy must handle). The server tags EVERY resident-evaluator decision
// — the Phase-1 rule bundle today and the embedded-OPA evaluator later (ADR-0003)
// — sourceLocalBundle, so this stays correct as evaluators evolve. Any other
// source (sourceFailOpenNoBundle, or an unknown/empty string from a stale/foreign
// peer) is treated as "no real verdict" → FailOpen — the safe direction, routing
// the decision to the failure policy rather than mistaking it for an allow.
func isRealVerdictSource(source string) bool {
	return source == sourceLocalBundle
}

// readBounded reads up to a newline or max bytes, whichever first. It caps the
// reply so a rogue/oversized response can't exhaust memory (INV-1 bounded read),
// symmetric with the server's request cap.
func readBounded(r *bufio.Reader, max int64) ([]byte, error) {
	buf := make([]byte, 0, 512)
	for int64(len(buf)) < max {
		b, err := r.ReadByte()
		if err != nil {
			if len(buf) > 0 && errors.Is(err, io.EOF) {
				return buf, nil
			}
			return nil, err
		}
		if b == '\n' {
			return buf, nil
		}
		buf = append(buf, b)
	}
	return buf, nil
}
