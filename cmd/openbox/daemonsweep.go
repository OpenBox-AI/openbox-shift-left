package main

import (
	"context"
	"io"
	"log"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/cli/sessionkey"
	"github.com/openbox-ai/openbox-shift-left/internal/gateway"
	"github.com/openbox-ai/openbox-shift-left/internal/trace"
	"github.com/openbox-ai/openbox-shift-left/internal/transport"
)

// traceRawCapture is the gateway.WithRawObserver callback every relay lane
// (gateway.go, transport.go) wires: it is the one place a RAW,
// pre-redaction request/response body reaches the local trace, satisfying
// the rule that the local trace keeps every body regardless of
// content_capture -- this is a LOCAL
// record, never egressed, so the redaction the client-facing Emitter still
// applies is untouched.
func traceRawCapture(c gateway.RawCapture) {
	session, run := rawSessionKey(c)
	trace.Emit(trace.Record{
		Stage:     trace.StageCapture,
		Outcome:   "raw",
		SessionID: session,
		RunID:     run,
		Detail: map[string]any{
			"request_id":       c.ResponseHeaders[upstreamRequestIDHeader],
			"method":           c.Method,
			"url":              c.URL,
			"status":           c.Status,
			"request_headers":  trace.JSON(c.RequestHeaders),
			"response_headers": trace.JSON(c.ResponseHeaders),
			"request_body":     trace.Body(c.RequestBody),
			"response_body":    trace.Body(c.ResponseBody),
		},
	})
}

// upstreamRequestIDHeader is the upstream's own id for a relayed call, the
// same one the lane emitters build a call's activity id from.
const upstreamRequestIDHeader = "Request-Id"

// rawSessionKey names the session a raw capture belongs to, and the run it
// belongs to now, by the same resolution the halt latch uses
// (haltDecorator.haltedRun): the provider's carrier header and that
// session's current run for a tool session, else a claude.ai conversation's
// key, which is both (a chat has no runs). "", "" when neither resolves,
// which leaves the record findable only by time.
func rawSessionKey(c gateway.RawCapture) (session, run string) {
	host, path := hostOf(c.URL), pathOf(c.URL)
	if providerName, ok := transport.ProviderForHost(host); ok {
		if sessionID, ok := sessionkey.ResolveProxy(sessionkey.Provider(providerName), c.RequestHeaders); ok {
			return sessionID, haltDecoratorRunID(sessionID)
		}
	}
	if key, ok := sessionkey.ResolveChat(host, path); ok {
		return key, key
	}
	return "", ""
}

// traceSweepInterval is every lane daemon's own periodic housekeeping tick:
// gzip yesterday's plain trace file, prune anything past trace.RetainDays.
// A hook process never sweeps (its own 30s budget is too tight for I/O this
// package does not need on the hot path); a long-lived daemon is the one
// place a ticker like this is cheap and correct.
const traceSweepInterval = time.Hour

// runTraceSweeps sweeps once at startup, then on the hour, for the life of
// ctx. Errors are logged and swallowed -- background hygiene, never a reason
// to stop serving a lane -- and a trace directory that was never configured
// (trace.Dir() == "") sweeps a Writer whose Emit already no-ops, so this is
// safe to start unconditionally.
func runTraceSweeps(ctx context.Context, logger *log.Logger) {
	sweep := func() {
		w := &trace.Writer{Dir: trace.Dir()}
		if err := w.Sweep(); err != nil {
			logger.Printf("openbox: trace sweep: %v", err)
		}
	}
	sweep()
	ticker := time.NewTicker(traceSweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sweep()
		}
	}
}

// traceLogTee wraps a daemon's own log writer so every line it writes ALSO
// becomes a trace.StageLog record -- every existing logger.Printf call site
// is traced for free, with no change at the call site. w is still written
// exactly as before; tracing is additive and, like every trace.Emit, never
// blocks the caller on I/O failure.
type traceLogTee struct {
	w io.Writer
}

func (t traceLogTee) Write(p []byte) (int, error) {
	trace.Emit(trace.Record{
		Stage:  trace.StageLog,
		Detail: map[string]any{"line": string(p)},
	})
	return t.w.Write(p)
}

// tracedLogger is the one place a lane daemon builds its *log.Logger, so
// every daemon's warnings and diagnostics land in the trace the same way.
func tracedLogger(w io.Writer, prefix string, flag int) *log.Logger {
	return log.New(traceLogTee{w: w}, prefix, flag)
}
