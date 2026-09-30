package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	obgit "github.com/openbox-ai/openbox-shift-left/internal/adapters/common/git"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/activation"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/gatewayemit"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/sessionkey"
	"github.com/openbox-ai/openbox-shift-left/internal/client"
	"github.com/openbox-ai/openbox-shift-left/internal/gateway"
	"github.com/openbox-ai/openbox-shift-left/internal/provider"
	"github.com/openbox-ai/openbox-shift-left/internal/trace"
	"github.com/openbox-ai/openbox-shift-left/internal/transport"
)

const transportSpoolSubdir = "cc-spool"

func (a *app) runTransport(args []string) int {
	// Unlike the hook binaries, this daemon is not bound to one tool's identity
	// store: it serves every provider that has a usable identity for its whole
	// life (resolveProviderIdentities below), so it comes up on a machine that
	// has only Codex's, or only Claude Code's. Nothing on its record path reads
	// an ambient bound provider.
	fs := a.newFlagSet("transport")
	addr := fs.String("addr", transport.DefaultAddr, "loopback listen address (host:port)")
	grace := fs.Duration("shutdown-grace", 30*time.Second, "how long to let in-flight relays finish after a stop signal")
	verbose := fs.Bool("verbose", false, "report every CONNECT and the capture outcome of every relayed call")
	elected := fs.Bool("elected", false, "force this lane to emit model-call turns, overriding the automatic producer election. Normally unnecessary: the election is derived from where the tool's settings route model calls")
	settings := fs.String("settings", "", "absolute path to the governed tool's settings file, written into the unit at install time. Empty falls back to deriving it from $HOME, which a daemon does not reliably have")
	codexSettings := fs.String("codex-settings", "", "absolute path to Codex's config.toml, written into the unit at install time when Codex is among the providers. Codex's election reads its model host from it")
	pacRecord := fs.String("pac-record", "", "absolute path to the activation record that holds the system-PAC entry, written into the unit at install time. Codex's election reads it to tell whether the relay is routed for Codex")
	providersFlag := fs.String("providers", "", "comma-separated provider names whose host-table union this lane intercepts and PACs. Absent keeps transport.Config's own default (today's claude-code-only lane); passed with an empty value intercepts nothing, for a machine with every provider uninstalled")
	if code, ok := parseFlags(fs, args); !ok {
		return code
	}
	// fs.Visit only visits a flag that appeared on the command line, which is
	// what tells "absent" (nil Providers, the daemon's own default) apart
	// from "--providers ''" (an explicitly empty union): *providersFlag alone
	// cannot distinguish the two, since both leave it at "".
	providersSeen := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "providers" {
			providersSeen = true
		}
	})

	logger := tracedLogger(a.stderr, "", 0)

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
	ccHookSpool := hookflow.Spool{Dir: laneSpoolDir(transportSpoolSubdir, logger)}
	ccSweeper := hookflow.Sweeper{Spool: ccHookSpool, Provider: string(provider.ClaudeCode)}
	codexHookSpool := hookflow.Spool{Dir: laneSpoolDir("codex-spool", logger)}
	codexSweeper := hookflow.Sweeper{Spool: codexHookSpool, Provider: string(provider.Codex)}
	museHookSpool := hookflow.Spool{Dir: laneSpoolDir("muse-spool", logger)}
	museSweeper := hookflow.Sweeper{Spool: museHookSpool, Provider: string(provider.Muse)}

	// Pre-resolved ONCE at startup, never per record; the shape is shared with
	// telemetry.go. Any non-empty subset of providers may resolve.
	identities := resolveProviderIdentities(logger.Printf)
	advisory := &hookflow.Advisory{Path: hookflow.DefaultAdvisoryPath(), Log: logger}

	// Every Claude Code/Codex lane record of this daemon's own now appends
	// into and drains through that provider's own session spool -- the SAME
	// one its hook events queue through -- instead of the spool-less bounded
	// pool this lane used to send every record through: a lane record now
	// interleaves with its session's hook events in append order, and an
	// unaccepted one halts the run exactly like any other single-attempt
	// delivery.
	queues := laneQueues(identities, logger)

	// A claude.ai chat completion has no tool session at all (no hook will
	// ever fire for one, so it has no session spool to append into), and
	// keeps the earlier in-process, spool-less pool -- signed by the
	// claude-code identity, the one that governs Anthropic's hosts on this
	// machine.
	chatPool, chatDeliver := newChatPool(identities, advisory, logger)

	// doctor is a separate process with no channel into this daemon's memory,
	// so the dropped-record count it discloses has to live on disk: a small,
	// rate-limited, atomically-written status file under the OpenBox home
	// this daemon already resolves for every other sink it owns. No IPC, no
	// port. An unresolvable home degrades to no status file, not an error.
	statusPersister := hookflow.NewStatusPersister(deliveryStatusPath("transport"), deliveryStatusInterval)
	dropped := combinedDroppedFrom(queues, chatPool)

	settingsPath := a.laneSettingsPath(*settings)
	codexPaths := newCodexElectionPaths(*codexSettings, *pacRecord, logger)
	em := &gatewayemit.Emitter{
		Lane: gatewayemit.LaneProxy,
		// A claude.ai chat completion (sessionkey.IsChatKey) keeps the
		// in-process pool; every other record appends into and drains
		// through its own provider's LaneQueue.
		Deliver: routeLaneRecord(queues, chatDeliver, logger, "transport"),
		Warn:    logger.Printf,
		// A call is attributed to one provider by host and carrier header; see
		// relayProviderLanes for what each provider's entry is.
		CandidatesForHost: transport.CandidatesForHost,
		Providers:         relayProviderLanes(identities, settingsPath, codexPaths, elected),
	}
	if *verbose {
		em.Verbose = logger.Printf
	}

	// The taxonomy lives in gatewayemit, which transport's import guard excludes.
	opts := []transport.Option{
		transport.WithBodyCapture(relayCapturesBody),
		// Same reason: the attribution parser lives in gatewayemit too.
		transport.WithRequestAttribution(gatewayemit.ParseRequestAttribution),
		// The cross-lane HALT latch: a run any lane latched halted refuses
		// every relayed POST for that run before it reaches the provider, with
		// no /evaluate round trip. gatedFn is deliberately every POST, not only
		// a completion: a halted session must not keep making ANY relayed
		// call. haltDecorator's own next stays nil for now, left ready for a
		// real /evaluate evaluator to chain in behind it -- until then an
		// unlatched call is allowed here exactly as it always was.
		transport.WithGate(haltDecorator{logger: logger, onCodexCall: codexCallObserver(codexPaths, *codexSettings, logger)}, gatedFn),
	}
	if *verbose {
		opts = append(opts, transport.WithVerbose(logger.Printf))
	}
	opts = append(opts, transport.WithRawObserver(traceRawCapture))
	p, err := transport.New(transport.Config{Addr: *addr, Providers: providersFromFlag(providersSeen, *providersFlag)}, ca, em, opts...)
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
	go museSweeper.Run(ctx, logger)
	go runTraceSweeps(ctx, logger)
	go reportDeliveryStatusPeriodically(ctx, statusPersister, dropped)
	go startTokenWarmers(ctx, logger)

	logger.Printf("openbox transport: listening on %s", cfg.Addr)
	logger.Printf("openbox transport: intercepting %s; every other host is tunnelled uninspected",
		strings.Join(p.Hosts(), ", "))
	if cleared := p.ClearedProxyEnv(); len(cleared) > 0 {
		logger.Printf("openbox transport: cleared inherited proxy settings in this process (%s) so the "+
			"relay's own upstream leg does not dial itself", strings.Join(cleared, ", "))
	}
	logger.Printf("openbox transport: model calls are recorded; a relayed call for a run any lane " +
		"latched HALTed is refused locally (no /evaluate round trip); a server-side verdict does not " +
		"yet refuse a call synchronously")
	reportElection(logger, "transport", settingsPath, activation.LaneTransport, *elected)
	if *codexSettings != "" {
		reportCodexElection(logger, codexPaths, activation.LaneTransport, *elected)
	}

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
	// nothing about a delivery already accepted into the chat pool or already
	// draining through a LaneQueue. Without this drain, os.Exit right behind
	// runTransport's return kills an otherwise-healthy in-flight delivery
	// mid-goroutine, uncounted (a LaneQueue's own drain instead leaves an
	// ordinary, reclaimable orphan; see LaneQueue.Close's own doc).
	drainCtx, drainCancel := context.WithTimeout(context.Background(), poolDrainDeadline(*grace))
	closers := make([]shutdownCloser, 0, 1+len(queues))
	closers = append(closers, chatPool)
	for _, q := range queues {
		closers = append(closers, q)
	}
	abandoned := closeAllConcurrently(drainCtx, closers...)
	if abandoned > 0 {
		logger.Printf("openbox transport: %d delivery(ies) still in flight at shutdown; abandoned", abandoned)
	}
	drainCancel()
	// Flush, not Report: the final count matters most and must not be the one
	// a rate limit ate.
	statusPersister.Flush(dropped.Dropped())
	logger.Printf("openbox transport: stopped; delivery pool dropped=%d", dropped.Dropped())
	return exitOK
}

