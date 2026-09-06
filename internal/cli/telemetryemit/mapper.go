// Package telemetryemit turns Claude Code's own OpenTelemetry export into
// governance events. A mapper needs `client`, so it cannot live behind that
// guard. Never average it together with the in-path lanes into "model calls
// are governed".
package telemetryemit

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"time"

	obgit "github.com/openbox-ai/openbox-shift-left/internal/adapters/common/git"
	"github.com/openbox-ai/openbox-shift-left/internal/client"
	"github.com/openbox-ai/openbox-shift-left/internal/telemetry"
)

// Policy is the emission policy, and its zero value suppresses. A half-built
// lane cannot corrupt a dashboard.
type Policy struct {
	// Elected reports that this lane is the chosen model-call producer. A
	// function, not a bool, and that is load-bearing rather than stylistic.
	Elected func() bool
}

func (p Policy) elected() bool { return p.Elected != nil && p.Elected() }

// Outcome says what happened to a record, because "nothing was emitted" covers
// two very different situations and only one of them is fine. A lane that goes
// quiet because every record now fails validation must not look identical to a
// quiet session.
type Outcome int

const (
	// Emitted: the event is usable.
	Emitted Outcome = iota
	// SkipNotElected: another lane owns this session's model calls.
	SkipNotElected
	// SkipUnhandledEvent: a record type this slice does not bind.
	SkipUnhandledEvent
	// DropBadSession: session.id absent or unusable as an identity and a
	// filename.
	DropBadSession
	// DropNoRequestID: neither request id is present and usable.
	DropNoRequestID
	// DropNoTimestamp: the record carries no time of its own.
	DropNoTimestamp
)

// IsDrop reports whether this outcome lost a record the lane wanted.
func (o Outcome) IsDrop() bool {
	return o == DropBadSession || o == DropNoRequestID || o == DropNoTimestamp
}

// String names the outcome for a counter key or a log line. Deliberately terse
// and stable: these strings reach operator-facing output.
func (o Outcome) String() string {
	switch o {
	case Emitted:
		return "emitted"
	case SkipNotElected:
		return "not-elected"
	case SkipUnhandledEvent:
		return "unhandled-event"
	case DropBadSession:
		return "bad-session-id"
	case DropNoRequestID:
		return "no-request-id"
	case DropNoTimestamp:
		return "no-timestamp"
	}
	return "unknown"
}

// Mapper maps telemetry records to events.
type Mapper struct {
	did    string
	policy Policy
	// runStore resolves this session's run identity (RunID/RunGeneration,
	// phase 08 R7). The zero value resolves to runs/ under the resolved
	// DefaultSessionDir() -- what a production daemon gets from its unit's
	// OPENBOX_SESSION_DIR (insight 7, a daemon has no $HOME) -- and a test
	// points Dir at a temp directory so it never touches the developer's real
	// registry.
	runStore obgit.RunStore
}

// New builds a mapper for one developer identity. It takes no redactor,
// deliberately.
func New(did string, p Policy) *Mapper {
	return &Mapper{did: did, policy: p}
}

// EventsFor maps one record to the PAIR one model turn is -- BOTH halves, which is
// the point. Until 1.7 this lane returned the close alone, so every row it
// produced was unpaired; returning one event again reintroduces that.
func (m *Mapper) EventsFor(rec telemetry.Record) ([]client.DevEvent, Outcome) {
	if m == nil || !m.policy.elected() {
		return nil, SkipNotElected
	}
	if rec.EventName != eventAPIRequest {
		return nil, SkipUnhandledEvent
	}
	return m.turnFor(rec)
}

const eventAPIRequest = "api_request"

const maxRequestIDLen = 128

const synthesizedLLMURL = "https://api.anthropic.com/v1/messages"

