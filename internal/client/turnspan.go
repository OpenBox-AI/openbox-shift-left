package client

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

type wireSpan struct {
	SpanID  string `json:"span_id"`
	TraceID string `json:"trace_id"`
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	Stage   string `json:"stage"`
	// StartTime/EndTime are epoch nanoseconds; core's OTel convention, not
	// rfc3339 (internal/content/governance.go SpanData).
	StartTime int64 `json:"start_time"`
	EndTime   int64 `json:"end_time"`
	// Attributes carries the classification keys core recomputes semantic_type
	// from, plus the marker that says they are synthesized.
	Attributes map[string]any `json:"attributes"`
	// ResponseBody is the assistant turn's text, wrapped in the OpenAI-chat shape
	// the extractor unmarshals.
	ResponseBody string `json:"response_body"`
	// SemanticType ships for readability in stored rows and is never relied on:
	// core recomputes it per span before storage
	// (internal/services/governance_workflow.go:303), so whatever is sent here is
	// overwritten by ComputeSemanticTypeFromSpan.
	SemanticType string `json:"semantic_type"`

	// --- Gateway-only fields (that decision, schema 1.5). ---
	RequestBody     string            `json:"request_body,omitempty"`
	RequestHeaders  map[string]string `json:"request_headers,omitempty"`
	ResponseHeaders map[string]string `json:"response_headers,omitempty"`
	HTTPMethod      string            `json:"http_method,omitempty"`
	HTTPURL         string            `json:"http_url,omitempty"`
	// HTTPStatus http_status_CODE, not http_status.
	HTTPStatus int `json:"http_status_code,omitempty"`
	// CredentialFingerprint is present whether or not content capture is on: it
	// is derived governance evidence, not content.
	CredentialFingerprint string `json:"credential_fingerprint,omitempty"`
}

// spanNameLLMCompletion is the span name.
const spanNameLLMCompletion = "llm_completion"

const spanKindClient = "CLIENT"

const synthesizedLLMURL = "https://api.anthropic.com/v1/messages"

// turnSpanAttributes the client cannot simply declare semantic_type: core
// recomputes it.
func turnSpanAttributes() map[string]any {
	return map[string]any{
		"http.method":            "POST",
		"http.url":               synthesizedLLMURL,
		"openbox.span_synthetic": true,
	}
}

func turnAssistantSpan(ev DevEvent) *wireSpan {
	if ev.Content == nil || ev.Content.Output == "" {
		return nil
	}
	if turnActivityIDFor(ev) == "" {
		return nil
	}
	// As silence.
	body, err := json.Marshal(map[string]any{
		"choices": []any{
			map[string]any{"message": map[string]any{"content": capBody(ev.Content.Output)}},
		},
	})
	if err != nil {
		return nil
	}

	start := rfc3339Nanos(firstNonEmpty(ev.StartedAt, ev.Timestamp))
	end := rfc3339Nanos(firstNonEmpty(ev.EndedAt, ev.Timestamp))
	if end < start {
		end = start
	}

	return &wireSpan{
		SpanID:       turnSpanID(ev),
		TraceID:      turnTraceID(ev),
		Name:         spanNameLLMCompletion,
		Kind:         spanKindClient,
		Stage:        "completed",
		StartTime:    start,
		EndTime:      end,
		Attributes:   turnSpanAttributes(),
		ResponseBody: string(body),
		SemanticType: activityTypeLLMCompletion,
	}
}

func turnSpanID(ev DevEvent) string {
	sum := sha256.Sum256([]byte("turnspan\x1f" + turnActivityIDFor(ev)))
	return hex.EncodeToString(sum[:])[:16]
}

func turnTraceID(ev DevEvent) string {
	sum := sha256.Sum256([]byte("turntrace\x1f" + ev.SessionID))
	return hex.EncodeToString(sum[:])[:32]
}