// relayProviderLanes is the relay emitter's per-provider table: each
// provider's identity (from the daemon's own startup resolution, so a
// provider with none yet answers "" and is reported by name) and its
// election. Muse has no entry: it has no session carrier, so no call is ever
// attributed to it.
//
// claude-code's election is derived from its settings env block, re-resolved
// per record. Codex's is derived from its config.toml and the system-PAC
// record (activation.ResolveCodexElection), also re-resolved per record: the
// relay is its producer once the PAC is committed and the relay has seen a
// Codex call, and until then the telemetry receiver is, so this lane records
// nothing for it. A unit that carries no Codex config path (no Codex in this
// relay's providers) has no proxy arm at all and always defers to telemetry.
func relayProviderLanes(identities map[string]providerIdentity, settingsPath string, codex codexElectionPaths, override *bool) map[string]gatewayemit.ProviderLane {
	didOf := func(tool string) func() string {
		return func() string { return identities[tool].DID }
	}
	codexLane := gatewayemit.ProviderLane{
		DID:         didOf(string(provider.Codex)),
		Elected:     func() bool { return false },
		ElectedName: func() string { return string(activation.LaneTelemetry) },
	}
	if codex.config != "" {
		codexLane.Elected = codexElectedFn(codex, activation.LaneTransport, override)
		codexLane.ElectionProblem = codexElectionProblemFn(codex, override)
		codexLane.ElectedName = codexElectedNameFn(codex, activation.LaneTransport, override)
	}
	return map[string]gatewayemit.ProviderLane{
		string(provider.ClaudeCode): {
			DID: didOf(string(provider.ClaudeCode)),
			// Re-resolved PER RECORD, never cached: see electedFn.
			Elected: electedFn(settingsPath, activation.LaneTransport, override),
			// Not derivable from Elected(): a false there means either "another lane
			// won" or "nothing could be read", and only the second is a defect.
			ElectionProblem: electionProblemFn(settingsPath, override),
			// Which lane, not just whether this one: "nobody" is a routing gap.
			ElectedName: electedNameFn(settingsPath, activation.LaneTransport, override),
		},
		string(provider.Codex): codexLane,
	}
}

