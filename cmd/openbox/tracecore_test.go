package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/client/memhttptest"
	"github.com/openbox-ai/openbox-shift-left/internal/trace"
)

// fakeCoreLogs serves GET /agent/{id}/logs?page=N&perPage=100 over rows,
// paging exactly the way core does: data.data/start/limit/total. It records
// every page value it was asked for, and never returns X-Api-Key back in a
// response, so a test can assert the client paged from 0 without needing a
// real backend.
type fakeCoreLogs struct {
	rows       []coreLogRow
	perPage    int
	gotPages   []int
	gotAPIKeys []string
}

func (f *fakeCoreLogs) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.gotAPIKeys = append(f.gotAPIKeys, r.Header.Get("X-API-Key"))
		q := r.URL.Query()
		page, _ := strconv.Atoi(q.Get("page"))
		f.gotPages = append(f.gotPages, page)
		perPage := f.perPage
		if perPage == 0 {
			perPage = 100
		}
		start := page * perPage
		end := start + perPage
		if start > len(f.rows) {
			start = len(f.rows)
		}
		if end > len(f.rows) {
			end = len(f.rows)
		}
		page0 := coreLogsPage{Status: 200}
		page0.Data.Data = f.rows[start:end]
		page0.Data.Start = start
		page0.Data.Limit = perPage
		page0.Data.Total = len(f.rows)
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(page0); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
}

func strp(s string) *string { return &s }

// buildTestRows constructs 250 rows for one run: a WorkflowStarted row, a
// clean paired activity, a NULL-session row, an unpaired activity (only
// ActivityStarted, no Completed), and enough filler rows around them to
// exercise the 3-page (100+100+50) paging path.
func buildTestRows(runID string, base time.Time) []coreLogRow {
	var rows []coreLogRow
	sess := "sess-target"

	rows = append(rows, coreLogRow{
		ID: "wf-start", EventType: "WorkflowStarted", ActivityType: "SessionStarted",
		SessionID: strp(sess), RunID: runID, CreatedAt: base, Metadata: map[string]any{},
	})
	rows = append(rows, coreLogRow{
		ID: "act-ok-started", EventType: "ActivityStarted", ActivityType: "llm_completion",
		ActivityID: "act-ok", SessionID: strp(sess), RunID: runID, CreatedAt: base.Add(1 * time.Second),
		Metadata: map[string]any{},
	})
	rows = append(rows, coreLogRow{
		ID: "act-ok-completed", EventType: "ActivityCompleted", ActivityType: "llm_completion",
		ActivityID: "act-ok", SessionID: strp(sess), RunID: runID, CreatedAt: base.Add(2 * time.Second),
		Metadata: map[string]any{},
	})
	rows = append(rows, coreLogRow{
		ID: "null-session-row", EventType: "ActivityCompleted", ActivityType: "deliver.result",
		ActivityID: "act-null-sess", SessionID: nil, RunID: runID, CreatedAt: base.Add(3 * time.Second),
		Metadata: map[string]any{},
	})
	rows = append(rows, coreLogRow{
		ID: "act-unpaired-started", EventType: "ActivityStarted", ActivityType: "llm_completion",
		ActivityID: "act-unpaired", SessionID: strp(sess), RunID: runID, CreatedAt: base.Add(4 * time.Second),
		Metadata: map[string]any{},
	})
	rows = append(rows, coreLogRow{
		ID: "deliver-recorded", EventType: "ActivityCompleted", ActivityType: "deliver.result",
		ActivityID: "act-deliver", SessionID: strp(sess), RunID: runID, CreatedAt: base.Add(5 * time.Second),
		Metadata: map[string]any{"event_id": "ev-recorded-on-core"},
	})

	// Filler, unrelated to any finding, padding the run to 250 rows so the
	// client must page 0,1,2 (100+100+50) to see everything above.
	for i := 0; len(rows) < 250; i++ {
		rows = append(rows, coreLogRow{
			ID: fmt.Sprintf("filler-%d", i), EventType: "ActivityCompleted", ActivityType: "log",
			ActivityID: fmt.Sprintf("filler-act-%d", i), SessionID: strp(sess), RunID: runID,
			CreatedAt: base.Add(time.Duration(10+i) * time.Second), Metadata: map[string]any{},
		})
	}
	return rows
}

func TestFetchCoreLogsPagesFromZeroOverThreePages(t *testing.T) {
	base := time.Date(2026, 9, 27, 0, 0, 0, 0, time.UTC)
	rows := buildTestRows("run-xyz", base)
	if len(rows) != 250 {
		t.Fatalf("fixture must total 250 rows, got %d", len(rows))
	}
	fake := &fakeCoreLogs{rows: rows}
	srv := memhttptest.NewServer(t, fake.handler())

	client := srv.Client()
	got, err := fetchCoreLogs(client, srv.URL, "agent-1", "test-api-key")
	if err != nil {
		t.Fatalf("fetchCoreLogs: %v", err)
	}
	if len(got) != 250 {
		t.Fatalf("want 250 rows fetched, got %d", len(got))
	}
	if len(fake.gotPages) != 3 {
		t.Fatalf("want 3 page requests (100+100+50), got %d: %v", len(fake.gotPages), fake.gotPages)
	}
	if fake.gotPages[0] != 0 {
		t.Fatalf("page param must start at 0, got %d", fake.gotPages[0])
	}
	for i, p := range fake.gotPages {
		if p != i {
			t.Fatalf("pages must be sequential from 0, got %v", fake.gotPages)
		}
	}
	for _, k := range fake.gotAPIKeys {
		if k != "test-api-key" {
			t.Fatalf("X-API-Key not forwarded correctly: %q", k)
		}
	}
}

