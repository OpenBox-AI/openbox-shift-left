package client

import (
	"encoding/json"
	"strings"
	"testing"
)

func gatewayTurn() DevEvent {
	idx := 3
	return DevEvent{
		SchemaVersion:    SchemaVersion,
		EventID:          "evt-gw-1",
		EventType:        EventTurnCompleted,
		SessionID:        "sess-1",
		DeveloperDID:     testDID,
		Timestamp:        "2026-08-25T12:00:00Z",
		StartedAt:        "2026-08-25T11:59:58Z",
		EndedAt:          "2026-08-25T12:00:00Z",
		Tool:             Tool{Name: "claude-code", Kind: ToolShell},
		TurnIndex:        &idx,
		GatewayRequestID: "req-abc123",
		Span: &Span{
			SemanticType:          "llm_completion",
			Stage:                 "completed",
			HTTPMethod:            "POST",
			HTTPURL:               "https://api.anthropic.com/v1/messages",
			HTTPStatus:            200,
			CredentialFingerprint: "a1b2c3d4e5f60718293a4b5c6d7e8f90",
			RequestHeaders:        map[string]string{"Authorization": "[redacted]", "Anthropic-Version": "2023-06-01"},
			ResponseHeaders:       map[string]string{"Request-Id": "req_upstream"},
			RequestBody:           `{"model":"claude-opus-4"}`,
			ResponseBody:          `{"type":"message","role":"assistant"}`,
		},
	}
}

// TestGatewayAndHookTurnIDsNeverCollide is requirement 8, and it is checked by
// construction rather than by sampling.
func TestGatewayAndHookTurnIDsNeverCollide(t *testing.T) {
	idx := 3
	hook := DevEvent{SessionID: "sess-1", EventType: EventTurnCompleted, TurnIndex: &idx}
	gw := gatewayTurn()

	hookID := turnActivityIDFor(hook)
	gwID := turnActivityIDFor(gw)

	if hookID == "" || gwID == "" {
		t.Fatalf("an id was empty: hook=%q gateway=%q", hookID, gwID)
	}
	if hookID == gwID {
		t.Fatalf("gateway and hook turn share activity_id %q", hookID)
	}
	if !strings.Contains(hookID, ":turn:") {
		t.Errorf("hook id %q lost its :turn: namespace", hookID)
	}
	if !strings.Contains(gwID, ":gateway:") {
		t.Errorf("gateway id %q lost its :gateway: namespace", gwID)
	}

	toolID := activityIDFor(sampleEvent())
	if strings.Contains(toolID, ":") {
		t.Errorf("tool activity id %q now contains ':', so the namespace argument no longer holds", toolID)
	}
	for _, id := range []string{hookID, gwID} {
		if id == toolID {
			t.Errorf("turn id %q collides with a tool call id", id)
		}
	}

	rollup := turnActivityIDFor(DevEvent{SessionID: "sess-1", SessionRollup: true})
	if rollup == gwID || rollup == hookID {
		t.Errorf("rollup id %q collides", rollup)
	}
}

// TestGatewaySpanOnWireWithCaptureOn asserts on the bytes that are actually
// POSTed, not on the struct. This repo has already shipped a defect where a
// field was present on the struct and absent from the wire, so the wire is the
// only assertion that counts.
func TestGatewaySpanOnWireWithCaptureOn(t *testing.T) {
	body, err := buildPayload(gatewayTurn())
	if err != nil {
		t.Fatalf("buildPayload: %v", err)
	}
	var p struct {
		ActivityID string `json:"activity_id"`
		Spans      []struct {
			SpanID                string            `json:"span_id"`
			Name                  string            `json:"name"`
			RequestBody           string            `json:"request_body"`
			ResponseBody          string            `json:"response_body"`
			RequestHeaders        map[string]string `json:"request_headers"`
			ResponseHeaders       map[string]string `json:"response_headers"`
			HTTPMethod            string            `json:"http_method"`
			HTTPURL               string            `json:"http_url"`
			HTTPStatus            int               `json:"http_status_code"`
			CredentialFingerprint string            `json:"credential_fingerprint"`
			Attributes            map[string]any    `json:"attributes"`
		} `json:"spans"`
		SpanCount int `json:"span_count"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if p.SpanCount != 1 || len(p.Spans) != 1 {
		t.Fatalf("span_count=%d spans=%d, want exactly one", p.SpanCount, len(p.Spans))
	}
	s := p.Spans[0]

	if !strings.HasPrefix(s.SpanID, "gw-") {
		t.Errorf("span_id %q is not the gateway's namespace", s.SpanID)
	}
	if s.HTTPMethod != "POST" || s.HTTPStatus != 200 {
		t.Errorf("classification root fields wrong: method=%q status=%d", s.HTTPMethod, s.HTTPStatus)
	}
	// Core spells it `http_status_code`; the shorter `http_status` was silently
	// dropped on ingest, and every test here passed while that was happening
	// because they all asserted outbound bytes rather than the receiving type.
	if strings.Contains(string(body), `"http_status"`) {
		t.Error(`span carries "http_status"; core's SpanData field is "http_status_code" and drops the other silently`)
	}
	if s.Attributes["openbox.credential_fingerprint"] == nil {
		t.Error("fingerprint absent from attributes; core has no credential_fingerprint field, so the top-level key alone is dropped and account binding can never match")
	}
	if s.CredentialFingerprint == "" {
		t.Error("credential_fingerprint absent from the wire")
	}
	if s.RequestHeaders["Anthropic-Version"] != "2023-06-01" {
		t.Errorf("request_headers did not reach the wire: %v", s.RequestHeaders)
	}
	if s.ResponseHeaders["Request-Id"] != "req_upstream" {
		t.Errorf("response_headers did not reach the wire: %v", s.ResponseHeaders)
	}
	if !strings.Contains(s.RequestBody, "claude-opus-4") {
		t.Errorf("request_body did not reach the wire: %q", s.RequestBody)
	}
	if !strings.Contains(s.ResponseBody, "assistant") {
		t.Errorf("response_body did not reach the wire: %q", s.ResponseBody)
	}

	if s.Attributes["http.method"] != "POST" {
		t.Errorf("attributes[http.method] = %v; core cannot classify without it", s.Attributes["http.method"])
	}
	url, _ := s.Attributes["http.url"].(string)
	if !strings.Contains(url, "api.anthropic.com") {
		t.Errorf("attributes[http.url] = %q; isLLMCall needs an LLM domain here", url)
	}
	if strings.Contains(url, "?") {
		t.Errorf("attributes[http.url] %q still carries a query string", url)
	}

	if !strings.Contains(p.ActivityID, ":gateway:") {
		t.Errorf("activity_id %q is not in the gateway namespace", p.ActivityID)
	}
}

