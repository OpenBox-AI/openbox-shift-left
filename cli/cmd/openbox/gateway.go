package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/openbox-ai/openbox-shift-left/gateway"
)

// runGateway serves the local OpenBox gateway in the foreground.
//
// Foreground is the only mode this command has: phase 07 wraps it in launchd or
// systemd, and those supervise a process that stays attached and logs to stdio.
// So there is no daemonize flag, no pidfile and no TTY assumption anywhere here —
// a double-fork would take the restart guarantee away from the thing that owns it.
func (a *app) runGateway(args []string) int {
	fs := a.newFlagSet("gateway")
	addr := fs.String("addr", gateway.DefaultAddr, "loopback listen address (host:port)")
	upstream := fs.String("upstream", gateway.DefaultUpstream, "provider base URL to forward to")
	grace := fs.Duration("shutdown-grace", 30*time.Second, "how long to let in-flight streams drain after a stop signal")
	if code, ok := parseFlags(fs, args); !ok {
		return code
	}

	// Listen validates, binds, and re-checks the address the kernel returned --
	// the last of those being the only check a resolver cannot talk its way past.
	// It hands back the validated config so nothing here validates a second time,
	// which for a hostname would mean resolving again.
	listener, cfg, err := gateway.Listen(gateway.Config{Addr: *addr, Upstream: *upstream})
	if err != nil {
		return a.errorf("%v", err)
	}
	g, err := gateway.New(cfg)
	if err != nil {
		listener.Close()
		return a.errorf("%v", err)
	}

	srv := &http.Server{
		Handler: g,
		// A header deadline only. There is deliberately no ReadTimeout or
		// WriteTimeout: a streamed completion legitimately runs for minutes and
		// either one would abort it mid-stream — the exact failure the relay is
		// written to avoid.
		ReadHeaderTimeout: 10 * time.Second,
	}

	// The BOUND address, not the configured one: "--addr 127.0.0.1:0" asks the OS
	// to pick, and printing the request back would name a port nothing listens on.
	fmt.Fprintf(a.stdout, "openbox gateway listening on %s, forwarding to %s\n", listener.Addr(), cfg.Upstream)
	fmt.Fprintln(a.stdout, "pass-through auth: this process resolves, stores and injects no credential")

	// Stop on the signals a service manager actually sends.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(listener) }()

	select {
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return a.errorf("gateway stopped: %v", err)
		}
		return exitOK
	case <-ctx.Done():
		fmt.Fprintln(a.stdout, "shutting down")
		// Bounded, so a stuck stream cannot hold a restart back forever -- but
		// Shutdown never force-closes an ACTIVE connection, so whatever is still
		// streaming when this expires gets cut when the process exits. That is
		// the same mid-stream abort the timeouts above are written to avoid,
		// reached by the shutdown path, which is why the window is generous and
		// configurable rather than a fixed 10s.
		//
		// It has to be coordinated with the supervisor, not just chosen: exceeding
		// the service manager's own stop timeout buys nothing, because it SIGKILLs
		// first (launchd defaults to 20s, systemd to 90s). Phase 07's unit should
		// set both ends to the same number.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), *grace)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			// Draining ran out of grace, which is expected when a completion is
			// mid-stream -- and this stop was ASKED FOR, by a Ctrl-C or by the
			// service manager. Exiting non-zero here would tell a supervisor
			// with restart-on-failure that a routine restart was a crash, so it
			// is reported and the exit stays clean.
			fmt.Fprintf(a.stderr, "warning: shutdown did not finish draining in time: %v\n", err)
		}
		return exitOK
	}
}
