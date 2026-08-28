package telemetry

import "time"

// Record is one normalized telemetry record, and it is the whole seam between
// this phase and the mapping phase: the receiver produces these, the mappers
// consume them, and neither has to know the collector's pdata types.
//
// The projection is deliberately NARROW, in the spirit of the transcript
// projection in usage.go. Claude Code's enhanced telemetry emits a dozen event
// types with many attributes each, and this binds a small named set: an unbound
// attribute is IGNORED, never an error. Two reasons, and the second is the one
// that matters. A telemetry export is a provider surface on a beta flag (OD3) —
// erroring on an unrecognized attribute would turn a routine upstream addition
// into a lane outage. And a projection that bound everything would be a
// content-carrying map whose contents no reader had reviewed, which is the shape
// INV-2 exists to keep out of this codebase.
//
// What is NOT here is as deliberate: no body text, no prompt, no tool output.
// Those arrive in the raw-API-body sink and on specific attributes, and phase 10
// decides field by field what is attached and under which gate. This type carries
// the identifiers and numbers a turn is built from.
type Record struct {
	// Signal names which OTLP endpoint the record arrived on. It is a routing
	// fact, not provider data.
	Signal Signal

	// EventName is the provider's own event discriminator (its `event.name`
	// attribute — "api_request", "tool_decision", "user_prompt", …). Phase 10
	// dispatches on it, so an unrecognized value must reach the mapper intact
	// rather than being normalized away here.
	EventName string

	// Timestamp is the record's own time, falling back to the observed time when
	// the exporter set none. Zero when neither was set — the mapper decides what
	// to do about that rather than having a fabricated `now` handed to it.
	Timestamp time.Time

	// Attrs is the merged attribute set: resource, scope and record, in that
	// order, so a record-level key wins over a resource-level one of the same
	// name. Values are flattened to their string form; the mapper parses what it
	// needs. Bounded by maxAttrs and maxAttrValueBytes.
	Attrs map[string]string
}

// Signal is the OTLP signal a record arrived on.
type Signal string

const (
	SignalLogs    Signal = "logs"
	SignalTraces  Signal = "traces"
	SignalMetrics Signal = "metrics"
)

// eventNameAttr is the attribute Claude Code's enhanced telemetry uses to name
// the event. A record without it is still delivered — the mapper is what decides
// that an unnameable record is uninteresting, because a receiver that dropped
// records on a missing attribute would silently hide a provider rename.
const eventNameAttr = "event.name"

// maxAttrs and maxAttrValueBytes bound one record's attribute set.
//
// The listener is unauthenticated by construction and MaxRequestBodyBytes bounds
// the REQUEST, not what one record can turn into once decoded. Without these, a
// single conforming request could expand into a map large enough to matter, and
// the spool would then carry it. Over-long values are truncated rather than
// dropped: the mapper's own caps are what decide egress, and a value that arrived
// is evidence even when it arrived clipped.
const (
	maxAttrs          = 128
	maxAttrValueBytes = 16 * 1024
)
