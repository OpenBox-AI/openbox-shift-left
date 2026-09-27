package gatewayemit

import (
	"net/url"
	"strings"

	"github.com/openbox-ai/openbox-shift-left/internal/cli/sessionkey"
	"github.com/openbox-ai/openbox-shift-left/internal/client"
)

// PathClass is what a relayed request actually was. Classifying on the HTTP
// method alone files every POST as a completion, token-count probes included,
// and probes are a large share of relayed POSTs. docs/mapping.md §2.
type PathClass int

const (
	// ClassCompletion is a real model completion request.
	ClassCompletion PathClass = iota
	// ClassTokenCount is a probe: the conversation minus the reply, and no usage.
	ClassTokenCount
	// ClassToolTelemetry is the tool reporting on itself. Headerless by design.
	ClassToolTelemetry
	// ClassUnknown is an unrecognized path. Still emitted, never mislabelled.
	ClassUnknown
	// ClassChatCompletion is a claude.ai chat completion: a model call from a
	// consumer chat surface, keyed on its conversation rather than a tool
	// session. Only the host tells it apart from ClassToolTelemetry, whose
	// /api/ prefix it shares.
	ClassChatCompletion
)

const (
	pathMessages   = "/v1/messages"
	pathCountToken = "/v1/messages/count_tokens"
	prefixToolAPI  = "/api/"
)

func (c PathClass) ActivityType() string {
	switch c {
	case ClassTokenCount:
		return client.ActivityTypeTokenCount
	case ClassToolTelemetry:
		return client.ActivityTypeToolTelemetry
	case ClassUnknown:
		return client.ActivityTypeProviderRequest
	}
	return client.ActivityTypeLLMCompletion
}

// CarriesContent reports whether this class may egress bodies; a probe may not.
func (c PathClass) CarriesContent() bool {
	return c == ClassCompletion || c == ClassUnknown || c == ClassChatCompletion
}

// WarnsOnMissingSession reports whether a headerless request is worth a warning.
func (c PathClass) WarnsOnMissingSession() bool {
	return c != ClassToolTelemetry && c != ClassTokenCount
}

// Emits reports whether this class produces governance events at all. A probe
// is classified so it can never again be miscounted as a turn, and then not
// sent: core drops it unread (isRelayedNonToolActivity), so the row is pure
// egress. Classification and emission are separate points on purpose.
func (c PathClass) Emits() bool { return c != ClassTokenCount }

func (c PathClass) Subject(rawURL string) string {
	switch c {
	case ClassCompletion:
		return "relayed model calls"
	case ClassChatCompletion:
		return "relayed claude.ai chat completions"
	}
	return "a relayed POST to " + requestPath(rawURL)
}

// classifyPath reads the host as well as the path: a relayed call's capture
// URL is absolute (upstream plus request URI), and a bare path -- what the
// gateway lane's body predicate passes -- has no host, which is never a chat
// host.
func classifyPath(rawURL string) PathClass {
	path := requestPath(rawURL)
	if _, ok := sessionkey.ResolveChat(requestHost(rawURL), path); ok {
		return ClassChatCompletion
	}
	switch {
	case path == pathCountToken:
		return ClassTokenCount
	case path == pathMessages:
		return ClassCompletion
	case strings.HasPrefix(path, prefixToolAPI):
		return ClassToolTelemetry
	}
	return ClassUnknown
}

func requestPath(rawURL string) string {
	path := rawURL
	if u, err := url.Parse(rawURL); err == nil && u.Path != "" {
		path = u.Path
	}
	if i := strings.IndexAny(path, "?#"); i >= 0 {
		path = path[:i]
	}
	if len(path) > 1 {
		path = strings.TrimRight(path, "/")
	}
	return path
}

func requestHost(rawURL string) string {
	if u, err := url.Parse(rawURL); err == nil {
		return u.Host
	}
	return ""
}

// CapturesBody is the relay-side half of CarriesContent.
func CapturesBody(rawURL string) bool {
	return classifyPath(rawURL).CarriesContent()
}

// CapturesBodyAt is CapturesBody for a request whose host arrives apart from
// its path, as the relay's body predicate sees it (r.Host, r.URL.Path).
func CapturesBodyAt(host, path string) bool {
	return classifyPath((&url.URL{Scheme: "https", Host: host, Path: path}).String()).CarriesContent()
}
