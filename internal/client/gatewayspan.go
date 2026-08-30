package client

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"unicode/utf8"
)

// It is a second span producer alongside turnspan.go, and the two are kept
// apart on purpose:
//   - Turnspan.go carries the assistant's reply, for exactly one reader;
//     core's
//   - This file carries an observed HTTP exchange: the real headers, the real

// observedSpanAttributes gatewaySpanAttributes carries the classification
// keys, and they are NOT decorative. It is decided from the lane rather than
// passed in by the caller, deliberately.
func observedSpanAttributes(lane string, s *Span) map[string]any {
	attrs := map[string]any{}
	if lane == "otel" {
		attrs["openbox.span_synthetic"] = true
	}
	if s.HTTPMethod != "" {
		attrs["http.method"] = s.HTTPMethod
	}
	if s.HTTPURL != "" {
		attrs["http.url"] = s.HTTPURL
	}
	if s.HTTPStatus != 0 {
		attrs["http.status_code"] = s.HTTPStatus
	}
	if s.CredentialFingerprint != "" {
		attrs["openbox.credential_fingerprint"] = s.CredentialFingerprint
	}
	return attrs
}

const (
	maxHeaderValueBytes = 4096
	maxHeaderCount      = 64
)

func capHeaders(h map[string]string) map[string]string {
	if len(h) == 0 {
		return nil
	}
	keys := make([]string, 0, len(h))
	for k := range h {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) > maxHeaderCount {
		keys = keys[:maxHeaderCount]
	}
	out := make(map[string]string, len(keys))
	for _, k := range keys {
		out[k] = capHeaderValue(h[k])
	}
	return out
}

func capHeaderValue(v string) string {
	if len(v) <= maxHeaderValueBytes {
		return v
	}
	end := maxHeaderValueBytes
	for end > 0 && !utf8.RuneStart(v[end]) {
		end--
	}
	return v[:end] + "…[truncated]"
}

// observedLane the precedence is turnActivityIDFor's, deliberately and not
// coincidentally: proxy, then gateway, then otel (that decision; in-path relay
// outranks a client-asserted lane).
func observedLane(ev DevEvent) (name, id string) {
	switch {
	case ev.ProxyRequestID != "":
		return "proxy", ev.ProxyRequestID
	case ev.GatewayRequestID != "":
		return "gw", ev.GatewayRequestID
	case ev.OtelRequestID != "":
		return "otel", ev.OtelRequestID
	}
	return "", ""
}

func observedSpanID(ev DevEvent) string {
	lane, id := observedLane(ev)
	if lane == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(lane + "span\x1f" + ev.SessionID + "\x1f" + id))
	return lane + "-" + hex.EncodeToString(sum[:16])
}

func observedSpan(ev DevEvent) *wireSpan {
	lane, _ := observedLane(ev)
	if lane == "" || ev.Span == nil {
		return nil
	}
	s := ev.Span
	if s.HTTPMethod == "" && s.HTTPURL == "" && s.CredentialFingerprint == "" {
		return nil
	}

	start := rfc3339Nanos(firstNonEmpty(ev.StartedAt, ev.Timestamp))
	end := rfc3339Nanos(firstNonEmpty(ev.EndedAt, ev.Timestamp))
	if end < start {
		end = start
	}

	return &wireSpan{
		SpanID:                observedSpanID(ev),
		TraceID:               turnTraceID(ev),
		Name:                  spanNameLLMCompletion,
		Kind:                  spanKindClient,
		Stage:                 "completed",
		StartTime:             start,
		EndTime:               end,
		ResponseBody:          capBody(s.ResponseBody),
		RequestBody:           capBody(s.RequestBody),
		RequestHeaders:        capHeaders(s.RequestHeaders),
		ResponseHeaders:       capHeaders(s.ResponseHeaders),
		Attributes:            observedSpanAttributes(lane, s),
		HTTPMethod:            s.HTTPMethod,
		HTTPURL:               s.HTTPURL,
		HTTPStatus:            s.HTTPStatus,
		CredentialFingerprint: s.CredentialFingerprint,
		SemanticType:          activityTypeLLMCompletion,
	}
}
