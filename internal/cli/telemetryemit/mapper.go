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
	"github.com/openbox-ai/openbox-shift-left/internal/cli/sessionkey"
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
	// SkipIncompleteCall: the call is observably incomplete and carries no
	// request id, which a failed call usually has not. That is the call failing,
	// not the lane losing a record, so it is not a drop.
	SkipIncompleteCall
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
	case SkipIncompleteCall:
		return "incomplete-call"
	}
	return "unknown"
}

// Mapper maps telemetry records to events.
type Mapper struct {
	did    string
	policy Policy
	// runStore resolves this session's run identity (RunID/RunGeneration),
	// the same record a hook event reads. The zero value resolves to runs/ under the resolved
	// DefaultSessionDir() -- what a production daemon gets from its unit's
	// OPENBOX_SESSION_DIR (a daemon has no $HOME) -- and a test
	// points Dir at a temp directory so it never touches the developer's real
	// registry.
	runStore obgit.RunStore
	// toolName stamps Tool.Name on every event this mapper builds. "" ⇒
	// "claude-code" (defaultToolName), so every caller before this field
	// existed keeps shipping the same byte-identical fixture: an idempotency
	// key must not move underneath an existing caller.
	toolName string
	// sessionAttr is the OTel attribute this mapper keys a turn's session
	// identity on. "" ⇒ "session.id" (defaultSessionAttr), which is every
	// existing caller's behavior -- Codex exports conversation.id instead, so
	// a caller that WANTS that surface opts in with WithSessionAttr; the
	// default path here is untouched.
	sessionAttr string
	// fields is which attributes of the record this mapper reads. The zero
	// value is Claude Code's (defaultFieldMap), which Codex shares, so every
	// existing caller keeps its byte-identical output.
	fields FieldMap
	// parentOf names the session a child's calls fold into, "" for none. It
	// reads the decision the hook path recorded; see WithParentOf.
	parentOf func(child string) string
}

// New builds a mapper for one developer identity. It takes no redactor,
// deliberately.
func New(did string, p Policy) *Mapper {
	return &Mapper{did: did, policy: p}
}

// WithToolName overrides Tool.Name on every event this mapper builds; "" (the
// zero value) keeps the default "claude-code". Returns the receiver so a
// caller can chain it onto New(...).
func (m *Mapper) WithToolName(name string) *Mapper {
	m.toolName = name
	return m
}

// WithSessionAttr overrides which OTel attribute this mapper reads a turn's
// session identity from; "" keeps the default "session.id". Codex exports
// conversation.id, not session.id -- a scoped, minimal attributability, not
// a full session-key package.
func (m *Mapper) WithSessionAttr(attr string) *Mapper {
	m.sessionAttr = attr
	return m
}

// FieldMap names where one tool's model-call record keeps the facts the
// mapper needs. The mapper's logic is the same for every tool; only the
// attribute names differ, so a tool is a FieldMap rather than a second mapper.
// A key left empty is a fact the tool does not export, and is not read.
type FieldMap struct {
	// Event is the event name that IS a model call; every other is skipped.
	Event string
	// RequestIDKeys are tried in order for the id that becomes the activity id.
	RequestIDKeys []string
	// ModelKey, DurationKey (milliseconds) and the token keys name the numbers.
	ModelKey    string
	DurationKey string
	InputTokens, OutputTokens,
	CacheReadTokens, CacheCreationTokens string
	// InputIncludesCache is true when the input count already contains the
	// cache-read tokens (the OpenAI and GenAI-semconv convention), so the total
	// must not add them a second time. Anthropic reports them separately.
	InputIncludesCache bool
	// ProviderKey, when set, is copied into metadata as `provider`.
	ProviderKey string
	// KindKey names what kind of session the record's own is (Muse: reminder),
	// carried as metadata subagent_kind on a call folded into its parent's
	// session. Whether a call folds is not a field of the record: see WithParentOf.
	KindKey string
	// URL is the synthesized stand-in for the call's endpoint on the span.
	URL string
	// CleanKey, when set, names a flag that says the call's stream ended cleanly
	// (Muse: eof_clean, "1" on every call of the scrubbed export). Only an
	// explicit false marks the call incomplete; absent or unreadable is unknown,
	// and unknown is not a failure. The tool exports no other outcome (no status,
	// error or HTTP code on the model_call record). The "not clean" value has not
	// been observed, so false is read as 0 or false, the encoding the same
	// record uses for has_tool_calls.
	CleanKey string

	// OnlyAttr/OnlyValue, when set, narrow Event to the records whose attribute
	// OnlyAttr equals OnlyValue: a tool that exports one event name for several
	// kinds of record (Codex's codex.sse_event carries every stream event) has
	// exactly one that is a model call.
	OnlyAttr, OnlyValue string
	// MintRequestID derives the request id from the record instead of reading
	// one, for a tool whose per-call record carries none (Codex). The id is a
	// pure function of the thread, the record's own times and its token counts,
	// so a restarted daemon reproduces it and no state is kept.
	MintRequestID bool
}

