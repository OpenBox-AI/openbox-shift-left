// Package gatewayemit turns a relayed model call into a governance event.
// Nothing joined them, so every capture the gateway made was discarded; the
// relay worked, the span builder worked, and no evidence ever left the
// machine. It lives in the CLI rather than in package gateway on purpose.
// Gateway's own import guard allows exactly {client, decision} and fails on a
// third, and the Emitter seam exists so the CLI supplies the same client, auth
// and signing the hook path already uses instead of the gateway growing a
// transport and credential handling of its own.
package gatewayemit

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/client"
	"github.com/openbox-ai/openbox-shift-left/internal/gateway"
)

// GatewayIDPrefix marks every id this package mints; the idempotency key and
// the fallback request id. One constant, because three hand-typed copies
// across two files can drift apart silently.
const GatewayIDPrefix = "gw-"

// ProxyIDPrefix marks every id minted for the transport lane.
const ProxyIDPrefix = "px-"

// Lane names the in-path producer an emitter speaks for.
type Lane struct {
	// Name is the activity_id namespace segment, and it must agree with client's
	// turnActivityIDFor.
	Name string

	// IDPrefix is what a minted fallback id starts with.
	IDPrefix string

	setDiscriminator func(*client.DevEvent, string)
}

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

func (l Lane) valid() bool {
	return l.Name != "" && l.IDPrefix != "" && l.setDiscriminator != nil
}

// Identity is what the daemon knows about the session a captured call belongs
// to.
type Identity struct {
	SessionID    string
	DeveloperDID string

	// AgentID scopes the call to a subagent when the request named one. It cannot
	// perturb the activity id; client.turnActivityIDFor returns from its
	// ":gateway:" branch before it ever reaches the ":agent:" one; so this is
	// attribution detail, not identity.
	AgentID string
}

// EventsFor builds one relayed call's two events from one call site, so there is
// no orphan half. The Started half is retroactive, which is safe: core pairs on
// activity_id at ingest, never on arrival order.
func EventsFor(lane Lane, id Identity, requestID string, at time.Time, c gateway.Captured) ([]client.DevEvent, error) {
	if !lane.valid() {
		return nil, fmt.Errorf("gatewayemit: no lane configured; "+
			"an event cannot be attributed to a producer (%+v)", lane)
	}
	class := classifyPath(c.HTTPURL)
	started, ended := boundsOf(c, at)

	// Note what is NOT here: the HEADERS. Nothing read them, and they were the
	// highest-risk class this client carried.
	span := func(stage string) *client.Span {
		return &client.Span{
			SemanticType:          semanticTypeFor(class),
			Stage:                 stage,
			HTTPMethod:            c.HTTPMethod,
			HTTPURL:               c.HTTPURL,
			HTTPStatus:            c.HTTPStatus,
			CredentialFingerprint: c.CredentialFingerprint,
		}
	}
	half := func(eventType client.EventType, stage string, ts time.Time) client.DevEvent {
		return client.DevEvent{
			SchemaVersion: client.SchemaVersion,
			EventType:     eventType,
			SessionID:     id.SessionID,
			DeveloperDID:  id.DeveloperDID,
			AgentID:       id.AgentID,
			Tool:          client.Tool{Name: "claude-code", Kind: client.ToolShell},
			ActivityType:  class.ActivityType(),
			Timestamp:     ts.Format(time.RFC3339Nano),
			StartedAt:     started.Format(time.RFC3339Nano),
			Span:          span(stage),
		}
	}

	startEv := half(client.EventTurnStarted, "started", started)
	doneEv := half(client.EventTurnCompleted, "completed", ended)
	doneEv.EndedAt = ended.Format(time.RFC3339Nano)

	// Request on Started, response on Completed: where core stores them.
	if class.CarriesContent() {
		startEv.Span.RequestBody = c.RequestBody
		doneEv.Span.ResponseBody = c.ResponseBody
	}

	return finishPair(lane, requestID, startEv, doneEv), nil
}

// boundsOf takes both ends from the relay's clock, falling back to the emit time
// where it measured none.
func boundsOf(c gateway.Captured, at time.Time) (started, ended time.Time) {
	started, ended = c.StartedAt, c.EndedAt
	if started.IsZero() {
		started = at
	}
	if !ended.After(started) {
		ended = at
	}
	if !ended.After(started) {
		ended = started
	}
	return started.UTC(), ended.UTC()
}

// finishPair stamps the discriminator, which makes both halves share one
// activity_id, and the idempotency key, which the event type keeps distinct.
func finishPair(lane Lane, requestID string, halves ...client.DevEvent) []client.DevEvent {
	out := make([]client.DevEvent, 0, len(halves))
	for _, ev := range halves {
		lane.setDiscriminator(&ev, requestID)
		ev.EventID = eventID(lane.IDPrefix, ev.SessionID, requestID, string(ev.EventType),
			ev.Timestamp, ev.Span.HTTPMethod, ev.Span.HTTPURL)
		out = append(out, ev)
	}
	return out
}

func semanticTypeFor(class PathClass) string {
	if class == ClassCompletion {
		return client.ActivityTypeLLMCompletion
	}
	return "internal"
}

// eventID derives the idempotency key (INV-5). Only structural fields feed the
// hash; never a header value, a body, or the fingerprint, which derives from a
// secret.
func eventID(prefix string, parts ...string) string {
	h := sha256.New()
	for _, part := range parts {
		h.Write([]byte(part))
		h.Write([]byte{0x1f}) // separator: two fields cannot merge into one preimage
	}
	return prefix + hex.EncodeToString(h.Sum(nil))[:32]
}
