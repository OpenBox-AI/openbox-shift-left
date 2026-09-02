// Package observation owns the read-only local-backend collection path for
// project assurance. It deliberately has no mutation methods.
package observation

import (
	"encoding/json"
	"net/http"
	"time"
)

const (
	Schema                = "ai.openbox.project-observation/v1"
	BackendSchema         = "ai.openbox.project-observation.backend/v1"
	RunSchema             = "ai.openbox.project-observation.run/v1"
	EffectsSchema         = "ai.openbox.project-observation.effects/v1"
	BehaviorSchema        = "ai.openbox.project-observation.behavior/v1"
	CoverageSchema        = "ai.openbox.project-observation.coverage/v1"
	ManifestSchema        = "ai.openbox.project-observation.manifest/v1"
	OpenShellRecordSchema = "ai.openbox.project-observation.openshell-record/v1"
	ExactBackendURL       = "http://127.0.0.1:3000"
	PageSize              = 100
	MaxPages              = 100
	MaxResponseBytes      = 8 << 20
	MaxCapturedBytes      = 64 << 20
	MaxRequests           = 1000
	CollectionTimeout     = 120 * time.Second
)

var RequiredPermissions = []string{
	"create:agent",
	"read:agent",
	"update:agent",
	"read:agent_session",
	"read:agent_log",
	"read:agent_guardrail",
	"read:agent_policy",
	"read:agent_behavior_rule",
}

const DashboardActivityContract = "dashboard-session-activity/v1"

type Config struct {
	BackendURL      string
	ControlToken    string
	AgentID         string
	HTTP            *http.Client
	ProxyConfigured bool
	Now             func() time.Time
	Sleep           func(time.Duration)
}

type Entry struct {
	Ordinal        int    `json:"ordinal"`
	Method         string `json:"method"`
	Path           string `json:"path"`
	Status         int    `json:"status"`
	ContentType    string `json:"content_type"`
	BodyBytes      int    `json:"body_bytes"`
	SHA256         string `json:"sha256"`
	BodyBase64     string `json:"body_base64"`
	Representation string `json:"representation"`
}

type Snapshot struct {
	OrganizationID string
	Backend        BackendIdentity
	Entries        []Entry
}

type BackendIdentity struct {
	URL         string `json:"url"`
	APIContract string `json:"api_contract"`
}

type Window struct {
	EvaluationID string
	StartedAt    time.Time
	Deadline     time.Time
}

type Session struct {
	ID          string          `json:"id"`
	AgentID     string          `json:"agent_id"`
	RunID       string          `json:"run_id"`
	Status      string          `json:"status"`
	StartedAt   time.Time       `json:"started_at"`
	CompletedAt time.Time       `json:"completed_at"`
	Raw         json.RawMessage `json:"-"`
}

type Event struct {
	ID            string          `json:"id"`
	Type          string          `json:"event_type"`
	AgentID       string          `json:"agent_id"`
	SessionID     string          `json:"session_id"`
	RunID         string          `json:"run_id"`
	CreatedAt     time.Time       `json:"created_at"`
	SourceOrdinal int             `json:"-"`
	SourceRecord  int             `json:"-"`
	Raw           json.RawMessage `json:"-"`
}

type Result struct {
	OrganizationID string
	Session        Session
	Events         []Event
	Entries        []Entry
}
