package main

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"sync"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/sessionkey"
	"github.com/openbox-ai/openbox-shift-left/internal/client"
	"github.com/openbox-ai/openbox-shift-left/internal/provider"
)

// zeroLaneRetries makes MaxRetries: 0 addressable for every lane daemon's own
// per-provider client (client.Config.MaxRetries is a *int so 0 is
// distinguishable from "unset, use the client's own default").
var zeroLaneRetries = 0

// providerIdentity is what a lane daemon pre-resolves ONCE at startup for one
// governed tool: its developer DID and the client.Client that SIGNS on its
// behalf. Resolved through devconfig's *For accessors, never through
// devconfig.BindProvider on the record path -- a lane daemon serves every
// configured tool for its whole life, and flipping the process-global bound
// provider per record would let two concurrent records resolve each other's
// identity store.
//
// The client is what actually decides who signs an egressed record, not the
// DID alone: a Codex record sent by the CC client would egress as the CC
// agent no matter what DID it carries. Named separately from
// cmd/openbox/initlane.go's laneIdentity (unit-file identity: label, systemd
// name, unit path), which this is not.
type providerIdentity struct {
	DID    string
	Client *client.Client
}

// resolveProviderIdentities pre-resolves one identity+client per
// provider.Supported() with a usable identity store. A provider with no
// store yet (no credentials, never `init`-ed) is skipped and reported through
// warn, not treated as a startup failure: a machine that has only run
// `openbox init --provider claude-code` legitimately has nothing for Codex to
// resolve, and the daemon still governs the tool(s) it can.
func resolveProviderIdentities(warn func(format string, args ...any)) map[string]providerIdentity {
	out := map[string]providerIdentity{}
	for _, tool := range provider.Supported() {
		creds, err := devconfig.ResolveCredentialsFor(tool)
		if err != nil {
			if warn != nil {
				warn("openbox: %s has no usable identity yet (%v); its lane records will not be sent", tool, err)
			}
			continue
		}
		c, err := client.New(client.Config{
			BaseURL:            creds.BaseURL,
			APIKey:             creds.APIKey,
			WorkloadPrivateKey: creds.WorkloadPrivateKey,
			// Memory, not creds.TokenCachePath: a lane daemon is a single long-lived
			// process holding this token in memory for its whole life anyway, and a
			// disk cache shared with the hook binary would let the two race a
			// concurrent Store/Invalidate against the same file.
			TokenCachePath:        "",
			ContentCaptureEnabled: creds.ContentCaptureEnabled,
			// 0, addressable: single-attempt delivery means
			// no client anywhere may retry a send on its own -- a lane record
			// delivered through hookflow.LaneQueue or the transport lane's chat
			// DeliverPool gets exactly one attempt, same as claude-code/codex's
			// own hook clients (creds.go in each adapter).
			MaxRetries: &zeroLaneRetries,
			// Without this, client.Emit's own diagnostic lines (a delivery
			// failure's detail, a dropped-unbuildable-event reason) fell back to
			// nopLogger and were silently discarded for both lane daemons' send
			// path -- the caller-level "delivery failed: %v" still printed, just
			// without the detail Emit itself would have added. warnFn is the
			// adapter's own Printf-shaped adapters/claude-code and adapters/codex
			// pass their NewClient(logger); this is the fourth site, brought to
			// parity with the other three.
			Logger: warnFn(warn),
		})
		if err != nil {
			if warn != nil {
				warn("openbox: %s's identity could not build a client (%v); its lane records will not be sent", tool, err)
			}
			continue
		}
		out[tool] = providerIdentity{DID: creds.DID, Client: c}
	}
	return out
}

// warnFn adapts a Printf-shaped warn closure to client.Logger, so a nil warn
// (a caller that wants silence, e.g. a test) becomes a nil client.Logger --
// client.New already treats that as "default discards" -- rather than a
// non-nil interface value wrapping a nil func, which would panic on first
// use instead.
type warnLogger func(format string, args ...any)

func (f warnLogger) Printf(format string, args ...any) { f(format, args...) }

func warnFn(warn func(format string, args ...any)) client.Logger {
	if warn == nil {
		return nil
	}
	return warnLogger(warn)
}

// providerSpoolSubdir names each supported provider's own session spool
// subdirectory, resolved through laneSpoolDir -- the SAME directory that
// provider's hook events already queue through, never a lane-daemon-private
// one: a lane record has to interleave, in append order, with the hook
// events of the same session. "cc-spool"/"codex-spool" match
// claude-code.DefaultSpoolDir/codex.DefaultSpoolDir's own literals exactly
// (both are themselves just devconfig.SpoolDir(that same string)).
var providerSpoolSubdir = map[string]string{
	string(provider.ClaudeCode): "cc-spool",
	string(provider.Codex):      "codex-spool",
}

