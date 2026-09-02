// Package targetposture owns the Phase 4 GET-only dashboard posture reader.
package targetposture

import (
	"net/http"
	"time"
)

const (
	Schema       = "ai.openbox.project-target-posture/v1"
	ReadContract = "local-dashboard-control-read/v1"
)

type Identity struct {
	Version string `json:"version"`
	Digest  string `json:"digest"`
}

type ObservationIdentity struct {
	PackDigest     string `json:"pack_digest"`
	AgentID        string `json:"agent_id"`
	OrganizationID string `json:"organization_id"`
}

type CaptureWindow struct {
	StartedAt   string `json:"started_at"`
	CompletedAt string `json:"completed_at"`
	Passes      int    `json:"passes"`
}

type Agent struct {
	ID             string `json:"id"`
	OrganizationID string `json:"organization_id"`
	Status         any    `json:"status,omitempty"`
	UpdatedAt      string `json:"updated_at,omitempty"`
}

type Seam struct {
	Status     string `json:"status"`
	Permission string `json:"permission"`
	Route      string `json:"route"`
}

type Seams struct {
	Guardrail           Seam `json:"guardrail"`
	Policy              Seam `json:"policy"`
	BehaviorRule        Seam `json:"behavior_rule"`
	ApprovalRequirement Seam `json:"approval_requirement"`
	SDKIntegration      Seam `json:"sdk_integration"`
}

type Aggregate struct {
	VersionHash string `json:"version_hash"`
	Count       int    `json:"count"`
	UpdatedAt   string `json:"updated_at,omitempty"`
}

type Guardrail struct {
	ID          string `json:"id"`
	VersionHash string `json:"version_hash"`
	Type        string `json:"type,omitempty"`
	Stage       string `json:"stage,omitempty"`
	Active      bool   `json:"active"`
	Order       int    `json:"order,omitempty"`
	TrustImpact string `json:"trust_impact,omitempty"`
	UpdatedAt   string `json:"updated_at,omitempty"`
	Opaque      bool   `json:"opaque"`
}

type Policy struct {
	ID          string `json:"id"`
	VersionHash string `json:"version_hash"`
	Active      bool   `json:"active"`
	Current     bool   `json:"current"`
	TrustImpact string `json:"trust_impact,omitempty"`
	UpdatedAt   string `json:"updated_at,omitempty"`
	Opaque      bool   `json:"opaque"`
}

type BehaviorRule struct {
	ID                   string `json:"id"`
	VersionHash          string `json:"version_hash"`
	BaseRuleID           string `json:"base_rule_id"`
	DependencyBaseRuleID string `json:"dependency_base_rule_id,omitempty"`
	Trigger              string `json:"trigger"`
	Verdict              string `json:"verdict"`
	Priority             int    `json:"priority"`
	Active               bool   `json:"active"`
	Current              bool   `json:"current"`
	TimeWindowSeconds    int    `json:"time_window_seconds,omitempty"`
	TrustImpact          string `json:"trust_impact,omitempty"`
	UpdatedAt            string `json:"updated_at,omitempty"`
	Opaque               bool   `json:"opaque"`
}

type Posture struct {
	Schema                string              `json:"schema"`
	ReadContract          string              `json:"read_contract"`
	Catalog               Identity            `json:"catalog"`
	Observation           ObservationIdentity `json:"observation"`
	CaptureWindow         CaptureWindow       `json:"capture_window"`
	Permissions           []string            `json:"permissions"`
	Agent                 Agent               `json:"agent"`
	Seams                 Seams               `json:"seams"`
	Guardrails            []Guardrail         `json:"guardrails"`
	GuardrailAggregate    *Aggregate          `json:"guardrail_aggregate,omitempty"`
	Policies              []Policy            `json:"policies"`
	CurrentPolicyID       string              `json:"current_policy_id,omitempty"`
	BehaviorRules         []BehaviorRule      `json:"behavior_rules"`
	BehaviorRuleAggregate *Aggregate          `json:"behavior_rule_aggregate,omitempty"`
}

type Config struct {
	BackendURL      string
	ControlToken    string
	AgentID         string
	OrganizationID  string
	PackDigest      string
	Catalog         Identity
	HTTP            *http.Client
	ProxyConfigured bool
	Now             func() time.Time
}
