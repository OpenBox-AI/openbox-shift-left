// Package telemetryemit turns Claude Code's own OpenTelemetry export into
// governance events.
//
// It lives in the CLI rather than in package telemetry for the same reason
// gatewayemit lives here rather than in package gateway: telemetry's import
// guard (telemetry/guard_test.go) allows the collector family and nothing else,
// and that guard is what quarantines a ~492-package dependency tree from the
// rest of the repo. A mapper needs `client`, so it cannot live behind that
// guard.
//
// What this lane is FOR, and its honest standing: it is the only lane that
// reaches the desktop app and subscription-OAuth sessions, because it rides the
// env block of ~/.claude/settings.json rather than per-client routing. It is
// also the WEAKEST claim in the product (ADR-0022) — it is the governed tool
// reporting its own calls, suppressible by the thing it observes. Never average
// it together with the in-path lanes into "model calls are governed". OD4 is the
// compensating control: telemetry silence on an otherwise-active session is a
// finding.
package telemetryemit

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"time"

	"github.com/openbox-ai/openbox-shift-left/client"
	"github.com/openbox-ai/openbox-shift-left/telemetry"
)

// Policy is the emission policy, and its ZERO VALUE SUPPRESSES.
//
// That is a correctness invariant, not a default. Two lanes can observe the same
// model call, and core does NOT absorb one as a duplicate of the other — the
// activity_id namespaces are disjoint by design, which is what prevents silent
// LOSS. Nothing prevents silent DOUBLING except exactly one lane emitting, and
// that election is phase 12's, which does not exist yet.
//
// So until it does, the only policy a caller can construct without naming
// Elected is the one that emits nothing. A half-built lane cannot corrupt a
// dashboard.
type Policy struct {
	// Elected reports that this lane is the chosen model-call producer.
	// Precedence is transport > gateway > telemetry: in-path outranks
	// client-asserted.
	//
	// A FUNCTION, not a bool, and that is load-bearing rather than stylistic.
	// The election changes WHILE THIS PROCESS RUNS: `openbox init --full`
	// installs telemetry first and transport second, so a daemon that resolved
	// the election once at startup booted correctly elected, froze that answer,
	// and kept emitting after the transport lane took the election from it —
	// both lanes then emitting a turn for the same model call. The namespaces
	// are deliberately disjoint so core's dedupe cannot merge them, so nothing
	// errors and every token count doubles. The reverse is just as bad and
	// quieter: remove the stronger lane and a daemon frozen at "not elected"
	// stays silent forever.
	//
	// A snapshot of derivable state is exactly what deriving the election was
	// meant to eliminate. Resolving live is the only form with nothing to keep
	// in sync — no restart choreography, and correct when the settings file is
	// changed by a hand edit or an MDM deployment rather than by this CLI.
	//
	// NIL SUPPRESSES, which keeps the zero value's guarantee structural: a
	// half-built caller that never names Elected emits nothing.
	Elected func() bool
}

// elected answers the policy's question, treating an unset gate as "no".
func (p Policy) elected() bool { return p.Elected != nil && p.Elected() }

// Outcome says what happened to a record, because "nothing was emitted" covers
// two very different situations and only one of them is fine.
//
// A SKIP is the expected case: the export carries 19 event types and this slice
// binds one, so most records are simply not interesting. A DROP is a record this
// lane WANTED and could not use — a malformed id, an unusable session, a missing
// timestamp. Collapsing both into a bare false is what phase 10's own report
// flagged as dangerous: this package's argument is that erroring on an
// unfamiliar event NAME would turn upstream drift into a lane outage, and
// id-format drift is the same class. A lane that goes quiet because every record
// now fails validation must not look identical to a quiet session.
//
// The daemon counts drops and warns; skips are not worth counting.
type Outcome int

