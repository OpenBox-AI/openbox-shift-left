package gatewayemit

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/client"
	"github.com/openbox-ai/openbox-shift-left/internal/gateway"
)

// TestClassifyPath is the table, asserted directly. Classifying on the HTTP
// method filed every POST as a completion, and roughly 40% of `llm_completion`
// rows were token-count probes.
func TestClassifyPath(t *testing.T) {
	for _, tc := range []struct {
		url  string
		want PathClass
	}{
		{"https://api.anthropic.com/v1/messages", ClassCompletion},
		{"https://api.anthropic.com/v1/messages/", ClassCompletion},
		{"https://api.anthropic.com/v1/messages/count_tokens", ClassTokenCount},
		{"https://api.anthropic.com/api/event_logging/v2/batch", ClassToolTelemetry},
		{"https://api.anthropic.com/api/claude_code/metrics", ClassToolTelemetry},
		{"https://api.anthropic.com/api/claude_code/policy_limits", ClassToolTelemetry},
		{"https://api.anthropic.com/api/claude_cli/bootstrap", ClassToolTelemetry},
		{"https://api.anthropic.com/v1/models", ClassUnknown},
		{"https://api.anthropic.com/v1/something-new", ClassUnknown},
		{"/v1/messages", ClassCompletion},
	} {
		if got := classifyPath(tc.url); got != tc.want {
			t.Errorf("classifyPath(%q) = %v, want %v", tc.url, got, tc.want)
		}
	}
}

// TestClassificationIsQueryIndependentAndHostInsensitive the capture already
// strips the query, but classification must not depend on that having happened
// upstream, and a host that differs only in case is the same host.
func TestClassificationIsQueryIndependentAndHostInsensitive(t *testing.T) {
	for _, url := range []string{
		"https://api.anthropic.com/v1/messages?beta=true",
		"https://API.ANTHROPIC.COM/v1/messages",
		"https://Api.Anthropic.Com/v1/messages?a=1&b=2",
	} {
		if got := classifyPath(url); got != ClassCompletion {
			t.Errorf("classifyPath(%q) = %v, want ClassCompletion", url, got)
		}
	}
	if got := classifyPath("https://API.ANTHROPIC.COM/v1/messages/count_tokens?x=1"); got != ClassTokenCount {
		t.Errorf("a query-carrying, upper-case-host probe classified as %v", got)
	}
}

// TestActivityTypesComeFromTheClosedVocabulary a class inventing a name outside
// AllActivityTypes would reach the dashboard's Activity column as an unknown
// value with nothing to reject it.
func TestActivityTypesComeFromTheClosedVocabulary(t *testing.T) {
	known := map[string]bool{}
	for _, a := range client.AllActivityTypes {
		known[a] = true
	}
	for _, class := range []PathClass{ClassCompletion, ClassTokenCount, ClassToolTelemetry, ClassUnknown} {
		if got := class.ActivityType(); !known[got] {
			t.Errorf("%v yields activity_type %q, which is not in client.AllActivityTypes", class, got)
		}
	}
}

// TestCompletionIsTheOnlyLLMCompletion is criterion 3 of the plan, asserted at
// the level that decides it.
func TestCompletionIsTheOnlyLLMCompletion(t *testing.T) {
	if got := ClassCompletion.ActivityType(); got != client.ActivityTypeLLMCompletion {
		t.Errorf("a completion stores as %q", got)
	}
	for _, class := range []PathClass{ClassTokenCount, ClassToolTelemetry, ClassUnknown} {
		if got := class.ActivityType(); got == client.ActivityTypeLLMCompletion {
			t.Errorf("%v stores as llm_completion; only a real completion may", class)
		}
	}
}

// capturedFor builds a session-bearing capture for one path.
func capturedFor(session, url string) gateway.Captured {
	c := capturedWithSession(session)
	c.HTTPMethod = http.MethodPost
	c.HTTPURL = url
	return c
}