// defaultFieldMap is Claude Code's api_request, and Codex's too.
var defaultFieldMap = FieldMap{
	Event:               eventAPIRequest,
	RequestIDKeys:       []string{"request_id", "client_request_id"},
	ModelKey:            "model",
	DurationKey:         "duration_ms",
	InputTokens:         "input_tokens",
	OutputTokens:        "output_tokens",
	CacheReadTokens:     "cache_read_tokens",
	CacheCreationTokens: "cache_creation_tokens",
	URL:                 synthesizedLLMURL,
}

// MuseFieldMap is Muse Code's `model_call` log record, read off Muse 1.4.1's
// own export (GenAI semconv 1.34, snake_case): the provider's response id with
// no fallback (Muse's message id can recur across calls, and a shared id would
// let core's dedupe absorb one call as a duplicate of another), and metadata only at the export level: Muse exports no
// content, so none is read here. The enricher may attach bodies it joins locally
// (session journal, request stash) under content_capture. Its input count includes the cached tokens.
var MuseFieldMap = FieldMap{
	Event:              "model_call",
	RequestIDKeys:      []string{"gen_ai_response_id"},
	ModelKey:           "gen_ai_request_model",
	DurationKey:        "duration_ms",
	InputTokens:        "gen_ai_usage_input_tokens",
	OutputTokens:       "gen_ai_usage_output_tokens",
	CacheReadTokens:    "tokens_cached",
	InputIncludesCache: true,
	ProviderKey:        "gen_ai_provider_name",
	KindKey:            "session_kind",
	CleanKey:           "eof_clean",
	URL:                "https://api.meta.ai/",
}

// CodexFieldMap is Codex's `codex.sse_event` log record with
// event.kind=response.completed, read off Codex 0.156.1's own export: the one
// per-call record that carries the thread (conversation.id), the model and the
// token counts. Its sibling codex.api_request carries none of those, and
// Codex's model traffic is a websocket, so the HTTP-shaped record Claude Code
// exports does not exist. No record carries a request id, so one is minted; the
// input count includes the cached tokens (the OpenAI convention).
var CodexFieldMap = FieldMap{
	Event:               "codex.sse_event",
	OnlyAttr:            "event.kind",
	OnlyValue:           "response.completed",
	MintRequestID:       true,
	ModelKey:            "model",
	InputTokens:         "input_token_count",
	OutputTokens:        "output_token_count",
	CacheReadTokens:     "cached_token_count",
	CacheCreationTokens: "cache_write_token_count",
	InputIncludesCache:  true,
	URL:                 "https://api.openai.com/v1/responses",
}

