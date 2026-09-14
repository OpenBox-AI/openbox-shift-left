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
	"github.com/openbox-ai/openbox-shift-left/internal/gateway"
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

	spool := hookflow.Spool{Dir: devconfig.SpoolDir(transportSpoolSubdir)}
	trigger := hookflow.RealtimeTrigger{Spool: spool, Provider: transportSpoolProvider}
	// The completeness net (see Sweeper): for files whose session is over.
	sweeper := hookflow.Sweeper{Spool: spool, Provider: transportSpoolProvider}
	settingsPath := a.laneSettingsPath(*settings)
	em := &gatewayemit.Emitter{
		Lane:  gatewayemit.LaneProxy,
		Spool: spool,
		DID:   devconfig.ResolveDIDOrEmpty,
		Warn:  logger.Printf,
		Flush: func(sessionID string) { trigger.Maybe(logger, sessionID) },
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

	go sweeper.Run(ctx, logger)

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
	logger.Printf("openbox transport: stopped")
	return exitOK
}