// laneSpoolDir resolves one provider's own spool directory for a lane
// daemon, adding ONE precedence tier ABOVE devconfig.SpoolDir's own three:
// OPENBOX_SPOOL_DIR (whole path) > OPENBOX_SPOOL_ROOT/subdir >
// filepath.Dir(OPENBOX_HALT_DIR)/subdir > devconfig.SpoolDir's own fallback
// (os.UserConfigDir(), which a HOME-less daemon cannot trust).
//
// The third tier exists for a unit installed BEFORE OPENBOX_SPOOL_ROOT
// existed: it still carries OPENBOX_HALT_DIR (unconditional since before
// this change), and hookflow.DefaultHaltDir's own shape --
// devconfig.ConfigDir()/halted-sessions -- means that env var's own PARENT
// directory already IS the same ConfigDir() a resolved OPENBOX_SPOOL_ROOT
// would otherwise carry. Without this fallback, an old unit's every lane
// record append resolves against a relative, HOME-less path and fails,
// latching the run on the very first lane record after the daemon's own
// binary upgrades -- exactly the class of failure a RESOLVED (not copied)
// unit env key exists to prevent, until the next `openbox init` rewrites
// the unit with OPENBOX_SPOOL_ROOT directly.
func laneSpoolDir(subdir string, logger *log.Logger) string {
	if p := os.Getenv(devconfig.EnvSpoolDir); p != "" {
		return p
	}
	if p := os.Getenv(devconfig.EnvSpoolRoot); p != "" {
		return filepath.Join(p, subdir)
	}
	if haltDir := os.Getenv(devconfig.EnvHaltDir); haltDir != "" {
		root := filepath.Dir(haltDir)
		if logger != nil {
			logger.Printf("openbox: %s is unset (an older install); resolving %q's spool root from "+
				"%s's own parent (%s) instead of this process's own possibly HOME-less default. "+
				"Re-run `openbox init` to write %s directly into the unit.",
				devconfig.EnvSpoolRoot, subdir, devconfig.EnvHaltDir, root, devconfig.EnvSpoolRoot)
		}
		return filepath.Join(root, subdir)
	}
	return devconfig.SpoolDir(subdir)
}

// laneQueues builds one hookflow.LaneQueue per provider.Supported() with a
// resolved identity in identities: each queue drains through its OWN Engine
// (that provider's own spool dir, so cc-spool is only ever drained by the
// claude-code queue and codex-spool only by the codex one) using that
// provider's own signing client -- one queue per provider, never a shared
// Engine between two (see LaneQueue's own doc for why). A provider absent
// from identities (no usable identity yet, per resolveProviderIdentities)
// gets no queue at all: routeLaneRecord below drops, counts and logs a
// record for it exactly as today's identities-keyed dispatch already did,
// since no run can be trusted without a signer.
func laneQueues(identities map[string]providerIdentity, logger *log.Logger) map[string]*hookflow.LaneQueue {
	out := make(map[string]*hookflow.LaneQueue, len(providerSpoolSubdir))
	for tool, subdir := range providerSpoolSubdir {
		id, ok := identities[tool]
		if !ok {
			continue
		}
		engine := hookflow.NewEngine(laneSpoolDir(subdir, logger))
		engine.Log = logger.Printf
		out[tool] = hookflow.NewLaneQueue(engine, id.Client, logger.Printf)
	}
	return out
}

// droppedCounter is what every delivery source this daemon owns shares (a
// chat DeliverPool, a provider's own LaneQueue), so their counts can be
// summed into the one number DeliverStatus persists: a status file reports
// one combined total per daemon, not a breakdown by source.
type droppedCounter interface{ Dropped() uint64 }

// combinedDropped sums every source's own Dropped(), satisfying the same
// `Dropped() uint64` shape reportDeliveryStatusPeriodically already reads
// (telemetry.go's deliveryPool interface) without either daemon needing to
// track a second counter of its own.
type combinedDropped []droppedCounter

func (c combinedDropped) Dropped() uint64 {
	var total uint64
	for _, d := range c {
		total += d.Dropped()
	}
	return total
}

// combinedDroppedFrom builds a combinedDropped from every LaneQueue this
// daemon built (laneQueues never stores a nil entry) plus any extra source
// (the transport lane's own chat DeliverPool; telemetry has none).
func combinedDroppedFrom(queues map[string]*hookflow.LaneQueue, extra ...droppedCounter) combinedDropped {
	out := make(combinedDropped, 0, len(queues)+len(extra))
	for _, q := range queues {
		out = append(out, q)
	}
	return append(out, extra...)
}

// shutdownCloser is the shape a shutdown-time drain shares (*hookflow.LaneQueue,
// *hookflow.DeliverPool): wait for in-flight work up to ctx's own deadline,
// reporting how much was abandoned.
type shutdownCloser interface {
	Close(ctx context.Context) int
}