// WithParentOf sets how a record's session is folded into a parent's: the
// function returns the parent a child session was recorded under, or "" when
// none was. A call folds only when it names a usable session other than the
// record's own, so a child nothing links stays a session of its own -- the hook
// path's decision, which this must follow rather than re-derive from the
// record: the export's own root attribute names a root for a child the hook
// path never folded, and the two lanes would then put one session's rows in
// different sessions and runs. Nil never folds.
func (m *Mapper) WithParentOf(f func(child string) string) *Mapper {
	m.parentOf = f
	return m
}

// WithFieldMap selects which attributes this mapper reads; the zero FieldMap
// keeps Claude Code's. Returns the receiver so a caller can chain it.
func (m *Mapper) WithFieldMap(f FieldMap) *Mapper {
	m.fields = f
	return m
}

func (m *Mapper) fieldMap() FieldMap {
	if m.fields.Event == "" {
		return defaultFieldMap
	}
	return m.fields
}

// defaultToolName is what an unset Mapper.toolName resolves to.
const defaultToolName = "claude-code"

func (m *Mapper) toolNameOrDefault() string {
	if m.toolName == "" {
		return defaultToolName
	}
	return m.toolName
}

// defaultSessionAttr is what an unset Mapper.sessionAttr resolves to -- the
// attribute every mapper read before this field existed. Named through
// sessionkey rather than as a second copy of the literal; the
// value is unchanged (sessionkey.OTelAttr's own default), so every existing
// caller stays byte-identical.
var defaultSessionAttr = sessionkey.OTelAttr(sessionkey.ClaudeCode)

func (m *Mapper) sessionAttrOrDefault() string {
	if m.sessionAttr == "" {
		return defaultSessionAttr
	}
	return m.sessionAttr
}

// EventsFor maps one record to the PAIR one model turn is -- BOTH halves, which is
// the point. Until 1.7 this lane returned the close alone, so every row it
// produced was unpaired; returning one event again reintroduces that.
func (m *Mapper) EventsFor(rec telemetry.Record) ([]client.DevEvent, Outcome) {
	t, out := m.TurnFor(rec)
	return t.Events, out
}

// Turn is one mapped model call: the pair, plus the session id as the record
// named it BEFORE the parent fold. The pair's events carry the folded id (the
// run they belong to); a producer that looks the call's content up in the
// tool's own files needs the unfolded one, because a child's log is filed under
// the child. Session is a side channel for in-process callers and never reaches
// the wire.
type Turn struct {
	Events  []client.DevEvent
	Session string
}

// TurnFor is EventsFor with the unfolded session id alongside the pair.
func (m *Mapper) TurnFor(rec telemetry.Record) (Turn, Outcome) {
	if m == nil || !m.policy.elected() {
		return Turn{}, SkipNotElected
	}
	fm := m.fieldMap()
	if rec.EventName != fm.Event {
		return Turn{}, SkipUnhandledEvent
	}
	if fm.OnlyAttr != "" && rec.Attrs[fm.OnlyAttr] != fm.OnlyValue {
		return Turn{}, SkipUnhandledEvent
	}
	events, unfolded, out := m.turnFor(rec, fm)
	return Turn{Events: events, Session: unfolded}, out
}

const eventAPIRequest = "api_request"

const maxRequestIDLen = 128

const synthesizedLLMURL = "https://api.anthropic.com/v1/messages"

