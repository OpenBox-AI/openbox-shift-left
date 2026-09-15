// Package observation owns the read-only local-backend collection path for
// project assurance. It deliberately has no mutation methods.
package observation

import (
	"encoding/json"
	"net/http"
	"time"
)

const (
	// /v2 because the evidence source changed, not because the format was
	// tidied. A pack whose isolation evidence comes from a typed provider
	// record means something different from one whose evidence was gateway log
	// text, and reusing the v1 identity for it would be the same mistake that
	// left three earlier packs unreadable under their own label.
	//
	// There is no v1 reader. Packs sealed under v1 are historical artifacts,
	// not verifiable evidence, by explicit decision.
	Schema                = "ai.openbox.project-observation/v2"
	BackendSchema         = "ai.openbox.project-observation.backend/v2"
	RunSchema             = "ai.openbox.project-observation.run/v2"
	EffectsSchema         = "ai.openbox.project-observation.effects/v2"
	BehaviorSchema        = "ai.openbox.project-observation.behavior/v2"
	CoverageSchema        = "ai.openbox.project-observation.coverage/v2"
	ManifestSchema        = "ai.openbox.project-observation.manifest/v2"
	SandboxEvidenceSchema = "ai.openbox.project-observation.sandbox-evidence/v2"
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