// closeAllConcurrently runs every closer's own Close AT ONCE against ctx,
// never one after another: ctx carries a fixed wall-clock deadline, so a
// caller closing several drainers in sequence gives an early, slow one the
// chance to burn through the ENTIRE shared budget before a later, faster
// one ever starts waiting on it -- a fast drain that would easily have
// finished within the shared deadline is then reported abandoned anyway,
// because by the time its own Close is even called, ctx has already
// expired. Starting every Close from the same instant is what makes the
// shared deadline actually shared rather than serially spent. Sums every
// abandoned count.
func closeAllConcurrently(ctx context.Context, closers ...shutdownCloser) int {
	var wg sync.WaitGroup
	results := make([]int, len(closers))
	for i, c := range closers {
		wg.Add(1)
		go func(i int, c shutdownCloser) {
			defer wg.Done()
			results[i] = c.Close(ctx)
		}(i, c)
	}
	wg.Wait()
	total := 0
	for _, r := range results {
		total += r
	}
	return total
}

// newChatPool builds the transport lane's own claude.ai chat delivery: a
// bounded, spool-less DeliverPool that sends one conversation's records
// in order, each after the one before it was accepted (a chat conversation has no tool session
// spool to append into, so this is the one place a lane record still uses
// the pool rather than LaneQueue), signed by the claude-code identity (the
// one that governs Anthropic's hosts on this machine). Each attempt is
// bounded at DeliveryAttemptTimeout (30s, the bound every other drainer
// uses), and a transient failure (client.RetryableDelivery) gets exactly one
// retry before the conversation is latched, so the pool's own timeout covers
// two attempts. The returned deliver closure additionally latches a chat
// event the pool could
// not even ACCEPT (saturated, or shutting down): it never reached core
// either, so it is treated exactly like an attempted-and-refused one (R4a)
// rather than silently counted only as a pool drop.
func newChatPool(identities map[string]providerIdentity, advisory *hookflow.Advisory, logger *log.Logger) (*hookflow.DeliverPool, func(client.DevEvent) bool) {
	pool := hookflow.NewDeliverPool(hookflow.DefaultPoolSize, 2*hookflow.DeliveryAttemptTimeout,
		func(ctx context.Context, ev client.DevEvent) error {
			// A refused record stops its conversation's drainer, but a record
			// of the same call submitted after that stop starts a fresh one:
			// the latch, not the queue, is what keeps it from going out
			// behind the refusal.
			if _, halted := hookflow.SessionHalted(ev.SessionID); halted {
				logger.Printf("openbox transport: not sending chat event %s: its conversation is latched halted", ev.EventID)
				return errChatLatched
			}
			id, ok := identities[string(provider.ClaudeCode)]
			if !ok {
				logger.Printf("openbox transport: no resolved claude-code identity; dropping chat event %s "+
					"and every record queued behind it in its conversation", ev.EventID)
				return errNoChatIdentity
			}
			attempt := func(n int) error {
				actx, cancel := context.WithTimeout(hookflow.WithDeliveryAttempt(ctx, n), hookflow.DeliveryAttemptTimeout)
				defer cancel()
				_, err := hookflow.Deliver(actx, id.Client, advisory, ev, logger)
				return err
			}
			err := attempt(1)
			if err != nil && client.RetryableDelivery(err) && ctx.Err() == nil {
				logger.Printf("openbox transport: chat event %s not accepted (%s); retrying once", ev.EventID, client.FailureClass(err))
				err = attempt(2)
			}
			if err != nil {
				logger.Printf("openbox transport: chat delivery failed for %s: %v", ev.EventID, err)
				hookflow.HaltOnDeliveryFailure(logger, ev, err)
			}
			return err
		})
	// A record accepted but abandoned at shutdown before its first attempt
	// never reached core: latched like one the pool could not accept.
	pool.OnAbandon = func(ev client.DevEvent) {
		hookflow.HaltOnDeliveryFailure(logger, ev, errChatPoolUnavailable)
	}
	deliver := func(ev client.DevEvent) bool {
		if pool.Submit(ev) {
			return true
		}
		hookflow.HaltOnDeliveryFailure(logger, ev, errChatPoolUnavailable)
		return false
	}
	return pool, deliver
}

// routeLaneRecord is the Deliver seam every gatewayemit.Emitter/
// telemetryemit.Emitter this daemon builds shares: a claude.ai chat record
// (sessionkey.IsChatKey) goes to chatDeliver (the bounded, spool-less
// DeliverPool this daemon keeps for chat only); every other record -- a
// Claude Code or Codex model-call turn -- goes to that provider's own
// LaneQueue. No queue for ev.Tool.Name means no resolved identity for that
// provider: dropped, counted (through logger, the same disclosure today's
// identities-keyed pool dispatch already gave) and logged, never latched --
// no run can be trusted without a signer to speak for it.
func routeLaneRecord(queues map[string]*hookflow.LaneQueue, chatDeliver func(client.DevEvent) bool, logger *log.Logger, laneName string) func(ctx context.Context, ev client.DevEvent) bool {
	return func(ctx context.Context, ev client.DevEvent) bool {
		if chatDeliver != nil && sessionkey.IsChatKey(ev.SessionID) {
			return chatDeliver(ev)
		}
		q, ok := queues[ev.Tool.Name]
		if !ok {
			logger.Printf("openbox %s: no resolved identity for %q; dropping event %s", laneName, ev.Tool.Name, ev.EventID)
			return false
		}
		return q.Deliver(ctx, ev)
	}
}
