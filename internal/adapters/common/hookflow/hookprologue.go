package hookflow

import (
	"bytes"
	"io"
	"log"
	"os"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/decision"
)

// RunSubcommand handles the two `openbox hook <provider>` subcommands that are
// not hook events: an empty one (a usage line) and `flush` (the detached
// flusher's body). It reports whether sub was one of them, so the caller
// returns without treating it as a hook name.
func RunSubcommand(logger *log.Logger, provider, spoolDir, sub string) (handled bool) {
	switch sub {
	case "":
		logger.Printf("usage: openbox hook %s <HookName|flush>", provider)
		return true
	case "flush":
		RunFlush(logger, spoolDir, os.Getenv(EnvFlushSession))
		return true
	}
	return false
}

// ReadHookEvent reads the whole payload BEFORE it is parsed, bounded the same
// way ParseHookEvent's own decoder is (MaxHookPayload): the trace records raw
// stdin even when parsing fails, and a stream decoder consumed on a failed
// parse cannot be replayed to build that record afterward. sessionOf names the
// parsed event's session for the trace.
func ReadHookEvent[E any](provider, hook string, stdin io.Reader, sessionOf func(*E) string) (*E, error) {
	rawStdin, _ := io.ReadAll(io.LimitReader(stdin, MaxHookPayload))
	ev, err := ParseHookEvent[E](bytes.NewReader(rawStdin))
	sessionID := ""
	if ev != nil {
		sessionID = sessionOf(ev)
	}
	TraceHookIn(provider, hook, sessionID, rawStdin, err)
	return ev, err
}

// TraceHookOutput tees stdout into a buffer and returns the writer to answer
// on plus the func that traces what was written; the caller defers it. The
// session is read when the trace is written, so a hook that moves its event to
// another session traces the session it ended in.
func TraceHookOutput(provider, hook string, stdout io.Writer, start time.Time, sessionID func() string) (io.Writer, func()) {
	hookOut := new(HookOutputBuffer)
	return io.MultiWriter(stdout, hookOut), func() {
		TraceHookOut(provider, hook, sessionID(), start, hookOut)
	}
}

// ContentRedactor is the secret redactor a Mapper runs every content body
// through, or nil (the identity) when local secret detection is off.
func ContentRedactor(secretDetection bool) func(string) string {
	if !secretDetection {
		return nil
	}
	redactor := decision.NewRedactor()
	return func(s string) string { return RedactText(redactor, s) }
}
