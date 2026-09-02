package observation

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func TestPreflightIsExactGETOnlyAndCredentialSeparated(t *testing.T) {
	agentID := "450999ca-ae2a-409c-8a26-d00a71132440"
	var paths []string
	httpClient := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		paths = append(paths, request.URL.RequestURI())
		if request.Method != http.MethodGet || request.Header.Get("authorization") != "" {
			t.Fatalf("request=%s auth=%q", request.Method, request.Header.Get("authorization"))
		}
		public := request.URL.Path == "/health"
		if public == (request.Header.Get("x-api-key") != "") {
			t.Fatalf("credential placement path=%s", request.URL.Path)
		}
		body := `{"status":200,"data":{}}`
		switch request.URL.Path {
		case "/health":
			body = `{"status":200,"data":"Success"}`
		case "/auth/profile":
			body = `{"status":200,"data":{"orgId":"openbox.ai","permissions":["create:agent","read:agent","update:agent","read:agent_session","read:agent_log","read:agent_guardrail","read:agent_policy","read:agent_behavior_rule"],"isApiKeyAuth":true,"setup":{"pending":false}}}`
		default:
			body = `{"status":200,"data":{"data":[],"start":0,"limit":100,"total":0}}`
		}
		return jsonResponse(body), nil
	})}
	client, err := New(Config{BackendURL: ExactBackendURL, ControlToken: "control-only", AgentID: agentID, HTTP: httpClient})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := client.Preflight(context.Background(), "ev-preflight")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"/health", "/auth/profile", "/agent/" + agentID + "/sessions?page=0&perPage=100&search=ev-preflight"}
	if !reflect.DeepEqual(paths, want) || len(snapshot.Entries) != len(want) {
		t.Fatalf("paths=%v", paths)
	}
	for index, entry := range snapshot.Entries {
		if entry.Ordinal != index+1 || entry.Method != "GET" || strings.Contains(entry.BodyBase64, "control-only") {
			t.Fatalf("entry=%+v", entry)
		}
	}
	if snapshot.Backend.APIContract != DashboardActivityContract || snapshot.Entries[2].Representation != "dashboard_public_projection" {
		t.Fatalf("snapshot=%+v", snapshot)
	}
}

func TestCollectRequiresStableExactTerminalSession(t *testing.T) {
	agentID, evaluationID, sessionID := "450999ca-ae2a-409c-8a26-d00a71132440", "ev-123", "ecfd94a0-e4c6-4ae8-96b2-72fc20f5e19a"
	started := time.Date(2026, 8, 26, 5, 0, 1, 0, time.UTC)
	session := `{"id":"` + sessionID + `","agent_id":"` + agentID + `","run_id":"` + evaluationID + `","status":"completed","started_at":"2026-08-26T05:00:01Z","completed_at":"2026-08-26T05:00:02Z"}`
	event := `{"id":"event-1","event_type":"WorkflowStarted","agent_id":"` + agentID + `","session_id":"` + sessionID + `","run_id":"` + evaluationID + `","created_at":"2026-08-26T05:00:01.1Z"}`
	httpClient := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		var body string
		switch {
		case strings.Contains(request.URL.Path, "/logs/chronological"):
			body = `{"status":200,"data":{"data":[` + event + `],"start":0,"limit":100,"total":1}}`
		case strings.HasSuffix(request.URL.Path, "/"+sessionID):
			body = `{"status":200,"data":` + session + `}`
		default:
			body = `{"status":200,"data":{"data":[` + session + `],"start":0,"limit":100,"total":1}}`
		}
		return jsonResponse(body), nil
	})}
	client, err := New(Config{BackendURL: ExactBackendURL, ControlToken: "control", AgentID: agentID, HTTP: httpClient, Now: func() time.Time { return started }})
	if err != nil {
		t.Fatal(err)
	}
	client.organization = "openbox.ai"
	result, err := client.Collect(context.Background(), Window{EvaluationID: evaluationID, StartedAt: started.Add(-time.Second), Deadline: started.Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if result.Session.ID != sessionID || len(result.Events) != 1 || result.Events[0].SourceOrdinal < 1 || len(result.Entries) != 6 {
		t.Fatalf("result=%+v entries=%d", result, len(result.Entries))
	}
}

func TestClientRejectsRemoteProxyAndBroaderPermission(t *testing.T) {
	base := Config{BackendURL: ExactBackendURL, ControlToken: "control", AgentID: "450999ca-ae2a-409c-8a26-d00a71132440", HTTP: http.DefaultClient}
	remote := base
	remote.BackendURL = "http://localhost:3000"
	proxy := base
	proxy.ProxyConfigured = true
	if _, err := New(remote); err == nil {
		t.Fatal("accepted alternate loopback")
	}
	if _, err := New(proxy); err == nil {
		t.Fatal("accepted proxy environment")
	}
	responses := []string{
		`{"status":200,"data":"Success"}`,
		`{"status":200,"data":{"orgId":"openbox.ai","permissions":["create:agent","read:agent","update:agent","read:agent_session","read:agent_log","read:agent_guardrail","read:agent_policy","read:agent_behavior_rule","manage:agent_session"],"isApiKeyAuth":true,"setup":{"pending":false}}}`,
	}
	index := 0
	broader := base
	broader.HTTP = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		body := responses[index]
		index++
		return jsonResponse(body), nil
	})}
	client, err := New(broader)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Preflight(context.Background(), "ev-preflight"); err == nil {
		t.Fatal("accepted broader permission profile")
	}
}