// TestGatewaySpanContentGatedOffTheWire is the capture-OFF half, and it has to
// be asserted wherever the ON half is.
func TestGatewaySpanContentGatedOffTheWire(t *testing.T) {
	stripped := stripContent(gatewayTurn())
	body, err := buildPayload(stripped)
	if err != nil {
		t.Fatalf("buildPayload: %v", err)
	}
	raw := string(body)

	for _, marker := range []string{"claude-opus-4", "assistant", "Anthropic-Version", "2023-06-01", "req_upstream", "request_headers", "response_headers"} {
		if strings.Contains(raw, marker) {
			t.Errorf("capture is OFF but the wire still carries %q", marker)
		}
	}
	for _, marker := range []string{"credential_fingerprint", "a1b2c3d4e5f60718293a4b5c6d7e8f90", "http_method", "http.url"} {
		if !strings.Contains(raw, marker) {
			t.Errorf("capture is OFF and %q disappeared too; it is structural, not content", marker)
		}
	}
}

// TestHookTurnUnaffectedByTheGatewayPath is the regression guard. A hook-only
// install must emit exactly what it emitted before: same span, same shape, and
// specifically NOT the gateway's.
func TestHookTurnUnaffectedByTheGatewayPath(t *testing.T) {
	idx := 3
	hook := DevEvent{
		SchemaVersion: SchemaVersion,
		EventID:       "evt-hook-1",
		EventType:     EventTurnCompleted,
		SessionID:     "sess-1",
		DeveloperDID:  testDID,
		Timestamp:     "2026-08-25T12:00:00Z",
		Tool:          Tool{Name: "claude-code", Kind: ToolShell},
		TurnIndex:     &idx,
		Content:       &Content{Output: "the assistant's reply"},
	}
	body, err := buildPayload(hook)
	if err != nil {
		t.Fatalf("buildPayload: %v", err)
	}
	raw := string(body)

	if strings.Contains(raw, "gw-") || strings.Contains(raw, ":gateway:") {
		t.Error("a hook turn acquired gateway identity")
	}
	if strings.Contains(raw, "credential_fingerprint") || strings.Contains(raw, "http_method") {
		t.Error("a hook turn acquired gateway span fields")
	}
	if !strings.Contains(raw, `\"choices\"`) {
		t.Errorf("the assistant span's chat wrapper is gone; alignment would read nothing:\n%s", raw)
	}
}

// TestGatewaySpanKeysMatchCoreSpanData pins the receiving contract by name.
func TestGatewaySpanKeysMatchCoreSpanData(t *testing.T) {
	coreKnows := map[string]bool{
		"span_id": true, "trace_id": true, "parent_span_id": true, "name": true,
		"kind": true, "start_time": true, "end_time": true, "duration_ns": true,
		"attributes": true, "status": true, "events": true,
		"request_headers": true, "response_headers": true,
		"request_body": true, "response_body": true,
		"semantic_type": true, "stage": true, "data": true, "hook_type": true,
		"error": true, "http_method": true, "http_url": true, "http_status_code": true,
		"credential_fingerprint": true,
	}

	body, err := buildPayload(gatewayTurn())
	if err != nil {
		t.Fatalf("buildPayload: %v", err)
	}
	var p struct {
		Spans []map[string]json.RawMessage `json:"spans"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(p.Spans) != 1 {
		t.Fatalf("want one span, got %d", len(p.Spans))
	}
	for key := range p.Spans[0] {
		if !coreKnows[key] {
			t.Errorf("span key %q is not a field on core's SpanData; it will be dropped silently on ingest", key)
		}
	}
}
