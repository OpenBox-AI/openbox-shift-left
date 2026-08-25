package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/client"
)

// recordingEvaluator records whether it was called, which is what lets the
// ordering invariant be asserted rather than reviewed.
type recordingEvaluator struct {
	calls   int
	verdict client.Verdict
	reason  string
	ref     string
	err     error
}

func (r *recordingEvaluator) Evaluate(context.Context, Captured) (client.Evaluation, error) {
	r.calls++
	if r.err != nil {
		return client.Evaluation{}, r.err
	}
	return client.Evaluation{Verdict: r.verdict, Reason: r.reason, ApprovalID: r.ref}, nil
}

// TestNoRefusalWithoutAnEvaluationAttempt is the MERGE BLOCKER.
//
// The rule: no refusal may be synthesized before an evaluation has been
// attempted. Pre-ADR-0017 the hook path got this wrong and a fail-closed org
// denied every gated call without ever asking; here refusal is UNCONDITIONAL on a
// missing verdict, so the same mistake would turn every control-plane blip into a
// total model-call outage reported as a policy decision no policy made.
//
// It is asserted across every branch rather than on one happy path, because the
// bug is precisely a branch that returns early.
func TestNoRefusalWithoutAnEvaluationAttempt(t *testing.T) {
	cases := []struct {
		name string
		ev   *recordingEvaluator
	}{
		{"unreachable", &recordingEvaluator{err: errors.New("dial tcp: connection refused")}},
		{"halt", &recordingEvaluator{verdict: client.VerdictHalt, reason: "policy X"}},
		{"block", &recordingEvaluator{verdict: client.VerdictBlock, reason: "policy Y"}},
		{"require approval", &recordingEvaluator{verdict: client.VerdictRequireApproval, ref: "apr-1"}},
		{"empty verdict", &recordingEvaluator{verdict: ""}},
		// NOTE: CONSTRAIN is deliberately absent here — it FORWARDS. See
		// TestConstrainForwardsLikeEverywhereElse.
		{"unknown verdict", &recordingEvaluator{verdict: client.Verdict("WAT")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := Decide(context.Background(), tc.ev, true, Captured{})
			if d.Forward {
				t.Fatalf("%s forwarded; every one of these must refuse", tc.name)
			}
			// The invariant.
			if !d.Evaluated {
				t.Error("REFUSAL WITHOUT AN EVALUATION ATTEMPT — a synthesized refusal fired before asking")
			}
			if tc.ev.calls != 1 {
				t.Errorf("evaluator called %d times, want exactly 1 before any refusal", tc.ev.calls)
			}
			if d.Reason == "" {
				t.Error("refusal carries no reason; requirement 6 wants the developer to see why")
			}
		})
	}
}

// TestUngatedCallAddsNoRoundTrip is requirement 5. Roughly 52 model calls per turn
// window were measured, so a round-trip on the ungated path is not affordable —
// and "we only evaluate what policy marks" is only true if this holds.
func TestUngatedCallAddsNoRoundTrip(t *testing.T) {
	ev := &recordingEvaluator{verdict: client.VerdictHalt}
	d := Decide(context.Background(), ev, false, Captured{})

	if !d.Forward {
		t.Error("an ungated call was not forwarded")
	}
	if ev.calls != 0 {
		t.Errorf("evaluator called %d times on an ungated call; there must be no round-trip", ev.calls)
	}
	if d.Evaluated {
		t.Error("Evaluated must be false when nothing was asked")
	}
}

// TestAllowForwards keeps the gate from being a blanket denier — the failure mode
// the security note calls out, where a bug that refuses everything is
// indistinguishable from an outage.
func TestAllowForwards(t *testing.T) {
	ev := &recordingEvaluator{verdict: client.VerdictAllow}
	d := Decide(context.Background(), ev, true, Captured{})
	if !d.Forward {
		t.Fatal("an ALLOW verdict did not forward")
	}
	if !d.Evaluated || ev.calls != 1 {
		t.Errorf("ALLOW must still have asked: evaluated=%v calls=%d", d.Evaluated, ev.calls)
	}
}

