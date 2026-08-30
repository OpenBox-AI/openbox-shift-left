package main

import (
	"context"
	"errors"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/activation"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/gatewayservice"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/telemetryemit"
	"github.com/openbox-ai/openbox-shift-left/internal/telemetry"
)

const (
	telemetrySpoolSubdir   = "cc-spool"
	telemetrySpoolProvider = "claude-code"
)

func (a *app) runTelemetry(args []string) int {
	fs := a.newFlagSet("telemetry")
	addr := fs.String("addr", telemetry.DefaultAddr, "loopback listen address (host:port)")
	grace := fs.Duration("shutdown-grace", 10*time.Second, "how long to let in-flight exports finish after a stop signal")
	verbose := fs.Bool("verbose", false, "report the outcome of every record (recorded, skipped, dropped)")
	elected := fs.Bool("elected", false, "force this lane to emit model-call turns, overriding the automatic producer election. Normally unnecessary: the election is derived from where the tool's settings route model calls")
	if code, ok := parseFlags(fs, args); !ok {
		return code
	}

	cfg := telemetry.Config{Addr: *addr}
	if err := cfg.Validate(); err != nil {
		return a.errorf("%v", err)
	}

	logger := log.New(a.stderr, "", 0)

	spool := hookflow.Spool{Dir: devconfig.SpoolDir(telemetrySpoolSubdir)}
	trigger := hookflow.RealtimeTrigger{Spool: spool, Provider: telemetrySpoolProvider}
	// The election is the producer's: it stops two lanes emitting the same turn.
	// Either one alone must silence emission.
	settingsPath := gatewayservice.SettingsPath(a.homeDir())
	electedNow := func() bool {
		return devconfig.ResolveTelemetry() &&
			(*elected || activation.ResolveElection(settingsPath).Elected == activation.LaneTelemetry)
	}
	recording := devconfig.ResolveTelemetry()
	election := activation.ResolveElection(settingsPath)
	emitting := electedNow()

	em := &telemetryemit.Emitter{
		Spool:  spool,
		Mapper: telemetryemit.New("", telemetryemit.Policy{Elected: electedNow}),
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
		logger.Printf("openbox telemetry: posture telemetry=false; receiving exports and recording NOTHING (config `telemetry` or %s)", devconfig.EnvTelemetry)
	}
	if !emitting {
		logger.Printf("openbox telemetry: NOT elected; the producer is %s (%s); receiving exports but emitting no model-call turns",
			orNone(string(election.Elected)), election.Reason)
	}

	<-ctx.Done()

	shutdown, cancel := context.WithTimeout(context.Background(), *grace)
	defer cancel()
	if err := rec.Shutdown(shutdown); err != nil && !errors.Is(err, context.Canceled) {
		logger.Printf("openbox telemetry: shutdown: %v", err)
	}
	logger.Printf("openbox telemetry: stopped; %s", em)
	return exitOK
}
