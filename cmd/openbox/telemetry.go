package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/activation"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/sessionkey"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/telemetryemit"
	"github.com/openbox-ai/openbox-shift-left/internal/provider"
	"github.com/openbox-ai/openbox-shift-left/internal/telemetry"
)

const (
	telemetrySpoolSubdir   = "cc-spool"
	telemetrySpoolProvider = "claude-code"
)

// dualProviderTelemetryEmitter fans one telemetry.Record out to whichever
// provider's mapper can key it: Claude Code exports session.id, Codex exports
// conversation.id instead, so presence of the attribute is the discriminator
// -- never a reused CC-specific key -- and a record carrying both is refused
// rather than signed by either tool. codex is nil when the
// machine has no Codex identity yet; in that case a Codex-shaped record still
// reaches cc's Emit so its drop is counted and reported rather than silently
// swallowed.
type dualProviderTelemetryEmitter struct {
	cc    *telemetryemit.Emitter
	codex *telemetryemit.Emitter
}

// errAmbiguousTelemetryProvider refuses a record that names both tools'
// session keys. The receiver logs it and moves on; no client signs it.
var errAmbiguousTelemetryProvider = errors.New("record carries both session.id and conversation.id; " +
	"refusing to guess which tool's identity signs it")

func (d *dualProviderTelemetryEmitter) Emit(ctx context.Context, rec telemetry.Record) error {
	if rec.Attrs["session.id"] != "" && rec.Attrs["conversation.id"] != "" {
		return errAmbiguousTelemetryProvider
	}
	if rec.Attrs["session.id"] == "" && rec.Attrs["conversation.id"] != "" && d.codex != nil {
		return d.codex.Emit(ctx, rec)
	}
	return d.cc.Emit(ctx, rec)
}

func (d *dualProviderTelemetryEmitter) String() string {
	if d.codex == nil {
		return d.cc.String()
	}
	return fmt.Sprintf("claude-code: %s; codex: %s", d.cc, d.codex)
}

