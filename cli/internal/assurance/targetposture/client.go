package targetposture

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/artifact"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/observation"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/safety"
)

const (
	maxPages         = 100
	pageSize         = 100
	maxRequests      = 1000
	maxResponseBytes = 8 << 20
	maxCapturedBytes = 64 << 20
	captureTimeout   = 120 * time.Second
)

type collector struct {
	base     *url.URL
	token    string
	agentID  string
	orgID    string
	pack     string
	catalog  Identity
	http     *http.Client
	now      func() time.Time
	requests int
	bytes    int
}

type pass struct {
	Permissions           []string       `json:"permissions"`
	Agent                 Agent          `json:"agent"`
	Guardrails            []Guardrail    `json:"guardrails"`
	GuardrailAggregate    *Aggregate     `json:"guardrail_aggregate,omitempty"`
	Policies              []Policy       `json:"policies"`
	CurrentPolicyID       string         `json:"current_policy_id,omitempty"`
	BehaviorRules         []BehaviorRule `json:"behavior_rules"`
	BehaviorRuleAggregate *Aggregate     `json:"behavior_rule_aggregate,omitempty"`
}

// NewHTTPClient is the bounded local-only client used by the public finalizer.
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

// Capture obtains two identical safe posture projections through a closed GET
// route sequence. It retains neither the token nor raw response bodies.
func Capture(ctx context.Context, config Config) (*Posture, error) {
	if config.ProxyConfigured {
		return nil, errors.New("target posture: proxy environment is forbidden")
	}
	if config.BackendURL != observation.ExactBackendURL {
		return nil, errors.New("target posture: OPENBOX_BACKEND_URL must be exactly http://127.0.0.1:3000")
	}
	if config.ControlToken == "" || strings.ContainsAny(config.ControlToken, "\x00\r\n") {
		return nil, errors.New("target posture: OPENBOX_CONTROL_TOKEN is required")
	}
	if !safeID(config.AgentID) || !safeID(config.OrganizationID) {
		return nil, errors.New("target posture: invalid observation target identity")
	}
	if _, err := artifact.ParseContentDigest(config.PackDigest); err != nil {
		return nil, errors.New("target posture: invalid observation digest")
	}
	if config.Catalog.Version == "" {
		return nil, errors.New("target posture: recommendation catalog identity is required")
	}
	if _, err := artifact.ParseContentDigest(config.Catalog.Digest); err != nil {
		return nil, errors.New("target posture: invalid recommendation catalog digest")
	}
	parsed, err := url.Parse(config.BackendURL)
	if err != nil || parsed.Scheme != "http" || parsed.Host != "127.0.0.1:3000" || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.User != nil {
		return nil, errors.New("target posture: invalid local backend coordinate")
	}
	httpClient := config.HTTP
	if httpClient == nil {
		httpClient = NewHTTPClient()
	}
	now := config.Now
	if now == nil {
		now = time.Now
	}
	client := &collector{base: parsed, token: config.ControlToken, agentID: config.AgentID, orgID: config.OrganizationID, pack: config.PackDigest, catalog: config.Catalog, http: httpClient, now: now}
	bounded, cancel := context.WithTimeout(ctx, captureTimeout)
	defer cancel()
	started := now().UTC()
	first, err := client.capturePass(bounded)
	if err != nil {
		return nil, err
	}
	second, err := client.capturePass(bounded)
	if err != nil {
		return nil, err
	}
	firstBytes, err := artifact.CanonicalJSON(first)
	if err != nil {
		return nil, err
	}
	secondBytes, err := artifact.CanonicalJSON(second)
	if err != nil || !bytes.Equal(firstBytes, secondBytes) {
		return nil, errors.New("target posture: control identities or versions drifted between capture passes")
	}
	completed := now().UTC()
	return &Posture{
		Schema: Schema, ReadContract: ReadContract, Catalog: config.Catalog,
		Observation:   ObservationIdentity{PackDigest: config.PackDigest, AgentID: config.AgentID, OrganizationID: config.OrganizationID},
		CaptureWindow: CaptureWindow{StartedAt: started.Format(time.RFC3339Nano), CompletedAt: completed.Format(time.RFC3339Nano), Passes: 2},
		Permissions:   first.Permissions, Agent: first.Agent,
		Seams: Seams{
			Guardrail:           Seam{Status: "observed", Permission: "read:agent_guardrail", Route: "/agent/{agentId}/guardrails"},
			Policy:              Seam{Status: "observed", Permission: "read:agent_policy", Route: "/agent/{agentId}/policies"},
			BehaviorRule:        Seam{Status: "observed", Permission: "read:agent_behavior_rule", Route: "/agent/{agentId}/behavior-rule"},
			ApprovalRequirement: Seam{Status: "observed", Permission: "read:agent_behavior_rule", Route: "/agent/{agentId}/behavior-rule"},
			SDKIntegration:      Seam{Status: "observed", Permission: "read:agent", Route: "/agent/{agentId}"},
		},
		Guardrails: first.Guardrails, GuardrailAggregate: first.GuardrailAggregate,
		Policies: first.Policies, CurrentPolicyID: first.CurrentPolicyID,
		BehaviorRules: first.BehaviorRules, BehaviorRuleAggregate: first.BehaviorRuleAggregate,
	}, nil
}

