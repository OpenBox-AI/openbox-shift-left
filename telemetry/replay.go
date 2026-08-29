package telemetry

import (
	"context"
	"fmt"

	"go.opentelemetry.io/collector/pdata/plog"
)

// ConsumeLogsJSON decodes an OTLP/JSON logs payload and delivers every record to
// the emitter, through the SAME projection the receiver's HTTP endpoint uses.
//
// It exists so that a recorded provider export can be replayed against the
// shipped chain on a host that cannot bind a listener. That is not a convenience:
// this lane's whole claim is that it maps REAL provider traffic correctly, and
// the only way to test that claim without a socket is to enter the production
// path one layer below HTTP.
//
// What a replay through here proves and what it does not, stated once so no
// report has to guess. It proves the decode, the resource/scope/record attribute
// merge, the timestamp fallback, the value bounds, and everything downstream of
// the emitter. It proves NOTHING about the OTLP HTTP layer itself — the listener,
// the protobuf encoding, the request-size limit, or the exporter's retries. That
// seam has its own control test, and that one is bind-guarded.
//
// It is deliberately not the receiver's own decode of the wire: the endpoint
// receives protobuf, this takes JSON. Both land on plog.Logs, which is where the
// projection begins, so the shared part is the part with the logic in it.
func (r *Receiver) ConsumeLogsJSON(ctx context.Context, raw []byte) error {
	ld, err := (&plog.JSONUnmarshaler{}).UnmarshalLogs(raw)
	if err != nil {
		return fmt.Errorf("telemetry: decode OTLP/JSON logs: %w", err)
	}
	return r.consumeLogs(ctx, ld)
}
