package observation

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/artifact"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/safety"
)

type Client struct {
	base         *url.URL
	token        string
	agentID      string
	http         *http.Client
	now          func() time.Time
	sleep        func(time.Duration)
	mu           sync.Mutex
	entries      []Entry
	captured     int
	organization string
}

func New(config Config) (*Client, error) {
	if config.ProxyConfigured {
		return nil, errors.New("observation: proxy environment is forbidden for local backend collection")
	}
	if config.BackendURL != ExactBackendURL {
		return nil, errors.New("observation: OPENBOX_BACKEND_URL must be exactly http://127.0.0.1:3000")
	}
	if strings.TrimSpace(config.ControlToken) == "" || strings.ContainsAny(config.ControlToken, "\x00\r\n") {
		return nil, errors.New("observation: OPENBOX_CONTROL_TOKEN is required")
	}
	if config.AgentID == "" || strings.ContainsAny(config.AgentID, "/?&#\x00\r\n") {
		return nil, errors.New("observation: invalid agent identity")
	}
	parsed, err := url.Parse(config.BackendURL)
	if err != nil || parsed.Scheme != "http" || parsed.Host != "127.0.0.1:3000" || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.User != nil {
		return nil, errors.New("observation: invalid local backend coordinate")
	}
	if config.HTTP == nil {
		return nil, errors.New("observation: HTTP client is required")
	}
	now := config.Now
	if now == nil {
		now = time.Now
	}
	sleep := config.Sleep
	if sleep == nil {
		sleep = time.Sleep
	}
	return &Client{base: parsed, token: config.ControlToken, agentID: config.AgentID, http: config.HTTP, now: now, sleep: sleep}, nil
}

func NewHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DisableCompression = true
	transport.MaxIdleConns = 2
	transport.MaxIdleConnsPerHost = 2
	transport.MaxConnsPerHost = 2
	transport.IdleConnTimeout = 5 * time.Second
	transport.MaxResponseHeaderBytes = 32 << 10
	return &http.Client{
		Transport: transport,
		Timeout:   5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func (client *Client) Preflight(ctx context.Context, evaluationID string) (*Snapshot, error) {
	if strings.TrimSpace(evaluationID) == "" {
		return nil, errors.New("observation: evaluation identity is required for dashboard API preflight")
	}
	health, err := client.get(ctx, "/health", false, nil)
	if err != nil {
		return nil, err
	}
	healthData, err := decodeEnvelope(health)
	if err != nil || string(healthData) != `"Success"` {
		return nil, errors.New("observation: local backend health identity is invalid")
	}
	profileBody, err := client.get(ctx, "/auth/profile", true, nil)
	if err != nil {
		return nil, err
	}
	profileData, err := decodeEnvelope(profileBody)
	if err != nil {
		return nil, fmt.Errorf("observation: invalid auth profile: %w", err)
	}
	var profile struct {
		Sub                   string          `json:"sub"`
		OrganizationID        string          `json:"orgId"`
		Picture               json.RawMessage `json:"picture"`
		Permissions           []string        `json:"permissions"`
		APIKey                bool            `json:"isApiKeyAuth"`
		APIKeyName            string          `json:"apiKeyName"`
		RequirePasswordChange bool            `json:"require_password_change"`
		Setup                 struct {
			Pending bool `json:"pending"`
		} `json:"setup"`
	}
	if decodeClosed(profileData, &profile) != nil || profile.OrganizationID == "" || !profile.APIKey || profile.Setup.Pending || !exactStringSet(profile.Permissions, RequiredPermissions) {
		return nil, errors.New("observation: control credential profile does not match the exact observation authority")
	}
	client.organization = profile.OrganizationID
	preexisting, err := client.sessionPages(ctx, evaluationID)
	if err != nil {
		return nil, fmt.Errorf("observation: dashboard session API preflight failed: %w", err)
	}
	if len(preexisting) != 0 {
		return nil, errors.New("observation: evaluation identity already resolves to a backend session before stimulus")
	}
	return &Snapshot{
		OrganizationID: profile.OrganizationID,
		Backend:        BackendIdentity{URL: ExactBackendURL, APIContract: DashboardActivityContract},
		Entries:        client.Entries(),
	}, nil
}

func (client *Client) Collect(ctx context.Context, window Window) (*Result, error) {
	if window.EvaluationID == "" || window.StartedAt.IsZero() || !window.Deadline.After(window.StartedAt) {
		return nil, errors.New("observation: invalid collection window")
	}
	var selected Session
	for {
		sessions, err := client.sessionPages(ctx, window.EvaluationID)
		if err != nil {
			return nil, err
		}
		matches := make([]Session, 0, 1)
		for _, session := range sessions {
			if session.AgentID == client.agentID && session.RunID == window.EvaluationID {
				if session.StartedAt.Before(window.StartedAt.Add(-2*time.Second)) || session.StartedAt.After(window.Deadline) || (!session.CompletedAt.IsZero() && session.CompletedAt.After(window.Deadline)) {
					return nil, errors.New("observation: matching session falls outside the bounded invocation window")
				}
				matches = append(matches, session)
			}
		}
		if len(matches) > 1 {
			return nil, errors.New("observation: multiple exact backend sessions match the evaluation")
		}
		if len(matches) == 1 {
			if terminal(matches[0].Status) {
				selected = matches[0]
				break
			}
			if !pending(matches[0].Status) {
				return nil, errors.New("observation: backend session has an unknown non-terminal status")
			}
		}
		if !client.now().Before(window.Deadline) {
			return nil, errors.New("observation: exact terminal backend session was not available before the deadline")
		}
		client.sleep(time.Second)
	}
	detail, err := client.sessionDetail(ctx, selected.ID)
	if err != nil {
		return nil, err
	}
	if !sameSession(selected, detail) {
		return nil, errors.New("observation: selected session identity drifted before log collection")
	}
	events, err := client.eventPages(ctx, selected.ID)
	if err != nil {
		return nil, err
	}
	for _, event := range events {
		if event.RunID != window.EvaluationID {
			return nil, errors.New("observation: chronological event run identity drifted")
		}
	}
	stableDetail, err := client.sessionDetail(ctx, selected.ID)
	if err != nil || !sameSession(detail, stableDetail) {
		return nil, errors.New("observation: selected session changed during collection")
	}
	stableEvents, err := client.eventPages(ctx, selected.ID)
	if err != nil || !sameEvents(events, stableEvents) {
		return nil, errors.New("observation: chronological event pages changed during collection")
	}
	stableSessions, err := client.sessionPages(ctx, window.EvaluationID)
	if err != nil || len(stableSessions) != 1 || !sameSession(stableDetail, stableSessions[0]) {
		return nil, errors.New("observation: session search pages changed during collection")
	}
	return &Result{OrganizationID: client.organization, Session: stableDetail, Events: stableEvents, Entries: client.Entries()}, nil
}

func (client *Client) Entries() []Entry {
	client.mu.Lock()
	defer client.mu.Unlock()
	return append([]Entry(nil), client.entries...)
}

func (client *Client) get(ctx context.Context, relative string, authenticated bool, validate func([]byte) error) ([]byte, error) {
	client.mu.Lock()
	if len(client.entries) >= MaxRequests {
		client.mu.Unlock()
		return nil, errors.New("observation: backend request limit exceeded")
	}
	client.mu.Unlock()
	requestURL := *client.base
	pathPart, query, _ := strings.Cut(relative, "?")
	requestURL.Path = pathPart
	requestURL.RawQuery = query
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL.String(), nil)
	if err != nil {
		return nil, errors.New("observation: construct bounded backend request")
	}
	request.Header.Set("accept", "application/json")
	request.Header.Set("accept-encoding", "identity")
	if authenticated {
		request.Header.Set("x-api-key", client.token)
	}
	response, err := client.http.Do(request)
	if err != nil {
		return nil, errors.New("observation: bounded backend request failed")
	}
	defer response.Body.Close()
	if response.StatusCode >= 300 && response.StatusCode < 400 {
		return nil, errors.New("observation: backend redirect rejected")
	}
	contentType := response.Header.Get("content-type")
	mediaType, parameters, mediaErr := mime.ParseMediaType(contentType)
	if mediaErr != nil || mediaType != "application/json" || (len(parameters) > 0 && (len(parameters) != 1 || !strings.EqualFold(parameters["charset"], "utf-8"))) {
		return nil, errors.New("observation: backend response content type rejected")
	}
	if response.Header.Get("content-encoding") != "" {
		return nil, errors.New("observation: transformed backend response rejected")
	}
	if response.ContentLength > MaxResponseBytes {
		return nil, errors.New("observation: backend response exceeds the byte limit")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, MaxResponseBytes+1))
	if err != nil || len(body) > MaxResponseBytes || (response.ContentLength >= 0 && int64(len(body)) != response.ContentLength) {
		return nil, errors.New("observation: incomplete or oversized backend response")
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("observation: backend GET %s returned status %d", pathPart, response.StatusCode)
	}
	if !utf8.Valid(body) || !json.Valid(body) {
		return nil, errors.New("observation: backend response is not valid JSON")
	}
	retainedBody := body
	representation := "backend_response"
	if dashboardActivityPath(pathPart, client.agentID) {
		retainedBody, err = projectDashboardActivityResponse(pathPart, body)
		if err != nil {
			return nil, err
		}
		representation = "dashboard_public_projection"
	}
	if err := rejectCredentialMaterial(retainedBody); err != nil {
		return nil, err
	}
	if validate != nil {
		if err := validate(retainedBody); err != nil {
			return nil, errors.New("observation: backend response does not match its closed retention contract")
		}
	}
	digest := sha256.Sum256(retainedBody)
	client.mu.Lock()
	defer client.mu.Unlock()
	client.captured += len(retainedBody)
	if client.captured > MaxCapturedBytes {
		return nil, errors.New("observation: total captured backend bytes exceeded")
	}
	client.entries = append(client.entries, Entry{
		Ordinal: len(client.entries) + 1, Method: http.MethodGet, Path: relative,
		Status: response.StatusCode, ContentType: contentType, BodyBytes: len(retainedBody),
		SHA256: "sha256:" + hex.EncodeToString(digest[:]), BodyBase64: base64.StdEncoding.EncodeToString(retainedBody),
		Representation: representation,
	})
	return retainedBody, nil
}

