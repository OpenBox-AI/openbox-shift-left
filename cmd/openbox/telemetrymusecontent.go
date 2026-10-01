package main

import (
	"context"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/muse"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/telemetryemit"
	"github.com/openbox-ai/openbox-shift-left/internal/decision"
)

// museContentPoll is how often a pending record rechecks the stash and the
// journal. The journal reader is resumable, so a poll scans only appended bytes.
const museContentPoll = 200 * time.Millisecond

// museContentSource supplies the bodies Muse's telemetry export does not carry:
// the request from the stash its PostLLMCall hook wrote, the reply from the
// session journal. Neither ever comes from an OTel attribute, so the lane's
// "attributes never egress" claim stays true; content enters only through the
// telemetryemit.Enricher seam this builds.
type museContentSource struct {
	// SessionsRoot is Muse's sessions directory, resolved at init and carried
	// by the unit: the daemon has no $HOME. Empty disables content.
	SessionsRoot string
	// SpoolDir is the hook spool the PostLLMCall stash lives under.
	SpoolDir string
	// Capture is Muse's own content_capture posture, re-resolved per record.
	Capture func() bool
	// Redact scrubs each text field of the reply. Nil uses the repo's
	// redactor, the same one the gateway uses.
	Redact func(string) string
	// Poll overrides museContentPoll (tests).
	Poll time.Duration
}

func defaultRedact() func(string) string {
	r := decision.NewRedactor()
	return func(s string) string {
		out, _, _ := r.RedactText(s)
		return out
	}
}

// Enricher is the seam the telemetry emitter runs per model call.
func (s *museContentSource) Enricher() *telemetryemit.Enricher {
	if s == nil {
		return nil
	}
	redact := s.Redact
	if redact == nil {
		redact = defaultRedact()
	}
	poll := s.Poll
	if poll <= 0 {
		poll = museContentPoll
	}
	return &telemetryemit.Enricher{
		Outcome: "muse.content",
		Enabled: func() bool { return s.SessionsRoot != "" && s.Capture != nil && s.Capture() },
		Enrich: func(ctx context.Context, c telemetryemit.Call) telemetryemit.Result {
			return s.enrich(ctx, c, redact, poll)
		},
	}
}

// enrich polls until both halves are in hand, the journal turns out to be
// unreadable, the deadline passes or the daemon stops. One resumable reader is
// held for the whole wait and dropped with it: nothing is kept past one emit.
func (s *museContentSource) enrich(ctx context.Context, c telemetryemit.Call, redact func(string) string, poll time.Duration) telemetryemit.Result {
	var res telemetryemit.Result
	if c.RequestID == "" || c.RequestID == muse.EchoResponseID {
		return res
	}
	reader := muse.NewContentReader(s.SessionsRoot, c.Session, []string{c.RequestID})
	var (
		haveReq, haveResp bool
		respState         muse.ContentState
		last              muse.ResponseContent
		stopped           bool
	)
	for {
		if !haveReq {
			if e, ok := muse.TakeRequest(s.SpoolDir, c.RequestID); ok {
				res.Request, haveReq = e.Body, true
			}
		}
		if !haveResp {
			content, st := reader.Read(c.Deadline)
			// A read cut short by the deadline says nothing new; keep the last
			// state a read actually settled on.
			if st != muse.ContentTimeout || respState == "" {
				respState = st
			}
			switch {
			case st == muse.ContentUnverified:
				stopped = true
			case st == muse.ContentVerified:
				last = content
				// The reply is committed after model_completed, so a verified
				// read without text or tool calls is "not yet", not "done".
				haveResp = content.Text != "" || len(content.ToolCalls) > 0
			}
			if haveResp {
				res.Response = muse.ResponseBodyJSON(c.RequestID, content, redact)
			}
		}
		if (haveReq && haveResp) || stopped {
			break
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

	if !haveResp && !stopped && respState == muse.ContentVerified {
		// Summaries without a committed reply by the deadline: ship what the
		// journal does hold rather than nothing.
		if body := muse.ResponseBodyJSON(c.RequestID, last, redact); body != "" {
			res.Response, haveResp = body, true
		}
	}
	if !haveReq {
		res.Misses = append(res.Misses, telemetryemit.Miss{Part: "request", Reason: missReason(ctx, "stash_absent")})
	}
	if !haveResp {
		reason := "log_absent"
		switch {
		case ctx.Err() != nil:
			reason = "shutdown"
		case stopped:
			reason = "unverified"
		case respState == muse.ContentTimeout, respState == muse.ContentVerified:
			reason = "timeout"
		}
		res.Misses = append(res.Misses, telemetryemit.Miss{Part: "response", Reason: reason})
	}
	return res
}

func missReason(ctx context.Context, reason string) string {
	if ctx.Err() != nil {
		return "shutdown"
	}
	return reason
}