func TestDashboardProjectionDropsInternalAgentRelationBeforeCapture(t *testing.T) {
	agentID := "450999ca-ae2a-409c-8a26-d00a71132440"
	client, err := New(Config{BackendURL: ExactBackendURL, ControlToken: "control", AgentID: agentID, HTTP: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return jsonResponse(`{"status":200,"data":{"data":[{"id":"event-1","event_type":"ToolCompleted","agent_id":"` + agentID + `","session_id":"session-1","run_id":"ev-1","created_at":"2026-08-26T05:00:01Z","output":{"ok":true},"agent":{"token":"stored-verifier","config":{"password":"secret"}}}],"start":0,"limit":100,"total":1}}`), nil
	})}})
	if err != nil {
		t.Fatal(err)
	}
	body, err := client.get(context.Background(), "/agent/"+agentID+"/sessions/session-1/logs/chronological?page=0&perPage=100", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "stored-verifier") || strings.Contains(string(body), `"agent"`) || !strings.Contains(string(body), `"ToolCompleted"`) || client.Entries()[0].Representation != "dashboard_public_projection" {
		t.Fatalf("projection=%s entry=%+v", body, client.Entries()[0])
	}
}

func TestDashboardProjectionPreservesSessionActivityFields(t *testing.T) {
	page := []byte(`{"status":200,"data":{"data":[{"id":"session-1","agent_id":"agent-1","run_id":"ev-1","status":"completed","started_at":"2026-08-26T05:00:01Z","completed_at":"2026-08-26T05:00:02Z","current_step":{"id":"event-1","event_type":"ToolCompleted","output":{"ok":true},"agent":{"token":"verifier"}},"governance_events":[{"id":"event-1","spans":[{"name":"model"}],"agent":{"token":"verifier"}}]}],"start":0,"limit":100,"total":1}}`)
	projected, err := projectDashboardActivityResponse("/agent/agent-1/sessions", page)
	if err != nil {
		t.Fatal(err)
	}
	text := string(projected)
	for _, retained := range []string{`"event_type":"ToolCompleted"`, `"output":{"ok":true}`, `"spans":[{"name":"model"}]`} {
		if !strings.Contains(text, retained) {
			t.Fatalf("activity field %s missing from %s", retained, text)
		}
	}
	if strings.Contains(text, `"agent"`) || strings.Contains(text, "verifier") {
		t.Fatalf("internal relation retained: %s", text)
	}
}

func TestCredentialMaterialScannerDistinguishesUsageFromCredentials(t *testing.T) {
	if err := rejectCredentialMaterial([]byte(`{"usage":{"inputTokens":12,"output_tokens":4}}`)); err != nil {
		t.Fatalf("ordinary model token counts were rejected: %v", err)
	}
	for _, body := range []string{
		`{"message":"nested value obx_key_0123456789"}`,
		`{"privateKey":"encoded-signing-material"}`,
		`{"nested":{"authorization":"Bearer value"}}`,
	} {
		if err := rejectCredentialMaterial([]byte(body)); err == nil {
			t.Fatalf("credential material accepted: %s", body)
		}
	}
}

