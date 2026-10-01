package main

import (
	"context"
	"sync"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/providers"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/telemetryemit"
	"github.com/openbox-ai/openbox-shift-left/internal/provider"
)

// codexContentPoll is how often a pending record rechecks the rollout. Each
// check reads the file again, so it is slower than Muse's resumable reader.
const codexContentPoll = 250 * time.Millisecond

// codexContentSource supplies the bodies Codex's telemetry export does not
// carry: the call's input and output items, read from the thread's rollout
// file. They never come from an OTel attribute, so the lane's "attributes never
// egress" claim stays true; content enters only through the
// telemetryemit.Enricher seam this builds.
type codexContentSource struct {
	// SessionsRoot is Codex's sessions directory, resolved at init and carried
	// by the unit: the daemon has no $HOME. Empty disables content.
	SessionsRoot string
	// Capture is Codex's own content_capture posture, re-resolved per record.
	Capture func() bool
	// Redact scrubs each text of the bodies. Nil uses the repo's redactor.
	Redact func(string) string
	// Poll overrides codexContentPoll (tests).
	Poll time.Duration

	mu       sync.Mutex
	claimed  map[string]string // thread + rollout record -> the request id that took it
	claimLog []string
}

// maxClaimed bounds the records remembered as taken; the oldest is forgotten
// first. A forgotten record could be taken by a second id only after
// thousands of calls, far past the 30 s window that makes it a candidate.
const maxClaimed = 4096

// owner returns the request id that took a rollout record, "" if none did.
func (s *codexContentSource) owner(thread, record string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.claimed[thread+"\x1f"+record]
}

// claim gives a record to a request id. It reports false when another id has
// it: one call's content is shipped once, so a retried stream's second
// response.completed, or a later call with the same counts, gets none of it.
// The same id again (a redelivered record) still succeeds.
func (s *codexContentSource) claim(thread, record, requestID string) bool {
	if record == "" {
		return true
	}
	key := thread + "\x1f" + record
	s.mu.Lock()
	defer s.mu.Unlock()
	if prior, ok := s.claimed[key]; ok {
		return prior == requestID
	}
	if s.claimed == nil {
		s.claimed = make(map[string]string)
	}
	s.claimed[key] = requestID
	s.claimLog = append(s.claimLog, key)
	if len(s.claimLog) > maxClaimed {
		delete(s.claimed, s.claimLog[0])
		s.claimLog = s.claimLog[1:]
	}
	return true
}

// contentCaptureFor reads one tool's content_capture posture from its own
// dev.json. An unreadable posture is off: this is the gate that decides whether
// a rollout is opened at all, so it fails closed.
func contentCaptureFor(tool provider.Name, warn func(string, ...any)) func() bool {
	return func() bool {
		on, err := devconfig.ResolveContentCaptureFor(string(tool))
		if err != nil {
			if warn != nil {
				warn("openbox telemetry: could not resolve %s's content_capture posture (%v); reading no content", tool, err)
			}
			return false
		}
		return on
	}
}

// Enricher is the seam the telemetry emitter runs per model call.
func (s *codexContentSource) Enricher() *telemetryemit.Enricher {
	if s == nil {
		return nil
	}
	redact := s.Redact
	if redact == nil {
		redact = defaultRedact()
	}
	poll := s.Poll
	if poll <= 0 {
		poll = codexContentPoll
	}
	return &telemetryemit.Enricher{
		Outcome: "codex.content",
		Enabled: func() bool { return s.SessionsRoot != "" && s.Capture != nil && s.Capture() },
		Enrich: func(ctx context.Context, c telemetryemit.Call) telemetryemit.Result {
			return s.enrich(ctx, c, redact, poll)
		},
	}
}

// enrich reads the rollout until the call is found and verified, the rollout
// turns out not to be the shape this reader knows, the deadline passes or the
// daemon stops. The rollout is written as the call completes, so a first miss
// is "not yet", not "never". Nothing is kept past the call.
func (s *codexContentSource) enrich(ctx context.Context, c telemetryemit.Call, redact func(string) string, poll time.Duration) telemetryemit.Result {
	both := func(reason string) telemetryemit.Result {
		return telemetryemit.Result{Misses: []telemetryemit.Miss{{Part: "both", Reason: reason}}}
	}
	t := c.Tokens
	if t == nil || t.Input == nil || t.Output == nil || c.At.IsZero() {
		// Nothing to join on: the export did not say what the call cost.
		return both("no_join")
	}
	// An export with no cached count means none were cached.
	cached := 0
	if t.CacheRead != nil {
		cached = *t.CacheRead
	}
	join := providers.CodexCallJoin{At: c.At, Input: *t.Input, Output: *t.Output, CacheRead: cached,
		Exclude: func(record string) bool {
			o := s.owner(c.Session, record)
			return o != "" && o != c.RequestID
		}}
	var last providers.CodexContentState
	for {
		content, st := providers.ReadCodexCallContent(s.SessionsRoot, c.Session, join, c.Deadline)
		// A read cut short by the deadline says nothing new; keep the state the
		// last complete read settled on.
		if st != providers.CodexContentTimeout || last == "" {
			last = st
		}
		if st == providers.CodexContentVerified {
			if s.claim(c.Session, content.ResponseID, c.RequestID) {
				return bodiesOf(content, redact)
			}
			// Taken between the read and now by a concurrent call: not this
			// call's, and its own record may yet be written.
			st = providers.CodexContentNoJoin
			if last == "" || last == providers.CodexContentTimeout {
				last = st
			}
		}
		if st == providers.CodexContentUnverified {
			return both("unverified")
		}
		remaining := time.Until(c.Deadline)
		if remaining <= 0 || ctx.Err() != nil {
			break
		}
		wait := poll
		if remaining < wait {
			wait = remaining
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
	}
	if ctx.Err() != nil {
		return both("shutdown")
	}
	return both(string(last))
}

// bodiesOf renders a verified call. A side with nothing to say is a miss of its
// own rather than an empty body.
func bodiesOf(content providers.CodexCallContent, redact func(string) string) telemetryemit.Result {
	res := telemetryemit.Result{
		Request:  providers.CodexRequestBodyJSON(content, redact),
		Response: providers.CodexResponseBodyJSON(content, redact),
	}
	if res.Request == "" {
		res.Misses = append(res.Misses, telemetryemit.Miss{Part: "request", Reason: "empty"})
	}
	if res.Response == "" {
		res.Misses = append(res.Misses, telemetryemit.Miss{Part: "response", Reason: "empty"})
	}
	return res
}