func (m *Mapper) turnFor(rec telemetry.Record, fm FieldMap) ([]client.DevEvent, string, Outcome) {
	session := rec.Attrs[m.sessionAttrOrDefault()]
	if !safeSessionID(session) {
		return nil, "", DropBadSession
	}
	unfolded := session
	if rec.Timestamp.IsZero() {
		return nil, "", DropNoTimestamp
	}
	incomplete := fm.CleanKey != "" && notClean(rec.Attrs[fm.CleanKey])
	reqID, ok := requestIDFrom(rec.Attrs, fm.RequestIDKeys)
	if fm.MintRequestID {
		reqID, ok = mintRequestID(session, rec, fm), true
	}
	if !ok {
		if incomplete {
			return nil, "", SkipIncompleteCall
		}
		return nil, "", DropNoRequestID
	}

	// The fold is decided before the run is read, so a subagent's call lands in
	// the parent's run and the parent's halt latch. A session with no recorded
	// parent stays in its own, exactly as an unlinked hook child does.
	folded := false
	if m.parentOf != nil {
		if parent := m.parentOf(session); parent != session && safeSessionID(parent) {
			session, folded = parent, true
		}
	}

	end := rec.Timestamp.UTC()
	start := end
	if d, ok := parseInt(rec.Attrs[fm.DurationKey]); fm.DurationKey != "" && ok && d > 0 {
		start = end.Add(-time.Duration(d) * time.Millisecond)
	}

	// The straddle rule: decided ONCE per Started/Completed pair, on the pair's own start
	// bound, before either half is built -- never per half, or one activity_id
	// would split across two run_ids. Read failure (absent/unreadable/
	// corrupt/inconsistent) fails open to generation 0.
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
			Tool:          client.Tool{Name: m.toolNameOrDefault(), Kind: client.ToolShell},
			ActivityType:  client.ActivityTypeLLMCompletion,
			Model:         rec.Attrs[fm.ModelKey],
			OtelRequestID: reqID,
			Metadata:      metadataFor(rec.Attrs, fm, folded),
			Span: &client.Span{
				SemanticType: client.ActivityTypeLLMCompletion,
				Stage:        stage,
				HTTPMethod:   "POST",
				HTTPURL:      fm.URL,
			},
		}
		// Usage is known only at the close; zero-filling would claim no spend.
		if eventType == client.EventTurnCompleted {
			ev.EndedAt = end.Format(time.RFC3339Nano)
			ev.Tokens = tokensFrom(rec.Attrs, fm)
			// The contract carries a failed status for a tool result and a model-call
			// finish, not for a turn's close, so a partial call is marked here
			// instead of reading as a clean turn.
			if incomplete {
				marked := make(map[string]any, len(ev.Metadata)+1)
				for k, v := range ev.Metadata {
					marked[k] = v
				}
				marked["response_incomplete"] = true
				ev.Metadata = marked
			}
		}
		ev.EventID = eventID(session, reqID, string(ev.EventType), ev.Timestamp)
		return ev
	}

	return []client.DevEvent{
		half(client.EventTurnStarted, "started", start),
		half(client.EventTurnCompleted, "completed", end),
	}, unfolded, Emitted
}

// mintRequestID names a call whose record carries no id: the thread, the
// record's own time (the export's and the attribute's, so a clock that rounds
// one still separates two calls) and the counts. Two calls of one thread cannot
// share it unless they are the same record.
func mintRequestID(session string, rec telemetry.Record, fm FieldMap) string {
	h := sha256.New()
	for _, part := range []string{session, strconv.FormatInt(rec.Timestamp.UnixNano(), 10), rec.Attrs["event.timestamp"],
		rec.Attrs[fm.InputTokens], rec.Attrs[fm.OutputTokens], rec.Attrs[fm.CacheReadTokens]} {
		h.Write([]byte(part))
		h.Write([]byte{0x1f})
	}
	return "call-" + hex.EncodeToString(h.Sum(nil)[:12])
}

// notClean reports an explicit "no" from a clean-completion flag.
func notClean(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "0", "false":
		return true
	}
	return false
}

