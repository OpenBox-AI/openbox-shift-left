package gitaction

import (
	"context"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/client"
)

// Emitter is the transport the action uses to send the Deploy event.
// *client.Client satisfies it directly (same Emit signature); tests inject a
// fake so resolution/build can be exercised without a network. It returns the
// rich Evaluation; the action records it, never acts on it.
type Emitter interface {
	Emit(ctx context.Context, ev client.DevEvent) (client.Evaluation, error)
}

// Logger is the minimal diagnostics sink (INV-1/INV-2: ids/types/errors only,
// never secrets or content).
type Logger interface {
	Printf(format string, args ...any)
}

type nopLogger struct{}

func (nopLogger) Printf(string, ...any) {}

// Action wires the server-side resolver to the emitter.
type Action struct {
	Resolver *Resolver
	Emitter  Emitter // nil => resolve + build only (no emit)
	Meta     DeployMeta
	Now      func() time.Time // nil => time.Now
	Log      Logger           // nil => discard
	// Advisory records the Advisory-tier verdict/guardrail signals for the Deploy
	// event. Nil ⇒ default sink (DefaultAdvisoryPath). Record-only: it never
	// gates the deploy (INV-3).
	Advisory *Advisory
}

// Result is the outcome of a single run; returned for logging and tests
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
// is nil) emits it. Emission is fail-open (INV-3): a transport failure is
// logged and the Result still carries the full Resolution + Event so CI never
// breaks over governance telemetry.
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
	eval, emitErr := a.Emitter.Emit(ctx, ev)
	if emitErr != nil {
		a.log().Printf("openbox-git-action: emit dropped for %v: %v", ev.EventID, emitErr)
		return out, nil
	}
	out.Verdict = eval.Verdict
	out.Emitted = true

	// Record-only; never gates the deploy (INV-3).
	a.advisory().Record(ev, eval)
	return out, nil
}

func (a *Action) advisory() *Advisory {
	if a.Advisory != nil {
		return a.Advisory
	}
	return &Advisory{Log: a.Log}
}

func reasonSuffix(res Resolution) string {
	if res.Reason == "" {
		return ""
	}
	return " reason=" + string(res.Reason)
}
