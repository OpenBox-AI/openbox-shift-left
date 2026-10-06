package evaluate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// The backend contract this file speaks (ADR-0023):
//
//	POST {backend}/agent/{agentId}/security-evaluations      {"run_id": "<id>"}
//	GET  {backend}/agent/{agentId}/security-evaluations/{id}
//
// authenticated by `x-api-key`, with responses wrapped as
// {"status":200,"data":{...}}. The evaluation itself runs at the backend; the
// CLI only asks and, with --wait, reads the result.

const (
	// maxResponseBytes caps every backend response body.
	maxResponseBytes = 1 << 20

	// sessionLookupWindow bounds the retry on 404. Core stores the session
	// asynchronously, so a run that just ended may not be visible for a moment.
	sessionLookupWindow   = 30 * time.Second
	sessionLookupInterval = 2 * time.Second

	pollInterval = 3 * time.Second
	// waitWindow bounds the whole --wait poll.
	waitWindow = 10 * time.Minute
	// pollTolerance is how many consecutive transport or 5xx failures a poll
	// absorbs before it gives up. A client error never retries.
	pollTolerance = 5

	StatusComplete = "complete"
	StatusFailed   = "failed"

	maxDisplayBytes = 200
)

// Evaluation is the backend's security-evaluation row, as far as the CLI reads
// it. Unknown fields are ignored on purpose: the backend versions its report
// independently of this binary.
type Evaluation struct {
	ID            string  `json:"id"`
	Status        string  `json:"status"`
	FailureReason string  `json:"failure_reason"`
	SessionID     string  `json:"session_id"`
	Report        *Report `json:"report"`
}

// Report is the part of the backend report the summary prints.
type Report struct {
	Result             string           `json:"result"`
	SecurityPass       bool             `json:"security_pass"`
	Issues             []ReportIssue    `json:"issues"`
	Recommendations    []Recommendation `json:"recommendations"`
	RejectedCandidates json.RawMessage  `json:"rejected_candidates"`
}

type ReportIssue struct {
	Title string `json:"title"`
}

type Recommendation struct {
	CatalogEntryID string `json:"catalog_entry_id"`
	Status         string `json:"status"`
	Target         struct {
		ActionName string `json:"action_name"`
	} `json:"target"`
	Rule json.RawMessage `json:"rule"`
}

// HasRule reports whether the recommendation carries a suggested rule.
func (recommendation Recommendation) HasRule() bool {
	trimmed := bytes.TrimSpace(recommendation.Rule)
	return len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null"))
}

// actionName is the action a rule is for: the recommendation's own target when
// the backend names one, otherwise the literal in the rule's first condition.
func (recommendation Recommendation) actionName() string {
	if recommendation.Target.ActionName != "" {
		return recommendation.Target.ActionName
	}
	var rule struct {
		Body struct {
			Conditions []struct {
				Right struct {
					Value any `json:"value"`
				} `json:"right"`
			} `json:"conditions"`
		} `json:"body"`
		Conditions []struct {
			Right struct {
				Value any `json:"value"`
			} `json:"right"`
		} `json:"conditions"`
	}
	if json.Unmarshal(recommendation.Rule, &rule) == nil {
		conditions := rule.Body.Conditions
		if len(conditions) == 0 {
			conditions = rule.Conditions
		}
		if len(conditions) > 0 {
			if name, ok := conditions[0].Right.Value.(string); ok && name != "" {
				return name
			}
		}
	}
	return "an unnamed action"
}

// rejectedCount accepts a list or a plain count; anything else is zero.
func (report *Report) rejectedCount() int {
	var list []json.RawMessage
	if json.Unmarshal(report.RejectedCandidates, &list) == nil {
		return len(list)
	}
	var count int
	if json.Unmarshal(report.RejectedCandidates, &count) == nil && count > 0 {
		return count
	}
	return 0
}

// statusError is a non-2xx answer, already reduced to what is safe to show.
type statusError struct {
	status  int
	message string
}

func (failure *statusError) Error() string { return failure.message }

