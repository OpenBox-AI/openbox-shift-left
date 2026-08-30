package client

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/openbox-ai/openbox-shift-left/internal/client/memhttptest"
	"testing"
	"time"
)

func approvalServer(t *testing.T, pub ed25519.PublicKey, respond func() (int, string)) (*memhttptest.Server, *ApprovalKey) {
	t.Helper()
	var got ApprovalKey
	srv := memhttptest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != approvalPath {
			t.Errorf("path = %q, want %q", r.URL.Path, approvalPath)
		}
		if h := r.Header.Get(headerAuthorization); h != "Bearer "+testAPIKey {
			t.Errorf("Authorization = %q — the poll must be agent-authenticated like /evaluate", h)
		}
		body, _ := io.ReadAll(r.Body)
		if err := verifyLikeCore(pub, r.Method, r.URL.Path, body, r.Header); err != nil {
			t.Errorf("core-mirror rejected the poll signature: %v", err)
			w.WriteHeader(401)
			return
		}
		if err := json.Unmarshal(body, &got); err != nil {
			t.Errorf("poll body unmarshal: %v", err)
		}
		status, resp := respond()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(resp))
	}))
	t.Cleanup(srv.Close)
	return srv, &got
}

// TestApprovalKeyFor_MatchesTheWirePayload is the load-bearing property of the
// whole hold: a poll must address the row the escalation created.
func TestApprovalKeyFor_MatchesTheWirePayload(t *testing.T) {
	for _, ev := range []DevEvent{sampleEvent(), func() DevEvent {
		e := sampleEvent()
		e.WorkspaceID = "ws-7" // the workspace identity wins over the DID fallback
		return e
	}()} {
		body, err := buildPayload(ev)
		if err != nil {
			t.Fatalf("buildPayload: %v", err)
		}
		var wire struct {
			WorkflowID string `json:"workflow_id"`
			RunID      string `json:"run_id"`
			ActivityID string `json:"activity_id"`
		}
		if err := json.Unmarshal(body, &wire); err != nil {
			t.Fatalf("payload unmarshal: %v", err)
		}
		k := ApprovalKeyFor(ev)
		if k.WorkflowID != wire.WorkflowID || k.RunID != wire.RunID || k.ActivityID != wire.ActivityID {
			t.Errorf("ApprovalKeyFor = %+v, want the wire ids %+v", k, wire)
		}
		if !k.Valid() {
			t.Errorf("key derived from a real event is not valid: %+v", k)
		}
	}
}

func TestPollApproval_PendingAndDecided(t *testing.T) {
	expiry := time.Date(2026, 8, 1, 12, 30, 0, 0, time.UTC)
	for _, tc := range []struct {
		name        string
		resp        string
		wantVerdict Verdict
		wantPending bool
	}{
		{"pending", `{"id":"ge-1","action":"require_approval","reason":"needs review",` +
			`"approval_expiration_time":"2026-08-01T12:30:00Z"}`, VerdictRequireApproval, true},
		{"approved", `{"id":"ge-1","action":"allow","approval_expiration_time":"2026-08-01T12:30:00Z"}`,
			VerdictAllow, false},
		{"rejected", `{"id":"ge-1","action":"halt","approval_expiration_time":"2026-08-01T12:30:00Z"}`,
			VerdictHalt, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, gotKey := approvalServer(t, pub(t), func() (int, string) { return 200, tc.resp })
			c, _ := newTestClient(t, srv.URL, false)

			want := ApprovalKeyFor(sampleEvent())
			st, err := c.PollApproval(context.Background(), want)
			if err != nil {
				t.Fatalf("PollApproval: %v", err)
			}
			if *gotKey != want {
				t.Errorf("core received key %+v, want %+v", *gotKey, want)
			}
			if st.Verdict != tc.wantVerdict {
				t.Errorf("verdict = %q, want %q", st.Verdict, tc.wantVerdict)
			}
			if st.Pending() != tc.wantPending {
				t.Errorf("Pending() = %t, want %t", st.Pending(), tc.wantPending)
			}
			if st.EventID != "ge-1" {
				t.Errorf("EventID = %q, want the governance event id", st.EventID)
			}
			if !st.ExpiresAt.Equal(expiry) {
				t.Errorf("ExpiresAt = %v, want %v", st.ExpiresAt, expiry)
			}
		})
	}
}

