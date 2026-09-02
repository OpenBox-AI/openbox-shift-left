package targetposture

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/observation"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func TestCaptureUsesExactStableGETSequence(t *testing.T) {
	paths := []string{
		"/auth/profile", "/agent/agent-1", "/agent/agent-1/guardrails?page=0&perPage=100",
		"/agent/agent-1/policies?page=0&perPage=100", "/agent/agent-1/policies/current",
		"/agent/agent-1/behavior-rule?page=0&perPage=100",
	}
	var observed []string
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodGet || request.Header.Get("x-api-key") != "test-control" || request.Header.Get("accept-encoding") != "identity" {
			t.Fatalf("unsafe request: %s %s %#v", request.Method, request.URL, request.Header)
		}
		observed = append(observed, request.URL.RequestURI())
		body := postureResponse(request.URL.Path)
		return jsonResponse(body), nil
	})}
	times := []time.Time{time.Date(2026, 8, 27, 1, 0, 0, 0, time.UTC), time.Date(2026, 8, 27, 1, 0, 1, 0, time.UTC)}
	posture, err := Capture(context.Background(), Config{
		BackendURL: observation.ExactBackendURL, ControlToken: "test-control",
		AgentID: "agent-1", OrganizationID: "org-1",
		PackDigest: "sha256:1111111111111111111111111111111111111111111111111111111111111111",
		Catalog:    Identity{Version: "2026-08-27-mvp1", Digest: "sha256:2222222222222222222222222222222222222222222222222222222222222222"},
		HTTP:       client, Now: func() time.Time { value := times[0]; times = times[1:]; return value },
	})
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	want := append(append([]string(nil), paths...), paths...)
	if strings.Join(observed, "\n") != strings.Join(want, "\n") {
		t.Fatalf("request sequence:\n%s\nwant:\n%s", strings.Join(observed, "\n"), strings.Join(want, "\n"))
	}
	if posture.CaptureWindow.Passes != 2 || posture.Agent.ID != "agent-1" || len(posture.Guardrails) != 1 || len(posture.Policies) != 1 || len(posture.BehaviorRules) != 1 {
		t.Fatalf("unexpected posture: %#v", posture)
	}
	if posture.CurrentPolicyID != "policy-1" || !posture.Policies[0].Current || posture.Guardrails[0].Type != "pii" || posture.BehaviorRuleAggregate.VersionHash != "behavior-set-v1" {
		t.Fatalf("safe projection lost stable identity: %#v", posture)
	}
	projection, err := json.Marshal(posture)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range [][]byte{[]byte("package ignored"), []byte(`"config"`), []byte(`"params"`), []byte(`"states"`), []byte(`"rego_code"`)} {
		if bytes.Contains(projection, forbidden) {
			t.Fatalf("unsafe raw control content entered the posture: %s", forbidden)
		}
	}
}

func TestCaptureRejectsDriftAndUnsafeResponse(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(request int, path, body string) string
	}{
		{name: "drift", mutate: func(request int, path, body string) string {
			if request >= 6 && path == "/agent/agent-1/guardrails" {
				return strings.Replace(body, "guardrail-v1", "guardrail-v2", 1)
			}
			return body
		}},
		{name: "credential field", mutate: func(_ int, path, body string) string {
			if path == "/agent/agent-1" {
				return `{"data":{"id":"agent-1","organization_id":"org-1","api_key":"forbidden"},"status":200}`
			}
			return body
		}},
		{name: "wrong control agent", mutate: func(_ int, path, body string) string {
			if path == "/agent/agent-1/policies" {
				return strings.Replace(body, `"agent_id":"agent-1"`, `"agent_id":"agent-2"`, 1)
			}
			return body
		}},
		{name: "wrong behavior organization", mutate: func(_ int, path, body string) string {
			if path == "/agent/agent-1/behavior-rule" {
				return strings.Replace(body, `"organization_id":"org-1"`, `"organization_id":"org-2"`, 1)
			}
			return body
		}},
		{name: "wrong permissions", mutate: func(_ int, path, body string) string {
			if path == "/auth/profile" {
				return strings.Replace(body, `,"read:agent_behavior_rule"`, "", 1)
			}
			return body
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			requests := 0
			client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				body := test.mutate(requests, request.URL.Path, postureResponse(request.URL.Path))
				requests++
				return jsonResponse(body), nil
			})}
			_, err := Capture(context.Background(), Config{
				BackendURL: observation.ExactBackendURL, ControlToken: "test-control",
				AgentID: "agent-1", OrganizationID: "org-1",
				PackDigest: "sha256:1111111111111111111111111111111111111111111111111111111111111111",
				Catalog:    Identity{Version: "2026-08-27-mvp1", Digest: "sha256:2222222222222222222222222222222222222222222222222222222222222222"},
				HTTP:       client,
			})
			if err == nil {
				t.Fatal("Capture accepted unsafe or drifting posture")
			}
		})
	}
}

