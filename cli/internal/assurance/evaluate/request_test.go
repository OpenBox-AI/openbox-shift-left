package evaluate

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	testToken = "ctl-secret-token-do-not-print"
	testAgent = "c59e95b6-2a4e-44a7-8c43-b69bfa77667e"
)

// fakeClock never really sleeps: Sleep advances Now, so a 10-minute bound is
// exercised in microseconds and the retry cadence is observable.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	sleeps []time.Duration
}

func newFakeClock() *fakeClock { return &fakeClock{now: time.Unix(1_800_000_000, 0).UTC()} }

func (clock *fakeClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.now
}

func (clock *fakeClock) Sleep(ctx context.Context, duration time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	clock.mu.Lock()
	defer clock.mu.Unlock()
	clock.now = clock.now.Add(duration)
	clock.sleeps = append(clock.sleeps, duration)
	return nil
}

func (clock *fakeClock) sleepCount() int {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return len(clock.sleeps)
}

func testClient(t *testing.T, server *httptest.Server, clock Clock) *evaluationClient {
	t.Helper()
	client, err := newEvaluationClient(server.URL, testToken, server.Client(), clock)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func evaluationJSON(status string) string {
	return fmt.Sprintf(`{"status":200,"data":{"id":"se-1","status":%q,"failure_reason":null,"session_id":"sess-1","report":null}}`, status)
}

func TestRequestPostsRunIDWithKeyHeader(t *testing.T) {
	var method, path, key, contentType, body string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		key, contentType = r.Header.Get("x-api-key"), r.Header.Get("content-type")
		raw := make([]byte, 512)
		n, _ := r.Body.Read(raw)
		body = string(raw[:n])
		if r.Header.Get("authorization") != "" {
			t.Errorf("token must travel in x-api-key only, saw Authorization")
		}
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, evaluationJSON("queued"))
	}))
	defer server.Close()

	evaluation, err := testClient(t, server, newFakeClock()).request(context.Background(), testAgent, "ev-abc")
	if err != nil {
		t.Fatal(err)
	}
	if method != http.MethodPost || path != "/agent/"+testAgent+"/security-evaluations" || key != testToken || contentType != "application/json" {
		t.Fatalf("method=%s path=%s key-set=%v content-type=%s", method, path, key == testToken, contentType)
	}
	if strings.TrimSpace(body) != `{"run_id":"ev-abc"}` {
		t.Fatalf("body=%q", body)
	}
	if evaluation.ID != "se-1" || evaluation.Status != "queued" || evaluation.SessionID != "sess-1" {
		t.Fatalf("evaluation=%+v", evaluation)
	}
}

func TestRequestRefusalsAreActionable(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		want   []string
	}{
		{"forbidden", 403, `{"statusCode":403,"message":"Forbidden resource"}`, []string{"evaluate:agent_security"}},
		{"both models missing", 422, `{"status":422,"code":"connector_required","missing":["decision","llm"]}`, []string{"organization", "decision model", "language model"}},
		{"only the decision model missing", 422, `{"status":422,"code":"connector_required","missing":["decision"]}`, []string{"decision model"}},
		{"only the language model missing", 422, `{"status":422,"code":"connector_required","missing":["llm"]}`, []string{"language model"}},
		{"missing not stated", 422, `{"status":422,"code":"connector_required"}`, []string{"decision model", "language model"}},
		{"other 422", 422, `{"code":"invalid_run","message":"run id is malformed"}`, []string{"422", "run id is malformed"}},
		// The backend reports a validation failure as 422 with the reasons in an
		// `error` array and only a generic title in `message`.
		{"validation reasons", 422, `{"status":422,"message":"Unprocessable Entity Exception","error":["run_id must be shorter than or equal to 255 characters","session_id must be a UUID"]}`, []string{"422", "run_id must be shorter", "session_id must be a UUID"}},
		{"conflict", 409, `{"message":"ambiguous"}`, []string{"more than one session", "409"}},
		{"bad request", 400, `{"message":"run_id must be a string"}`, []string{"400", "run_id must be a string"}},
		{"unauthorized", 401, `{}`, []string{"did not accept the control token"}},
		{"server error", 500, `oops`, []string{"500"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.WriteHeader(test.status)
				fmt.Fprint(w, test.body)
			}))
			defer server.Close()
			clock := newFakeClock()
			_, err := testClient(t, server, clock).request(context.Background(), testAgent, "ev-abc")
			if err == nil {
				t.Fatal("refusal returned no error")
			}
			for _, want := range test.want {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error %q does not contain %q", err, want)
				}
			}
			if strings.Contains(err.Error(), testToken) {
				t.Fatalf("error leaks the token: %v", err)
			}
			// Only 404 is retried; every other refusal is final.
			if calls.Load() != 1 || clock.sleepCount() != 0 {
				t.Fatalf("calls=%d sleeps=%d", calls.Load(), clock.sleepCount())
			}
		})
	}
}