// codexCallObserver is the gate-side hook that records the relay's evidence of
// Codex, nil when the unit carries no Codex config path. It runs at REQUEST
// time, before the provider is called: Codex's own telemetry for a call only
// exists after the call, so the marker is on disk before telemetry can have
// decided anything about that call, and it is the one call that could
// otherwise be recorded twice.
func codexCallObserver(paths codexElectionPaths, codexConfig string, logger *log.Logger) func() {
	if codexConfig == "" {
		return nil
	}
	return codexObserver(paths, logger)
}

// errChatPoolUnavailable is what chatDeliver reports to RecordDeliveryFailure
// when the chat pool itself could not accept a record (saturated, or already
// shutting down): the record never reached core either way, and
// client.FailureClass's own catch-all ("network") is an honest enough class
// for "never even attempted" as any other unclassified failure.
var errChatPoolUnavailable = errors.New("the delivery pool could not accept this chat event (saturated, or shutting down)")

// errNoChatIdentity tells the chat pool a record was not sent because no
// claude-code identity resolved, so the rest of its conversation's queue is
// not sent either. Nothing is latched: without a signer nothing can be
// attributed to a run in the first place.
var errNoChatIdentity = errors.New("no resolved claude-code identity for a chat event")

// errChatLatched tells the chat pool a record was not sent because its
// conversation is already latched halted: the refusal that latched it has
// been recorded, and nothing of the run may follow it.
var errChatLatched = errors.New("the chat conversation is latched halted")