// TestUnreachableRefusesRegardlessOfPosture is requirement 4 — the owner's
// divergence from the hook path. There is no fail_closed input to this gate at
// all, and that absence IS the decision: a posture key here would be a way to
// switch the gateway's enforcement off.
func TestUnreachableRefusesRegardlessOfPosture(t *testing.T) {
	ev := &recordingEvaluator{err: errors.New("no route to host")}
	d := Decide(context.Background(), ev, true, Captured{})

	if d.Forward {
		t.Fatal("a gated call forwarded when /evaluate was unreachable")
	}
	if !d.Unreachable {
		t.Error("Unreachable not set; a core outage would be indistinguishable from a denial")
	}
	// The reason must name the outage, not imply a policy decided anything.
	if !strings.Contains(strings.ToLower(d.Reason), "unreachable") {
		t.Errorf("reason does not name the outage: %q", d.Reason)
	}
	if strings.Contains(strings.ToLower(d.Reason), "refused by policy") {
		t.Errorf("an outage is being reported as a policy denial: %q", d.Reason)
	}
}

// TestPolicyRefusalNamesPolicy is the other side: a real denial must not read as
// an outage, or a developer chases an infrastructure problem that does not exist.
func TestPolicyRefusalNamesPolicy(t *testing.T) {
	ev := &recordingEvaluator{verdict: client.VerdictBlock, reason: "secrets policy: credential in prompt"}
	d := Decide(context.Background(), ev, true, Captured{})

	if d.Unreachable {
		t.Error("a policy denial is flagged as unreachable")
	}
	if !strings.Contains(d.Reason, "refused by policy") {
		t.Errorf("reason does not name policy: %q", d.Reason)
	}
	if !strings.Contains(d.Reason, "secrets policy") {
		t.Errorf("the server's own reason was dropped: %q", d.Reason)
	}
}

// TestRefusalShapeIsProbePending exists so nobody mistakes the two provisional
// constants for a verified answer. What it CAN assert without probe A is the
// requirement: the shape must not look like a transient provider error, because
// Claude Code's retry logic matches on upstream error wording.
func TestRefusalShapeIsProbePending(t *testing.T) {
	// Not a transience status. Each of these is one the client is built to retry.
	for _, transient := range []int{
		http.StatusTooManyRequests, http.StatusInternalServerError,
		http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout,
	} {
		if refusalStatus == transient {
			t.Errorf("refusalStatus %d is a transience signal; a denial would be retried around", refusalStatus)
		}
	}
	// Not one of the provider's own error type literals.
	for _, providerType := range []string{
		"overloaded_error", "rate_limit_error", "api_error",
		"authentication_error", "invalid_request_error", "permission_error",
		"not_found_error", "request_too_large",
	} {
		if refusalErrorType == providerType {
			t.Errorf("refusalErrorType %q is the provider's own literal; a wording-based retry rule could match it", refusalErrorType)
		}
	}
	if !strings.Contains(refusalErrorType, "openbox") {
		t.Errorf("refusalErrorType %q is not identifiably ours", refusalErrorType)
	}
}

// TestWriteRefusalRendersTheReason is requirement 6 on the actual response bytes.
// A bare status is indistinguishable from the gateway being broken.
func TestWriteRefusalRendersTheReason(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteRefusal(rec, Decision{Reason: reasonPolicyRefused("secrets policy triggered")})

	if rec.Code != refusalStatus {
		t.Errorf("status = %d want %d", rec.Code, refusalStatus)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}
	var got refusalBody
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("refusal body is not valid JSON (%v): %s", err, rec.Body.String())
	}
	if got.Error.Type != refusalErrorType {
		t.Errorf("error.type = %q want %q", got.Error.Type, refusalErrorType)
	}
	if !strings.Contains(got.Error.Message, "secrets policy triggered") {
		t.Errorf("the reason did not reach the developer: %q", got.Error.Message)
	}
	if got.Type != "error" {
		t.Errorf("envelope type = %q; a client parsing errors structurally needs the provider's shape", got.Type)
	}
}

// TestRefusalCarriesNoCapturedContent keeps a refusal from becoming a content
// leak. It is rendered to the developer and stored; neither may carry the prompt.
func TestRefusalCarriesNoCapturedContent(t *testing.T) {
	secret := "the-developers-private-prompt-text"
	ev := &recordingEvaluator{verdict: client.VerdictHalt, reason: "policy X"}
	d := Decide(context.Background(), ev, true, Captured{
		RequestBody:  `{"messages":[{"content":"` + secret + `"}]}`,
		ResponseBody: secret,
	})

	rec := httptest.NewRecorder()
	WriteRefusal(rec, d)
	if strings.Contains(rec.Body.String(), secret) {
		t.Errorf("the refusal echoed captured content:\n%s", rec.Body.String())
	}
	if strings.Contains(d.Reason, secret) {
		t.Errorf("the decision reason echoed captured content: %q", d.Reason)
	}
}