func (a *app) runTelemetry(args []string) int {
	// This bind identifies the process for anything that still reads the
	// ambient BoundProvider (the DID/coordinate resolvers a few other
	// commands share) and happens ONCE at startup, never per record. Every
	// posture question this daemon asks per tool -- ResolveTelemetryFor,
	// and ContentCaptureEnabled inside resolveProviderIdentities -- is
	// resolved through devconfig's *For accessors against THAT tool's own
	// dev.json, never against whichever tool is bound here: a shared lane
	// daemon serves every configured tool for its whole life, and a Codex
	// content_capture:false or telemetry:false must be honoured for Codex
	// while Claude Code's own posture stays whatever Claude Code's own
	// dev.json says.
	release, err := devconfig.BindProvider(telemetrySpoolProvider)
	if err != nil {
		return a.errorf("cannot resolve %s's identity store: %v", telemetrySpoolProvider, err)
	}
	defer release()

	fs := a.newFlagSet("telemetry")
	addr := fs.String("addr", telemetry.DefaultAddr, "loopback listen address (host:port)")
	grace := fs.Duration("shutdown-grace", 30*time.Second, "how long to let in-flight exports finish after a stop signal")
	verbose := fs.Bool("verbose", false, "report the outcome of every record (recorded, skipped, dropped)")
	elected := fs.Bool("elected", false, "force this lane to emit model-call turns, overriding the automatic producer election. Normally unnecessary: the election is derived from where the tool's settings route model calls")
	settings := fs.String("settings", "", "absolute path to Claude Code's settings file, written into the unit at install time. Empty falls back to deriving it from $HOME, which a daemon does not reliably have")
	codexSettings := fs.String("codex-settings", "", "absolute path to Codex's config.toml, written into the unit at install time when Codex's telemetry lane is installed")
	if code, ok := parseFlags(fs, args); !ok {
		return code
	}

	cfg := telemetry.Config{Addr: *addr}
	if err := cfg.Validate(); err != nil {
		return a.errorf("%v", err)
	}

	logger := tracedLogger(a.stderr, "", 0)

	// Pre-resolved ONCE at startup, never per record: this daemon serves every
	// configured tool for its whole life, so there is no BindProvider dance on
	// the record path at all.
	identities := resolveProviderIdentities(logger.Printf)

	// Every record now appends into and drains through that provider's own
	// session spool -- the SAME one its hook events queue through -- instead
	// of the spool-less bounded pool this lane used to send every record
	// through: a record interleaves with its session's hook events in append
	// order, and an unaccepted one halts the run exactly like any other
	// single-attempt delivery. The client that signs is
	// resolved from the event's own Tool.Name via each provider's own queue,
	// so a Codex record is never sent by the Claude Code client. Telemetry has
	// no chat completions (claude.ai is a transport/proxy concept only), so
	// routeLaneRecord is built with no chat delivery seam at all.
	queues := laneQueues(identities, logger)
	deliver := routeLaneRecord(queues, nil, logger, "telemetry")
	// doctor is a separate process with no channel into this daemon's memory,
	// so the dropped-record count it discloses has to live on disk: a small,
	// rate-limited, atomically-written status file under the OpenBox home
	// this daemon already resolves for every other sink it owns. No IPC, no
	// port. An unresolvable home degrades to no status file, not an error.
	statusPersister := hookflow.NewStatusPersister(deliveryStatusPath("telemetry"), deliveryStatusInterval)
	dropped := combinedDroppedFrom(queues)

	// The HOOKS lane's own spool is untouched by the daemons' in-process sender
	// (that only replaces the two daemons' OWN records): a hook process cannot
	// dial out, so its events still land in cc-spool/codex-spool for a
	// flusher, and this daemon keeps sweeping both -- the completeness net for
	// a session that never got a realtime nudge, or a hook process that could
	// not spawn one.
	ccHookSpool := hookflow.Spool{Dir: laneSpoolDir(telemetrySpoolSubdir, logger)}
	ccSweeper := hookflow.Sweeper{Spool: ccHookSpool, Provider: telemetrySpoolProvider}
	codexHookSpool := hookflow.Spool{Dir: laneSpoolDir("codex-spool", logger)}
	codexSweeper := hookflow.Sweeper{Spool: codexHookSpool, Provider: string(provider.Codex)}

	settingsPath := a.laneSettingsPath(*settings)
	// electedFn, not a second copy: hand-writing it is how the three diverge.
	elect := electedFn(settingsPath, activation.LaneTelemetry, elected)
	// Re-resolved on every call, like elect() itself: a dev.json edited while
	// the daemon runs takes effect on the next record, and each closure reads
	// only ITS tool's own file (telemetryPostureFor), never whichever tool
	// happened to be BindProvider'd at startup.
	ccRecording := telemetryPostureFor(string(provider.ClaudeCode), logger.Printf)
	electedNow := func() bool { return ccRecording() && elect() }
	election := activation.ResolveElection(settingsPath)
	emitting := electedNow()

	ccEmitter := &telemetryemit.Emitter{
		Mapper:  telemetryemit.New("", telemetryemit.Policy{Elected: electedNow}),
		DID:     func() string { return identities[string(provider.ClaudeCode)].DID },
		Warn:    logger.Printf,
		Deliver: deliver,
	}
	var codexEmitter *telemetryemit.Emitter
	var codexRecording func() bool
	if *codexSettings != "" {
		codexElect := codexElectedFn(*codexSettings, elected)
		codexRecording = telemetryPostureFor(string(provider.Codex), logger.Printf)
		codexElectedNow := func() bool { return codexRecording() && codexElect() }
		codexEmitter = &telemetryemit.Emitter{
			Mapper: telemetryemit.New("", telemetryemit.Policy{Elected: codexElectedNow}).
				WithSessionAttr(sessionkey.OTelAttr(sessionkey.Codex)).WithToolName(string(provider.Codex)),
			DID:     func() string { return identities[string(provider.Codex)].DID },
			Warn:    logger.Printf,
			Deliver: deliver,
		}
	}
	em := &dualProviderTelemetryEmitter{cc: ccEmitter, codex: codexEmitter}
	if *verbose {
		ccEmitter.Verbose = logger.Printf
		if codexEmitter != nil {
			codexEmitter.Verbose = logger.Printf
		}
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

	go ccSweeper.Run(ctx, logger)
	go codexSweeper.Run(ctx, logger)
	go runTraceSweeps(ctx, logger)
	go reportDeliveryStatusPeriodically(ctx, statusPersister, dropped)
	go startTokenWarmers(ctx, logger)

	if err := rec.StartStandalone(ctx); err != nil {
		return a.errorf("starting the telemetry receiver: %v", err)
	}
	logger.Printf("openbox telemetry: listening on %s", rec.Addr())
	if a.telemetryReady != nil {
		a.telemetryReady(rec.Addr())
	}
	if !ccRecording() {
		logger.Printf("openbox telemetry: claude-code's own posture telemetry=false; receiving exports and recording NOTHING for claude-code (its dev.json `telemetry` or %s)", devconfig.EnvTelemetry)
	}
	// One reporter for every lane, so an unreadable file reads the same in each.
	reportElection(logger, "telemetry", settingsPath, activation.LaneTelemetry, *elected)
	if !emitting && election.Usable() {
		logger.Printf("openbox telemetry: receiving exports but emitting no model-call turns")
	}
	if codexEmitter != nil {
		reportCodexElection(logger, *codexSettings, *elected)
		if !codexRecording() {
			logger.Printf("openbox telemetry: codex's own posture telemetry=false; receiving exports and recording NOTHING for codex (its dev.json `telemetry` or %s)", devconfig.EnvTelemetry)
		}
	}

	<-ctx.Done()

	shutdown, cancel := context.WithTimeout(context.Background(), *grace)
	defer cancel()
	// Not errors.Is: Shutdown joins every receiver's error, and errors.Is
	// matches if ANY of them is a cancellation -- which would swallow a genuine
	// failure that happened alongside one, in the shutdown an operator most
	// needs to read about.
	if err := rec.Shutdown(shutdown); err != nil && !onlyCancellation(err) {
		logger.Printf("openbox telemetry: shutdown: %v", err)
	}
	// rec.Shutdown only stops the receiver taking new exports; it says
	// nothing about a delivery already draining through a LaneQueue. Without
	// this drain, os.Exit right behind runTelemetry's return kills an
	// otherwise-healthy in-flight delivery mid-goroutine (a LaneQueue's own
	// drain instead leaves an ordinary, reclaimable orphan; see
	// LaneQueue.Close's own doc). The deadline stays at or under the
	// single-attempt bound: waiting longer cannot help a delivery that has
	// already given up on its own context.
	drainCtx, drainCancel := context.WithTimeout(context.Background(), poolDrainDeadline(*grace))
	closers := make([]shutdownCloser, 0, len(queues))
	for _, q := range queues {
		closers = append(closers, q)
	}
	abandoned := closeAllConcurrently(drainCtx, closers...)
	if abandoned > 0 {
		logger.Printf("openbox telemetry: %d delivery(ies) still in flight at shutdown; abandoned", abandoned)
	}
	drainCancel()
	// Flush, not Report: the final count matters most and must not be the one
	// a rate limit ate.
	statusPersister.Flush(dropped.Dropped())
	logger.Printf("openbox telemetry: stopped; %s; delivery pool dropped=%d", em, dropped.Dropped())
	return exitOK
}

// poolDrainDeadline bounds a shutdown-time drain (a LaneQueue.Close, or the
// transport lane's own chat DeliverPool.Close) to at most the single-attempt
// bound (DeliveryAttemptTimeout), whatever the caller's own shutdown-grace
// setting says: waiting past the point a delivery gives up on its own
// context only delays process exit for nothing.
func poolDrainDeadline(grace time.Duration) time.Duration {
	if grace <= 0 || grace > hookflow.DeliveryAttemptTimeout {
		return hookflow.DeliveryAttemptTimeout
	}
	return grace
}

// deliveryStatusInterval bounds how often a lane daemon rewrites its
// DeliverPool status file for `doctor` while it runs; a shorter interval buys
// a fresher doctor row at the cost of more disk writes on a busy machine.
const deliveryStatusInterval = 5 * time.Second

// deliveryStatusPath resolves one lane's DeliverPool status file under the
// OpenBox home, or "" when that home cannot be resolved -- which
// StatusPersister treats as "disable persistence", not an error, since this
// is a diagnostic surface for `doctor`, never load-bearing for governance.
func deliveryStatusPath(lane string) string {
	home, err := devconfig.Home()
	if err != nil {
		return ""
	}
	return hookflow.DeliverStatusPath(home, lane)
}

// deliveryPool is the subset of *hookflow.DeliverPool
// reportDeliveryStatusPeriodically needs, so a test can fake it without
// driving a real pool.
type deliveryPool interface{ Dropped() uint64 }

// reportDeliveryStatusPeriodically ticks statusPersister.Report with pool's
// current Dropped() count until ctx is done, so a saturation drop mid-session
// reaches `doctor` within one interval instead of only at shutdown.
func reportDeliveryStatusPeriodically(ctx context.Context, statusPersister *hookflow.StatusPersister, pool deliveryPool) {
	ticker := time.NewTicker(deliveryStatusInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			statusPersister.Report(pool.Dropped())
		}
	}
}

