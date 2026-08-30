package approver

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/cli/backend"
)

// Queue is the control-plane surface an approver works: the same two routes a
// human uses through `openbox approve`. Narrow on purpose — an autonomous
// approver gets no capability a person does not have.
type Queue interface {
	PendingApprovals(ctx context.Context, orgID string) ([]backend.Approval, error)
	DecideApproval(ctx context.Context, agentID, eventID, action string) error
}

// Config is one run of the loop.
type Config struct {
	OrgID    string
	Envelope Envelope
	Host     Host // nil ⇒ nothing is consultable; Consult behaves as Escalate

	// Shadow decides nothing and records what it would have decided. It is the
	// default everywhere it can be, because an envelope's real auto-approval
	// rate is the only evidence that its classes are drawn correctly.
	Shadow bool

	// Interval paces the poll. The queue is small and human-latency; seconds
	// are plenty, and the hook's hold is ~20s.
	Interval time.Duration
	// Once runs a single pass (used by tests and by `--once`).
	Once bool

	// SelfAgentID is this machine's own developer agent, if it has one.
	// Requests from it are refused unless AllowSelfAgent — approving your own
	// machine's request is a convenience control, not four-eyes (E9 §3.7).
	SelfAgentID    string
	AllowSelfAgent bool

	// MaxPerHour bounds autonomous decisions. 0 ⇒ unbounded.
	MaxPerHour int

	// EvidencePath is the local audit trail. Every outcome lands here,
	// including the ones that decided nothing.
	EvidencePath string

	// Now is injectable so the budget window is testable.
	Now func() time.Time

	Log io.Writer
}

// Record is one line of the audit trail: what was asked, what the envelope
// said, what the host proposed, and what actually happened.
type Record struct {
	Time       string `json:"time"`
	ApprovalID string `json:"approval_id"`
	AgentID    string `json:"agent_id"`
	Tool       string `json:"tool"`
	Captured   bool   `json:"request_captured"`
	Envelope   string `json:"envelope"`            // auto_approve|auto_deny|consult|escalate
	Rule       string `json:"rule,omitempty"`      // the envelope rule's note
	Host       string `json:"host,omitempty"`      // which host was consulted
	HostSays   string `json:"host_says,omitempty"` // approve|deny|escalate
	HostReason string `json:"host_reason,omitempty"`
	Applied    string `json:"applied"` // the queue's own vocabulary: approve|reject|none
	// WouldApply is set only in shadow mode: what a deciding run would have
	// done. Counting these is how an org reads its auto-approval rate before
	// giving the approver any authority.
	WouldApply string `json:"would_apply,omitempty"`
	Shadow     bool   `json:"shadow"`
	SelfAgent  bool   `json:"self_agent,omitempty"`
	LatencyMS  int64  `json:"latency_ms"`
	Error      string `json:"error,omitempty"`
}

// Loop works the queue until the context ends (or once, with Config.Once).
func Loop(ctx context.Context, q Queue, cfg Config) error {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Interval <= 0 {
		cfg.Interval = time.Second
	}
	if cfg.Log == nil {
		cfg.Log = io.Discard
	}
	seen := map[string]bool{} // decided (or deliberately skipped) this run
	var window []time.Time    // autonomous decisions, for the budget

	for {
		pending, err := q.PendingApprovals(ctx, cfg.OrgID)
		if err != nil {
			fmt.Fprintf(cfg.Log, "approver: read queue: %v\n", err)
		}
		for _, ap := range pending {
			if seen[ap.ID] || ap.Expired() {
				continue
			}
			rec := decide(ctx, q, cfg, ap, &window)
			seen[ap.ID] = true
			appendRecord(cfg, rec)
			fmt.Fprintf(cfg.Log, "approver: %s %s → envelope=%s host=%s applied=%s%s\n",
				ap.ActivityType, short(ap.ID), rec.Envelope, orDash(rec.HostSays), rec.Applied, shadowNote(cfg.Shadow))
		}
		if cfg.Once {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(cfg.Interval):
		}
	}
}

