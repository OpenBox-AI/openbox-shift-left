package decision

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/openbox-ai/openbox-shift-left/client"
)

const testSecretBody = "AWS_SECRET_ACCESS_KEY=abcd1234EXAMPLEabcd1234EXAMPLEabcd1234EX"

// Tier-1 redaction is decoupled from the verdict: it does not consult the
// policy, the bundle, or the session, so a request that cannot be evaluated
// still gets scanned. It used to run at the end of the evaluation body, which
// the two degraded branches returned before ever reaching — so a hook payload
// with a missing session_id proceeded fail-open with the secret unscanned.
//
// These were untested, which is how it went unnoticed.
func TestDecide_RedactsOnDegradedPaths(t *testing.T) {
	s := newTestServerWithBundle(t)

	for _, tc := range []struct {
		name string
		req  DecisionRequest
	}{
		{
			name: "missing session id",
			req: DecisionRequest{
				EventType: client.EventToolCall,
				Tool:      client.Tool{Name: "Write", Kind: client.ToolFile},
				Content:   &client.Content{FileText: testSecretBody},
			},
		},
		{
			name: "unsupported protocol",
			req: DecisionRequest{
				Protocol:  ProtocolVersion + 99,
				SessionID: "sess-1",
				EventType: client.EventToolCall,
				Tool:      client.Tool{Name: "Write", Kind: client.ToolFile},
				Content:   &client.Content{FileText: testSecretBody},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := s.decide(tc.req)

			// The degraded verdict itself must not change: no real evaluation
			// happened, so it stays fail-open-with-no-bundle.
			if resp.Evaluation.Verdict != client.VerdictUnknown {
				t.Errorf("verdict = %v, want unknown on a degraded path", resp.Evaluation.Verdict)
			}
			if resp.Source != sourceFailOpenNoBundle {
				t.Errorf("source = %q, want %q", resp.Source, sourceFailOpenNoBundle)
			}
			if resp.Error == "" {
				t.Error("a degraded path must still report why it could not evaluate")
			}

			if resp.RedactedContent == nil {
				t.Fatal("the secret was not scanned on a path that returns no verdict")
			}
			if strings.Contains(resp.RedactedContent.FileText, "abcd1234EXAMPLE") {
				t.Error("the secret survived redaction")
			}
			if len(resp.RedactionCategories) == 0 {
				t.Error("expected at least one redaction category")
			}
		})
	}
}

// The ordinary path must keep both halves: a real verdict and the redaction.
func TestDecide_RedactsAlongsideRealVerdict(t *testing.T) {
	s := newTestServerWithBundle(t)
	req := toolCall("Write", client.ToolFile, map[string]any{"file_path": "config.env"})
	req.Content = &client.Content{FileText: testSecretBody}

	resp := s.decide(req)
	if resp.Source != sourceLocalBundle {
		t.Errorf("source = %q, want %q", resp.Source, sourceLocalBundle)
	}
	if resp.RedactedContent == nil {
		t.Fatal("expected redaction on the evaluated path")
	}
}

// Content with no secret must be left alone on every path — redaction attaches
// only when something actually changed.
func TestDecide_CleanContentIsNotRedacted(t *testing.T) {
	s := newTestServerWithBundle(t)
	req := DecisionRequest{
		Tool:    client.Tool{Name: "Write", Kind: client.ToolFile},
		Content: &client.Content{FileText: "package main\n\nfunc main() {}\n"},
	} // no session id: the degraded path
	if resp := s.decide(req); resp.RedactedContent != nil {
		t.Errorf("clean content must not be rewritten: %q", resp.RedactedContent.FileText)
	}
}

func newTestServerWithBundle(t *testing.T) *engine {
	t.Helper()
	path := writeBundle(t, &Bundle{Version: "v1", DefaultDecision: "allow"})
	return NewInProcessDecider(InProcessConfig{BundlePath: path}).srv
}

// OD-RF-4. Freshness is measured from the bundle file's mtime, not from when
// this process loaded it. Load time made the flag permanently false: the
// decider is built per tool call in a short-lived hook process, so the window
// could never elapse and Stale reported "fresh" for a policy of any age.
func TestDecide_StaleReflectsBundleAge(t *testing.T) {
	path := writeBundle(t, &Bundle{Version: "v1", DefaultDecision: "allow"})

	// Age the bundle on disk past the freshness window.
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}

	d := NewInProcessDecider(InProcessConfig{BundlePath: path, Freshness: time.Hour})
	got := d.Decide(context.Background(), toolCall("Write", client.ToolFile, nil))
	if !got.Stale {
		t.Error("a bundle written two hours ago must read as stale under a one-hour window")
	}
	// Staleness is a signal, not a verdict: the decision still comes from the
	// bundle rather than degrading.
	if got.Source != sourceLocalBundle {
		t.Errorf("source = %q, want %q — staleness must not stop the bundle being used", got.Source, sourceLocalBundle)
	}
}

// A freshly written bundle is not stale.
func TestDecide_FreshBundleIsNotStale(t *testing.T) {
	path := writeBundle(t, &Bundle{Version: "v1", DefaultDecision: "allow"})
	d := NewInProcessDecider(InProcessConfig{BundlePath: path, Freshness: time.Hour})
	if got := d.Decide(context.Background(), toolCall("Write", client.ToolFile, nil)); got.Stale {
		t.Error("a bundle just written must not read as stale")
	}
}