func TestRequestRetriesNotFoundUntilTheSessionAppears(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) < 4 {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"message":"no session"}`)
			return
		}
		fmt.Fprint(w, evaluationJSON("queued"))
	}))
	defer server.Close()
	clock := newFakeClock()
	evaluation, err := testClient(t, server, clock).request(context.Background(), testAgent, "ev-abc")
	if err != nil || evaluation.ID != "se-1" {
		t.Fatalf("evaluation=%+v err=%v", evaluation, err)
	}
	if calls.Load() != 4 || clock.sleepCount() != 3 || clock.sleeps[0] != sessionLookupInterval {
		t.Fatalf("calls=%d sleeps=%v", calls.Load(), clock.sleeps)
	}
}

func TestRequestGivesUpOnNotFoundWithinTheWindow(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()
	clock := newFakeClock()
	started := clock.Now()
	_, err := testClient(t, server, clock).request(context.Background(), testAgent, "ev-abc")
	if err == nil || !strings.Contains(err.Error(), "ev-abc") || !strings.Contains(err.Error(), "no session") {
		t.Fatalf("err=%v", err)
	}
	if elapsed := clock.Now().Sub(started); elapsed > sessionLookupWindow || elapsed < sessionLookupWindow-2*sessionLookupInterval {
		t.Fatalf("retried for %s, want about %s", elapsed, sessionLookupWindow)
	}
	if calls.Load() < 10 {
		t.Fatalf("calls=%d", calls.Load())
	}
}

func TestRequestHonoursContextWhileRetrying(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		cancel()
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()
	_, err := testClient(t, server, newFakeClock()).request(ctx, testAgent, "ev-abc")
	if err == nil || classify(err) != "interrupted" {
		t.Fatalf("class=%s err=%v", classify(err), err)
	}
}

func TestRequestRejectsAnAnswerWithoutAnID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"status":200,"data":{"status":"queued"}}`)
	}))
	defer server.Close()
	if _, err := testClient(t, server, newFakeClock()).request(context.Background(), testAgent, "ev-abc"); err == nil {
		t.Fatal("accepted an evaluation with no id")
	}
}

func TestWaitPollsToComplete(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/agent/"+testAgent+"/security-evaluations/se-1" || r.Header.Get("x-api-key") != testToken {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		switch calls.Add(1) {
		case 1:
			fmt.Fprint(w, evaluationJSON("waiting_for_session"))
		case 2:
			fmt.Fprint(w, evaluationJSON("analyzing"))
		default:
			fmt.Fprint(w, `{"status":200,"data":{"id":"se-1","status":"complete","report":{"result":"fail"}}}`)
		}
	}))
	defer server.Close()
	clock := newFakeClock()
	evaluation, err := testClient(t, server, clock).wait(context.Background(), testAgent, "se-1")
	if err != nil || evaluation.Status != StatusComplete || evaluation.Report == nil || evaluation.Report.Result != "fail" {
		t.Fatalf("evaluation=%+v err=%v", evaluation, err)
	}
	if calls.Load() != 3 || clock.sleepCount() != 2 || clock.sleeps[0] != pollInterval {
		t.Fatalf("calls=%d sleeps=%v", calls.Load(), clock.sleeps)
	}
}

