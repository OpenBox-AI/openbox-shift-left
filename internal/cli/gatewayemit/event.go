// Package gatewayemit turns a relayed model call into a governance event.
//
// It serves BOTH in-path model-call lanes: the base-URL gateway it is named for
// (`:gateway:`) and the transport relay (`:proxy:`). The name is the first
// lane's, kept rather than churned — this sits on the credential path, and
// renaming it buys readability at the cost of touching every import of a package
// whose behaviour did not change. What DID change is that the lane is now a
// required parameter: see Lane.
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
	"fmt"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/client"
	"github.com/openbox-ai/openbox-shift-left/internal/gateway"
)

// GatewayIDPrefix marks every id this package mints — the idempotency key and
// the fallback request id. One constant, because three hand-typed copies across
// two files can drift apart silently.
const GatewayIDPrefix = "gw-"

// ProxyIDPrefix marks every id minted for the transport lane. Distinct from
// GatewayIDPrefix so a fallback id — the one minted when the provider sent no
// Request-Id of its own — still says which lane produced it, in a log and in the
// event's own idempotency key.
const ProxyIDPrefix = "px-"

// Lane names the in-path producer an emitter speaks for.
//
// It is a REQUIRED parameter with no meaningful zero value, and that is the
// whole point. The tempting shape is a zero Lane meaning "gateway", because
// gateway was here first — and it would mean a transport emitter someone forgot
// to configure files its evidence under `:gateway:`, where core's dedupe absorbs
// it against the real gateway lane's event. Half the evidence would vanish with
// no error anywhere, which is exactly the failure that decision's disjoint
// namespaces exist to prevent. So an unset Lane is refused, loudly, at both
// levels: EventFor returns an error and Emitter.Emit warns and drops.
type Lane struct {
	// Name is the activity_id namespace segment, and it must agree with client's
	// turnActivityIDFor. Agreement is asserted on POSTed bytes rather than on this
	// struct (TestLaneNamesMatchTheActivityIDNamespaces) — this repo has shipped a
	// field that was right on the struct and absent from the wire.
	Name string

	// IDPrefix is what a minted fallback id starts with.
	IDPrefix string

	// setDiscriminator writes this lane's request id onto the event. One function
	// per lane rather than a switch on Name, so adding a lane cannot forget the
	// wiring and silently produce an event with NO discriminator — which
	// turnActivityIDFor renders as an empty activity_id on a payload that is still
	// spooled, signed and POSTed.
	setDiscriminator func(*client.DevEvent, string)
}

// The two in-path lanes. The telemetry lane (`:otel:`) is NOT here: it does not
// observe a relayed exchange, so it has no gateway.Captured to map and builds its
// event in internal/cli/telemetryemit instead.
var (
	LaneGateway = Lane{
		Name:             "gateway",
		IDPrefix:         GatewayIDPrefix,
		setDiscriminator: func(ev *client.DevEvent, id string) { ev.GatewayRequestID = id },
	}
	LaneProxy = Lane{
		Name:             "proxy",
		IDPrefix:         ProxyIDPrefix,
		setDiscriminator: func(ev *client.DevEvent, id string) { ev.ProxyRequestID = id },
	}
)

// valid reports whether this lane can produce an event.
func (l Lane) valid() bool {
	return l.Name != "" && l.IDPrefix != "" && l.setDiscriminator != nil
}

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
	// branch moved when that decision added the ":proxy:" and ":otel:" lanes, and
	// a line number is a citation that rots silently.) ":proxy:" now precedes
	// ":gateway:", which does not affect this: a gateway-built event sets no proxy
	// id.
	AgentID string
}

// EventFor builds the governance event for one relayed model call.
//
// EventTurnCompleted is not a choice. client/payload.go attaches a gateway span
// only under that case; any other event type is accepted, spooled and POSTed
// while silently carrying none of the evidence — the failure would look exactly
// like a working gateway.
func EventFor(lane Lane, id Identity, requestID string, at time.Time, c gateway.Captured) (client.DevEvent, error) {
	if !lane.valid() {
		// An ERROR, not a default. See Lane: a lane nobody configured must never
		// borrow another lane's namespace, because core's dedupe would then absorb
		// this event against the other lane's and half the evidence would vanish
		// with no error anywhere.
		return client.DevEvent{}, fmt.Errorf("gatewayemit: no lane configured; "+
			"an event cannot be attributed to a producer (%+v)", lane)
	}
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
	// The lane's namespace for activity_id (that decision requirement 8,
	// generalized by). Without a discriminator turnActivityIDFor falls through to
	// the hook path's TurnIndex branch and, with no index, returns an EMPTY id —
	// on a payload that is still spooled, signed and POSTed.
	lane.setDiscriminator(&ev, requestID)

	// The lane name is NOT in the hash. The id is already lane-scoped (the fallback
	// carries the lane's prefix, and a provider Request-Id belongs to one exchange
	// that only one lane observed), and adding it would change every gateway
	// event's idempotency key in a release that only claims to add a lane — after
	// which core would count one redelivered call twice.
	ev.EventID = eventID(lane.IDPrefix, id.SessionID, requestID, string(ev.EventType), ev.Timestamp, c.HTTPMethod, c.HTTPURL)
	return ev, nil
}

// eventID derives the idempotency key (INV-5).
//
// Deterministic rather than random, for the reason the hook path's deriveID
// gives: the spool can be drained by a different process long after the daemon
// that wrote it exited, and a redelivery has to present the same key or core
// counts the call twice. Only structural fields feed the hash — never a header
// value, a body, or the fingerprint, which derives from a secret.
//
// The PREFIX is the lane's, so a proxy event's key does not read `gw-`. Note what
// is NOT hashed: the lane name. The id is already lane-scoped — a fallback carries
// the lane's own prefix, and a provider Request-Id belongs to one exchange only one
// lane observed — so hashing the lane would change every SHIPPED gateway event's
// key for no gain, after which core counts a redelivered call twice.
//
// It takes the fields flat rather than a DevEvent so that it has no reachable
// nil precondition: reading them back out of a Span pointer built one statement
// earlier bought nothing and made a panic possible from any future caller.
func eventID(prefix string, parts ...string) string {
	h := sha256.New()
	for _, part := range parts {
		h.Write([]byte(part))
		h.Write([]byte{0x1f}) // separator: two fields cannot merge into one preimage
	}
	return prefix + hex.EncodeToString(h.Sum(nil))[:32]
}