// evaluationClient talks to the backend with one control token. The token is
// held privately, sent only in the x-api-key header, and scrubbed from every
// message this client produces.
type evaluationClient struct {
	base  *url.URL
	token string
	http  HTTPDoer
	clock Clock
}

func newEvaluationClient(baseURL, token string, doer HTTPDoer, clock Clock) (*evaluationClient, error) {
	if doer == nil || clock == nil {
		return nil, errors.New("project evaluate: evaluation client is missing its HTTP client or clock")
	}
	if strings.TrimSpace(token) == "" || strings.ContainsAny(token, "\x00\r\n") {
		return nil, errors.New("project evaluate: OPENBOX_CONTROL_TOKEN is required to request the security evaluation")
	}
	base, err := parseBaseURL("backend", strings.TrimRight(baseURL, "/"))
	if err != nil {
		return nil, err
	}
	if base.RawQuery != "" || base.Fragment != "" {
		return nil, errors.New("project evaluate: backend URL must not carry a query or fragment")
	}
	return &evaluationClient{base: base, token: token, http: doer, clock: clock}, nil
}

func (client *evaluationClient) endpoint(agentID string, parts ...string) string {
	escaped := client.base.EscapedPath() + "/agent/" + url.PathEscape(agentID) + "/security-evaluations"
	for _, part := range parts {
		escaped += "/" + url.PathEscape(part)
	}
	endpoint := *client.base
	endpoint.Path, _ = url.PathUnescape(escaped)
	endpoint.RawPath = escaped
	return endpoint.String()
}

// request asks the backend to evaluate runID. A 404 means the session is not
// stored yet, so it is retried for sessionLookupWindow; every other refusal is
// final.
func (client *evaluationClient) request(ctx context.Context, agentID, runID string) (*Evaluation, error) {
	body, err := json.Marshal(map[string]string{"run_id": runID})
	if err != nil {
		return nil, err
	}
	deadline := client.clock.Now().Add(sessionLookupWindow)
	for {
		evaluation, err := client.do(ctx, http.MethodPost, client.endpoint(agentID), body)
		var refusal *statusError
		if err == nil || !errors.As(err, &refusal) || refusal.status != http.StatusNotFound {
			if err == nil && evaluation.ID == "" {
				return nil, errors.New("project evaluate: backend accepted the request but returned no evaluation id")
			}
			return evaluation, err
		}
		if !client.clock.Now().Add(sessionLookupInterval).Before(deadline) {
			return nil, client.scrub(fmt.Errorf(
				"project evaluate: the backend has no session for run %s after %s; Core may not have stored it yet, so request the evaluation again from the dashboard",
				runID, sessionLookupWindow))
		}
		if err := client.clock.Sleep(ctx, sessionLookupInterval); err != nil {
			return nil, &classifiedError{class: contextClassification(err), err: errors.New("project evaluate: interrupted while waiting for the run's session")}
		}
	}
}

func (client *evaluationClient) get(ctx context.Context, agentID, evaluationID string) (*Evaluation, error) {
	return client.do(ctx, http.MethodGet, client.endpoint(agentID, evaluationID), nil)
}

// wait polls until the evaluation is complete or failed. A failed evaluation is
// returned, not an error: reporting it is the caller's decision.
func (client *evaluationClient) wait(ctx context.Context, agentID, evaluationID string) (*Evaluation, error) {
	deadline := client.clock.Now().Add(waitWindow)
	transient := 0
	last := ""
	for {
		evaluation, err := client.get(ctx, agentID, evaluationID)
		switch {
		case err == nil:
			transient = 0
			last = evaluation.Status
			if evaluation.Status == StatusComplete || evaluation.Status == StatusFailed {
				return evaluation, nil
			}
		case ctx.Err() != nil:
			return nil, &classifiedError{class: contextClassification(ctx.Err()), err: errors.New("project evaluate: interrupted while waiting for the security evaluation")}
		default:
			var refusal *statusError
			if errors.As(err, &refusal) && refusal.status < 500 {
				return nil, err
			}
			transient++
			if transient > pollTolerance {
				return nil, err
			}
		}
		if !client.clock.Now().Add(pollInterval).Before(deadline) {
			status := last
			if status == "" {
				status = "unknown"
			}
			return nil, fmt.Errorf("project evaluate: security evaluation %s was still %s after %s; check the dashboard", evaluationID, status, waitWindow)
		}
		if err := client.clock.Sleep(ctx, pollInterval); err != nil {
			return nil, &classifiedError{class: contextClassification(err), err: errors.New("project evaluate: interrupted while waiting for the security evaluation")}
		}
	}
}