// requestIDFrom picks the id that becomes part of activity_id, and validates
// it.
func requestIDFrom(attrs map[string]string, keys []string) (string, bool) {
	for _, key := range keys {
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
func tokensFrom(attrs map[string]string, fm FieldMap) *client.Tokens {
	var (
		t       client.Tokens
		sum     int
		any     bool
		unknown bool
	)
	for _, f := range []struct {
		key      string
		dest     **int
		addToSum bool
	}{
		{fm.InputTokens, &t.Input, true},
		{fm.OutputTokens, &t.Output, true},
		// A cache read already inside the input count is not a second spend.
		{fm.CacheReadTokens, &t.CacheRead, !fm.InputIncludesCache},
		{fm.CacheCreationTokens, &t.CacheCreationInput, !fm.InputIncludesCache},
	} {
		if f.key == "" {
			continue
		}
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
		if f.addToSum {
			sum += n
		}
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

// maxIdentifierBytes bounds capIdentifier's output. 256 bytes is far above any
// real vendor identifier (prompt.id is a UUID, 36 bytes) and far below
// anything that could crowd metadata, mirroring modelBudget's reasoning
// (internal/gateway/requestselect.go:56). telemetry.MaxAttrValueBytes, the
// collection-layer bound, is four orders of magnitude too loose for an
// identifier on its own.
const maxIdentifierBytes = 256

// capIdentifier bounds a vendor identifier to maxIdentifierBytes, backing off
// to the nearest rune boundary instead of splitting a multi-byte UTF-8
// sequence in half.
//
// Deliberately NOT enumOr: query_source's vocabulary is vendor-owned and
// grows (34 literals observed in the provider binary, 6 in the corpus), and
// the repo already states the reason at
// internal/adapters/claude-code/mapper.go:257-259 -- an unconfirmed
// allowlist silently discards real data. capStr there is unreachable from
// this package (telemetryemit imports no adapter) and counts RUNES rather
// than bytes, so it would not serve as this bound either.
func capIdentifier(s string) string {
	if len(s) <= maxIdentifierBytes {
		return s
	}
	return strings.ToValidUTF8(s[:maxIdentifierBytes], "")
}

// attributionMetadata is the classifier a renderer needs to tell a
// conversation turn from a background chore: which query produced this call,
// and which prompt it belongs to. It reads exactly these two keys out of the
// merged attribute map and no others -- the lane's content sentinel
// (TestNoContentOnWireAtEitherPosture in sentinel_test.go) is the backstop
// that would catch this ever widening into a copy of more than two keys.
//
// Absence of query_source means unclassified: neither "background" nor
// "conversation". There is no default and no "sdk" fallback -- a producer
// that cannot tell must say nothing rather than guess, because a wrong guess
// would let a background call render as a message the user never sent.
//
// Returns nil, not an empty map, when both keys are absent, so
// DevEvent.Metadata stays nil exactly as it did before this bind existed.
func attributionMetadata(attrs map[string]string) map[string]any {
	m := make(map[string]any, 2)
	if v := attrs["query_source"]; v != "" {
		m["query_source"] = capIdentifier(v)
	}
	// Wire key is prompt_id -- the well-known correlation key docs/mapping.md
	// already documents -- not the OTel attribute name prompt.id.
	if v := attrs["prompt.id"]; v != "" {
		m["prompt_id"] = capIdentifier(v)
	}
	if len(m) == 0 {
		return nil
	}
	return m
}

// subagentAgentType is the agent_type the hook path tags a folded Muse child
// with; the same two keys (agent_type, agent_id) carry it here.
const subagentAgentType = "subagent"

// metadataFor is attributionMetadata plus what a tool's FieldMap adds: the
// provider, and -- for a call folded into its parent's session -- the subagent
// tags. Each value is an identifier bounded by capIdentifier, never content. A
// map with neither extra is attributionMetadata exactly.
func metadataFor(attrs map[string]string, fm FieldMap, folded bool) map[string]any {
	m := attributionMetadata(attrs)
	add := func(key, value string) {
		if value == "" {
			return
		}
		if m == nil {
			m = make(map[string]any, 3)
		}
		m[key] = capIdentifier(value)
	}
	if fm.ProviderKey != "" {
		add("provider", attrs[fm.ProviderKey])
	}
	if folded {
		add("agent_type", subagentAgentType)
		if fm.KindKey != "" {
			// Not agent_id: the hook path's agent_id is the subagent's own id (for
			// example skill-reminder), which the export does not carry, and a kind
			// under that key would make the two lanes disagree about the same child.
			add("subagent_kind", attrs[fm.KindKey])
		}
	}
	return m
}
