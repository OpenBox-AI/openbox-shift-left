package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Httptest.NewRecorder needs no listener, which matters: the host that will
// most likely need to reason about this tool is the same sandboxed one that
// cannot bind, and a probe whose own logic is untested there is a probe nobody
// can trust when it finally runs.

func TestInjectorRefusesOnlyAfterTheConfiguredCount(t *testing.T) {
	shape, ok := ShapeByName("invalid_request_error")
	if !ok {
		t.Fatal("candidate shape is missing from the table")
	}
	inj, err := NewInjector("https://api.anthropic.com", shape, 2, "/v1/messages")
	if err != nil {
		t.Fatalf("NewInjector: %v", err)
	}

	for i := 1; i <= 2; i++ {
		if inj.Matches(httptest.NewRequest(http.MethodPost, "/v1/messages", nil)) {
			t.Errorf("request %d was refused; After=2 means the first two pass through", i)
		}
	}
	if !inj.Matches(httptest.NewRequest(http.MethodPost, "/v1/messages", nil)) {
		t.Error("the third qualifying request was not refused")
	}
}

func TestInjectorIgnoresOtherPaths(t *testing.T) {
	shape, _ := ShapeByName("invalid_request_error")
	inj, err := NewInjector("https://api.anthropic.com", shape, 0, "/v1/messages")
	if err != nil {
		t.Fatalf("NewInjector: %v", err)
	}
	if inj.Matches(httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", nil)) {
		t.Error("count_tokens qualified for injection; it is not a model call")
	}
	if inj.Seen() != 0 {
		t.Errorf("a non-qualifying request advanced the counter to %d", inj.Seen())
	}
}

func TestInjectedResponseIsTheCandidateVerbatim(t *testing.T) {
	for _, shape := range Shapes {
		inj, err := NewInjector("https://api.anthropic.com", shape, 0, "/v1/messages")
		if err != nil {
			t.Fatalf("NewInjector: %v", err)
		}
		rec := httptest.NewRecorder()
		inj.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", nil))

		if rec.Code != shape.Status {
			t.Errorf("%s: status %d, want %d", shape.Name, rec.Code, shape.Status)
		}
		if got := rec.Body.String(); got != shape.Body {
			t.Errorf("%s: body was altered in flight:\n got %q\nwant %q", shape.Name, got, shape.Body)
		}
		if got := rec.Header().Get("Content-Type"); got != shape.ContentType {
			t.Errorf("%s: content-type %q, want %q", shape.Name, got, shape.ContentType)
		}
		if got := rec.Header().Get("Cache-Control"); !strings.Contains(got, "no-store") {
			t.Errorf("%s: Cache-Control = %q, want no-store", shape.Name, got)
		}
		if inj.Injected() != 1 {
			t.Errorf("%s: injected count = %d, want 1", shape.Name, inj.Injected())
		}
	}
}

// TestTheNegativeControlIsStillInTheTable guards the one candidate that exists
// to be retried. If every shape in the table were terminal, a probe run
// showing "no retries" would be indistinguishable from a probe that cannot
// observe retries at all.
func TestTheNegativeControlIsStillInTheTable(t *testing.T) {
	s, ok := ShapeByName("overloaded_error")
	if !ok {
		t.Fatal("the negative control is gone; a run reporting no retries would prove nothing")
	}
	if s.Status != http.StatusTooManyRequests {
		t.Errorf("the negative control is %d; it must be a status the client retries", s.Status)
	}
}