// decide resolves one request. It is the whole authority model in one function:
// the envelope classifies, the host may only narrow, and shadow applies nothing.
func decide(ctx context.Context, q Queue, cfg Config, ap backend.Approval, window *[]time.Time) Record {
	start := cfg.Now()
	request := ap.Request()
	outcome, rule := cfg.Envelope.Classify(ap.ActivityType, request)

	rec := Record{
		Time:       start.UTC().Format(time.RFC3339),
		ApprovalID: ap.ID,
		AgentID:    ap.AgentID,
		Tool:       ap.ActivityType,
		Captured:   request != "",
		Envelope:   string(outcome),
		Rule:       rule.Note,
		Shadow:     cfg.Shadow,
		Applied:    "none",
	}

	// Self-approval boundary first: it outranks the envelope, because no rule
	// an org writes should be able to turn a machine into its own approver.
	if cfg.SelfAgentID != "" && ap.AgentID == cfg.SelfAgentID {
		rec.SelfAgent = true
		if !cfg.AllowSelfAgent {
			rec.Envelope = string(Escalate)
			rec.Error = "same-agent request left for a human (pass --allow-same-agent to override)"
			rec.LatencyMS = ms(cfg.Now().Sub(start))
			return rec
		}
	}

	action := ""
	switch outcome {
	case AutoApprove:
		action = backend.ApprovalApprove
	case AutoDeny:
		action = backend.ApprovalReject
	case Consult:
		if cfg.Host == nil {
			rec.Envelope = string(Escalate)
			rec.Error = "consultable, but no host is configured"
			rec.LatencyMS = ms(cfg.Now().Sub(start))
			return rec
		}
		rec.Host = cfg.Host.Name()
		p, err := cfg.Host.Consult(ctx, Request{
			ID: ap.ID, Tool: ap.ActivityType, Agent: ap.Name(),
			Reason: derefReason(ap.Reason), Request: request,
		})
		rec.HostSays, rec.HostReason = p.Decision, p.Reason
		if err != nil {
			rec.Error = err.Error()
		}
		// The narrowing rule: deny always lands; approve lands only because the
		// envelope already placed this request in the consultable set; anything
		// else leaves it for a human.
		switch p.Decision {
		case "deny":
			action = backend.ApprovalReject
		case "approve":
			action = backend.ApprovalApprove
		default:
			rec.LatencyMS = ms(cfg.Now().Sub(start))
			return rec
		}
	default: // Escalate
		rec.LatencyMS = ms(cfg.Now().Sub(start))
		return rec
	}

	if cfg.MaxPerHour > 0 {
		*window = prune(*window, cfg.Now())
		if len(*window) >= cfg.MaxPerHour {
			rec.Error = fmt.Sprintf("hourly budget of %d autonomous decisions reached — left for a human", cfg.MaxPerHour)
			rec.LatencyMS = ms(cfg.Now().Sub(start))
			return rec
		}
	}

	if cfg.Shadow {
		// Shadow makes no decision and says what it would have made. Keeping
		// those two fields separate is the whole point of the mode.
		rec.WouldApply = action
		rec.LatencyMS = ms(cfg.Now().Sub(start))
		return rec
	}

	if err := q.DecideApproval(ctx, ap.AgentID, ap.ID, action); err != nil {
		rec.Error = err.Error()
		rec.LatencyMS = ms(cfg.Now().Sub(start))
		return rec
	}
	rec.Applied = action
	*window = append(*window, cfg.Now())
	rec.LatencyMS = ms(cfg.Now().Sub(start))
	return rec
}

// prune drops decisions older than the budget window.
func prune(window []time.Time, now time.Time) []time.Time {
	cut := now.Add(-time.Hour)
	kept := window[:0]
	for _, t := range window {
		if t.After(cut) {
			kept = append(kept, t)
		}
	}
	return kept
}

func appendRecord(cfg Config, rec Record) {
	if cfg.EvidencePath == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(cfg.EvidencePath), 0o700); err != nil {
		fmt.Fprintf(cfg.Log, "approver: evidence dir: %v\n", err)
		return
	}
	f, err := os.OpenFile(cfg.EvidencePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		fmt.Fprintf(cfg.Log, "approver: evidence open: %v\n", err)
		return
	}
	defer f.Close()
	line, err := json.Marshal(rec)
	if err != nil {
		return
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		fmt.Fprintf(cfg.Log, "approver: evidence write: %v\n", err)
	}
}

func ms(d time.Duration) int64 { return d.Milliseconds() }

func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func shadowNote(shadow bool) string {
	if shadow {
		return " (shadow — nothing was decided)"
	}
	return ""
}

func derefReason(r *string) string {
	if r == nil {
		return ""
	}
	return *r
}