func (client *collector) capturePass(ctx context.Context) (pass, error) {
	permissions, err := client.profile(ctx)
	if err != nil {
		return pass{}, err
	}
	agent, err := client.agent(ctx)
	if err != nil {
		return pass{}, err
	}
	guardrails, guardrailAggregate, err := client.guardrails(ctx)
	if err != nil {
		return pass{}, err
	}
	policies, err := client.policies(ctx)
	if err != nil {
		return pass{}, err
	}
	currentPolicy, err := client.currentPolicy(ctx)
	if err != nil {
		return pass{}, err
	}
	if currentPolicy != "" {
		found := false
		for index := range policies {
			if policies[index].ID == currentPolicy {
				policies[index].Current = true
				found = true
			}
		}
		if !found {
			return pass{}, errors.New("target posture: current policy is absent from the complete policy list")
		}
	}
	behavior, behaviorAggregate, err := client.behaviorRules(ctx)
	if err != nil {
		return pass{}, err
	}
	return pass{Permissions: permissions, Agent: agent, Guardrails: guardrails, GuardrailAggregate: guardrailAggregate, Policies: policies, CurrentPolicyID: currentPolicy, BehaviorRules: behavior, BehaviorRuleAggregate: behaviorAggregate}, nil
}

func (client *collector) profile(ctx context.Context) ([]string, error) {
	body, err := client.get(ctx, "/auth/profile")
	if err != nil {
		return nil, err
	}
	raw, err := unwrap(body)
	if err != nil {
		return nil, errors.New("target posture: auth profile envelope is invalid")
	}
	object, err := decodeObject(raw)
	if err != nil {
		return nil, errors.New("target posture: auth profile is invalid")
	}
	org, ok := stringField(object, "orgId")
	apiKey, apiOK := boolField(object, "isApiKeyAuth")
	var permissions []string
	if err := json.Unmarshal(object["permissions"], &permissions); err != nil || !ok || org != client.orgID || !apiOK || !apiKey {
		return nil, errors.New("target posture: credential identity does not match the observation target")
	}
	sort.Strings(permissions)
	want := append([]string(nil), observation.RequiredPermissions...)
	sort.Strings(want)
	if !equalStrings(permissions, want) {
		return nil, errors.New("target posture: credential permissions do not match the exact read authority")
	}
	return permissions, nil
}

func (client *collector) agent(ctx context.Context) (Agent, error) {
	body, err := client.get(ctx, "/agent/"+client.agentID)
	if err != nil {
		return Agent{}, err
	}
	raw, err := unwrap(body)
	if err != nil {
		return Agent{}, errors.New("target posture: agent envelope is invalid")
	}
	object, err := decodeObject(raw)
	if err != nil {
		return Agent{}, errors.New("target posture: agent response is invalid")
	}
	id, idOK := stringField(object, "id")
	org, orgOK := stringField(object, "organization_id")
	if !idOK || !orgOK || id != client.agentID || org != client.orgID {
		return Agent{}, errors.New("target posture: agent or organization identity does not reconcile")
	}
	agent := Agent{ID: id, OrganizationID: org}
	if rawStatus, ok := object["status"]; ok && string(rawStatus) != "null" {
		var scalar any
		decoder := json.NewDecoder(bytes.NewReader(rawStatus))
		decoder.UseNumber()
		if decoder.Decode(&scalar) != nil {
			return Agent{}, errors.New("target posture: agent status is invalid")
		}
		switch value := scalar.(type) {
		case string:
			if !safeText(value, 64) {
				return Agent{}, errors.New("target posture: agent status is unsafe")
			}
			agent.Status = value
		case json.Number:
			integer, numberErr := value.Int64()
			if numberErr != nil {
				return Agent{}, errors.New("target posture: agent status is not an integer")
			}
			agent.Status = integer
		default:
			return Agent{}, errors.New("target posture: agent status has an unknown shape")
		}
	}
	agent.UpdatedAt, _ = optionalTime(object, "updated_at")
	return agent, nil
}

