package provider

import (
	"io"
	"log"
	"time"
)

// HookCeiling is the wall-clock limit a provider imposes on one hook run.
//
// It is a hard kill, not a latency preference: a hook that overruns is killed
// mid-flight and both shipped providers then let the tool run. On the gating
// hook that is a silently ungoverned execution, which no org setting can
// prevent — so the engine must always write its verdict before Gating elapses.
// Since ADR-0017 every gated call waits on a network verdict, which turns this
// from an arithmetic detail into the enforce path's outer failure boundary.
//
// The ceiling is not always the provider's own limit. Where a provider lets the
// installer choose (Codex defaults to 600s and honours what we write), the
// declared value is the one the installer actually wrote, because that is what
// will kill the hook. An adapter therefore declares what is true of an INSTALLED
// hook, not what the tool's documentation permits.
type HookCeiling struct {
	// Gating is the ceiling on the hook that can deny a tool call (PreToolUse):
	// the only hook that runs the enforce gate, and so the only one that can
	// hold for an approval decision.
	Gating time.Duration
	// Other is the ceiling on every non-gating hook event.
	Other time.Duration
}

// HookEngine is the runtime half of the SPI: what a provider's adapter does
// when its tool fires a hook.
//
// Both shipped adapters already had this exact shape as a free function; it was
// simply never expressed as a contract, so the CLI reached the adapters through
// a hard-coded switch and a new provider meant editing the composition root's
// dispatch as well as registering it. Naming the contract lets the registry
// carry install and runtime together.
type HookEngine interface {
	// RunHook handles one native hook event, reading the tool's payload from
	// stdin and writing any response to stdout.
	//
	// It must never fail the developer's tool call (INV-3): every fault is
	// recovered and logged to the logger, stdout carries only a well-formed
	// response or nothing at all, and the caller always exits 0.
	RunHook(event string, stdin io.Reader, stdout io.Writer, logger *log.Logger)

	// Capabilities declares what this adapter supports, so a session's
	// coverage tier can be derived and displayed rather than assumed.
	Capabilities() []Capability

	// HookCeilings declares the wall-clock limits this provider kills a hook
	// at, so the engine derives its own budget rather than trusting each
	// adapter to do the arithmetic. It is on the interface, not an optional
	// side channel, because an adapter that does not declare one cannot be
	// governed safely: the engine would have no boundary to stay inside.
	HookCeilings() HookCeiling
}

// Rewaker is the OPTIONAL background-wake half of the runtime SPI: a provider
// whose host can wake a live session from a process that outlives the hook.
//
// It is separate from HookEngine, and optional, because the two obligations are
// opposite. A hook must never fail a tool call, so RunHook returns nothing and
// its caller always exits 0. A rewake handler blocks nothing and has no tool
// call to protect — its EXIT CODE is the output, and that is the whole
// mechanism. Folding it into HookEngine would put a non-zero exit inside an
// interface documented to never produce one.
//
// Only Claude Code implements it today (its `asyncRewake` handler). Hosts that
// do not simply fall back to the advisory findings channel, so the type
// assertion failing is a supported state, not an error.
type Rewaker interface {
	// RunRewake watches for a decision that lands after the hook already
	// returned, writes the content-free message to wake, and reports the exit
	// code the host reads — non-zero only when there is genuinely something to
	// tell the session.
	RunRewake(stdin io.Reader, wake io.Writer, logger *log.Logger) int
}

// Capability is one entry in a provider's declared capability profile.
//
// OpenBox core is written against the normalized event contract and never
// assumes a capability; an adapter declares what it supports so a per-session
// coverage tier can be derived and displayed, with no false sense of coverage.
//
// "Supported" means the adapter implements the mechanism, not that it is active
// in a given session: the enforce capability is opt-in and
// default off, so an unconfigured session still observes only (INV-3).
type Capability struct {
	Key       string // stable capability id
	Supported bool
	How       string // one-line note on the mechanism / caveat
}