const (
	// Emitted: the event is usable.
	Emitted Outcome = iota
	// SkipNotElected: another lane owns this session's model calls.
	SkipNotElected
	// SkipUnhandledEvent: a record type this slice does not bind.
	SkipUnhandledEvent
	// DropBadSession: session.id absent or unusable as an identity and a filename.
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

// Mapper maps telemetry records to events. It holds no per-session state: every
// event this slice produces is derivable from a single record, which is what
// keeps it safe to call from the receiver's consumer goroutines.
type Mapper struct {
	did    string
	policy Policy
}

// New builds a mapper for one developer identity.
//
// It takes no redactor, deliberately. Nothing this slice binds is content —
// api_request carries a model id, four token counts, a cost, a duration and two
// request ids, and no free text at all. A redactor parameter with no call site
// would read like a wired control and not be one, which is precisely the shape
// that let the gateway discard every capture it made. It arrives with the body
// attachment that needs it.
func New(did string, p Policy) *Mapper {
	return &Mapper{did: did, policy: p}
}

// EventFor maps one record to one event, or reports false when the record maps
// to nothing.
//
// False is the normal answer for most records. The export emits 19 event types
// and this slice binds one; an unrecognised name is IGNORED rather than an
// error, because the export is a provider surface behind a beta flag (OD3) and
// erroring on an unfamiliar name would turn a routine upstream addition into a
// lane outage.
func (m *Mapper) EventFor(rec telemetry.Record) (client.DevEvent, Outcome) {
	if m == nil || !m.policy.elected() {
		return client.DevEvent{}, SkipNotElected
	}
	if rec.EventName != eventAPIRequest {
		return client.DevEvent{}, SkipUnhandledEvent
	}
	return m.turnFor(rec)
}

// eventAPIRequest is the provider's discriminator for one completed model call.
//
// One turn per api_request, and only per api_request. The corpus also carries
// api_request_body / api_response_body for the SAME call, and a
// claude_code.llm_request SPAN for it as well; binding any of those as a second
// producer would triple-count within this one lane, which no namespace and no
// election protects against.
const eventAPIRequest = "api_request"

// maxRequestIDLen bounds a provider value before it becomes part of event
// identity. 128 is generous against the observed shapes (a 29-char `req_…` and a
// 36-char UUID) and small enough that activity_id stays a sane key.
const maxRequestIDLen = 128

// synthesizedLLMURL must be a host core's isLLMCall matches, or the span
// classifies as something else and every model-call reader goes quiet. The turn
// span in client/turnspan.go asserts the same URL for the same reason.
const synthesizedLLMURL = "https://api.anthropic.com/v1/messages"

func (m *Mapper) turnFor(rec telemetry.Record) (client.DevEvent, Outcome) {
	session := rec.Attrs["session.id"]
	if !safeSessionID(session) {
		return client.DevEvent{}, DropBadSession
	}
	if rec.Timestamp.IsZero() {
		// record.go binds the record's own time and leaves a zero to "the mapper
		// to decide what to do about". This is the decision: drop it. Formatting a
		// zero time yields a valid RFC3339 string in year 0001, so nothing
		// downstream would reject it — the turn would simply be filed a
		// millennium out, and every window and latency reader would quietly
		// disagree with every other lane.
		return client.DevEvent{}, DropNoTimestamp
	}
	reqID, ok := requestIDFrom(rec.Attrs)
	if !ok {
		return client.DevEvent{}, DropNoRequestID
	}

	end := rec.Timestamp.UTC()
	start := end
	if d, ok := parseInt(rec.Attrs["duration_ms"]); ok && d > 0 {
		// The export reports a duration, not a start. Without deriving the window
		// the span's start and end collapse and every latency reader shows zero.
		start = end.Add(-time.Duration(d) * time.Millisecond)
	}

	ev := client.DevEvent{
		SchemaVersion: client.SchemaVersion,
		EventType:     client.EventTurnCompleted,
		SessionID:     session,
		DeveloperDID:  m.did,
		Timestamp:     end.Format(time.RFC3339Nano),
		StartedAt:     start.Format(time.RFC3339Nano),
		EndedAt:       end.Format(time.RFC3339Nano),
		// A model call is not a shell/file/MCP invocation, but Tool is a required
		// wire object. It names the governed TOOL, which is what every other
		// event from this machine already reports. Same choice gatewayemit makes.
		Tool:  client.Tool{Name: "claude-code", Kind: client.ToolShell},
		Model: rec.Attrs["model"],
		// The lane discriminator (ADR-0022). Without it turnActivityIDFor falls
		// through to the hook path's TurnIndex branch and, with no index, returns
		// an EMPTY activity_id.
		OtelRequestID: reqID,
		Tokens:        tokensFrom(rec.Attrs),
		// The span exists for ONE reader: core recomputes semantic_type per span,
		// and isLLMCall's attribute inputs are the only path to llm_completion.
		// http_method and http_url are SYNTHESIZED here — the export carries
		// neither — and client marks them as such on this lane. http_status is
		// deliberately left unset: api_request reports no status (only api_error
		// does), and asserting 200 would be inventing an observation.
		Span: &client.Span{
			SemanticType: "llm_completion",
			Stage:        "completed",
			HTTPMethod:   "POST",
			HTTPURL:      synthesizedLLMURL,
		},
	}
	ev.EventID = eventID(session, reqID, string(ev.EventType), ev.Timestamp)
	return ev, Emitted
}

// requestIDFrom picks the id that becomes part of activity_id, and validates it.
//
// It is a provider value arriving straight off an unauthenticated loopback
// listener, and activity_id is this product's event identity — byte-pinned and
// load-bearing for core's dedupe. So it is bounded and charset-checked, and a
// value that fails is a DROPPED turn rather than a malformed identity: a gap is
// recoverable, a colliding or ambiguous activity_id corrupts a stored row.
//
// ':' is the one that matters. activity_id reads "<session>:otel:<id>", so a
// colon inside the id makes the namespace boundary ambiguous — and the
// namespaces are what keep two lanes' evidence from being merged.
//
// No id is minted when both are absent. A locally minted id would break INV-5:
// the spool can be drained by a different process long after the daemon exited,
// and a re-flush must present the same idempotency key.
func requestIDFrom(attrs map[string]string) (string, bool) {
	for _, key := range []string{"request_id", "client_request_id"} {
		if id := attrs[key]; safeRequestID(id) {
			return id, true
		}
	}
	return "", false
}

// safeSessionID validates the session id, which is a provider value arriving off
// the same unauthenticated loopback listener as everything else here.
//
// It needs at least as much rigor as the request id, and arguably more: it is the
// activity_id PREFIX, it becomes core's run_id, and every spool consumer in this
// repo turns it into a FILENAME (`<session>.jsonl`, hookflow/spool.go). An
// unchecked value from a local process therefore reaches a path join.
//
// The charset does the structural work — no `/` or `\` can survive it, so
// traversal is unrepresentable rather than merely forbidden — and `.`/`..` are
// rejected as whole tokens on top, because both pass a charset test and neither
// is a filename anyone wants. Measured safe against the corpus: all 59 real
// session ids are UUIDs.
//
// gatewayemit.usableSessionID makes the same refusal for the same reason; the
// rules are deliberately NOT shared, because that one's printableASCII admits
// ':' and this lane's namespace argument forbids it.
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

// tokensFrom reads the four counts.
//
// `input_tokens` is PURE input and is passed through as such. Measured on the
// corpus: input=2 beside cache_read=90485, so it excludes cache — exactly
// contract v1.1's redefinition. Adding cache into it would double-count ~90k
// tokens on a single call.
//
// A MISSING count and a MALFORMED one are different, and conflating them is how
// spend gets understated. Missing means not applicable: the field stays nil and
// the total is still meaningful. Malformed means a number was reported and could
// not be read: the field stays nil AND the total is withheld, because a sum that
// silently omits a component reads as authoritative and is wrong.
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

// parseInt reads a count from its string form.
//
// Every attribute arrives as a string because consume.go flattens all OTLP value
// types through AsString — which is load-bearing, not lazy: the provider types
// the SAME attribute differently per event (duration_ms is intValue on
// api_request and stringValue on tool_result), so a typed read would return zero
// on one of them with no error.
//
// Negative counts are rejected rather than clamped: a negative token count means
// the export is not what this mapper thinks it is, and a silent 0 would hide it.
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

// eventID derives the idempotency key (INV-5). Deterministic for the reason
// gatewayemit's copy gives: the spool outlives the process that wrote it, so a
// redelivery has to present the same key or core stores a second row.
func eventID(session, reqID, eventType, ts string) string {
	sum := sha256.Sum256([]byte("otelemit\x1f" + session + "\x1f" + reqID + "\x1f" + eventType + "\x1f" + ts))
	return "otel-" + hex.EncodeToString(sum[:16])
}
