// Package gatewayemit turns a relayed model call into a governance event.
//
// It is the connector between two halves that were each built and tested against
// a fake of the other: gateway.Captured on one side, client.DevEvent and its wire
// mapping on the other. Nothing joined them, so every capture the gateway made
// was discarded — the relay worked, the span builder worked, and no evidence ever
// left the machine.
//
// It lives in the CLI rather than in package gateway on purpose. gateway's own
// import guard allows exactly {client, decision} and fails on a third, and the
// Emitter seam exists so the CLI supplies the same client, auth and signing the
// hook path already uses instead of the gateway growing a transport and
// credential handling of its own.
package gatewayemit

import (
	"crypto/sha256"
	"encoding/hex"
	"time"

	"github.com/openbox-ai/openbox-shift-left/client"
	"github.com/openbox-ai/openbox-shift-left/gateway"
)

// GatewayIDPrefix marks every id this package mints — the idempotency key and
// the fallback request id. One constant, because three hand-typed copies across
// two files can drift apart silently.
const GatewayIDPrefix = "gw-"

// Identity is what the daemon knows about the session a captured call belongs to.
//
// It is a parameter rather than something this package resolves, because the
// fields come from different places with different certainties: the DID is read
// from local config and is always available, while the session and agent ids have
// to be recovered from the request itself.
type Identity struct {
	SessionID    string
	DeveloperDID string

	// AgentID scopes the call to a subagent when the request named one. Optional:
	// Claude Code sends it only when an agent context exists, unlike the session
	// header. It cannot perturb the activity id — client.turnActivityIDFor returns
	// from its ":gateway:" branch before it ever reaches the ":agent:" one — so
	// this is attribution detail, not identity. (Cited by symbol, not line: the
	// branch moved when ADR-0022 added the ":proxy:" and ":otel:" lanes, and a
	// line number is a citation that rots silently.) ":proxy:" now precedes
	// ":gateway:", which does not affect this: a gateway-built event sets no
	// proxy id.
	AgentID string
}

// EventFor builds the governance event for one relayed model call.
//
// EventTurnCompleted is not a choice. client/payload.go attaches a gateway span
// only under that case; any other event type is accepted, spooled and POSTed
// while silently carrying none of the evidence — the failure would look exactly
// like a working gateway.
func EventFor(id Identity, requestID string, at time.Time, c gateway.Captured) client.DevEvent {
	ev := client.DevEvent{
		SchemaVersion: client.SchemaVersion,
		EventType:     client.EventTurnCompleted,
		SessionID:     id.SessionID,
		DeveloperDID:  id.DeveloperDID,
		AgentID:       id.AgentID,
		Timestamp:     at.UTC().Format(time.RFC3339Nano),
		// A model call is not a shell/file/MCP invocation, but Tool is a required
		// wire object. It names the governed TOOL — the coding agent — which is
		// what every other event from this machine already reports.
		Tool: client.Tool{Name: "claude-code", Kind: client.ToolShell},
		// The gateway namespace for activity_id (ADR-0021 requirement 8). Without
		// it turnActivityIDFor falls through to the hook path's TurnIndex branch
		// and, with no index, returns an empty id.
		GatewayRequestID: requestID,
		Span: &client.Span{
			SemanticType:          "llm_completion",
			Stage:                 "completed",
			HTTPMethod:            c.HTTPMethod,
			HTTPURL:               c.HTTPURL,
			HTTPStatus:            c.HTTPStatus,
			CredentialFingerprint: c.CredentialFingerprint,
			RequestHeaders:        c.RequestHeaders,
			ResponseHeaders:       c.ResponseHeaders,
			RequestBody:           c.RequestBody,
			ResponseBody:          c.ResponseBody,
		},
	}
	ev.EventID = eventID(id.SessionID, requestID, string(ev.EventType), ev.Timestamp, c.HTTPMethod, c.HTTPURL)
	return ev
}

// eventID derives the idempotency key (INV-5).
//
// Deterministic rather than random, for the reason the hook path's deriveID
// gives: the spool can be drained by a different process long after the daemon
// that wrote it exited, and a redelivery has to present the same key or core
// counts the call twice. Only structural fields feed the hash — never a header
// value, a body, or the fingerprint, which derives from a secret.
//
// It takes the fields flat rather than a DevEvent so that it has no reachable
// nil precondition: reading them back out of a Span pointer built one statement
// earlier bought nothing and made a panic possible from any future caller.
func eventID(parts ...string) string {
	h := sha256.New()
	for _, part := range parts {
		h.Write([]byte(part))
		h.Write([]byte{0x1f}) // separator: two fields cannot merge into one preimage
	}
	return GatewayIDPrefix + hex.EncodeToString(h.Sum(nil))[:32]
}