// providersFromFlag turns --providers' raw value into transport.Config's own
// nil/present-but-empty distinction: seen is false when the flag never
// appeared on argv (nil, so Config.Validate applies its claude-code
// default), and true whenever it did, even with an empty or all-blank value
// (a non-nil, zero-length slice, so Validate intercepts nothing rather than
// defaulting). Entries are trimmed and blanks dropped so a trailing comma or
// stray space in a hand-typed unit does not turn into an empty provider
// name Config has to reject.
func providersFromFlag(seen bool, raw string) []string {
	if !seen {
		return nil
	}
	out := []string{}
	for _, p := range strings.Split(raw, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// gatedFn is the gated-class selector transport.WithGate takes: every POST,
// the same permissive rule gatewayemit's own isModelCall uses to decide
// "worth attributing at all" -- a halted session must not keep making ANY
// relayed call, not only a /v1/messages completion, and the decorator's own
// check is a local file stat, cheap enough that gating too widely costs
// nothing the way a real /evaluate round trip would (a future gated-class
// question for a server verdict, which trades latency differently, is
// unrelated to this one).
func gatedFn(r *http.Request) bool {
	return strings.EqualFold(r.Method, http.MethodPost)
}

// haltDecorator is a latch-only Evaluator: it answers HALT from the
// cross-lane latch without a round trip when the run a relayed call's
// session currently belongs to is latched, and otherwise delegates to next.
// Built here rather than in internal/transport: this is the one place
// hookflow (the latch) and sessionkey (the per-provider carrier) are both
// reachable at once -- that package's import guard
// (internal/depguard/guards_test.go) excludes both.
//
// next is nil for now, left ready for a real /evaluate evaluator to chain in
// behind it. A nil next allows every unlatched call exactly as this relay
// always has -- this decorator only ever SUBTRACTS a call a real evaluator
// would also have refused, never adds a refusal that isn't already
// intended.
//
// logger receives the refusal line and any audit-write failure; nil discards.
type haltDecorator struct {
	next   gateway.Evaluator
	logger *log.Logger
	// onCodexCall, when set, is told of every relayed model completion that
	// carries Codex's carrier: the relay's proof that Codex really does route
	// through it, which Codex's election requires before the relay outranks
	// telemetry. It never affects the verdict.
	onCodexCall func()
}

// Evaluate implements gateway.Evaluator.
func (h haltDecorator) Evaluate(ctx context.Context, c gateway.Captured) (client.Evaluation, error) {
	h.noteCodexCall(c)
	if sessionID, info, halted := h.haltedRun(c); halted {
		// Reuse SessionHaltDecision's wording rather than inventing a second
		// refusal vocabulary: the same text a replayed hook refusal renders.
		dec := hookflow.SessionHaltDecision(info)
		// The refusal is recorded here, not left to the relay's own event:
		// when another lane is the elected producer that event is skipped, the
		// call never reaches the provider, and no lane would otherwise know
		// it was refused. Same audit sink a replayed hook refusal writes.
		logger := h.logger
		if logger == nil {
			logger = log.New(io.Discard, "", 0)
		}
		logger.Printf("transport: refused a relayed call for session %s: its run is latched halted", sessionID)
		hookflow.RecordEnforcement(logger, sessionID, "llm_completion", dec, hookflow.ApplyResult{Decision: "deny", Emitted: true})
		trace.Emit(trace.Record{
			Stage:     trace.StageGateVerdict,
			SessionID: sessionID,
			Outcome:   string(dec.Evaluation.Verdict),
			Detail: map[string]any{
				"method": c.HTTPMethod,
				"url":    c.HTTPURL,
				"reason": "run_latched_halted",
			},
		})
		return dec.Evaluation, nil
	}
	if h.next == nil {
		return client.Evaluation{Verdict: client.VerdictAllow}, nil
	}
	return h.next.Evaluate(ctx, c)
}

// noteCodexCall reports a call as Codex evidence only when it is attributed
// to Codex by its carrier AND is a model completion: a browser's chatgpt.com
// traffic, a probe or a telemetry POST is not evidence that Codex's model
// calls reach this relay, and the relay records only completions.
func (h haltDecorator) noteCodexCall(c gateway.Captured) {
	if h.onCodexCall == nil || !gatewayemit.IsModelCompletion(c.HTTPURL) {
		return
	}
	if p, _, ok := attributeRelayed(hostOf(c.HTTPURL), c.RequestHeaders); ok && p == sessionkey.Codex {
		h.onCodexCall()
	}
}

// haltedRun resolves the session key for c's provider (sessionkey.Resolve,
// per host), then the run that session CURRENTLY belongs to
// (haltDecoratorRunID), and reports whether that run -- not the session id --
// is latched halted. A call whose provider or session key cannot be
// resolved is never treated as latched: the caller falls through to next
// (or allow), exactly the skip-and-count posture every other consumer of an
// unresolvable carrier already takes.
//
// A claude.ai chat completion carries no CLI carrier header at all (the
// browser/desktop client, never Claude Code, made the request), so
// sessionkey.ResolveProxy always misses for one; ResolveChat is tried next,
// keyed directly on the conversation -- a chat has no runs (git.RunStore
// never sees one), so its latch key IS the chat key itself, the same
// resolution RecordDeliveryFailure/runIDFor already fall back to for a chat
// event carrying no RunID (gatewayemit/emitter.go's own emitChat never sets
// one).
func (h haltDecorator) haltedRun(c gateway.Captured) (string, hookflow.SessionHaltInfo, bool) {
	if _, sessionID, ok := attributeRelayed(hostOf(c.HTTPURL), c.RequestHeaders); ok {
		info, halted := hookflow.SessionHalted(haltDecoratorRunID(sessionID))
		return sessionID, info, halted
	}
	if chatKey, ok := sessionkey.ResolveChat(hostOf(c.HTTPURL), pathOf(c.HTTPURL)); ok {
		info, halted := hookflow.SessionHalted(chatKey)
		return chatKey, info, halted
	}
	return "", hookflow.SessionHaltInfo{}, false
}

// attributeRelayed names the one provider a relayed call belongs to and its
// session id: the providers whose host rows cover host, narrowed to exactly
// one by whose carrier header the call carries. ok is false when none or more
// than one resolves; the caller never guesses, and the emitter counts the
// skip. This is the same rule the emitter applies, so a refusal and a record
// can never disagree about whose call it was.
func attributeRelayed(host string, headers map[string]string) (sessionkey.Provider, string, bool) {
	p, id, skip := sessionkey.AttributeProxy(transport.CandidatesForHost(host), headers)
	return p, id, skip == ""
}

// haltDecoratorRunID resolves session to the run it CURRENTLY belongs to
// (git.RunStore's own current record), so a `/clear` or `--resume` bump is
// what gets consulted -- never a generation the session already left behind
// (the latch bites for a continued run only when THAT run halted). A read
// failure or generation 0 falls back to the session id itself: at generation
// 0 the run IS the session, the same selection runIDFor makes everywhere
// else in this repo.
func haltDecoratorRunID(sessionID string) string {
	rec, err := (obgit.RunStore{}).Read(sessionID)
	if err != nil || rec.Generation == 0 {
		return sessionID
	}
	return rec.RunID
}

// hostOf extracts the host component transport.CandidatesForHost matches
// against, from the full upstream URL gateway.Captured.HTTPURL carries
// (e.g. "https://api.anthropic.com/v1/messages", never carrying the query --
// RequestCapture.ForGate's own URL is already stripped). An unparseable URL
// resolves to "", which matches no provider's host table, so the decorator
// falls through rather than guessing.
func hostOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return u.Host
}

// pathOf is hostOf's own path half, for sessionkey.ResolveChat's other
// argument -- the same gateway.Captured.HTTPURL, already stripped of its
// query.
func pathOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return u.Path
}

// relayCapturesBody is the transport lane's body predicate. It passes the
// request's host as well as its path: a claude.ai chat completion shares its
// /api/ prefix with Claude Code's own telemetry on api.anthropic.com, and only
// the host says which one is a model call whose body is kept.
func relayCapturesBody(r *http.Request) bool {
	return gatewayemit.CapturesBodyAt(r.Host, r.URL.Path)
}