func (client *collector) guardrails(ctx context.Context) ([]Guardrail, *Aggregate, error) {
	raws, extras, err := client.pages(ctx, "/agent/"+client.agentID+"/guardrails", []string{"guardrail_versions_hash", "guardrail_versions_count", "guardrail_versions_updated_at"})
	if err != nil {
		return nil, nil, err
	}
	aggregate, err := aggregateFrom(extras, "guardrail_versions")
	if err != nil {
		return nil, nil, err
	}
	result := make([]Guardrail, 0, len(raws))
	for _, raw := range raws {
		object, decodeErr := decodeObject(raw)
		if decodeErr != nil {
			return nil, nil, errors.New("target posture: guardrail item is invalid")
		}
		id, idOK := stringField(object, "id")
		version, versionOK := stringField(object, "version_hash")
		agentID, agentOK := stringField(object, "agent_id")
		if !idOK || !versionOK || !agentOK || agentID != client.agentID {
			return nil, nil, errors.New("target posture: guardrail lacks a stable target identity")
		}
		item := Guardrail{ID: id, VersionHash: version}
		item.Type, _ = firstString(object, "guardrail_type", "type")
		item.Stage, _ = firstString(object, "processing_stage", "stage")
		item.Active, _ = firstBool(object, "is_active", "active")
		item.Order, _ = firstInt(object, "order", "priority")
		item.TrustImpact, _ = stringField(object, "trust_impact")
		item.UpdatedAt, _ = optionalTime(object, "updated_at")
		item.Opaque = item.Type == "" || item.Stage == ""
		if !safeProjectionStrings(item.ID, item.VersionHash, item.Type, item.Stage, item.TrustImpact) {
			return nil, nil, errors.New("target posture: guardrail projection is unsafe")
		}
		result = append(result, item)
	}
	if err := sortAndUnique(result, func(value Guardrail) string { return value.ID }); err != nil {
		return nil, nil, err
	}
	return result, aggregate, nil
}

func (client *collector) policies(ctx context.Context) ([]Policy, error) {
	raws, _, err := client.pages(ctx, "/agent/"+client.agentID+"/policies", nil)
	if err != nil {
		return nil, err
	}
	result := make([]Policy, 0, len(raws))
	for _, raw := range raws {
		object, decodeErr := decodeObject(raw)
		if decodeErr != nil {
			return nil, errors.New("target posture: policy item is invalid")
		}
		id, idOK := stringField(object, "id")
		version, versionOK := stringField(object, "version_hash")
		agentID, agentOK := stringField(object, "agent_id")
		if !idOK || !versionOK || !agentOK || agentID != client.agentID {
			return nil, errors.New("target posture: policy lacks a stable target identity")
		}
		item := Policy{ID: id, VersionHash: version}
		item.Active, _ = firstBool(object, "is_active", "active")
		item.Current, _ = firstBool(object, "is_current_version", "current")
		item.TrustImpact, _ = stringField(object, "trust_impact")
		item.UpdatedAt, _ = optionalTime(object, "updated_at")
		item.Opaque = true
		if !safeProjectionStrings(item.ID, item.VersionHash, item.TrustImpact) {
			return nil, errors.New("target posture: policy projection is unsafe")
		}
		result = append(result, item)
	}
	if err := sortAndUnique(result, func(value Policy) string { return value.ID }); err != nil {
		return nil, err
	}
	return result, nil
}

