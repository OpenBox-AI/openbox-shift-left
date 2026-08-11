package provider

import (
	"io"
	"log"
)

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
