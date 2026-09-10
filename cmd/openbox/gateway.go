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

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/activation"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/gatewayemit"
	"github.com/openbox-ai/openbox-shift-left/internal/gateway"
)

const (
	gatewaySpoolSubdir   = "cc-spool"
	gatewaySpoolProvider = "claude-code"
)

func (a *app) runGateway(args []string) int {
	fs := a.newFlagSet("gateway")
	addr := fs.String("addr", gateway.DefaultAddr, "loopback listen address (host:port)")
	upstream := fs.String("upstream", gateway.DefaultUpstream, "provider base URL to forward to")
	grace := fs.Duration("shutdown-grace", 30*time.Second, "how long to let in-flight streams drain after a stop signal")
	verbose := fs.Bool("verbose", false, "log every relayed call and whether it was recorded (no credentials, headers or bodies are printed)")
	refuseAll := fs.Bool("refuse-all", false, "PROBE A ONLY: refuse every model call, to measure how Claude Code reacts to the refusal shape")
	elected := fs.Bool("elected", false, "force this lane to emit model-call turns, overriding the automatic producer election. Normally unnecessary: the election is derived from where the tool's settings route model calls")
	settings := fs.String("settings", "", "absolute path to the governed tool's settings file, written into the unit at install time. Empty falls back to deriving it from $HOME, which a daemon does not reliably have")
	refuseStatus := fs.Int("refusal-status", 0, "PROBE A ONLY: override the refusal status code (default: the provisional 403)")
	refuseType := fs.String("refusal-error-type", "", "PROBE A ONLY: override the refusal error type string")
	if code, ok := parseFlags(fs, args); !ok {
		return code
	}

	listener, cfg, err := gateway.Listen(gateway.Config{Addr: *addr, Upstream: *upstream})
	if err != nil {
		return a.errorf("%v", err)
	}
	g, err := gateway.New(cfg)
	if err != nil {
		listener.Close()
		return a.errorf("%v", err)
	}

	// Probe-A mode replaces the handler wholesale below, so `g`; and the emitter
	// wired onto it; would never be served.
	logger := log.New(a.stderr, "", 0)
	settingsPath := a.laneSettingsPath(*settings)
	spool := hookflow.Spool{Dir: devconfig.SpoolDir(gatewaySpoolSubdir)}
	// The completeness net (see Sweeper); out here because probe-A mode still spools.
	sweeper := hookflow.Sweeper{Spool: spool, Provider: gatewaySpoolProvider}
	if !*refuseAll {
		trigger := hookflow.RealtimeTrigger{Spool: spool, Provider: gatewaySpoolProvider}
		em := &gatewayemit.Emitter{
			Lane:  gatewayemit.LaneGateway,
			Spool: spool,
			DID:   devconfig.ResolveDIDOrEmpty,
			Warn:  logger.Printf,
			Flush: func(sessionID string) { trigger.Maybe(logger, sessionID) },
			// See transport.go: derived per record, not cached at startup.
			Elected: electedFn(settingsPath, activation.LaneGateway, elected),
			// Not derivable from Elected(): a false there means either "another lane
			// won" or "nothing could be read", and only the second is a defect.
			ElectionProblem: electionProblemFn(settingsPath, elected),
			// Which lane, not just whether this one: "nobody" is a routing gap.
			ElectedName: electedNameFn(settingsPath, activation.LaneGateway, elected),
		}
		if *verbose {
			em.Verbose = logger.Printf
		}
		g = g.WithCapture(em)
		// ~40% of relayed POSTs are probes, whose bodies CarriesContent() discards.
		g = g.WithBodyCapture(func(r *http.Request) bool {
			return gatewayemit.CapturesBody(r.URL.Path)
		})
		// The parser lives in gatewayemit, which the gateway's import guard
		// excludes; injected as a closure for the same reason as the path
		// taxonomy above.
		g = g.WithRequestAttribution(gatewayemit.ParseRequestAttribution)
	}
	if *verbose {
		g = g.WithVerbose(logger.Printf)
	}

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
		fmt.Fprintf(a.stderr, "  CAPTURE IS ALSO OFF in this mode: nothing reaches the relay, so nothing is\n")
		fmt.Fprintf(a.stderr, "  recorded. A probe run leaves no governance events behind.\n")
		fmt.Fprintf(a.stderr, "  Stop this process when you are done.\n")
	} else if *refuseStatus != 0 || *refuseType != "" {
		listener.Close()
		return a.errorf("--refusal-status/--refusal-error-type only apply with --refuse-all (they are probe-A tools, not configuration)")
	}

	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	fmt.Fprintf(a.stdout, "openbox gateway listening on %s, forwarding to %s\n", listener.Addr(), cfg.Upstream)
	fmt.Fprintln(a.stdout, "pass-through auth: this process resolves, stores and injects no credential")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if a.gatewayCtx != nil {
		merged, cancel := context.WithCancel(ctx)
		defer cancel()
		defer context.AfterFunc(a.gatewayCtx, cancel)()
		ctx = merged
	}
	if a.gatewayReady != nil {
		a.gatewayReady(listener.Addr())
	}
	go sweeper.Run(ctx, logger)
	if !*refuseAll {
		reportElection(logger, "gateway", settingsPath, activation.LaneGateway, *elected)
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
		shutdownCtx, cancel := context.WithTimeout(context.Background(), *grace)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			fmt.Fprintf(a.stderr, "warning: shutdown did not finish draining in time: %v\n", err)
		}
		return exitOK
	}
}