func (client *collector) currentPolicy(ctx context.Context) (string, error) {
	body, err := client.get(ctx, "/agent/"+client.agentID+"/policies/current")
	if err != nil {
		return "", err
	}
	raw, err := unwrap(body)
	if err != nil {
		return "", errors.New("target posture: current-policy envelope is invalid")
	}
	if string(raw) == "null" {
		return "", nil
	}
	object, err := decodeObject(raw)
	if err != nil {
		return "", errors.New("target posture: current-policy response is invalid")
	}
	id, ok := stringField(object, "id")
	agentID, agentOK := stringField(object, "agent_id")
	if !ok || !agentOK || agentID != client.agentID {
		return "", errors.New("target posture: current policy lacks a stable target identity")
	}
	return id, nil
}

func (client *collector) behaviorRules(ctx context.Context) ([]BehaviorRule, *Aggregate, error) {
	raws, extras, err := client.pages(ctx, "/agent/"+client.agentID+"/behavior-rule", []string{"behavior_rule_versions_hash", "behavior_rule_versions_count", "behavior_rule_versions_updated_at"})
	if err != nil {
		return nil, nil, err
	}
	aggregate, err := aggregateFrom(extras, "behavior_rule_versions")
	if err != nil {
		return nil, nil, err
	}
	result := make([]BehaviorRule, 0, len(raws))
	for _, raw := range raws {
		object, decodeErr := decodeObject(raw)
		if decodeErr != nil {
			return nil, nil, errors.New("target posture: behavior-rule item is invalid")
		}
		id, idOK := stringField(object, "id")
		version, versionOK := stringField(object, "version_hash")
		base, baseOK := stringField(object, "base_rule_id")
		trigger, triggerOK := stringField(object, "trigger")
		verdict, verdictOK := stringField(object, "verdict")
		agentID, agentOK := stringField(object, "agent_id")
		organizationID, organizationOK := stringField(object, "organization_id")
		if !idOK || !versionOK || !baseOK || !triggerOK || !verdictOK || !agentOK || agentID != client.agentID || !organizationOK || organizationID != client.orgID {
			return nil, nil, errors.New("target posture: behavior rule lacks a stable safe target identity")
		}
		item := BehaviorRule{ID: id, VersionHash: version, BaseRuleID: base, Trigger: trigger, Verdict: verdict}
		item.DependencyBaseRuleID, _ = stringField(object, "dependency_base_rule_id")
		item.Priority, _ = firstInt(object, "priority")
		item.Active, _ = firstBool(object, "is_active", "active")
		item.Current, _ = firstBool(object, "is_current_version", "current")
		item.TimeWindowSeconds, _ = firstInt(object, "time_window")
		item.TrustImpact, _ = stringField(object, "trust_impact")
		item.UpdatedAt, _ = optionalTime(object, "updated_at")
		item.Opaque = item.TimeWindowSeconds == 0
		if !safeProjectionStrings(item.ID, item.VersionHash, item.BaseRuleID, item.DependencyBaseRuleID, item.Trigger, item.Verdict, item.TrustImpact) {
			return nil, nil, errors.New("target posture: behavior-rule projection is unsafe")
		}
		result = append(result, item)
	}
	if err := sortAndUnique(result, func(value BehaviorRule) string { return value.ID }); err != nil {
		return nil, nil, err
	}
	return result, aggregate, nil
}

func (client *collector) pages(ctx context.Context, path string, extraNames []string) ([]json.RawMessage, map[string]json.RawMessage, error) {
	var result []json.RawMessage
	var stableExtras map[string]json.RawMessage
	seen := make(map[string]bool)
	for page := 0; page < maxPages; page++ {
		relative := path + "?page=" + strconv.Itoa(page) + "&perPage=" + strconv.Itoa(pageSize)
		body, err := client.get(ctx, relative)
		if err != nil {
			return nil, nil, err
		}
		raw, err := unwrap(body)
		if err != nil {
			return nil, nil, errors.New("target posture: list envelope is invalid")
		}
		items, start, limit, total, extras, err := decodePage(raw, extraNames)
		if err != nil || start != page || limit < 1 || limit > pageSize || total < 0 || total > maxPages*pageSize {
			return nil, nil, errors.New("target posture: pagination contract is invalid")
		}
		if stableExtras == nil {
			stableExtras = extras
		} else if !equalRawMaps(stableExtras, extras) {
			return nil, nil, errors.New("target posture: aggregate list identity drifted across pages")
		}
		for _, item := range items {
			object, objectErr := decodeObject(item)
			id, idOK := stringField(object, "id")
			if objectErr != nil || !idOK || seen[id] {
				return nil, nil, errors.New("target posture: list contains a malformed or duplicate item")
			}
			seen[id] = true
			result = append(result, item)
		}
		if len(result) >= total {
			if len(result) != total {
				return nil, nil, errors.New("target posture: pagination total does not reconcile")
			}
			return result, stableExtras, nil
		}
	}
	return nil, nil, errors.New("target posture: pagination page bound exceeded")
}

