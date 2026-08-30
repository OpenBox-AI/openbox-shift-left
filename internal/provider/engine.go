package provider

import (
	"io"
	"log"
	"time"
)

// HookCeiling is the wall-clock limit a provider imposes on one hook run. On
// the gating hook that is a silently ungoverned execution, which no org
// setting can prevent; so the engine must always write its verdict before
// Gating elapses.
type HookCeiling struct {
	// Gating is the ceiling on the hook that can deny a tool call (PreToolUse):
	// the only hook that runs the enforce gate, and so the only one that can hold
	// for an approval decision.
	Gating time.Duration
	// Other is the ceiling on every non-gating hook event.
	Other time.Duration
}

// HookEngine is the runtime half of the SPI: what a provider's adapter does
// when its tool fires a hook.
type HookEngine interface {
	// RunHook handles one native hook event, reading the tool's payload from
	// stdin and writing any response to stdout.
	RunHook(event string, stdin io.Reader, stdout io.Writer, logger *log.Logger)

	// Capabilities declares what this adapter supports, so a session's coverage
	// tier can be derived and displayed rather than assumed.
	Capabilities() []Capability

	// HookCeilings declares the wall-clock limits this provider kills a hook at,
	// so the engine derives its own budget rather than trusting each adapter to
	// do the arithmetic.
	HookCeilings() HookCeiling
}

// Rewaker is the optional background-wake half of the runtime SPI: a provider
// whose host can wake a live session from a process that outlives the hook. A
// hook must never fail a tool call, so RunHook returns nothing and its caller
// always exits 0.
type Rewaker interface {
	// RunRewake watches for a decision that lands after the hook already
	// returned, writes the content-free message to wake, and reports the exit
	// code the host reads; non-zero only when there is genuinely something to
	// tell the session.
	RunRewake(stdin io.Reader, wake io.Writer, logger *log.Logger) int
}

// Capability is one entry in a provider's declared capability profile. OpenBox
// core is written against the normalized event contract and never assumes a
// capability; an adapter declares what it supports so a per-session coverage
// tier can be derived and displayed, with no false sense of coverage.
type Capability struct {
	Key       string // stable capability id
	Supported bool
	How       string // one-line note on the mechanism / caveat
}