func TestCaptureRejectsTransportEnvelopePaginationAndBoundsViolations(t *testing.T) {
	for _, test := range []struct {
		name      string
		configure func(*Config)
		response  func(*http.Request) *http.Response
	}{
		{name: "proxy environment", configure: func(config *Config) { config.ProxyConfigured = true }},
		{name: "redirect", response: func(request *http.Request) *http.Response {
			response := jsonResponse(postureResponse(request.URL.Path))
			response.StatusCode = http.StatusFound
			return response
		}},
		{name: "compressed representation", response: func(request *http.Request) *http.Response {
			response := jsonResponse(postureResponse(request.URL.Path))
			response.Header.Set("Content-Encoding", "gzip")
			return response
		}},
		{name: "oversized representation", response: func(request *http.Request) *http.Response {
			response := jsonResponse(postureResponse(request.URL.Path))
			response.ContentLength = maxResponseBytes + 1
			return response
		}},
		{name: "unknown envelope", response: func(request *http.Request) *http.Response {
			if request.URL.Path == "/auth/profile" {
				return jsonResponse(`{"data":{},"status":200,"extra":true}`)
			}
			return jsonResponse(postureResponse(request.URL.Path))
		}},
		{name: "pagination total mismatch", response: func(request *http.Request) *http.Response {
			body := postureResponse(request.URL.Path)
			if request.URL.Path == "/agent/agent-1/guardrails" {
				body = strings.Replace(body, `"total":1`, `"total":2`, 1)
			}
			return jsonResponse(body)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			config := Config{
				BackendURL: observation.ExactBackendURL, ControlToken: "test-control",
				AgentID: "agent-1", OrganizationID: "org-1",
				PackDigest: "sha256:1111111111111111111111111111111111111111111111111111111111111111",
				Catalog:    Identity{Version: "2026-08-27-mvp1", Digest: "sha256:2222222222222222222222222222222222222222222222222222222222222222"},
			}
			if test.configure != nil {
				test.configure(&config)
			}
			config.HTTP = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				calls++
				if test.response != nil {
					return test.response(request), nil
				}
				return jsonResponse(postureResponse(request.URL.Path)), nil
			})}
			if _, err := Capture(context.Background(), config); err == nil {
				t.Fatal("Capture accepted a closed transport or representation contract violation")
			}
			if test.name == "proxy environment" && calls != 0 {
				t.Fatalf("proxy rejection made %d requests", calls)
			}
		})
	}
}

func postureResponse(path string) string {
	permissions := `"create:agent","read:agent","update:agent","read:agent_session","read:agent_log","read:agent_guardrail","read:agent_policy","read:agent_behavior_rule"`
	switch path {
	case "/auth/profile":
		return `{"data":{"orgId":"org-1","isApiKeyAuth":true,"permissions":[` + permissions + `]},"status":200}`
	case "/agent/agent-1":
		return `{"data":{"id":"agent-1","organization_id":"org-1","status":0,"updated_at":"2026-08-27T00:00:00Z"},"status":200}`
	case "/agent/agent-1/guardrails":
		return `{"data":{"data":[{"id":"guardrail-1","version_hash":"guardrail-v1","agent_id":"agent-1","guardrail_type":"pii","processing_stage":"input","is_active":true,"order":1,"trust_impact":"none","params":{"ignored":true}}],"start":0,"limit":100,"total":1,"guardrail_versions_hash":"guardrail-set-v1","guardrail_versions_count":1,"guardrail_versions_updated_at":"2026-08-27T00:00:00Z"},"status":200}`
	case "/agent/agent-1/policies":
		return `{"data":{"data":[{"id":"policy-1","version_hash":"policy-v1","agent_id":"agent-1","is_active":true,"is_current_version":true,"rego_code":"package ignored","config":{"ignored":true}}],"start":0,"limit":100,"total":1},"status":200}`
	case "/agent/agent-1/policies/current":
		return `{"data":{"id":"policy-1","version_hash":"policy-v1","agent_id":"agent-1","rego_code":"package ignored"},"status":200}`
	case "/agent/agent-1/behavior-rule":
		return `{"data":{"data":[{"id":"behavior-1","version_hash":"behavior-v1","base_rule_id":"base-1","agent_id":"agent-1","organization_id":"org-1","trigger":"http_post","verdict":"REQUIRE_APPROVAL","priority":80,"is_active":true,"is_current_version":true,"time_window":60,"states":[{"ignored":true}]}],"start":0,"limit":100,"total":1,"behavior_rule_versions_hash":"behavior-set-v1","behavior_rule_versions_count":1,"behavior_rule_versions_updated_at":"2026-08-27T00:00:00Z"},"status":200}`
	default:
		return `{}`
	}
}

func jsonResponse(body string) *http.Response {
	return &http.Response{
		StatusCode:    http.StatusOK,
		Header:        http.Header{"Content-Type": []string{"application/json"}},
		Body:          io.NopCloser(bytes.NewBufferString(body)),
		ContentLength: int64(len(body)),
	}
}
