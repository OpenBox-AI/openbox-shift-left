package gatewayemit

import (
	"net/url"
	"strings"

	"github.com/openbox-ai/openbox-shift-left/internal/client"
)

// PathClass is what a relayed request actually was. `isModelCall` did two jobs
// with one predicate and, as a CLASSIFIER, filed every POST as a completion while
// ~40% were probes. docs/mapping.md §2.
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
	return c == ClassCompletion || c == ClassUnknown
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
	if c == ClassCompletion {
		return "relayed model calls"
	}
	return "a relayed POST to " + requestPath(rawURL)
}

func classifyPath(rawURL string) PathClass {
	path := requestPath(rawURL)
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

// CapturesBody is the relay-side half of CarriesContent.
func CapturesBody(rawURL string) bool {
	return classifyPath(rawURL).CarriesContent()
}