func TestWaitReturnsAFailedEvaluationToTheCaller(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"status":200,"data":{"id":"se-1","status":"failed","failure_reason":"model refused"}}`)
	}))
	defer server.Close()
	evaluation, err := testClient(t, server, newFakeClock()).wait(context.Background(), testAgent, "se-1")
	if err != nil || evaluation.Status != StatusFailed || evaluation.FailureReason != "model refused" {
		t.Fatalf("evaluation=%+v err=%v", evaluation, err)
	}
}

func TestWaitIsBoundedByTenMinutes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, evaluationJSON("analyzing"))
	}))
	defer server.Close()
	clock := newFakeClock()
	started := clock.Now()
	_, err := testClient(t, server, clock).wait(context.Background(), testAgent, "se-1")
	if err == nil || !strings.Contains(err.Error(), "analyzing") || !strings.Contains(err.Error(), "se-1") {
		t.Fatalf("err=%v", err)
	}
	if elapsed := clock.Now().Sub(started); elapsed > waitWindow || elapsed < waitWindow-2*pollInterval {
		t.Fatalf("polled for %s, want about %s", elapsed, waitWindow)
	}
}

func TestWaitAbsorbsBriefServerErrorsButNotClientErrors(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		fmt.Fprint(w, evaluationJSON("complete"))
	}))
	defer server.Close()
	if evaluation, err := testClient(t, server, newFakeClock()).wait(context.Background(), testAgent, "se-1"); err != nil || evaluation.Status != StatusComplete {
		t.Fatalf("evaluation=%+v err=%v", evaluation, err)
	}

	var forbidden atomic.Int32
	denied := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		forbidden.Add(1)
		w.WriteHeader(http.StatusForbidden)
	}))
	defer denied.Close()
	if _, err := testClient(t, denied, newFakeClock()).wait(context.Background(), testAgent, "se-1"); err == nil || !strings.Contains(err.Error(), "evaluate:agent_security") || forbidden.Load() != 1 {
		t.Fatalf("err=%v calls=%d", err, forbidden.Load())
	}

	var unavailable atomic.Int32
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		unavailable.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer down.Close()
	if _, err := testClient(t, down, newFakeClock()).wait(context.Background(), testAgent, "se-1"); err == nil || unavailable.Load() != pollTolerance+1 {
		t.Fatalf("err=%v calls=%d", err, unavailable.Load())
	}
}

func TestWaitHonoursContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		cancel()
		fmt.Fprint(w, evaluationJSON("analyzing"))
	}))
	defer server.Close()
	_, err := testClient(t, server, newFakeClock()).wait(ctx, testAgent, "se-1")
	if err == nil || classify(err) != "interrupted" {
		t.Fatalf("class=%s err=%v", classify(err), err)
	}
}

// The token is the one value that must never reach output, on any path.
func TestTheTokenNeverAppearsInAnyError(t *testing.T) {
	echo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A misbehaving backend or proxy that reflects the credential.
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(w, `{"message":"bad key %s"}`, r.Header.Get("x-api-key"))
	}))
	defer echo.Close()
	_, err := testClient(t, echo, newFakeClock()).request(context.Background(), testAgent, "ev-abc")
	if err == nil || strings.Contains(err.Error(), testToken) {
		t.Fatalf("err=%v", err)
	}
	if !strings.Contains(err.Error(), "[redacted]") {
		t.Fatalf("expected the reflected token to be redacted, got %v", err)
	}

	// A transport failure whose own text contains the token (here, a base URL
	// path segment that happens to equal it).
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := dead.URL + "/" + testToken
	dead.Close()
	client, err := newEvaluationClient(url, testToken, http.DefaultClient, newFakeClock())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.request(context.Background(), testAgent, "ev-abc"); err == nil || strings.Contains(err.Error(), testToken) {
		t.Fatalf("transport error leaks the token: %v", err)
	}
	if _, err := client.wait(context.Background(), testAgent, "se-1"); err == nil || strings.Contains(err.Error(), testToken) {
		t.Fatalf("poll error leaks the token: %v", err)
	}
}

func TestResponseBodiesAreCapped(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"status":200,"data":{"id":"se-1","status":"queued","pad":"`)
		fmt.Fprint(w, strings.Repeat("a", maxResponseBytes))
		fmt.Fprint(w, `"}}`)
	}))
	defer server.Close()
	_, err := testClient(t, server, newFakeClock()).request(context.Background(), testAgent, "ev-abc")
	if err == nil || !strings.Contains(err.Error(), "1 MiB") {
		t.Fatalf("err=%v", err)
	}
}

