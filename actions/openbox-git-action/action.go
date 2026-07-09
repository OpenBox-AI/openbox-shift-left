package gitaction

import (
	"context"
	"time"

	"github.com/openbox-ai/openbox-shift-left/client"
)

// Emitter is the transport the action uses to send the Deploy event. The
// SL-3 *client.Client satisfies it directly (same Emit signature); tests inject
// a fake so resolution/build can be exercised without a network.
type Emitter interface {
	Emit(ctx context.Context, ev client.DevEvent) (client.Verdict, error)
}

// Logger is the minimal diagnostics sink (INV-1/INV-2: ids/types/errors only,
// never secrets or content). A nil Logger discards.
type Logger interface {
	Printf(format string, args ...any)
}

type nopLogger struct{}

func (nopLogger) Printf(string, ...any) {}

// Action wires the server-side resolver to the SL-3 emitter. Construct it with
// a Resolver, an Emitter (or nil for dry-run/observe), and the deploy context.
type Action struct {
	Resolver *Resolver
	Emitter  Emitter // nil => resolve + build only (no emit)
	Meta     DeployMeta
	Now      func() time.Time // nil => time.Now
	Log      Logger           // nil => discard
}

// Result is the outcome of a single run — returned for logging and tests
// regardless of whether the emit succeeded (INV-3 fail-open).
type Result struct {
	Resolution Resolution
	Event      client.DevEvent
	Verdict    client.Verdict
	Emitted    bool // false in dry-run / when Emitter is nil
}

func (a *Action) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func (a *Action) log() Logger {
	if a.Log != nil {
		return a.Log
	}
	return nopLogger{}
}

// Run resolves the pushed commit, builds the Deploy event, and (unless Emitter
// is nil) emits it. Emission is FAIL-OPEN (INV-3): a transport failure is
// logged and the Result still carries the full Resolution + Event so CI never
// breaks over governance telemetry. A resolution failure (bad SHA) IS returned
// — it is a precondition the operator must fix, not a droppable telemetry loss.
func (a *Action) Run(ctx context.Context, target, base string) (Result, error) {
	res, err := a.Resolver.Resolve(ctx, target, base)
	if err != nil {
		return Result{}, err
	}
	ev := BuildDeployEvent(res, a.Meta, a.now())

	a.log().Printf("openbox-git-action: %s %s status=%s sessions=%d%s",
		short(res.CommitSHA), ev.Metadata["deploy_id"], res.Status, len(res.Sessions), reasonSuffix(res))

	out := Result{Resolution: res, Event: ev}
	if a.Emitter == nil {
		return out, nil
	}
	verdict, emitErr := a.Emitter.Emit(ctx, ev)
	if emitErr != nil {
		// Emit only returns a non-nil error for an unbuildable event (a caller
		// precondition), never a transport failure — but stay fail-open here so
		// the git action can never break a deploy over telemetry.
		a.log().Printf("openbox-git-action: emit dropped for %v: %v", ev.EventID, emitErr)
		return out, nil
	}
	out.Verdict = verdict
	out.Emitted = true
	return out, nil
}

func reasonSuffix(res Resolution) string {
	if res.Reason == "" {
		return ""
	}
	return " reason=" + string(res.Reason)
}