func TestPaginationAndRedirectShapesAreClosed(t *testing.T) {
	if _, _, _, _, err := decodePage([]byte(`{"data":[],"start":100,"limit":100,"total":0}`), 0, nil); err == nil {
		t.Fatal("accepted wrong page start")
	}
	if _, _, _, _, err := decodePage([]byte(`{"data":[],"start":0,"limit":100,"total":0,"next":"x"}`), 0, nil); err == nil {
		t.Fatal("accepted unknown pagination field")
	}
	httpClient := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 302, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{}`)), ContentLength: 2}, nil
	})}
	client, err := New(Config{BackendURL: ExactBackendURL, ControlToken: "control", AgentID: "450999ca-ae2a-409c-8a26-d00a71132440", HTTP: httpClient})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Preflight(context.Background(), "ev-preflight"); err == nil {
		t.Fatal("accepted redirect")
	}
}

func TestCollectionRejectsAmbiguousPendingWrongRunAndCrossWindowSessions(t *testing.T) {
	agentID, evaluationID := "450999ca-ae2a-409c-8a26-d00a71132440", "ev-123"
	base := time.Date(2026, 8, 26, 5, 0, 0, 0, time.UTC)
	makeSession := func(id, agent, run, status, started string) string {
		return `{"id":"` + id + `","agent_id":"` + agent + `","run_id":"` + run + `","status":"` + status + `","started_at":"` + started + `","completed_at":null}`
	}
	tests := map[string]string{
		"multiple":     makeSession("session-1", agentID, evaluationID, "completed", "2026-08-26T05:00:01Z") + `,` + makeSession("session-2", agentID, evaluationID, "completed", "2026-08-26T05:00:01Z"),
		"pending":      makeSession("session-1", agentID, evaluationID, "running", "2026-08-26T05:00:01Z"),
		"wrong-run":    makeSession("session-1", agentID, "ev-other", "completed", "2026-08-26T05:00:01Z"),
		"cross-window": makeSession("session-1", agentID, evaluationID, "completed", "2026-08-26T04:00:01Z"),
	}
	for name, sessions := range tests {
		t.Run(name, func(t *testing.T) {
			httpClient := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return jsonResponse(`{"status":200,"data":{"data":[` + sessions + `],"start":0,"limit":100,"total":` + strconv.Itoa(strings.Count(sessions, `"id"`)) + `}}`), nil
			})}
			client, err := New(Config{BackendURL: ExactBackendURL, ControlToken: "control", AgentID: agentID, HTTP: httpClient, Now: func() time.Time { return base.Add(time.Minute) }})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := client.Collect(context.Background(), Window{EvaluationID: evaluationID, StartedAt: base, Deadline: base.Add(time.Minute)}); err == nil {
				t.Fatal("accepted invalid session resolution")
			}
		})
	}
}

func TestCollectionRejectsTerminalStatusDriftAndDuplicateEvents(t *testing.T) {
	agentID, evaluationID, sessionID := "450999ca-ae2a-409c-8a26-d00a71132440", "ev-123", "ecfd94a0-e4c6-4ae8-96b2-72fc20f5e19a"
	base := time.Date(2026, 8, 26, 5, 0, 0, 0, time.UTC)
	terminal := `{"id":"` + sessionID + `","agent_id":"` + agentID + `","run_id":"` + evaluationID + `","status":"completed","started_at":"2026-08-26T05:00:01Z","completed_at":"2026-08-26T05:00:02Z"}`
	drifted := strings.Replace(terminal, `"status":"completed"`, `"status":"failed"`, 1)
	event := `{"id":"event-1","event_type":"WorkflowStarted","agent_id":"` + agentID + `","session_id":"` + sessionID + `","run_id":"` + evaluationID + `","created_at":"2026-08-26T05:00:01.1Z"}`
	for name, driftStatus := range map[string]bool{"status-drift": true, "duplicate-event": false} {
		t.Run(name, func(t *testing.T) {
			detailCalls := 0
			httpClient := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				body := `{"status":200,"data":{"data":[` + terminal + `],"start":0,"limit":100,"total":1}}`
				if strings.Contains(request.URL.Path, "/logs/chronological") {
					if !driftStatus {
						body = `{"status":200,"data":{"data":[` + event + `,` + event + `],"start":0,"limit":100,"total":2}}`
					} else {
						body = `{"status":200,"data":{"data":[` + event + `],"start":0,"limit":100,"total":1}}`
					}
				}
				if strings.HasSuffix(request.URL.Path, "/"+sessionID) {
					detailCalls++
					selected := terminal
					if driftStatus && detailCalls > 1 {
						selected = drifted
					}
					body = `{"status":200,"data":` + selected + `}`
				}
				return jsonResponse(body), nil
			})}
			client, err := New(Config{BackendURL: ExactBackendURL, ControlToken: "control", AgentID: agentID, HTTP: httpClient, Now: func() time.Time { return base }})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := client.Collect(context.Background(), Window{EvaluationID: evaluationID, StartedAt: base, Deadline: base.Add(time.Minute)}); err == nil {
				t.Fatal("accepted unstable collection")
			}
		})
	}
}

func TestCaptureRejectsCompressedAndOversizedResponses(t *testing.T) {
	agentID := "450999ca-ae2a-409c-8a26-d00a71132440"
	for name, response := range map[string]*http.Response{
		"compressed": {StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}, "Content-Encoding": []string{"gzip"}}, Body: io.NopCloser(strings.NewReader(`{}`)), ContentLength: 2},
		"oversized":  {StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{}`)), ContentLength: MaxResponseBytes + 1},
	} {
		t.Run(name, func(t *testing.T) {
			client, err := New(Config{BackendURL: ExactBackendURL, ControlToken: "control", AgentID: agentID, HTTP: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return response, nil })}})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := client.get(context.Background(), "/health", false, nil); err == nil {
				t.Fatal("accepted transformed or oversized response")
			}
		})
	}
}

func jsonResponse(body string) *http.Response {
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json; charset=utf-8"}}, Body: io.NopCloser(bytes.NewBufferString(body)), ContentLength: int64(len(body))}
}
