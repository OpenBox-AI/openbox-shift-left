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

	"github.com/openbox-ai/openbox-shift-left/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/adapters/common/hookflow"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/gatewayemit"
	"github.com/openbox-ai/openbox-shift-left/gateway"
	"github.com/openbox-ai/openbox-shift-left/transport"
)

// The transport lane files its evidence in the Claude Code adapter's spool, under
// the session id the request carried, so the hook path's existing flushers deliver
// it. Same subdir and provider as the gateway and telemetry lanes, and for the
// same reason: one delivery mechanism, and every producer's events for a session
// travel together.
const (
	transportSpoolSubdir   = "cc-spool"
	transportSpoolProvider = "claude-code"
)

// runTransport serves the local in-path relay in the foreground.
//
// Foreground only, exactly like `openbox gateway` and `openbox telemetry`:
// launchd and systemd supervise a process that stays attached and logs to stdio,
// so a double-fork would take the restart guarantee away from the thing that owns
// it.
func (a *app) runTransport(args []string) int {
	fs := a.newFlagSet("transport")
	addr := fs.String("addr", transport.DefaultAddr, "loopback listen address (host:port)")
	grace := fs.Duration("shutdown-grace", 10*time.Second, "how long to let in-flight relays finish after a stop signal")
	verbose := fs.Bool("verbose", false, "report every CONNECT and the capture outcome of every relayed call")
	if code, ok := parseFlags(fs, args); !ok {
		return code
	}

	logger := log.New(a.stderr, "", 0)

	home, err := devconfig.Home()
	if err != nil {
		return a.errorf("%v", err)
	}
	// The CA is loaded (or generated) BEFORE the listener exists. A relay that is
	// listening but cannot mint a leaf would accept every CONNECT and fail every
	// handshake — which the developer sees as the provider being down.
	ca, err := transport.LoadOrCreateCA(home)
	if err != nil {
		return a.errorf("%v", err)
	}

	// TURN RECORDING ON. package transport refuses to construct without an emitter
	// precisely so this cannot be forgotten — the gateway lane shipped relaying
	// perfectly and discarding every capture because nothing in the binary opted
	// in. transportcapture_test.go is the control, and it drives THIS function.
	spool := hookflow.Spool{Dir: devconfig.SpoolDir(transportSpoolSubdir)}
	trigger := hookflow.RealtimeTrigger{Spool: spool, Provider: transportSpoolProvider}
	em := &gatewayemit.Emitter{
		// The lane is what puts this event's activity_id in the `:proxy:`
		// namespace. Without it core's dedupe would absorb these against the
		// gateway lane's events for the same turn.
		Lane:  gatewayemit.LaneProxy,
		Spool: spool,
		DID:   devconfig.ResolveDIDOrEmpty,
		Warn:  logger.Printf,
		Flush: func(sessionID string) { trigger.Maybe(logger, sessionID) },
	}
	if *verbose {
		em.Verbose = logger.Printf
	}

	p, err := transport.New(transport.Config{Addr: *addr}, ca, em)
	if err != nil {
		return a.errorf("%v", err)
	}
	if *verbose {
		p.Apply(transport.WithVerbose(logger.Printf))
	}

	listener, cfg, err := gateway.Listen(gateway.Config{Addr: *addr, Upstream: gateway.DefaultUpstream})
	if err != nil {
		return a.errorf("%v", err)
	}

	srv := &http.Server{
		Handler: p,
		// No ReadTimeout or WriteTimeout: a streamed completion legitimately runs
		// for minutes, and a CONNECT tunnel is held open for the life of the
		// client's connection. Same omission, same reason, as the gateway's server.
		ReadHeaderTimeout: 30 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if a.transportCtx != nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(ctx)
		defer context.AfterFunc(a.transportCtx, cancel)()
	}

	logger.Printf("openbox transport: listening on %s", cfg.Addr)
	logger.Printf("openbox transport: intercepting %s; every other host is tunnelled uninspected",
		strings.Join(p.Hosts(), ", "))
	// launchd sends stdio to /dev/null by default, so a line nobody is told to
	// look for is no signal at all — but a DROPPED proxy setting is exactly the
	// kind of thing a developer would otherwise spend an afternoon on.
	if cleared := p.ClearedProxyEnv(); len(cleared) > 0 {
		logger.Printf("openbox transport: cleared inherited proxy settings in this process (%s) so the "+
			"relay's own upstream leg does not dial itself", strings.Join(cleared, ", "))
	}
	// Said once, loudly, at startup: refusal is dormant, so this lane OBSERVES and
	// never stops a call. A reader who assumes otherwise would think the product
	// enforces model calls in-path, which it does not yet.
	logger.Printf("openbox transport: observe-only — model calls are recorded, never refused (probe A pending)")

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