func (client *collector) get(ctx context.Context, relative string) ([]byte, error) {
	if client.requests >= maxRequests {
		return nil, errors.New("target posture: request bound exceeded")
	}
	client.requests++
	requestURL := *client.base
	path, query, _ := strings.Cut(relative, "?")
	requestURL.Path = path
	requestURL.RawQuery = query
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL.String(), nil)
	if err != nil {
		return nil, errors.New("target posture: construct bounded GET request")
	}
	request.Header.Set("accept", "application/json")
	request.Header.Set("accept-encoding", "identity")
	request.Header.Set("x-api-key", client.token)
	response, err := client.http.Do(request)
	if err != nil {
		return nil, errors.New("target posture: bounded backend GET failed")
	}
	defer response.Body.Close()
	if response.StatusCode >= 300 && response.StatusCode < 400 {
		return nil, errors.New("target posture: backend redirect rejected")
	}
	mediaType, parameters, mediaErr := mime.ParseMediaType(response.Header.Get("content-type"))
	if mediaErr != nil || mediaType != "application/json" || (len(parameters) > 0 && (len(parameters) != 1 || !strings.EqualFold(parameters["charset"], "utf-8"))) || response.Header.Get("content-encoding") != "" {
		return nil, errors.New("target posture: backend response representation rejected")
	}
	if response.ContentLength > maxResponseBytes {
		return nil, errors.New("target posture: backend response exceeds the byte bound")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil || len(body) > maxResponseBytes || (response.ContentLength >= 0 && int64(len(body)) != response.ContentLength) {
		return nil, errors.New("target posture: incomplete or oversized backend response")
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("target posture: backend GET %s returned status %d", path, response.StatusCode)
	}
	if !utf8.Valid(body) || !json.Valid(body) {
		return nil, errors.New("target posture: backend response is not valid JSON")
	}
	if _, err := artifact.CanonicalizeJSON(body); err != nil {
		return nil, errors.New("target posture: backend response JSON is ambiguous")
	}
	contains, err := safety.JSONContainsCredentialMaterial(body)
	if err != nil || contains {
		return nil, errors.New("target posture: backend response contains unsafe credential-derived material")
	}
	client.bytes += len(body)
	if client.bytes > maxCapturedBytes {
		return nil, errors.New("target posture: total response byte bound exceeded")
	}
	return body, nil
}

func unwrap(body []byte) (json.RawMessage, error) {
	object, err := decodeObject(body)
	if err != nil {
		return nil, err
	}
	for key := range object {
		if key != "data" && key != "status" {
			return nil, errors.New("unknown envelope field")
		}
	}
	raw, ok := object["data"]
	if !ok {
		return nil, errors.New("missing data envelope")
	}
	return raw, nil
}

func decodePage(raw json.RawMessage, extraNames []string) ([]json.RawMessage, int, int, int, map[string]json.RawMessage, error) {
	object, err := decodeObject(raw)
	if err != nil {
		return nil, 0, 0, 0, nil, err
	}
	allowed := map[string]bool{"data": true, "start": true, "limit": true, "total": true}
	for _, name := range extraNames {
		allowed[name] = true
	}
	for key := range object {
		if !allowed[key] {
			return nil, 0, 0, 0, nil, errors.New("unknown pagination field")
		}
	}
	var items []json.RawMessage
	var start, limit, total int
	if json.Unmarshal(object["data"], &items) != nil || json.Unmarshal(object["start"], &start) != nil || json.Unmarshal(object["limit"], &limit) != nil || json.Unmarshal(object["total"], &total) != nil {
		return nil, 0, 0, 0, nil, errors.New("invalid pagination fields")
	}
	extras := make(map[string]json.RawMessage, len(extraNames))
	for _, name := range extraNames {
		rawValue, ok := object[name]
		if !ok {
			return nil, 0, 0, 0, nil, errors.New("missing aggregate field")
		}
		extras[name] = append([]byte(nil), rawValue...)
	}
	return items, start, limit, total, extras, nil
}