// TestEmittedActivityTypeFollowsThePath asserts the classification survives all
// the way onto the wire payload, not merely onto the struct: the whole defect
// was that a value nobody checked reached storage. The count_tokens row lives
// in TestAProbeIsClassifiedAndNotSpooled instead: production never emits this
// shape (Emit drops the probe before EventsFor), so a table named "emitted"
// must not carry a row for a class that is not.
func TestEmittedActivityTypeFollowsThePath(t *testing.T) {
	for _, tc := range []struct {
		url  string
		want string
	}{
		{"https://api.anthropic.com/v1/messages", client.ActivityTypeLLMCompletion},
		{"https://api.anthropic.com/v1/something-new", client.ActivityTypeProviderRequest},
	} {
		t.Run(tc.want, func(t *testing.T) {
			// Both halves, because a class that reached only one of them would
			// classify half of every model call.
			for _, ev := range mustPair(LaneProxy, sampleIdentity(), "px-1", sampleAt, capturedFor("sess-1", tc.url)) {
				if ev.ActivityType != tc.want {
					t.Errorf("%s: DevEvent.ActivityType = %q, want %q", ev.EventType, ev.ActivityType, tc.want)
				}
				if got := wireActivityType(t, ev); got != tc.want {
					t.Errorf("%s: wire activity_type = %q, want %q", ev.EventType, got, tc.want)
				}
			}
		})
	}
}

