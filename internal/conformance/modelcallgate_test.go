package conformance

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// currentVersion is the contract version under test, restated: this package
// must not import internal/client (its dependency closure is pinned by the
// depguard allowlist), so the pairwise check against client.SchemaVersion lives
// in internal/client/acceptancetest.
const currentVersion = "1.11"

func gateBase(eventType string) map[string]any {
	m := turnBase(eventType)
	m["schema_version"] = currentVersion
	m["model_call_request_id"] = "req_01HZ.1"
	return m
}

// TestModelCallGateHalvesValidate: both halves validate, with and without the
// explicit class, and carry their own metadata keys.
func TestModelCallGateHalvesValidate(t *testing.T) {
	for _, et := range []string{"ModelCallRequested", "ModelCallFinished"} {
		ev := gateBase(et)
		if err := ValidateDevEvent(marshalEvent(t, ev), false); err != nil {
			t.Errorf("%s bare: %v", et, err)
		}
		ev["activity_type"] = "model_call_gate"
		ev["model"] = "muse-spark"
		ev["metadata"] = map[string]any{
			"provider": "meta", "message_count": 3, "tool_count": 1, "tool_names": []string{"Bash"},
			"finish_reason": "stop", "response_id": "r1", "error_class": "rate_limit", "tool_call_count": 0,
		}
		if err := ValidateDevEvent(marshalEvent(t, ev), false); err != nil {
			t.Errorf("%s with class and metadata: %v", et, err)
		}
	}
}

// TestModelCallGateRejectsWhatItMayNotCarry: a gate is not a model-call record.
func TestModelCallGateRejectsWhatItMayNotCarry(t *testing.T) {
	banned := map[string]any{
		"tokens":             map[string]any{"input": 1},
		"cost":               map[string]any{"amount": 0.1},
		"turn_index":         0,
		"session_rollup":     true,
		"gateway_request_id": "gw-1",
		"otel_request_id":    "ot-1",
		"proxy_request_id":   "px-1",
		"activity_type":      "llm_completion",
	}
	for _, et := range []string{"ModelCallRequested", "ModelCallFinished"} {
		for field, v := range banned {
			ev := gateBase(et)
			ev[field] = v
			if err := ValidateDevEvent(marshalEvent(t, ev), true); err == nil {
				t.Errorf("%s carrying %s: want rejection, got nil", et, field)
			}
		}
	}
}

// TestModelCallSpanIsOnlyForTheStartedHalf: the started half carries the pending
// request as span.request_body (gated content); the finished half has no span.
func TestModelCallSpanIsOnlyForTheStartedHalf(t *testing.T) {
	span := map[string]any{"semantic_type": "internal", "stage": "started", "request_body": "{\"messages\":[]}"}

	ev := gateBase("ModelCallRequested")
	ev["span"] = span
	raw := marshalEvent(t, ev)
	if err := ValidateDevEvent(raw, false); err != ErrContentDisabled {
		t.Errorf("started with a request body, capture off: want ErrContentDisabled, got %v", err)
	}
	if err := ValidateDevEvent(raw, true); err != nil {
		t.Errorf("started with a request body, capture on: %v", err)
	}

	ev = gateBase("ModelCallFinished")
	ev["span"] = span
	if err := ValidateDevEvent(marshalEvent(t, ev), true); err == nil {
		t.Error("finished half carrying a span: want rejection")
	}
}

// TestPreviousStampIsRefused: a 1.10 stamp is not the current contract.
func TestPreviousStampIsRefused(t *testing.T) {
	ev := gateBase("ModelCallRequested")
	ev["schema_version"] = "1.10"
	if err := ValidateDevEvent(marshalEvent(t, ev), false); err == nil {
		t.Error("a 1.10 stamp validated against 1.11")
	}
}

func TestModelCallGateRequiresABoundedID(t *testing.T) {
	for _, et := range []string{"ModelCallRequested", "ModelCallFinished"} {
		ev := gateBase(et)
		delete(ev, "model_call_request_id")
		if err := ValidateDevEvent(marshalEvent(t, ev), false); err == nil {
			t.Errorf("%s without model_call_request_id: want rejection", et)
		}
		for name, bad := range map[string]string{
			"oversized": strings.Repeat("x", 129), "newline": "req\n1", "space": "req 1",
			"control": "req\x011", "empty": "", "non-ascii": "req_ü1",
		} {
			ev := gateBase(et)
			ev["model_call_request_id"] = bad
			if err := ValidateDevEvent(marshalEvent(t, ev), false); err == nil {
				t.Errorf("%s id %q (%s): want rejection", et, bad, name)
			}
		}
		ev = gateBase(et)
		ev["model_call_request_id"] = strings.Repeat("x", 128)
		if err := ValidateDevEvent(marshalEvent(t, ev), false); err != nil {
			t.Errorf("%s at the 128 bound: %v", et, err)
		}
	}
}

// TestModelCallRequestIDIsOnlyForTheGate: the id names no turn producer, so a
// turn carrying it alone still names no producer.
func TestModelCallRequestIDIsOnlyForTheGate(t *testing.T) {
	for _, et := range []string{"TurnStarted", "TurnCompleted"} {
		ev := turnBase(et)
		ev["schema_version"] = currentVersion
		ev["model_call_request_id"] = "req_01HZ.1"
		if err := ValidateDevEvent(marshalEvent(t, ev), false); err == nil {
			t.Errorf("%s with only model_call_request_id: want rejection, got nil", et)
		}
	}
}

func TestMessagePreviewsAreGatedContent(t *testing.T) {
	ev := gateBase("ModelCallRequested")
	ev["metadata"] = map[string]any{"message_previews": []string{"hello"}}
	raw := marshalEvent(t, ev)
	if err := ValidateDevEvent(raw, false); err != ErrContentDisabled {
		t.Errorf("capture off: want ErrContentDisabled, got %v", err)
	}
	if err := ValidateDevEvent(raw, true); err != nil {
		t.Errorf("capture on: %v", err)
	}
}

// TestPreviousVersionPayloadsValidateOnceRestamped: 1.10 and 1.11 add to the
// contract and remove nothing, so a 1.9 event differs from a valid current one only in its
// stamp. Unrestamped, the const rejects it, which is what a stale producer
// should see.
func TestPreviousVersionPayloadsValidateOnceRestamped(t *testing.T) {
	files, err := filepath.Glob("testdata/prev19/*.json")
	if err != nil || len(files) == 0 {
		t.Fatalf("no 1.9 fixtures (err %v); the test would assert nothing", err)
	}
	for _, f := range files {
		raw := read(t, f)
		content := strings.Contains(f, "with_")
		if err := ValidateDevEvent(raw, content); err == nil {
			t.Errorf("%s: a 1.9 stamp validated against 1.11", f)
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		if m["schema_version"] != "1.9" {
			t.Fatalf("%s: fixture is stamped %v, want 1.9", f, m["schema_version"])
		}
		m["schema_version"] = currentVersion
		if err := ValidateDevEvent(marshalEvent(t, m), content); err != nil {
			t.Errorf("%s restamped: %v", f, err)
		}
	}
}