func (client *evaluationClient) do(ctx context.Context, method, endpoint string, body []byte) (*Evaluation, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return nil, client.scrub(fmt.Errorf("project evaluate: construct backend request: %w", err))
	}
	request.Header.Set("accept", "application/json")
	request.Header.Set("x-api-key", client.token)
	if body != nil {
		request.Header.Set("content-type", "application/json")
	}
	response, err := client.http.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return nil, &classifiedError{class: contextClassification(ctx.Err()), err: errors.New("project evaluate: interrupted during the backend request")}
		}
		// The transport error names the URL, never a header. It is scrubbed
		// anyway: a token must not reach output through any path.
		return nil, client.scrub(fmt.Errorf("project evaluate: backend request failed: %w", err))
	}
	defer response.Body.Close()
	content, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return nil, client.scrub(fmt.Errorf("project evaluate: read backend response: %w", err))
	}
	if len(content) > maxResponseBytes {
		return nil, errors.New("project evaluate: backend response exceeds 1 MiB")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, client.refusal(response.StatusCode, content)
	}
	var envelope struct {
		Data *Evaluation `json:"data"`
	}
	if json.Unmarshal(content, &envelope) != nil || envelope.Data == nil {
		return nil, errors.New("project evaluate: backend response is not a security evaluation")
	}
	return envelope.Data, nil
}

// refusal turns a non-2xx answer into the sentence a developer can act on.
func (client *evaluationClient) refusal(status int, content []byte) error {
	var body struct {
		Code    string   `json:"code"`
		Message any      `json:"message"`
		Error   any      `json:"error"`
		Missing []string `json:"missing"`
	}
	_ = json.Unmarshal(content, &body)
	detail := ""
	if text, ok := body.Message.(string); ok {
		detail = display(text)
	}
	// A validation failure carries its reasons in `error`, a list, and only a
	// generic title in `message`; the reasons are what the developer can act on.
	if reasons, ok := body.Error.([]any); ok && len(reasons) > 0 {
		listed := make([]string, 0, len(reasons))
		for _, reason := range reasons {
			if text, ok := reason.(string); ok {
				listed = append(listed, display(text))
			}
		}
		if len(listed) > 0 {
			detail = strings.Join(listed, "; ")
		}
	}
	var message string
	switch {
	case status == http.StatusForbidden:
		message = "project evaluate: the backend refused the control token: it needs permission evaluate:agent_security"
	case status == http.StatusUnauthorized:
		message = "project evaluate: the backend did not accept the control token; check OPENBOX_CONTROL_TOKEN"
	case status == http.StatusUnprocessableEntity && body.Code == "connector_required":
		message = "project evaluate: the organization must configure " + missingModels(body.Missing) + " before a security evaluation can run"
	case status == http.StatusNotFound:
		message = "project evaluate: the backend found no matching agent, session or evaluation (404)"
	case status == http.StatusConflict:
		message = "project evaluate: the run id matches more than one session, so the backend will not choose between them (409)"
	default:
		message = fmt.Sprintf("project evaluate: the backend rejected the request (%d)", status)
		if detail != "" {
			message += ": " + detail
		}
	}
	return &statusError{status: status, message: client.scrubText(message)}
}

func (client *evaluationClient) scrub(err error) error {
	return errors.New(client.scrubText(err.Error()))
}

func (client *evaluationClient) scrubText(text string) string {
	return strings.ReplaceAll(text, client.token, "[redacted]")
}