func (client *Client) sessionPages(ctx context.Context, evaluationID string) ([]Session, error) {
	path := "/agent/" + client.agentID + "/sessions"
	var result []Session
	seen := map[string]bool{}
	for page := 0; page < MaxPages; page++ {
		relative := path + "?page=" + strconv.Itoa(page) + "&perPage=" + strconv.Itoa(PageSize) + "&search=" + url.QueryEscape(evaluationID)
		body, getErr := client.get(ctx, relative, true, nil)
		if getErr != nil {
			return nil, getErr
		}
		data, decodeErr := decodeEnvelope(body)
		if decodeErr != nil {
			return nil, decodeErr
		}
		items, _, limit, total, pageErr := decodePage(data, page, nil)
		if pageErr != nil {
			return nil, pageErr
		}
		for _, item := range items {
			session, sessionErr := decodeSession(item)
			if sessionErr != nil || seen[session.ID] {
				return nil, errors.New("observation: malformed or duplicate session")
			}
			seen[session.ID] = true
			result = append(result, session)
		}
		if len(result) >= total {
			if len(result) != total {
				return nil, errors.New("observation: session count drift")
			}
			return result, nil
		}
		if len(items) != limit {
			return nil, errors.New("observation: premature session page")
		}
	}
	return nil, errors.New("observation: session pages exceeded the limit")
}

func (client *Client) sessionDetail(ctx context.Context, sessionID string) (Session, error) {
	body, err := client.get(ctx, "/agent/"+client.agentID+"/sessions/"+sessionID, true, nil)
	if err != nil {
		return Session{}, err
	}
	data, err := decodeEnvelope(body)
	if err != nil {
		return Session{}, err
	}
	return decodeSession(data)
}

func (client *Client) eventPages(ctx context.Context, sessionID string) ([]Event, error) {
	path := "/agent/" + client.agentID + "/sessions/" + sessionID + "/logs/chronological"
	var result []Event
	seen := map[string]bool{}
	for page := 0; page < MaxPages; page++ {
		body, err := client.get(ctx, path+"?page="+strconv.Itoa(page)+"&perPage="+strconv.Itoa(PageSize), true, nil)
		if err != nil {
			return nil, err
		}
		data, err := decodeEnvelope(body)
		if err != nil {
			return nil, err
		}
		items, _, limit, total, err := decodePage(data, page, []string{"merkle_root", "event_count", "attestation"})
		if err != nil {
			return nil, err
		}
		entryOrdinal := len(client.Entries())
		for recordIndex, item := range items {
			event, eventErr := decodeEvent(item)
			if eventErr != nil || seen[event.ID] || event.AgentID != client.agentID || event.SessionID != sessionID {
				return nil, errors.New("observation: malformed, duplicate, or drifting chronological event")
			}
			event.SourceOrdinal = entryOrdinal
			event.SourceRecord = recordIndex
			seen[event.ID] = true
			result = append(result, event)
		}
		if len(result) >= total {
			if len(result) != total {
				return nil, errors.New("observation: chronological event count drift")
			}
			return result, nil
		}
		if len(items) != limit {
			return nil, errors.New("observation: premature chronological page")
		}
	}
	return nil, errors.New("observation: chronological pages exceeded the limit")
}