// telemetryPostureFor returns a closure resolving whether tool's OWN dev.json
// (never the ambient BoundProvider) currently allows recording, re-read on
// every call so a live edit takes effect without a restart -- the same
// freshness devconfig.ResolveTelemetry() always had, just scoped to one
// tool. DevConfigPathFor only fails when Home() itself is unresolvable, which
// every other startup path in this daemon would already have failed on; on
// the off chance it has not, this fails open (recording ON) rather than
// silently going dark, logging once per call so the condition is visible.
func telemetryPostureFor(tool string, warn func(format string, args ...any)) func() bool {
	return func() bool {
		on, err := devconfig.ResolveTelemetryFor(tool)
		if err != nil {
			if warn != nil {
				warn("openbox telemetry: could not resolve %s's telemetry posture (%v); defaulting ON (fail-open)", tool, err)
			}
			return true
		}
		return on
	}
}

// onlyCancellation reports whether err is cancellation and nothing else,
// descending through errors.Join's tree. A shutdown that was cancelled is
// routine and says nothing; a shutdown where one receiver failed is worth a
// line even if another was cancelled at the same moment.
func onlyCancellation(err error) bool {
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, e := range joined.Unwrap() {
			if !onlyCancellation(e) {
				return false
			}
		}
		return true
	}
	return errors.Is(err, context.Canceled)
}
