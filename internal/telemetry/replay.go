package telemetry

import (
	"context"
	"fmt"

	"go.opentelemetry.io/collector/pdata/plog"
)

// ConsumeLogsJSON decodes an OTLP/JSON logs payload and delivers every record
// to the emitter, through the same projection the receiver's HTTP endpoint
// uses. It exists so that a recorded provider export can be replayed against
// the shipped chain on a host that cannot bind a listener.
func (r *Receiver) ConsumeLogsJSON(ctx context.Context, raw []byte) error {
	ld, err := (&plog.JSONUnmarshaler{}).UnmarshalLogs(raw)
	if err != nil {
		return fmt.Errorf("telemetry: decode OTLP/JSON logs: %w", err)
	}
	return r.consumeLogs(ctx, ld)
}