func TestBaseURLIsStrictLikeTheConnector(t *testing.T) {
	for name, value := range map[string]string{
		"no scheme":    "backend.example.com",
		"wrong scheme": "ftp://backend.example.com",
		"userinfo":     "https://user:pass@backend.example.com",
		"query":        "https://backend.example.com?x=1",
		"fragment":     "https://backend.example.com#x",
		"empty":        "",
	} {
		if _, err := newEvaluationClient(value, testToken, http.DefaultClient, newFakeClock()); err == nil {
			t.Fatalf("%s was accepted", name)
		}
	}
	if _, err := newEvaluationClient("https://backend.example.com", "", http.DefaultClient, newFakeClock()); err == nil {
		t.Fatal("accepted an empty token")
	}
	if _, err := newEvaluationClient("https://backend.example.com", "a\r\nx-evil: 1", http.DefaultClient, newFakeClock()); err == nil {
		t.Fatal("accepted a token with control characters")
	}
	client, err := newEvaluationClient("https://backend.example.com/", testToken, http.DefaultClient, newFakeClock())
	if err != nil {
		t.Fatal(err)
	}
	if got := client.endpoint("a/b", "se 1"); got != "https://backend.example.com/agent/a%2Fb/security-evaluations/se%201" {
		t.Fatalf("endpoint=%s", got)
	}
}

// A redirect must not carry x-api-key anywhere: Go strips only Authorization
// across hosts, so the production client refuses to follow at all.
func TestRedirectsAreNotFollowedAndTheKeyGoesNowhereElse(t *testing.T) {
	var leaked atomic.Int32
	elsewhere := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { leaked.Add(1) }))
	defer elsewhere.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+"/steal", http.StatusTemporaryRedirect)
	}))
	defer origin.Close()
	client, err := newEvaluationClient(origin.URL, testToken, newBackendHTTPClient(), newFakeClock())
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.request(context.Background(), testAgent, "ev-abc")
	if err == nil || !strings.Contains(err.Error(), "307") || leaked.Load() != 0 {
		t.Fatalf("err=%v leaked=%d", err, leaked.Load())
	}
}

// The reflected token is cut by the 200-byte display bound; scrubbing has to
// happen first or a token straddling the cut survives as a prefix.
func TestTokenStraddlingTheDisplayBoundIsNotLeakedAsAPrefix(t *testing.T) {
	echo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		padding := strings.Repeat("x", maxDisplayBytes-10)
		fmt.Fprintf(w, `{"message":"%s %s","error":["%s %s"]}`, padding, r.Header.Get("x-api-key"), padding, r.Header.Get("x-api-key"))
	}))
	defer echo.Close()
	_, err := testClient(t, echo, newFakeClock()).request(context.Background(), testAgent, "ev-abc")
	if err == nil {
		t.Fatal("expected a refusal")
	}
	if prefix := testToken[:6]; strings.Contains(err.Error(), prefix) {
		t.Fatalf("a prefix of the token survived truncation: %v", err)
	}
}

