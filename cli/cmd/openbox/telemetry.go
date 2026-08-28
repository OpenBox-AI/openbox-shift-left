package main

import (
	"context"
	"errors"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/openbox-ai/openbox-shift-left/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/adapters/common/hookflow"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/telemetryemit"
	"github.com/openbox-ai/openbox-shift-left/telemetry"
)

// The telemetry lane files its evidence in the Claude Code adapter's spool, under
// the session id the record carried, so the hook path's existing flushers deliver
// it. Same subdir and provider as the gateway lane, and for the same reason: one
// delivery mechanism, and every producer's events for a session travel together.
const (
	telemetrySpoolSubdir   = "cc-spool"
	telemetrySpoolProvider = "claude-code"
)

// runTelemetry serves the local OTLP receiver in the foreground.
//
// Foreground only, exactly like `openbox gateway`: launchd and systemd supervise
// a process that stays attached and logs to stdio, so a double-fork would take
// the restart guarantee away from the thing that owns it.
func (a *app) runTelemetry(args []string) int {
	fs := a.newFlagSet("telemetry")
	addr := fs.String("addr", telemetry.DefaultAddr, "loopback listen address (host:port)")
	grace := fs.Duration("shutdown-grace", 10*time.Second, "how long to let in-flight exports finish after a stop signal")
	verbose := fs.Bool("verbose", false, "report the outcome of every record (recorded, skipped, dropped)")
	elected := fs.Bool("elected", false, "emit model-call turns from this lane. OFF by default: two lanes emitting the same turn doubles every token count downstream, and the producer election is phase 12's")
	if code, ok := parseFlags(fs, args); !ok {
		return code
	}

	cfg := telemetry.Config{Addr: *addr}
	if err := cfg.Validate(); err != nil {
		return a.errorf("%v", err)
	}

	logger := log.New(a.stderr, "", 0)

	// TURN RECORDING ON. Without this the receiver accepts every export and
	// discards it: package telemetry keeps its Emitter seam optional so the
	// receiver can be tested bare, and until this command existed nothing in the
	// shipping binary opted in — which is the gateway's WithCapture bug exactly,
	// one lane over. telemetrycapture_test.go is the control, and it drives THIS
	// function rather than constructing a receiver.
	spool := hookflow.Spool{Dir: devconfig.SpoolDir(telemetrySpoolSubdir)}
	trigger := hookflow.RealtimeTrigger{Spool: spool, Provider: telemetrySpoolProvider}
	// Two independent switches, and conflating them would be a bug. The posture
	// key is the ORG's: telemetry:false means this machine's lane records nothing,
	// without uninstalling it. --elected is the PRODUCER ELECTION's: it stops two
	// lanes emitting the same turn. Either one alone must silence emission.
	recording := devconfig.ResolveTelemetry()
	em := &telemetryemit.Emitter{
		Spool:  spool,
		Mapper: telemetryemit.New("", telemetryemit.Policy{Elected: *elected && recording}),
		DID:    devconfig.ResolveDIDOrEmpty,
		Warn:   logger.Printf,
		Flush:  func(sessionID string) { trigger.Maybe(logger, sessionID) },
	}
	if *verbose {
		em.Verbose = logger.Printf
	}

	rec, err := telemetry.New(cfg,
		telemetry.WithEmitter(em),
		telemetry.WithWarnFunc(logger.Printf),
	)
	if err != nil {
		return a.errorf("%v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if a.telemetryCtx != nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(ctx)
		defer context.AfterFunc(a.telemetryCtx, cancel)()
	}

	if err := rec.StartStandalone(ctx); err != nil {
		return a.errorf("starting the telemetry receiver: %v", err)
	}
	logger.Printf("openbox telemetry: listening on %s", rec.Addr())
	if a.telemetryReady != nil {
		a.telemetryReady(rec.Addr())
	}
	if !recording {
		// Same reasoning as the unelected notice: a lane switched off by config
		// looks exactly like a broken one, and the remedy is completely different.
		logger.Printf("openbox telemetry: posture telemetry=false — receiving exports and recording NOTHING (config `telemetry` or %s)", devconfig.EnvTelemetry)
	}
	if !*elected {
		// Said once, loudly, at startup. A lane that is listening and recording
		// nothing looks identical to a broken one, and this is the single most
		// likely reason for it in the field today.
		logger.Printf("openbox telemetry: NOT elected — receiving exports but emitting no model-call turns (pass --elected once the producer election is settled)")
	}

	<-ctx.Done()

	shutdown, cancel := context.WithTimeout(context.Background(), *grace)
	defer cancel()
	if err := rec.Shutdown(shutdown); err != nil && !errors.Is(err, context.Canceled) {
		logger.Printf("openbox telemetry: shutdown: %v", err)
	}
	// The last word in the log is what was RECORDED, not what was received.
	// launchd sends stdio to /dev/null unless told otherwise, so this line is the
	// only place a perfectly-reachable, silently-not-recording lane is visible.
	logger.Printf("openbox telemetry: stopped — %s", em)
	return exitOK
}