func (m *Mapper) turnFor(rec telemetry.Record) ([]client.DevEvent, Outcome) {
	session := rec.Attrs["session.id"]
	if !safeSessionID(session) {
		return nil, DropBadSession
	}
	if rec.Timestamp.IsZero() {
		return nil, DropNoTimestamp
	}
	reqID, ok := requestIDFrom(rec.Attrs)
	if !ok {
		return nil, DropNoRequestID
	}

	end := rec.Timestamp.UTC()
	start := end
	if d, ok := parseInt(rec.Attrs["duration_ms"]); ok && d > 0 {
		start = end.Add(-time.Duration(d) * time.Millisecond)
	}

	// R12: decided ONCE per Started/Completed pair, on the pair's own start
	// bound, before either half is built -- never per half, or one activity_id
	// would split across two run_ids. Read failure (absent/unreadable/
	// corrupt/inconsistent) fails open to generation 0 (INV-3).
	runID, runGen := "", 0
	if runRec, err := m.runStore.Read(session); err == nil && runRec.Generation > 0 {
		if start.UnixNano() < runRec.UpdatedAt {
			runID, runGen = runRec.PreviousRunID, runRec.Generation-1
		} else {
			runID, runGen = runRec.RunID, runRec.Generation
		}
	}

	half := func(eventType client.EventType, stage string, ts time.Time) client.DevEvent {
		ev := client.DevEvent{
			SchemaVersion: client.SchemaVersion,
			EventType:     eventType,
			SessionID:     session,
			DeveloperDID:  m.did,
			RunID:         runID,
			RunGeneration: runGen,
			Timestamp:     ts.Format(time.RFC3339Nano),
			StartedAt:     start.Format(time.RFC3339Nano),
			Tool:          client.Tool{Name: "claude-code", Kind: client.ToolShell},
			ActivityType:  client.ActivityTypeLLMCompletion,
			Model:         rec.Attrs["model"],
			OtelRequestID: reqID,
			Span: &client.Span{
				SemanticType: client.ActivityTypeLLMCompletion,
				Stage:        stage,
				HTTPMethod:   "POST",
				HTTPURL:      synthesizedLLMURL,
			},
		}
		// Usage is known only at the close; zero-filling would claim no spend.
		if eventType == client.EventTurnCompleted {
			ev.EndedAt = end.Format(time.RFC3339Nano)
			ev.Tokens = tokensFrom(rec.Attrs)
		}
		ev.EventID = eventID(session, reqID, string(ev.EventType), ev.Timestamp)
		return ev
	}

	return []client.DevEvent{
		half(client.EventTurnStarted, "started", start),
		half(client.EventTurnCompleted, "completed", end),
	}, Emitted
}

// requestIDFrom picks the id that becomes part of activity_id, and validates
// it.
func requestIDFrom(attrs map[string]string) (string, bool) {
	for _, key := range []string{"request_id", "client_request_id"} {
		if id := attrs[key]; safeRequestID(id) {
			return id, true
		}
	}
	return "", false
}

// safeSessionID gatewayemit.usableSessionID makes the same refusal for the
// same reason; the rules are deliberately NOT shared, because that one's
// printableASCII admits ':' and this lane's namespace argument forbids it.
func safeSessionID(s string) bool {
	if s == "." || s == ".." {
		return false
	}
	return safeRequestID(s)
}

func safeRequestID(s string) bool {
	if s == "" || len(s) > maxRequestIDLen {
		return false
	}
	return strings.IndexFunc(s, func(r rune) bool {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return false
		case r == '-', r == '_', r == '.':
			return false
		}
		return true
	}) < 0
}

// tokensFrom malformed means a number was reported and could not be read: the
// field stays nil AND the total is withheld, because a sum that silently omits
// a component reads as authoritative and is wrong.
func tokensFrom(attrs map[string]string) *client.Tokens {
	var (
		t       client.Tokens
		sum     int
		any     bool
		unknown bool
	)
	for _, f := range []struct {
		key  string
		dest **int
	}{
		{"input_tokens", &t.Input},
		{"output_tokens", &t.Output},
		{"cache_read_tokens", &t.CacheRead},
		{"cache_creation_tokens", &t.CacheCreationInput},
	} {
		raw, present := attrs[f.key]
		if !present {
			continue
		}
		n, ok := parseInt(raw)
		if !ok {
			unknown = true
			continue
		}
		v := n
		*f.dest = &v
		sum += n
		any = true
	}
	if !any {
		return nil
	}
	if !unknown {
		total := sum
		t.Total = &total
	}
	return &t
}

func parseInt(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

func eventID(session, reqID, eventType, ts string) string {
	sum := sha256.Sum256([]byte("otelemit\x1f" + session + "\x1f" + reqID + "\x1f" + eventType + "\x1f" + ts))
	return "otel-" + hex.EncodeToString(sum[:16])
}
