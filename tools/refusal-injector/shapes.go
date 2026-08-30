package main

import "net/http"

// Shape is one candidate refusal the probe can inject. That decision is the
// only thing keeping that decision record in draft, and it asks an empirical
// question about a provider we do not control: which refusal shape does Claude
// Code surface to the developer without retrying around it?
type Shape struct {
	// Name is how the runbook and the results table refer to this candidate.
	Name string

	// Status is the HTTP status the client receives.
	Status int

	// Body is the response body, verbatim.
	Body string

	// ContentType is the response content type.
	ContentType string

	// Rationale says why this shape is a candidate; what about it might make the
	// client surface it rather than retry.
	Rationale string

	// Retryable records what the provider's own SDK conventions suggest the
	// client will do. It is the prediction the probe tests, never the result.
	Retryable string
}

// Shapes is the candidate set. The 4xx family is over-represented on purpose:
// a 5xx is retried by every SDK this provider ships, so a refusal expressed as
// one would be invisible to the developer AND would burn the session's retry
// budget.
var Shapes = []Shape{
	{
		Name:        "invalid_request_error",
		Status:      http.StatusBadRequest,
		ContentType: "application/json",
		Body:        `{"type":"error","error":{"type":"invalid_request_error","message":"Blocked by OpenBox policy."}}`,
		Rationale:   "the provider's own error envelope; most likely to render in the client's error path rather than a crash",
		Retryable:   "predicted NO — 400 is terminal in the provider's SDK retry table",
	},
	{
		Name:        "permission_error",
		Status:      http.StatusForbidden,
		ContentType: "application/json",
		Body:        `{"type":"error","error":{"type":"permission_error","message":"Blocked by OpenBox policy."}}`,
		Rationale:   "semantically closest to what a policy refusal IS; risk is the client treating it as an auth failure and re-authenticating",
		Retryable:   "predicted NO, but may trigger a credential re-prompt, which would be worse than a retry",
	},
	{
		Name:        "authentication_error",
		Status:      http.StatusUnauthorized,
		ContentType: "application/json",
		Body:        `{"type":"error","error":{"type":"authentication_error","message":"Blocked by OpenBox policy."}}`,
		Rationale:   "included to be RULED OUT: if 401 makes the client discard or re-request credentials, the shape is disqualified no matter how well it renders",
		Retryable:   "predicted NO retry, but predicted credential side effects — a disqualifier",
	},
	{
		Name:        "overloaded_error",
		Status:      http.StatusTooManyRequests,
		ContentType: "application/json",
		Body:        `{"type":"error","error":{"type":"rate_limit_error","message":"Blocked by OpenBox policy."}}`,
		Rationale:   "the control. It SHOULD be retried; if the probe cannot distinguish this from the candidates above, the instrument is not measuring retries at all",
		Retryable:   "predicted YES — this is the negative control, not a candidate",
	},
	{
		Name:        "sse_error_event",
		Status:      http.StatusOK,
		ContentType: "text/event-stream",
		Body:        "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"invalid_request_error\",\"message\":\"Blocked by OpenBox policy.\"}}\n\n",
		Rationale:   "a streamed refusal on a 200. The client is already committed to a stream, so there is nothing to retry; the open question is whether the message reaches the human or is swallowed as a malformed stream",
		Retryable:   "unknown — the shape most worth measuring and least predictable",
	},
}

// ShapeByName returns a candidate by name.
func ShapeByName(name string) (Shape, bool) {
	for _, s := range Shapes {
		if s.Name == name {
			return s, true
		}
	}
	return Shape{}, false
}