func TestBackendSuppliedStatusCannotCarryTerminalEscapes(t *testing.T) {
	var out strings.Builder
	writeSummary(&out, &Evaluation{ID: "se-1", Status: "complete\x1b[2J\x1b]0;pwned\a"})
	if strings.ContainsAny(out.String(), "\x1b\a") {
		t.Fatalf("summary carries a terminal escape:\n%q", out.String())
	}

	// The same field in the poll-timeout message.
	clock := newFakeClock()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"status":200,"data":{"id":"se-1","status":"analyzing\u001b[31m"}}`)
	}))
	defer server.Close()
	_, err := testClient(t, server, clock).wait(context.Background(), testAgent, "se-1")
	if err == nil || strings.Contains(err.Error(), "\x1b") {
		t.Fatalf("timeout message carries a terminal escape: %q", err)
	}
}

func TestSummaryCountsIssuesRulesAndRejections(t *testing.T) {
	var out strings.Builder
	evaluation := &Evaluation{ID: "se-1", Status: StatusComplete}
	if err := decodeInto(`{
		"result":"fail","security_pass":false,
		"issues":[{"title":"Credential read then egress"},{"title":"Unapproved\u001b[31m send"}],
		"recommendations":[
			{"catalog_entry_id":"human-authorization","status":"new_gap","target":{"action_name":"send_email"},"rule":{"body":{"decision":"REQUIRE_APPROVAL"}}},
			{"catalog_entry_id":"human-authorization","status":"new_gap","rule":{"body":{"conditions":[{"right":{"value":"post_webhook"}}]}}},
			{"catalog_entry_id":"observation-gap","status":"unavailable","rule":null},
			{"catalog_entry_id":"guardrail-x","status":"unavailable"}
		],
		"rejected_candidates":[{"why":"uncited"},{"why":"forbidden key"}]
	}`, &evaluation.Report); err != nil {
		t.Fatal(err)
	}
	writeSummary(&out, evaluation)
	text := out.String()
	for _, want := range []string{
		"security evaluation se-1: complete",
		"result: fail",
		"issues: 2",
		"  - Credential read then egress",
		"  - Unapproved [31m send", // the escape byte is neutralised
		"recommendations with a suggested rule: 2",
		"  suggested rule: human-authorization on send_email",
		"  suggested rule: human-authorization on post_webhook",
		"rejected candidates: 2",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("summary missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "\x1b") {
		t.Fatalf("summary carries a terminal escape:\n%q", text)
	}
	if strings.Contains(text, "observation-gap") || strings.Contains(text, "guardrail-x") {
		t.Fatalf("recommendations without a rule were listed as rules:\n%s", text)
	}
}

func TestSummaryWithoutAReportAndWithACountOfRejections(t *testing.T) {
	var out strings.Builder
	writeSummary(&out, &Evaluation{ID: "se-1", Status: StatusComplete})
	if !strings.Contains(out.String(), "result: unavailable") {
		t.Fatalf("summary=%q", out.String())
	}
	report := &Report{RejectedCandidates: []byte(`3`)}
	if report.rejectedCount() != 3 || (&Report{}).rejectedCount() != 0 || (&Report{RejectedCandidates: []byte(`null`)}).rejectedCount() != 0 {
		t.Fatal("rejected count is wrong")
	}
}

func TestDisplayBoundsAndCleansBackendText(t *testing.T) {
	if got := display("a\x00b\r\nc\x1b[2Jd"); strings.ContainsAny(got, "\x00\r\n\x1b") {
		t.Fatalf("display=%q", got)
	}
	long := display(strings.Repeat("é", 500))
	if len(long) > maxDisplayBytes+3 || !strings.HasSuffix(long, "...") {
		t.Fatalf("len=%d", len(long))
	}
}

func decodeInto(text string, target **Report) error {
	report := &Report{}
	if err := json.Unmarshal([]byte(text), report); err != nil {
		return err
	}
	*target = report
	return nil
}

func TestConnectorRequiredNamesOnlyWhatIsMissing(t *testing.T) {
	for _, test := range []struct {
		body    string
		without string
	}{
		{`{"code":"connector_required","missing":["decision"]}`, "language model"},
		{`{"code":"connector_required","missing":["llm"]}`, "decision model"},
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(422)
			fmt.Fprint(w, test.body)
		}))
		_, err := testClient(t, server, newFakeClock()).request(context.Background(), testAgent, "ev-abc")
		server.Close()
		if err == nil || strings.Contains(err.Error(), test.without) {
			t.Fatalf("error %v should not mention the %s it already has", err, test.without)
		}
	}
}