// requestAndReport is the last step of a successful run: ask, say so, and with
// Wait read the result to its end.
func requestAndReport(ctx context.Context, input Input, dependencies Dependencies, prepared *prepared, result Result, stdout io.Writer) (Result, error) {
	client, err := newEvaluationClient(prepared.connector.backendURL, input.ControlToken, dependencies.BackendHTTP, dependencies.Clock)
	if err != nil {
		return result, &classifiedError{class: "evaluation_request_failure", err: err}
	}
	evaluation, err := client.request(ctx, input.OpenBoxAgent, prepared.evaluationID)
	if err != nil {
		return result, withClass("evaluation_request_failure", err)
	}
	result.Evaluation = evaluation
	fmt.Fprintf(stdout, "security evaluation requested: %s (agent %s)\n", display(evaluation.ID), input.OpenBoxAgent)
	if !input.Wait {
		result.Succeeded = true
		return result, nil
	}
	final, err := client.wait(ctx, input.OpenBoxAgent, evaluation.ID)
	if err != nil {
		return result, withClass("evaluation_wait_failure", err)
	}
	result.Evaluation = final
	if final.Status == StatusFailed {
		reason := display(final.FailureReason)
		if reason == "" {
			reason = "no reason given"
		}
		return result, failf("evaluation_failed", "project evaluate: security evaluation %s failed: %s", display(final.ID), reason)
	}
	writeSummary(stdout, final)
	result.Succeeded = true
	return result, nil
}

// withClass gives an unclassified error a class and leaves a classified one
// (interruption, for example) alone.
func withClass(class string, err error) error {
	var classified *classifiedError
	if errors.As(err, &classified) {
		return err
	}
	return &classifiedError{class: class, err: err}
}

func writeSummary(out io.Writer, evaluation *Evaluation) {
	fmt.Fprintf(out, "security evaluation %s: %s\n", display(evaluation.ID), evaluation.Status)
	report := evaluation.Report
	if report == nil {
		fmt.Fprintln(out, "result: unavailable (the backend returned no report)")
		return
	}
	fmt.Fprintf(out, "result: %s\n", display(report.Result))
	fmt.Fprintf(out, "issues: %d\n", len(report.Issues))
	for _, issue := range report.Issues {
		fmt.Fprintf(out, "  - %s\n", display(issue.Title))
	}
	withRule := 0
	for _, recommendation := range report.Recommendations {
		if recommendation.HasRule() {
			withRule++
		}
	}
	fmt.Fprintf(out, "recommendations with a suggested rule: %d\n", withRule)
	for _, recommendation := range report.Recommendations {
		if recommendation.HasRule() {
			fmt.Fprintf(out, "  suggested rule: %s on %s\n", display(recommendation.CatalogEntryID), display(recommendation.actionName()))
		}
	}
	fmt.Fprintf(out, "rejected candidates: %d\n", report.rejectedCount())
}

// display makes backend-supplied text safe for a terminal: control characters
// (including escape sequences) become spaces and the length is bounded.
func display(text string) string {
	cleaned := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == utf8.RuneError {
			return ' '
		}
		return r
	}, text)
	cleaned = strings.Join(strings.Fields(cleaned), " ")
	if len(cleaned) > maxDisplayBytes {
		cut := maxDisplayBytes
		for cut > 0 && !utf8.RuneStart(cleaned[cut]) {
			cut--
		}
		cleaned = cleaned[:cut] + "..."
	}
	return cleaned
}

func newBackendHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DisableCompression = true
	transport.MaxResponseHeaderBytes = 32 << 10
	transport.IdleConnTimeout = 5 * time.Second
	return &http.Client{
		Transport: transport,
		Timeout:   15 * time.Second,
		// A redirect would carry x-api-key to wherever it points.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// missingModels names what the organization has not configured. A backend that
// does not say gets both named, which is never wrong: an evaluation needs both.
func missingModels(missing []string) string {
	var names []string
	for _, kind := range missing {
		switch kind {
		case "decision":
			names = append(names, "its decision model")
		case "llm":
			names = append(names, "its language model")
		}
	}
	if len(names) == 0 {
		return "its decision model and its language model"
	}
	return strings.Join(names, " and ")
}
