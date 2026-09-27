package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/trace"
)

// defaultTraceBackendURL is used when neither --backend nor devconfig's own
// resolver names one.
const defaultTraceBackendURL = "https://openbox-api.node.lat"

// corePerPage is the page size `openbox trace --against-core` requests.
const corePerPage = 100

// coreLogRow is one row of GET /agent/{id}/logs's data.data[]. SessionID is a
// pointer because core may record it null, and "session_id is null" is
// itself one of the findings this command flags -- collapsing it to "" would
// make that row indistinguishable from one that legitimately has none to
// report on an unrelated code path.
type coreLogRow struct {
	ID           string         `json:"id"`
	EventType    string         `json:"event_type"`
	ActivityType string         `json:"activity_type"`
	ActivityID   string         `json:"activity_id"`
	SessionID    *string        `json:"session_id"`
	RunID        string         `json:"run_id"`
	CreatedAt    time.Time      `json:"created_at"`
	Metadata     map[string]any `json:"metadata"`
}

// coreLogsPage is the whole response envelope for one page of
// /agent/{id}/logs.
type coreLogsPage struct {
	Status int `json:"status"`
	Data   struct {
		Data  []coreLogRow `json:"data"`
		Start int          `json:"start"`
		Limit int          `json:"limit"`
		Total int          `json:"total"`
	} `json:"data"`
}

// traceAgainstCore fetches every /agent/{id}/logs row for agentID, keeps the
// ones belonging to target (by run_id or session_id), reconciles them
// against the local trace under dir, and prints one line per finding.
func (a *app) traceAgainstCore(dir, target, agentID, backendFlag, apiKeyFile string) int {
	apiKey, err := resolveTraceAPIKey(a.getenv, apiKeyFile)
	if err != nil {
		return a.traceCoreErrorf("%v", err)
	}
	backendURL := resolveTraceBackendURL(backendFlag)

	client := &http.Client{Timeout: 30 * time.Second}
	rows, err := fetchCoreLogs(client, backendURL, agentID, apiKey)
	if err != nil {
		// The error itself must never carry apiKey -- fetchCoreLogs' own errors
		// only ever quote the URL (which has no key in it, X-API-Key is a header)
		// and the response body core sent back.
		return a.traceCoreErrorf("fetching core logs: %v", err)
	}

	var matched []coreLogRow
	for _, r := range rows {
		sid := ""
		if r.SessionID != nil {
			sid = *r.SessionID
		}
		if r.RunID == target || sid == target {
			matched = append(matched, r)
		}
	}

	localRecs, _, err := trace.Read(dir, func(r trace.Record) bool {
		return r.SessionID == target || r.RunID == target
	})
	if err != nil && !os.IsNotExist(err) {
		return a.traceCoreErrorf("reading local trace: %v", err)
	}

	findings := reconcileAgainstCore(matched, localRecs)
	for _, f := range findings {
		fmt.Fprintln(a.stdout, f)
	}
	if len(findings) > 0 {
		return traceExitFindings
	}
	fmt.Fprintf(a.stdout, "clean: %d core row(s) for %q, no findings\n", len(matched), target)
	return traceExitClean
}

// traceCoreErrorf reports a reconciliation-command failure (bad args,
// network, unreadable trace) and always returns traceExitError (2), never
// exitError's 1 -- which this command reserves for "findings were flagged".
func (a *app) traceCoreErrorf(format string, args ...any) int {
	fmt.Fprintf(a.stderr, "error: "+format+"\n", args...)
	return traceExitError
}

// resolveTraceAPIKey resolves the core API key from --api-key-file, else
// $OPENBOX_API_KEY. It is the only place this command reads the key, and the
// returned value is never logged, printed, or included in an error -- every
// caller downstream only ever puts it in the X-API-Key header.
func resolveTraceAPIKey(getenv func(string) string, apiKeyFile string) (string, error) {
	if apiKeyFile != "" {
		b, err := os.ReadFile(apiKeyFile)
		if err != nil {
			return "", fmt.Errorf("reading --api-key-file: %w", err)
		}
		key := strings.TrimSpace(string(b))
		if key == "" {
			return "", fmt.Errorf("--api-key-file %s is empty", apiKeyFile)
		}
		return key, nil
	}
	if v := strings.TrimSpace(getenv("OPENBOX_API_KEY")); v != "" {
		return v, nil
	}
	return "", fmt.Errorf("no API key: set OPENBOX_API_KEY or pass --api-key-file")
}

// resolveTraceBackendURL: --backend wins when set; else devconfig's own
// resolver (env OPENBOX_BACKEND_URL, then the org dev config); else
// defaultTraceBackendURL.
func resolveTraceBackendURL(flagURL string) string {
	if flagURL != "" {
		return flagURL
	}
	if u := devconfig.ResolveBackendURL(); u != "" {
		return u
	}
	return defaultTraceBackendURL
}