// wireActivityType reads the field off the bytes the client would POST.
func wireActivityType(t *testing.T, ev client.DevEvent) string {
	t.Helper()
	raw := postThroughRealClient(t, ev, true)
	var p struct {
		ActivityType string `json:"activity_type"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("payload is not JSON: %v", err)
	}
	return p.ActivityType
}

// TestTheToolsOwnTelemetryDoesNotWarnAboutAMissingSession is 3b: the warning
// said "no governance events are being sent", which reads as "completions are
// being dropped", on hundreds of headerless telemetry POSTs a day. It is what
// sent a whole investigation to the wrong lane.
func TestTheToolsOwnTelemetryDoesNotWarnAboutAMissingSession(t *testing.T) {
	headerless := func(url string) gateway.Captured {
		c := sampleCaptured()
		c.HTTPMethod = http.MethodPost
		c.HTTPURL = url
		c.RequestHeaders = map[string]string{"Anthropic-Version": "2023-06-01"}
		return c
	}

	for _, url := range []string{
		"https://api.anthropic.com/api/event_logging/v2/batch",
		"https://api.anthropic.com/api/claude_code/metrics",
		"https://api.anthropic.com/api/claude_code/settings",
		"https://api.anthropic.com/api/claude_cli/bootstrap",
	} {
		t.Run(url, func(t *testing.T) {
			em, _, warnings := newTestEmitter(t)
			em.Emit(context.Background(), headerless(url))
			if got := warnings.String(); got != "" {
				t.Errorf("the tool's own telemetry produced a governance warning: %q", got)
			}
		})
	}

	t.Run("a headerless completion is still loud", func(t *testing.T) {
		em, _, warnings := newTestEmitter(t)
		em.Emit(context.Background(), headerless("https://api.anthropic.com/v1/messages"))
		got := warnings.String()
		if !strings.Contains(strings.ToLower(got), "x-claude-code-session-id") {
			t.Errorf("a headerless completion did not warn: %q", got)
		}
		if !strings.Contains(got, "model calls") {
			t.Errorf("the warning does not say a MODEL CALL was affected: %q", got)
		}
	})

	t.Run("an unknown headerless POST names its path", func(t *testing.T) {
		em, _, warnings := newTestEmitter(t)
		em.Emit(context.Background(), headerless("https://api.anthropic.com/v1/something-new"))
		got := warnings.String()
		if got == "" {
			t.Fatal("an unrecognised POST was silently ignored; the predicate must err toward warning")
		}
		if !strings.Contains(got, "/v1/something-new") {
			t.Errorf("the warning does not name the path that had no header: %q", got)
		}
	})
}

// TestAProbeWarningCannotSilenceACompletionWarning the throttles are separate
// on purpose: sharing one clock would let an hour of probe noise suppress the
// single case that means governance evidence is being lost.
func TestAProbeWarningCannotSilenceACompletionWarning(t *testing.T) {
	var warnings bytes.Buffer
	em, _, _ := newTestEmitter(t)
	em.Warn = func(format string, args ...any) { fmt.Fprintf(&warnings, format+"\n", args...) }

	headerless := func(url string) gateway.Captured {
		c := sampleCaptured()
		c.HTTPMethod = http.MethodPost
		c.HTTPURL = url
		c.RequestHeaders = map[string]string{"Anthropic-Version": "2023-06-01"}
		return c
	}

	em.Emit(context.Background(), headerless("https://api.anthropic.com/v1/messages/count_tokens"))
	em.Emit(context.Background(), headerless("https://api.anthropic.com/v1/messages"))

	if !strings.Contains(warnings.String(), "model calls") {
		t.Errorf("a probe warning suppressed the completion warning: %q", warnings.String())
	}
}

// TestAProbeIsClassifiedAndNotSpooled is the verdict for dropping the probe's
// emission without touching its classification: a session-bearing count_tokens
// capture must spool zero events, while a session-bearing completion capture
// -- the positive control -- must still spool its Started/Completed pair. A
// zero-only assertion would also pass on a broken emitter that spools nothing
// at all; the paired completion assertion is what makes this a verdict.
func TestAProbeIsClassifiedAndNotSpooled(t *testing.T) {
	// Relocated from TestEmittedActivityTypeFollowsThePath: EventsFor itself is
	// unchanged and still classifies+pairs a probe correctly on the wire; what
	// changed is that Emit (below) never calls it for this class. Proving both
	// is what keeps insight 1 true -- classification and emission stay separate
	// points, so the 40%-distortion bug cannot reopen by deleting the class.
	for _, ev := range mustPair(LaneProxy, sampleIdentity(), "px-1", sampleAt, capturedFor("sess-1", "https://api.anthropic.com/v1/messages/count_tokens")) {
		if ev.ActivityType != client.ActivityTypeTokenCount {
			t.Errorf("%s: DevEvent.ActivityType = %q, want %q", ev.EventType, ev.ActivityType, client.ActivityTypeTokenCount)
		}
		if got := wireActivityType(t, ev); got != client.ActivityTypeTokenCount {
			t.Errorf("%s: wire activity_type = %q, want %q", ev.EventType, got, client.ActivityTypeTokenCount)
		}
	}

	em, spool, _ := newTestEmitter(t)

	em.Emit(context.Background(), capturedFor("sess-probe", "https://api.anthropic.com/v1/messages/count_tokens"))
	if got := spooledEvents(t, spool, "sess-probe"); len(got) != 0 {
		t.Errorf("a count_tokens capture spooled %d event(s), want 0", len(got))
	}

	em.Emit(context.Background(), capturedFor("sess-completion", "https://api.anthropic.com/v1/messages"))
	if got := spooledEvents(t, spool, "sess-completion"); len(got) != 2 {
		t.Errorf("a completion capture spooled %d event(s), want 2", len(got))
	}
}

const chatURL = "https://claude.ai/api/organizations/5c1d9a7e-3b2f-4e8a-b6c4-9f0e1d2a3b4c/" +
	"chat_conversations/0f8e2d4c-6b1a-4c3e-9d7f-2a5b8c1e4f60/completion"

// TestClassifyPathReadsTheHostForAChatCompletion the same /api/ prefix means
// Claude Code reporting on itself on api.anthropic.com and a model call on
// claude.ai; only the host tells them apart.
func TestClassifyPathReadsTheHostForAChatCompletion(t *testing.T) {
	if got := classifyPath(chatURL); got != ClassChatCompletion {
		t.Fatalf("classifyPath(claude.ai completion) = %v, want ClassChatCompletion", got)
	}
	if got := ClassChatCompletion.ActivityType(); got != client.ActivityTypeLLMCompletion {
		t.Errorf("ActivityType = %q, want llm_completion", got)
	}
	if !ClassChatCompletion.CarriesContent() || !ClassChatCompletion.Emits() {
		t.Error("a chat completion is a model call: it carries content and emits")
	}
	apiTwin := strings.Replace(chatURL, "https://claude.ai", "https://api.anthropic.com", 1)
	if got := classifyPath(apiTwin); got != ClassToolTelemetry {
		t.Errorf("the same path on api.anthropic.com = %v, want ClassToolTelemetry (unchanged)", got)
	}
	title := strings.Replace(chatURL, "/completion", "/title", 1)
	if got := classifyPath(title); got == ClassChatCompletion {
		t.Errorf("a claude.ai title call classified as a chat completion")
	}
}

// TestCapturesBodyAtKeepsAChatCompletionBody the relay's body predicate sees
// the request's host separately from its path; without it a chat completion's
// body would never be captured.
func TestCapturesBodyAtKeepsAChatCompletionBody(t *testing.T) {
	path := strings.TrimPrefix(chatURL, "https://claude.ai")
	if !CapturesBodyAt("claude.ai", path) {
		t.Error("a claude.ai chat completion body must be captured")
	}
	if CapturesBodyAt("api.anthropic.com", path) {
		t.Error("the same path on api.anthropic.com is tool telemetry and keeps no body")
	}
	if !CapturesBodyAt("127.0.0.1:8788", pathMessages) {
		t.Error("the gateway lane's /v1/messages body must still be captured")
	}
}
