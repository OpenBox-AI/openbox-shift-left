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

	// setDiscriminator writes this lane's request id onto the event.
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

// EventFor builds the governance event for one relayed model call.
// Client/payload.go attaches a gateway span only under that case; any other
// event type is accepted, spooled and POSTed while silently carrying none of
// the evidence; the failure would look exactly like a working gateway.
func EventFor(lane Lane, id Identity, requestID string, at time.Time, c gateway.Captured) (client.DevEvent, error) {
	if !lane.valid() {
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
		Tool:          client.Tool{Name: "claude-code", Kind: client.ToolShell},
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
	lane.setDiscriminator(&ev, requestID)

	ev.EventID = eventID(lane.IDPrefix, id.SessionID, requestID, string(ev.EventType), ev.Timestamp, c.HTTPMethod, c.HTTPURL)
	return ev, nil
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