func dashboardActivityPath(path, agentID string) bool {
	return path == "/agent/"+agentID+"/sessions" || strings.HasPrefix(path, "/agent/"+agentID+"/sessions/")
}

// projectDashboardActivityResponse retains the public activity fields consumed
// by the dashboard while dropping only ORM relations that are not part of that
// UI contract. The unprojected response is never hashed or retained.
func projectDashboardActivityResponse(path string, body []byte) ([]byte, error) {
	var envelope map[string]any
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if decoder.Decode(&envelope) != nil || decoder.Decode(&struct{}{}) != io.EOF {
		return nil, errors.New("observation: dashboard activity response is malformed")
	}
	data, ok := envelope["data"]
	if !ok {
		return nil, errors.New("observation: dashboard activity response has no data")
	}
	switch {
	case strings.HasSuffix(path, "/logs/chronological"):
		page, ok := data.(map[string]any)
		if !ok {
			return nil, errors.New("observation: dashboard log page is malformed")
		}
		items, ok := page["data"].([]any)
		if !ok {
			return nil, errors.New("observation: dashboard log records are malformed")
		}
		for _, item := range items {
			event, ok := item.(map[string]any)
			if !ok {
				return nil, errors.New("observation: dashboard log record is malformed")
			}
			delete(event, "agent")
		}
	case strings.HasSuffix(path, "/sessions"):
		page, ok := data.(map[string]any)
		if !ok {
			return nil, errors.New("observation: dashboard session page is malformed")
		}
		items, ok := page["data"].([]any)
		if !ok {
			return nil, errors.New("observation: dashboard session records are malformed")
		}
		for _, item := range items {
			session, ok := item.(map[string]any)
			if !ok {
				return nil, errors.New("observation: dashboard session record is malformed")
			}
			projectDashboardSession(session)
		}
	default:
		session, ok := data.(map[string]any)
		if !ok {
			return nil, errors.New("observation: dashboard session detail is malformed")
		}
		projectDashboardSession(session)
	}
	projected, err := artifact.CanonicalJSON(envelope)
	if err != nil {
		return nil, errors.New("observation: canonicalize dashboard activity projection")
	}
	return projected, nil
}

func projectDashboardSession(session map[string]any) {
	delete(session, "agent")
	if current, ok := session["current_step"].(map[string]any); ok {
		delete(current, "agent")
	}
	if events, ok := session["governance_events"].([]any); ok {
		for _, item := range events {
			if event, ok := item.(map[string]any); ok {
				delete(event, "agent")
			}
		}
	}
}

func decodeEnvelope(body []byte) (json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var envelope struct {
		Status int             `json:"status"`
		Data   json.RawMessage `json:"data"`
	}
	if decoder.Decode(&envelope) != nil || decoder.Decode(&struct{}{}) != io.EOF || envelope.Status != 200 || len(envelope.Data) == 0 {
		return nil, errors.New("invalid response envelope")
	}
	return envelope.Data, nil
}

func decodeOne(raw json.RawMessage, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := decoder.Decode(destination); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
		return errors.New("invalid JSON object")
	}
	return nil
}

func decodeClosed(raw json.RawMessage, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
		return errors.New("invalid closed JSON object")
	}
	return nil
}

func rejectCredentialMaterial(body []byte) error {
	contains, err := safety.JSONContainsCredentialMaterial(body)
	if err != nil {
		return errors.New("observation: cannot inspect backend response for credential material")
	}
	if contains {
		return errors.New("observation: backend response contains credential-derived material")
	}
	return nil
}