// TestConstrainForwardsLikeEverywhereElse pins a verdict that was being refused.
//
// CONSTRAIN is non-blocking in every other consumer in this repo (hookflow's
// cascade groups it with ALLOW and UNKNOWN). It fell to this gate's default branch
// and was refused, so once wired, one policy verdict would have meant "proceed"
// for tool calls and "deny" for model calls — a divergence nobody decided. The
// owner's always-refuse decision is about a MISSING verdict, not about a verdict
// that says go ahead.
func TestConstrainForwardsLikeEverywhereElse(t *testing.T) {
	ev := &recordingEvaluator{verdict: client.VerdictConstrain}
	d := Decide(context.Background(), ev, true, Captured{})

	if !d.Forward {
		t.Fatal("CONSTRAIN was refused; it is non-blocking everywhere else in this repo")
	}
	if !d.Evaluated || ev.calls != 1 {
		t.Errorf("CONSTRAIN must still have asked: evaluated=%v calls=%d", d.Evaluated, ev.calls)
	}
	if d.Unreachable {
		t.Error("CONSTRAIN flagged as unreachable")
	}

	// And the asymmetry holds: a verdict this build does not know still refuses.
	unknown := &recordingEvaluator{verdict: client.Verdict("SOME_FUTURE_VERDICT")}
	if Decide(context.Background(), unknown, true, Captured{}).Forward {
		t.Error("an uninterpretable verdict forwarded; a future blocking verdict would be waved through")
	}
}

// TestRefuseEverythingIsProbeOnlyAndSaysSo covers the probe affordance. It must be
// unmistakable in what it returns: a refusal that read like a real policy denial
// would be indistinguishable from one in whatever the probe records, and knowing
// which behaviour came from where is the entire point of running the probe.
func TestRefuseEverythingIsProbeOnlyAndSaysSo(t *testing.T) {
	shape := RefusalShape{Status: 418, ErrorType: "openbox_probe_candidate"}
	srv := httptest.NewServer(RefuseEverything(shape))
	defer srv.Close()

	resp, err := srv.Client().Post(srv.URL+"/v1/messages", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != shape.Status {
		t.Errorf("status = %d want the injected %d", resp.StatusCode, shape.Status)
	}
	var got refusalBody
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("body is not valid JSON: %v", err)
	}
	if got.Error.Type != shape.ErrorType {
		t.Errorf("error.type = %q want the injected %q", got.Error.Type, shape.ErrorType)
	}
	if !strings.Contains(got.Error.Message, "PROBE MODE") {
		t.Errorf("a probe refusal is not distinguishable from a real one: %q", got.Error.Message)
	}
	if !strings.Contains(got.Error.Message, "No policy was consulted") {
		t.Errorf("the message does not say no policy ran: %q", got.Error.Message)
	}
}

// TestRefusalShapeValidateRejectsHopelessCandidates keeps a probe run from burning
// a real session on a shape the requirement already rules out.
func TestRefusalShapeValidateRejectsHopelessCandidates(t *testing.T) {
	bad := []RefusalShape{
		{Status: 200, ErrorType: "x"},                    // not a refusal at all
		{Status: 503, ErrorType: "x"},                    // 5xx reads as the provider failing
		{Status: 429, ErrorType: "x"},                    // the canonical retry signal
		{Status: 408, ErrorType: "x"},                    // likewise
		{Status: 403, ErrorType: ""},                     // unnamed error
		{Status: 403, ErrorType: "overloaded_error"},     // the provider's own literal
		{Status: 403, ErrorType: "authentication_error"}, // ditto
	}
	for _, s := range bad {
		if err := s.Validate(); err == nil {
			t.Errorf("Validate accepted a hopeless candidate: %+v", s)
		}
	}
	// And the provisional default must itself be valid, or the shipped path is
	// asserting a shape its own validator rejects.
	if err := DefaultRefusalShape().Validate(); err != nil {
		t.Errorf("the provisional default fails its own validator: %v", err)
	}
	if err := (RefusalShape{Status: 403, ErrorType: "openbox_probe_candidate"}).Validate(); err != nil {
		t.Errorf("a legitimate candidate was rejected: %v", err)
	}
}