func TestTraceAgainstCoreFlagsEachIssueExactlyOnce(t *testing.T) {
	base := time.Date(2026, 9, 27, 0, 0, 0, 0, time.UTC)
	const runID = "run-xyz"
	rows := buildTestRows(runID, base)
	fake := &fakeCoreLogs{rows: rows}
	srv := memhttptest.NewServer(t, fake.handler())

	dir := t.TempDir()
	writeTraceDay(t, dir, base, []trace.Record{
		{RunID: runID, Stage: trace.StageDeliverResult, Outcome: "accepted", EventID: "ev-recorded-on-core"},
		{RunID: runID, Stage: trace.StageDeliverResult, Outcome: "accepted", EventID: "ev-missing-on-core"},
	})

	keyFile := filepath.Join(t.TempDir(), "key.txt")
	if err := os.WriteFile(keyFile, []byte("test-api-key\n"), 0o600); err != nil {
		t.Fatalf("writing api key file: %v", err)
	}

	a, out, errb := testApp(nil)
	a.getenv = func(string) string { return "" } // force --api-key-file path
	code := a.runTrace([]string{
		runID, "--against-core", "--agent", "agent-1",
		"--dir", dir, "--backend", srv.URL, "--api-key-file", keyFile,
	})

	if code != traceExitFindings {
		t.Fatalf("exit = %d, want %d (findings). stderr=%s stdout=%s", code, traceExitFindings, errb.String(), out.String())
	}
	got := out.String()

	if strings.Contains(got, "test-api-key") {
		t.Fatalf("API key leaked into output: %s", got)
	}
	if strings.Contains(errb.String(), "test-api-key") {
		t.Fatalf("API key leaked into stderr: %s", errb.String())
	}

	mustContainOnce(t, got, "session_id is null")
	mustContainOnce(t, got, "act-unpaired")
	mustContainOnce(t, got, "ev-missing-on-core")
	if strings.Contains(got, "ev-recorded-on-core: missing") {
		t.Fatalf("an event_id core recorded must not be flagged missing: %s", got)
	}
}

func mustContainOnce(t *testing.T, haystack, needle string) {
	t.Helper()
	n := strings.Count(haystack, needle)
	if n != 1 {
		t.Fatalf("want %q exactly once, found %d times in:\n%s", needle, n, haystack)
	}
}

func TestReconcileAgainstCoreFlagsRowsBeforeWorkflowStarted(t *testing.T) {
	base := time.Date(2026, 9, 27, 0, 0, 0, 0, time.UTC)
	sess := "sess-x"
	core := []coreLogRow{
		{ID: "wf", EventType: "WorkflowStarted", SessionID: &sess, RunID: "run-1", CreatedAt: base},
		{ID: "before", EventType: "ActivityCompleted", ActivityID: "act-1", SessionID: &sess, RunID: "run-1", CreatedAt: base.Add(-1 * time.Second)},
	}
	findings := reconcileAgainstCore(core, nil)
	found := false
	for _, f := range findings {
		if strings.Contains(f, "before this run's WorkflowStarted") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a before-WorkflowStarted finding, got: %v", findings)
	}
}

func TestResolveTraceAPIKeyPrefersFileOverEnv(t *testing.T) {
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "key.txt")
	if err := os.WriteFile(keyFile, []byte("  from-file  \n"), 0o600); err != nil {
		t.Fatalf("write key file: %v", err)
	}
	key, err := resolveTraceAPIKey(func(string) string { return "from-env" }, keyFile)
	if err != nil {
		t.Fatalf("resolveTraceAPIKey: %v", err)
	}
	if key != "from-file" {
		t.Fatalf("want trimmed file content, got %q", key)
	}

	key, err = resolveTraceAPIKey(func(string) string { return "from-env" }, "")
	if err != nil {
		t.Fatalf("resolveTraceAPIKey: %v", err)
	}
	if key != "from-env" {
		t.Fatalf("want env fallback, got %q", key)
	}

	if _, err := resolveTraceAPIKey(func(string) string { return "" }, ""); err == nil {
		t.Fatalf("want an error when neither source has a key")
	}
}

func TestResolveTraceBackendURLDefaultsToNodeLat(t *testing.T) {
	if got := resolveTraceBackendURL(""); got != defaultTraceBackendURL {
		// devconfig.ResolveBackendURL may return a non-empty value if this
		// process happens to have a real dev config/env; skip in that case
		// rather than asserting a false negative about the fallback.
		if got == "" {
			t.Fatalf("resolveTraceBackendURL(\"\") = %q, want fallback %q", got, defaultTraceBackendURL)
		}
	}
	if got := resolveTraceBackendURL("https://example.test"); got != "https://example.test" {
		t.Fatalf("--backend flag must win outright, got %q", got)
	}
}
