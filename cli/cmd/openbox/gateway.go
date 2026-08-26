package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/openbox-ai/openbox-shift-left/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/adapters/common/hookflow"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/gatewayemit"
	"github.com/openbox-ai/openbox-shift-left/gateway"
)

// The gateway files its evidence in the Claude Code adapter's spool, under the
// session id the request carried, so the hook path's existing flushers deliver it
// — no second delivery mechanism, and the two producers' events for one session
// travel together. ADR-0021 scopes the gateway to Claude Code; a second provider
// needs its own resolution here, not a rename.
const (
	gatewaySpoolSubdir   = "cc-spool"
	gatewaySpoolProvider = "claude-code"
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
	// PROBE-A AFFORDANCE, not an org knob. Probe A has to try several refusal
	// shapes against a real Claude Code session; without these it is a recompile
	// per candidate, on a probe that already needs a human. With them the probe
	// exercises the REAL refusal path rather than a throwaway stand-in that only
	// approximates it. Once probe A names a shape, the defaults change and these
	// stay a probe tool.
	refuseAll := fs.Bool("refuse-all", false, "PROBE A ONLY: refuse every model call, to measure how Claude Code reacts to the refusal shape")
	refuseStatus := fs.Int("refusal-status", 0, "PROBE A ONLY: override the refusal status code (default: the provisional 403)")
	refuseType := fs.String("refusal-error-type", "", "PROBE A ONLY: override the refusal error type string")
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

	// TURN CAPTURE ON. Without this the relay works perfectly and reports
	// nothing: package gateway keeps its emitter optional so phase 04's
	// byte-identity suite can assert a bare relay, and for a while nothing in
	// the shipping binary opted in — so every observed exchange was discarded
	// and no gateway span ever reached core. gatewaycapture_test.go is the
	// control, and it drives this function rather than constructing a Gateway.
	//
	// An unconfigured machine relays WITHOUT capture rather than spooling events
	// nobody can attribute: with no DID the flusher cannot build a client, so
	// those events would sit on disk forever while looking like success here.
	if did := devconfig.ResolveDIDOrEmpty(); did != "" {
		spool := hookflow.Spool{Dir: devconfig.SpoolDir(gatewaySpoolSubdir)}
		trigger := hookflow.RealtimeTrigger{Spool: spool, Provider: gatewaySpoolProvider}
		logger := log.New(a.stderr, "", 0)
		g = g.WithCapture(&gatewayemit.Emitter{
			Spool:        spool,
			DeveloperDID: did,
			Warn:         func(format string, args ...any) { fmt.Fprintf(a.stderr, format+"\n", args...) },
			Flush:        func(sessionID string) { trigger.Maybe(logger, sessionID) },
		})
	} else {
		fmt.Fprintln(a.stderr, "openbox gateway: no developer DID configured, so model calls are relayed but NOT recorded. Run `openbox auth`.")
	}

	// Probe-A mode. Announced loudly, because a gateway that refuses everything
	// looks exactly like a gateway that is broken — the failure mode phase 06's own
	// security note names — and nobody should discover this state by debugging it.
	var handler http.Handler = g
	if *refuseAll {
		shape := gateway.DefaultRefusalShape()
		if *refuseStatus != 0 {
			shape.Status = *refuseStatus
		}
		if *refuseType != "" {
			shape.ErrorType = *refuseType
		}
		if err := shape.Validate(); err != nil {
			listener.Close()
			return a.errorf("%v", err)
		}
		handler = gateway.RefuseEverything(shape)
		fmt.Fprintf(a.stderr, "PROBE MODE: refusing EVERY model call with status %d, error type %q.\n", shape.Status, shape.ErrorType)
		fmt.Fprintf(a.stderr, "  Nothing is forwarded and no governance decision is consulted. This exists to\n")
		fmt.Fprintf(a.stderr, "  measure how Claude Code reacts to the refusal shape (probe A). Watch the CLIENT:\n")
		fmt.Fprintf(a.stderr, "  how many requests arrive for one prompt, what the session prints, whether it\n")
		fmt.Fprintf(a.stderr, "  disables a capability for the rest of its life, and its exit code.\n")
		fmt.Fprintf(a.stderr, "  Stop this process when you are done.\n")
	} else if *refuseStatus != 0 || *refuseType != "" {
		listener.Close()
		return a.errorf("--refusal-status/--refusal-error-type only apply with --refuse-all (they are probe-A tools, not configuration)")
	}

	srv := &http.Server{
		Handler: handler,
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

	// Stop on the signals a service manager actually sends. A test supplies its
	// own context instead, because a test binary cannot signal itself safely.
	ctx := a.gatewayCtx
	if ctx == nil {
		signalCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		ctx = signalCtx
	}
	if a.gatewayReady != nil {
		a.gatewayReady(listener.Addr())
	}

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