func decodePage(raw json.RawMessage, requestedPage int, extras []string) ([]json.RawMessage, int, int, int, error) {
	var object map[string]json.RawMessage
	if decodeOne(raw, &object) != nil {
		return nil, 0, 0, 0, errors.New("observation: pagination object is malformed")
	}
	allowed := map[string]bool{"data": true, "start": true, "limit": true, "total": true}
	for _, name := range extras {
		allowed[name] = true
	}
	for name := range object {
		if !allowed[name] {
			return nil, 0, 0, 0, fmt.Errorf("observation: unknown pagination field %q", name)
		}
	}
	var items []json.RawMessage
	var start, limit, total int
	if json.Unmarshal(object["data"], &items) != nil || json.Unmarshal(object["start"], &start) != nil || json.Unmarshal(object["limit"], &limit) != nil || json.Unmarshal(object["total"], &total) != nil || start != requestedPage*PageSize || limit != PageSize || total < 0 || total > MaxPages*PageSize || len(items) > PageSize {
		return nil, 0, 0, 0, errors.New("observation: pagination metadata is invalid")
	}
	return items, start, limit, total, nil
}

func decodeSession(raw json.RawMessage) (Session, error) {
	var wire struct {
		ID          string `json:"id"`
		AgentID     string `json:"agent_id"`
		RunID       string `json:"run_id"`
		Status      string `json:"status"`
		StartedAt   string `json:"started_at"`
		CompletedAt string `json:"completed_at"`
	}
	if decodeOne(raw, &wire) != nil || wire.ID == "" || wire.AgentID == "" || wire.RunID == "" || wire.Status == "" {
		return Session{}, errors.New("invalid session")
	}
	started, err := time.Parse(time.RFC3339Nano, wire.StartedAt)
	if err != nil {
		return Session{}, errors.New("invalid session start time")
	}
	var completed time.Time
	if wire.CompletedAt != "" {
		completed, err = time.Parse(time.RFC3339Nano, wire.CompletedAt)
		if err != nil || completed.Before(started) {
			return Session{}, errors.New("invalid session completion time")
		}
	}
	return Session{ID: wire.ID, AgentID: wire.AgentID, RunID: wire.RunID, Status: wire.Status, StartedAt: started, CompletedAt: completed, Raw: append(json.RawMessage(nil), raw...)}, nil
}

func decodeEvent(raw json.RawMessage) (Event, error) {
	var wire struct {
		ID, EventType, AgentID, SessionID, RunID, CreatedAt string
	}
	var object map[string]json.RawMessage
	if decodeOne(raw, &object) != nil {
		return Event{}, errors.New("invalid event")
	}
	fields := map[string]*string{"id": &wire.ID, "event_type": &wire.EventType, "agent_id": &wire.AgentID, "session_id": &wire.SessionID, "run_id": &wire.RunID, "created_at": &wire.CreatedAt}
	for name, destination := range fields {
		if json.Unmarshal(object[name], destination) != nil || *destination == "" {
			return Event{}, errors.New("invalid event identity")
		}
	}
	created, err := time.Parse(time.RFC3339Nano, wire.CreatedAt)
	if err != nil {
		return Event{}, errors.New("invalid event time")
	}
	return Event{ID: wire.ID, Type: wire.EventType, AgentID: wire.AgentID, SessionID: wire.SessionID, RunID: wire.RunID, CreatedAt: created, Raw: append(json.RawMessage(nil), raw...)}, nil
}

func exactStringSet(got, want []string) bool {
	left, right := append([]string(nil), got...), append([]string(nil), want...)
	slices.Sort(left)
	slices.Sort(right)
	return slices.Equal(left, right) && len(left) == len(slices.Compact(left))
}

func terminal(status string) bool {
	return status == "completed" || status == "failed" || status == "blocked" || status == "halted"
}
func pending(status string) bool {
	return status == "pending" || status == "running" || status == "started"
}

func sameSession(left, right Session) bool {
	return left.ID == right.ID && left.AgentID == right.AgentID && left.RunID == right.RunID && left.Status == right.Status && left.StartedAt.Equal(right.StartedAt) && left.CompletedAt.Equal(right.CompletedAt)
}

func sameEvents(left, right []Event) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index].ID != right[index].ID || left[index].Type != right[index].Type || left[index].RunID != right[index].RunID || !left[index].CreatedAt.Equal(right[index].CreatedAt) {
			return false
		}
	}
	return true
}
