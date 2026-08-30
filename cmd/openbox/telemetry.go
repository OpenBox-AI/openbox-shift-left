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
	elected := fs.Bool("elected", false, "force this lane to emit model-call turns, overriding the automatic producer election. Normally unnecessary: the election is derived from where the tool's settings route model calls")
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
	// without uninstalling it. The election is the PRODUCER's: it stops two lanes
	// emitting the same turn. Either one alone must silence emission.
	// THE PRODUCER ELECTION, resolved from where the tool's settings actually
	// route model calls rather than from a flag baked into this unit's argv.
	//
	// This lane is the only one that needs to ask. The two in-path lanes see a
	// call only if the client is routed to them, so for those the routing IS the
	// mutual exclusion; telemetry keeps receiving either way, because the tool
	// exports to it over its own channel regardless of where the call itself
	// went. Asking the same question doctor asks, through the same function, so
	// the two cannot give a developer different answers.
	//
	// RESOLVED PER RECORD, not once at startup, and that is the whole point of
	// deriving it. `openbox init --full` installs telemetry FIRST and transport
	// second, so a daemon that snapshotted the election booted correctly elected
	// and then kept emitting after the transport lane took the election from it —
	// two lanes emitting a turn for the same model call, which the disjoint
	// namespaces guarantee core will store twice rather than reject. The reverse
	// is quieter and just as wrong: remove the stronger lane and a snapshot of
	// "not elected" stays silent forever. Live resolution has nothing to keep in
	// sync, and it is also correct when the settings file changes by a hand edit
	// or an MDM deployment rather than through this CLI.
	settingsPath := gatewayservice.SettingsPath(a.homeDir())
	electedNow := func() bool {
		// The posture key is read live for the same reason. Both reads are
		// stamp-cached on the file, so this costs a stat in the common case.
		return devconfig.ResolveTelemetry() &&
			(*elected || activation.ResolveElection(settingsPath).Elected == activation.LaneTelemetry)
	}
	// Snapshots for the STARTUP LINES only — what this process reports about
	// itself when it comes up. The gate above is what decides each record.
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
		// Same reasoning as the unelected notice: a lane switched off by config
		// looks exactly like a broken one, and the remedy is completely different.
		logger.Printf("openbox telemetry: posture telemetry=false — receiving exports and recording NOTHING (config `telemetry` or %s)", devconfig.EnvTelemetry)
	}
	if !emitting {
		// Said once, loudly, at startup, and it names the WINNER. A lane that is
		// listening and recording nothing looks identical to a broken one, and an
		// automatic precedence the developer cannot see is exactly the state that
		// decision promised would always be detectable.
		logger.Printf("openbox telemetry: NOT elected — the producer is %s (%s); receiving exports but emitting no model-call turns",
			orNone(string(election.Elected)), election.Reason)
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