// TestApprovalStatus_DecidedRequiresTheWindow a 404 means the request was
// never filed (or has not landed yet); an answer worth retrying at the poll
// cadence, not an outage for the failure policy. Decided must require the
// approval window.
func TestApprovalStatus_DecidedRequiresTheWindow(t *testing.T) {
	window := time.Now().Add(30 * time.Minute)
	for _, tc := range []struct {
		name string
		st   ApprovalStatus
		want bool
	}{
		{"approved inside a window", ApprovalStatus{Verdict: VerdictAllow, ExpiresAt: window}, true},
		{"rejected inside a window", ApprovalStatus{Verdict: VerdictHalt, ExpiresAt: window}, true},
		{"still pending", ApprovalStatus{Verdict: VerdictRequireApproval, ExpiresAt: window}, false},
		{"allow with no window — never governed, not approved", ApprovalStatus{Verdict: VerdictAllow}, false},
		{"unrecognized verdict", ApprovalStatus{Verdict: VerdictUnknown, ExpiresAt: window}, false},
	} {
		if got := tc.st.Decided(); got != tc.want {
			t.Errorf("%s: Decided() = %t, want %t", tc.name, got, tc.want)
		}
	}
}

func TestPollApproval_NotFoundIsDistinctFromAnOutage(t *testing.T) {
	srv, _ := approvalServer(t, pub(t), func() (int, string) { return 404, `{"error":"not found"}` })
	c, _ := newTestClient(t, srv.URL, false)

	_, err := c.PollApproval(context.Background(), ApprovalKeyFor(sampleEvent()))
	if !errors.Is(err, ErrApprovalNotFound) {
		t.Errorf("err = %v, want ErrApprovalNotFound", err)
	}
	if errors.Is(err, ErrDelivery) {
		t.Error("not-found must not read as a delivery failure — they mean opposite things to a hold")
	}
}

func TestPollApproval_ServerErrorIsADeliveryFailure(t *testing.T) {
	srv, _ := approvalServer(t, pub(t), func() (int, string) { return 500, `{"error":"boom"}` })
	c, _ := newTestClient(t, srv.URL, false)

	if _, err := c.PollApproval(context.Background(), ApprovalKeyFor(sampleEvent())); !errors.Is(err, ErrDelivery) {
		t.Errorf("err = %v, want ErrDelivery", err)
	}
}

// TestPollApproval_MakesOneAttempt pollApproval makes exactly one attempt: the
// caller's poll interval is the retry.
func TestPollApproval_MakesOneAttempt(t *testing.T) {
	calls := 0
	srv, _ := approvalServer(t, pub(t), func() (int, string) {
		calls++
		return 500, `{"error":"boom"}`
	})
	c, _ := newTestClient(t, srv.URL, false)

	_, _ = c.PollApproval(context.Background(), ApprovalKeyFor(sampleEvent()))
	if calls != 1 {
		t.Errorf("server saw %d calls, want exactly 1", calls)
	}
}

func TestPollApproval_RejectsAPartialKey(t *testing.T) {
	c, _ := newTestClient(t, "https://core.invalid", false)
	if _, err := c.PollApproval(context.Background(), ApprovalKey{RunID: "s"}); err == nil {
		t.Error("a key core would 400 on must be refused before the round-trip")
	}
}

// TestPollApproval_UnparseableStatusErrors an unparseable status is "unknown",
// never "allow": a hold that guessed would either block a granted call or
// release a pending one.
func TestPollApproval_UnparseableStatusErrors(t *testing.T) {
	srv, _ := approvalServer(t, pub(t), func() (int, string) { return 200, `not json` })
	c, _ := newTestClient(t, srv.URL, false)

	st, err := c.PollApproval(context.Background(), ApprovalKeyFor(sampleEvent()))
	if err == nil {
		t.Fatal("unparseable status must error")
	}
	if st.Verdict != VerdictUnknown {
		t.Errorf("verdict = %q, want unknown", st.Verdict)
	}
}