// fetchCoreLogs pages GET /agent/{id}/logs?page=N&perPage=100 from page 0
// until every row total reports has been fetched.
func fetchCoreLogs(client *http.Client, backendURL, agentID, apiKey string) ([]coreLogRow, error) {
	var all []coreLogRow
	base := strings.TrimRight(backendURL, "/")
	seenRow := map[string]bool{}
	fetched := 0
	for page := 0; ; page++ {
		u := fmt.Sprintf("%s/agent/%s/logs?page=%d&perPage=%d", base, url.PathEscape(agentID), page, corePerPage)
		req, err := http.NewRequest(http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("X-API-Key", apiKey)
		req.Header.Set("Accept", "application/json")

		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("GET %s: %w", u, err)
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
		resp.Body.Close()
		if readErr != nil {
			return nil, fmt.Errorf("GET %s: reading response: %w", u, readErr)
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("GET %s: status %d: %s", u, resp.StatusCode, strings.TrimSpace(string(body)))
		}

		var page0 coreLogsPage
		if err := json.Unmarshal(body, &page0); err != nil {
			return nil, fmt.Errorf("decoding page %d: %w", page, err)
		}
		// Newest-first and live: a row inserted while this pages shifts the
		// next page by one, so the same row can arrive twice. Keep the first.
		fetched += len(page0.Data.Data)
		for _, r := range page0.Data.Data {
			if !seenRow[r.ID] {
				seenRow[r.ID] = true
				all = append(all, r)
			}
		}
		if len(page0.Data.Data) == 0 || fetched >= page0.Data.Total {
			break
		}
	}
	return all, nil
}

// reconcileAgainstCore is a pure comparison flagging: NULL
// session_id, an activity_id core recorded only one half of, a row created
// before this run's own WorkflowStarted, and a local deliver.result the local
// trace marked accepted but whose event_id core never recorded. Findings are
// sorted for a deterministic, testable order; core rows and local records
// are otherwise read-only inputs.
func reconcileAgainstCore(core []coreLogRow, local []trace.Record) []string {
	var findings []string

	for _, r := range core {
		if r.SessionID == nil {
			findings = append(findings, fmt.Sprintf(
				"core row %s (%s/%s, activity=%s): session_id is null", r.ID, r.EventType, r.ActivityType, r.ActivityID))
		}
	}

	started := map[string]bool{}
	completed := map[string]bool{}
	var activityOrder []string
	seenActivity := map[string]bool{}
	for _, r := range core {
		if r.ActivityID == "" {
			continue
		}
		if !seenActivity[r.ActivityID] {
			seenActivity[r.ActivityID] = true
			activityOrder = append(activityOrder, r.ActivityID)
		}
		switch r.EventType {
		case "ActivityStarted":
			started[r.ActivityID] = true
		case "ActivityCompleted":
			completed[r.ActivityID] = true
		}
	}
	sort.Strings(activityOrder)
	for _, id := range activityOrder {
		if started[id] != completed[id] {
			findings = append(findings, fmt.Sprintf(
				"activity %s: unpaired on core (ActivityStarted=%v ActivityCompleted=%v)", id, started[id], completed[id]))
		}
	}

	var workflowStart time.Time
	haveStart := false
	for _, r := range core {
		if r.EventType != "WorkflowStarted" {
			continue
		}
		if !haveStart || r.CreatedAt.Before(workflowStart) {
			workflowStart = r.CreatedAt
			haveStart = true
		}
	}
	if haveStart {
		for _, r := range core {
			if r.EventType == "WorkflowStarted" {
				continue
			}
			if r.CreatedAt.Before(workflowStart) {
				findings = append(findings, fmt.Sprintf(
					"core row %s (%s/%s) created %s, before this run's WorkflowStarted at %s",
					r.ID, r.EventType, r.ActivityType, r.CreatedAt.Format(time.RFC3339Nano), workflowStart.Format(time.RFC3339Nano)))
			}
		}
	}

	coreEventIDs := map[string]bool{}
	for _, r := range core {
		if v, ok := r.Metadata["event_id"].(string); ok && v != "" {
			coreEventIDs[v] = true
		}
	}
	for _, r := range local {
		if r.Stage != trace.StageDeliverResult || r.Outcome != "accepted" || r.EventID == "" {
			continue
		}
		if !coreEventIDs[r.EventID] {
			findings = append(findings, fmt.Sprintf(
				"local deliver.result accepted event_id=%s: missing on core", r.EventID))
		}
	}

	sort.Strings(findings)
	return findings
}
