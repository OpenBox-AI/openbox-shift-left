package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/activation"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/gatewayemit"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/providers"
	"github.com/openbox-ai/openbox-shift-left/internal/client"
	"github.com/openbox-ai/openbox-shift-left/internal/gateway"
	"github.com/openbox-ai/openbox-shift-left/internal/provider"
	"github.com/openbox-ai/openbox-shift-left/internal/transport"
)

const (
	transportSpoolSubdir   = "cc-spool"
	transportSpoolProvider = "claude-code"
)

func (a *app) runTransport(args []string) int {
	// A lane daemon acts for exactly one tool for its whole life, and takes it
	// from a compile-time constant rather than argv: this is the one bind site
	// the environment cannot redirect. A daemon that cannot resolve its own
	// identity must not come up half-governing.
	release, err := devconfig.BindProvider(transportSpoolProvider)
	if err != nil {
		return a.errorf("cannot resolve %s's identity store: %v", transportSpoolProvider, err)
	}
	defer release()

	fs := a.newFlagSet("transport")
	addr := fs.String("addr", transport.DefaultAddr, "loopback listen address (host:port)")
	grace := fs.Duration("shutdown-grace", 10*time.Second, "how long to let in-flight relays finish after a stop signal")
	verbose := fs.Bool("verbose", false, "report every CONNECT and the capture outcome of every relayed call")
	elected := fs.Bool("elected", false, "force this lane to emit model-call turns, overriding the automatic producer election. Normally unnecessary: the election is derived from where the tool's settings route model calls")
	settings := fs.String("settings", "", "absolute path to the governed tool's settings file, written into the unit at install time. Empty falls back to deriving it from $HOME, which a daemon does not reliably have")
	if code, ok := parseFlags(fs, args); !ok {
		return code
	}

	logger := log.New(a.stderr, "", 0)

	home, err := devconfig.Home()
	if err != nil {
		return a.errorf("%v", err)
	}
	ca, err := transport.LoadOrCreateCA(home)
	if err != nil {
		return a.errorf("%v", err)
	}

	// The HOOKS lane's own spool is untouched by the daemons' in-process
	// sender (that only replaces the two daemons' OWN records): a hook
	// process cannot dial out, so its events still land in cc-spool/codex-spool
	// for a flusher, and this daemon keeps sweeping both -- the completeness
	// net for a session that never got a realtime nudge.
	ccHookSpool := hookflow.Spool{Dir: devconfig.SpoolDir(transportSpoolSubdir)}
	ccSweeper := hookflow.Sweeper{Spool: ccHookSpool, Provider: transportSpoolProvider}
	codexHookSpool := hookflow.Spool{Dir: providers.CodexSpoolDir()}
	codexSweeper := hookflow.Sweeper{Spool: codexHookSpool, Provider: string(provider.Codex)}

	// Pre-resolved ONCE at startup, never per record. The transport/proxy lane
	// has only one producer today -- Codex's proxy arm is the system PAC, not
	// this relay -- but the shape is shared with telemetry.go for when a
	// second provider lands on this lane too.
	identities := resolveProviderIdentities(logger.Printf)
	advisory := &hookflow.Advisory{Path: hookflow.DefaultAdvisoryPath(), Log: logger}
	pool := hookflow.NewDeliverPool(hookflow.DefaultPoolSize, hookflow.DefaultPoolTimeout,
		func(ctx context.Context, ev client.DevEvent) {
			id, ok := identities[ev.Tool.Name]
			if !ok {
				logger.Printf("openbox transport: no resolved identity for %q; dropping event %s", ev.Tool.Name, ev.EventID)
				return
			}
			if _, err := hookflow.Deliver(ctx, id.Client, advisory, ev, logger); err != nil {
				logger.Printf("openbox transport: delivery failed for %s: %v", ev.EventID, err)
			}
		})
	// doctor is a separate process with no channel into this daemon's memory,
	// so the dropped-record count it discloses has to live on disk: a small,
	// rate-limited, atomically-written status file under the OpenBox home
	// this daemon already resolves for every other sink it owns. No IPC, no
	// port. An unresolvable home degrades to no status file, not an error.
	statusPersister := hookflow.NewStatusPersister(deliveryStatusPath("transport"), deliveryStatusInterval)

	settingsPath := a.laneSettingsPath(*settings)
	em := &gatewayemit.Emitter{
		Lane: gatewayemit.LaneProxy,
		// In-process, bounded delivery: no Spool.Append, no
		// RealtimeTrigger.Maybe, no spawned flusher for this lane's OWN
		// records.
		Deliver: func(ctx context.Context, ev client.DevEvent) bool {
			return pool.Submit(ev)
		},
		DID:  devconfig.ResolveDIDOrEmpty,
		Warn: logger.Printf,
		// Re-resolved PER RECORD, never cached: see electedFn.
		Elected: electedFn(settingsPath, activation.LaneTransport, elected),
		// Not derivable from Elected(): a false there means either "another lane
		// won" or "nothing could be read", and only the second is a defect.
		ElectionProblem: electionProblemFn(settingsPath, elected),
		// Which lane, not just whether this one: "nobody" is a routing gap.
		ElectedName: electedNameFn(settingsPath, activation.LaneTransport, elected),
	}
	if *verbose {
		em.Verbose = logger.Printf
	}

	// The taxonomy lives in gatewayemit, which transport's import guard excludes.
	opts := []transport.Option{
		transport.WithBodyCapture(func(r *http.Request) bool {
			return gatewayemit.CapturesBody(r.URL.Path)
		}),
		// Same reason: the attribution parser lives in gatewayemit too.
		transport.WithRequestAttribution(gatewayemit.ParseRequestAttribution),
	}
	if *verbose {
		opts = append(opts, transport.WithVerbose(logger.Printf))
	}
	p, err := transport.New(transport.Config{Addr: *addr}, ca, em, opts...)
	if err != nil {
		return a.errorf("%v", err)
	}

	listener, cfg, err := gateway.Listen(gateway.Config{Addr: *addr, Upstream: gateway.DefaultUpstream})
	if err != nil {
		return a.errorf("%v", err)
	}

	srv := &http.Server{
		Handler:           p,
		ReadHeaderTimeout: 30 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if a.transportCtx != nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(ctx)
		defer context.AfterFunc(a.transportCtx, cancel)()
	}

	go ccSweeper.Run(ctx, logger)
	go codexSweeper.Run(ctx, logger)
	go reportDeliveryStatusPeriodically(ctx, statusPersister, pool)

	logger.Printf("openbox transport: listening on %s", cfg.Addr)
	logger.Printf("openbox transport: intercepting %s; every other host is tunnelled uninspected",
		strings.Join(p.Hosts(), ", "))
	if cleared := p.ClearedProxyEnv(); len(cleared) > 0 {
		logger.Printf("openbox transport: cleared inherited proxy settings in this process (%s) so the "+
			"relay's own upstream leg does not dial itself", strings.Join(cleared, ", "))
	}
	logger.Printf("openbox transport: observe-only; model calls are recorded, never refused (probe A pending)")
	reportElection(logger, "transport", settingsPath, activation.LaneTransport, *elected)

	if a.transportReady != nil {
		a.transportReady(cfg.Addr)
	}

	errc := make(chan error, 1)
	go func() {
		err := srv.Serve(listener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		errc <- err
	}()

	select {
	case err := <-errc:
		if err != nil {
			return a.errorf("openbox transport: %v", err)
		}
	case <-ctx.Done():
	}

	shutdown, cancel := context.WithTimeout(context.Background(), *grace)
	defer cancel()
	if err := srv.Shutdown(shutdown); err != nil && !errors.Is(err, context.Canceled) {
		logger.Printf("openbox transport: shutdown: %v", err)
	}
	// srv.Shutdown only stops the relay taking new connections; it says
	// nothing about a delivery Submit already accepted into the pool. Without
	// this drain, os.Exit right behind runTransport's return kills an
	// otherwise-healthy in-flight delivery mid-goroutine, uncounted.
	drainCtx, drainCancel := context.WithTimeout(context.Background(), poolDrainDeadline(*grace))
	if abandoned := pool.Close(drainCtx); abandoned > 0 {
		logger.Printf("openbox transport: %d delivery(ies) still in flight at shutdown; abandoned", abandoned)
	}
	drainCancel()
	// Flush, not Report: the final count matters most and must not be the one
	// a rate limit ate.
	statusPersister.Flush(pool.Dropped())
	logger.Printf("openbox transport: stopped; delivery pool dropped=%d", pool.Dropped())
	return exitOK
}