func aggregateFrom(values map[string]json.RawMessage, prefix string) (*Aggregate, error) {
	if values == nil {
		return nil, errors.New("target posture: aggregate identity is missing")
	}
	var aggregate Aggregate
	if json.Unmarshal(values[prefix+"_hash"], &aggregate.VersionHash) != nil || !safeText(aggregate.VersionHash, 256) || json.Unmarshal(values[prefix+"_count"], &aggregate.Count) != nil || aggregate.Count < 0 || aggregate.Count > maxPages*pageSize {
		return nil, errors.New("target posture: aggregate identity is invalid")
	}
	if raw, ok := values[prefix+"_updated_at"]; ok && string(raw) != "null" {
		if json.Unmarshal(raw, &aggregate.UpdatedAt) != nil || !validTime(aggregate.UpdatedAt) {
			return nil, errors.New("target posture: aggregate update identity is invalid")
		}
	}
	return &aggregate, nil
}

func decodeObject(raw []byte) (map[string]json.RawMessage, error) {
	var object map[string]json.RawMessage
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := decoder.Decode(&object); err != nil || object == nil {
		return nil, errors.New("not a JSON object")
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return nil, errors.New("trailing JSON data")
	}
	return object, nil
}

func stringField(object map[string]json.RawMessage, key string) (string, bool) {
	raw, ok := object[key]
	if !ok || string(raw) == "null" {
		return "", false
	}
	var value string
	if json.Unmarshal(raw, &value) != nil || !safeText(value, 256) {
		return "", false
	}
	return value, true
}

func boolField(object map[string]json.RawMessage, key string) (bool, bool) {
	raw, ok := object[key]
	if !ok {
		return false, false
	}
	var value bool
	if json.Unmarshal(raw, &value) != nil {
		return false, false
	}
	return value, true
}

func firstString(object map[string]json.RawMessage, keys ...string) (string, bool) {
	for _, key := range keys {
		if value, ok := stringField(object, key); ok {
			return value, true
		}
	}
	return "", false
}

func firstBool(object map[string]json.RawMessage, keys ...string) (bool, bool) {
	for _, key := range keys {
		if value, ok := boolField(object, key); ok {
			return value, true
		}
		if text, ok := stringField(object, key); ok {
			switch strings.ToLower(text) {
			case "active", "enabled", "true":
				return true, true
			case "inactive", "disabled", "false":
				return false, true
			}
		}
	}
	return false, false
}

func firstInt(object map[string]json.RawMessage, keys ...string) (int, bool) {
	for _, key := range keys {
		raw, ok := object[key]
		if !ok || string(raw) == "null" {
			continue
		}
		var value int
		if json.Unmarshal(raw, &value) == nil {
			return value, true
		}
	}
	return 0, false
}

func optionalTime(object map[string]json.RawMessage, key string) (string, bool) {
	value, ok := stringField(object, key)
	if !ok || !validTime(value) {
		return "", false
	}
	return value, true
}

func validTime(value string) bool {
	_, err := time.Parse(time.RFC3339Nano, value)
	return err == nil
}

func safeID(value string) bool {
	return safeText(value, 256) && !strings.ContainsAny(value, "/?&#")
}

func safeText(value string, maximum int) bool {
	if value == "" || len(value) > maximum || !utf8.ValidString(value) {
		return false
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return false
		}
	}
	return true
}

func safeProjectionStrings(values ...string) bool {
	for _, value := range values {
		if value != "" && !safeText(value, 256) {
			return false
		}
	}
	return true
}

func sortAndUnique[T any](values []T, id func(T) string) error {
	sort.Slice(values, func(left, right int) bool { return id(values[left]) < id(values[right]) })
	for index := 1; index < len(values); index++ {
		if id(values[index-1]) == id(values[index]) {
			return errors.New("target posture: duplicate projected control identity")
		}
	}
	return nil
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func equalRawMaps(left, right map[string]json.RawMessage) bool {
	leftBytes, leftErr := artifact.CanonicalJSON(left)
	rightBytes, rightErr := artifact.CanonicalJSON(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftBytes, rightBytes)
}
