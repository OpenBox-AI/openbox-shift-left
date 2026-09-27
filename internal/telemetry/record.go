package telemetry

import "time"

// Record is one normalized telemetry record, and it is the whole seam between
// the receiver and the mappers: the receiver produces these, the mappers
// consume them, and neither has to know the collector's pdata types.
type Record struct {
	// Signal names which OTLP endpoint the record arrived on.
	Signal Signal

	// EventName is the provider's own event discriminator (its `event.name`
	// attribute; "api_request", "tool_decision", "user_prompt", …). The mapper
	// dispatches on it, so an unrecognized value must reach the mapper intact
	// rather than being normalized away here.
	EventName string

	// Timestamp is the record's own time, falling back to the observed time when
	// the exporter set none.
	Timestamp time.Time

	// Attrs is the merged attribute set: resource, scope and record, in that
	// order, so a record-level key wins over a resource-level one of the same
	// name.
	Attrs map[string]string
}

// Signal is the OTLP signal a record arrived on.
type Signal string

const (
	SignalLogs    Signal = "logs"
	SignalTraces  Signal = "traces"
	SignalMetrics Signal = "metrics"
)

const eventNameAttr = "event.name"

const (
	maxAttrs = 128

	// maxAttrValueBytes any smaller and real content would truncate ~4x
	// tighter than the accepted wire truncation, silently.
	maxAttrValueBytes = MaxAttrValueBytes
)

// MaxAttrValueBytes is the collection bound, exported so the relation to the
// wire cap is pinned by a test in the module that can see both.
const MaxAttrValueBytes = 4 * 65536
