// Package securityreport owns Phase 4 candidate validation, deterministic
// recommendation mapping, report rendering, and sealed report-pack handling.
package securityreport

import (
	"context"
	"net/http"
	"time"

	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/observation"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/targetposture"
)

const (
	Schema                      = "ai.openbox.project-security-report/v1"
	ManifestSchema              = "ai.openbox.project-security-report.manifest/v1"
	ReportSchema                = "ai.openbox.project-security-report.report/v1"
	RecommendationSchema        = "ai.openbox.project-recommendation-catalog/v1"
	RecommendationVersion       = "2026-08-27-mvp1"
	RecommendationDigest        = "sha256:96ba1937ffa01aa8515da33cbd8b374c7981a9b7b160e36fa6a4ba7d60bf3dbe"
	MaxCandidateBytes     int64 = 4 << 20
)

type Skill struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Digest  string `json:"digest"`
}

type Analyzer struct {
	Host    string `json:"host,omitempty"`
	Product string `json:"product,omitempty"`
	Version string `json:"version,omitempty"`
	Model   string `json:"model,omitempty"`
}

type Candidate struct {
	Schema      string `json:"schema"`
	Skill       Skill  `json:"skill"`
	Observation struct {
		Schema     string `json:"schema"`
		PackDigest string `json:"pack_digest"`
	} `json:"observation"`
	Analyzer       *Analyzer        `json:"analyzer,omitempty"`
	Result         string           `json:"result"`
	CoverageGapIDs []string         `json:"coverage_gap_ids"`
	Issues         []CandidateIssue `json:"issues"`
}

type CandidateIssue struct {
	CandidateID      string              `json:"candidate_id"`
	Title            string              `json:"title"`
	ObservedBehavior string              `json:"observed_behavior"`
	CrossedBoundary  string              `json:"crossed_boundary"`
	Rationale        string              `json:"rationale"`
	Inference        bool                `json:"inference"`
	Confidence       string              `json:"confidence"`
	Severity         string              `json:"severity"`
	Evidence         []EvidenceReference `json:"evidence"`
	Standards        []StandardReference `json:"standards"`
	CoverageGapIDs   []string            `json:"coverage_gap_ids"`
}

type EvidenceReference struct {
	Index string `json:"index"`
	ID    string `json:"id"`
	Role  string `json:"role"`
}

type StandardReference struct {
	Catalog string `json:"catalog"`
	Version string `json:"version"`
	ID      string `json:"id"`
}

type ObservedFact struct {
	EvidenceID string `json:"evidence_id"`
	Authority  string `json:"authority"`
	Type       string `json:"type"`
	Timestamp  string `json:"timestamp,omitempty"`
}

type Action struct {
	Class            string `json:"class"`
	Name             string `json:"name"`
	SourceEvidenceID string `json:"source_evidence_id"`
}

type Issue struct {
	IssueID                   string              `json:"issue_id"`
	CandidateID               string              `json:"candidate_id"`
	Title                     string              `json:"title"`
	ObservedBehaviorAssertion string              `json:"observed_behavior_assertion"`
	CrossedBoundaryAssertion  string              `json:"crossed_boundary_assertion"`
	RationaleAssertion        string              `json:"rationale_assertion"`
	Inference                 bool                `json:"inference"`
	Confidence                string              `json:"confidence"`
	Severity                  string              `json:"severity"`
	ObservedFacts             []ObservedFact      `json:"observed_facts"`
	Action                    *Action             `json:"action,omitempty"`
	Evidence                  []EvidenceReference `json:"evidence"`
	Standards                 []StandardReference `json:"standards"`
	CoverageGapIDs            []string            `json:"coverage_gap_ids"`
	RecommendationMapping     string              `json:"recommendation_mapping"`
}

type VersionIdentity struct {
	Version string `json:"version"`
	Digest  string `json:"digest"`
}

type SchemaIdentity struct {
	Schema string `json:"schema"`
	Digest string `json:"digest"`
}

type RecommendationTarget struct {
	AgentID     string `json:"agent_id"`
	ActionClass string `json:"action_class,omitempty"`
	ActionName  string `json:"action_name,omitempty"`
}

type FutureVerification struct {
	ExpectedBehavior           string   `json:"expected_behavior"`
	AuthoritativeEvidenceRoles []string `json:"authoritative_evidence_roles"`
	SuccessCriteria            string   `json:"success_criteria"`
	RefusalCriteria            string   `json:"refusal_criteria"`
}

type Recommendation struct {
	RecommendationID          string               `json:"recommendation_id"`
	IssueID                   string               `json:"issue_id"`
	CatalogEntryID            string               `json:"catalog_entry_id"`
	Catalog                   VersionIdentity      `json:"catalog"`
	Kind                      string               `json:"kind"`
	Status                    string               `json:"status"`
	Target                    RecommendationTarget `json:"target"`
	CurrentControlIDs         []string             `json:"current_control_ids"`
	IntendedConstraint        string               `json:"intended_constraint"`
	ExpectedProtectedBehavior string               `json:"expected_protected_behavior"`
	FutureVerification        FutureVerification   `json:"future_verification"`
	Limitations               []string             `json:"limitations"`
}

type Report struct {
	Schema                string           `json:"schema"`
	Observation           SchemaIdentity   `json:"observation"`
	Analysis              SchemaIdentity   `json:"analysis"`
	Standards             VersionIdentity  `json:"standards"`
	RecommendationCatalog VersionIdentity  `json:"recommendation_catalog"`
	TargetPosture         SchemaIdentity   `json:"target_posture"`
	Result                string           `json:"result"`
	SecurityPass          bool             `json:"security_pass"`
	CoverageGapIDs        []string         `json:"coverage_gap_ids"`
	Issues                []Issue          `json:"issues"`
	Recommendations       []Recommendation `json:"recommendations"`
	Limitations           []string         `json:"limitations"`
}

type Prepared struct {
	ObservationPath string
	CandidatePath   string
	OutputPath      string
	Observation     *observation.Pack
	Candidate       Candidate
	CandidateBytes  []byte
	StandardsBytes  []byte
	PackDigest      string
	AgentID         string
	OrganizationID  string
	Issues          []Issue
}

type Input struct {
	Evaluation      string
	Analysis        string
	Output          string
	BackendURL      string
	ControlToken    string
	ProxyConfigured bool
	HTTP            *http.Client
	Now             func() time.Time
}

type RuntimeInput struct {
	BackendURL      string
	ControlToken    string
	ProxyConfigured bool
	HTTP            *http.Client
	Now             func() time.Time
}

type Result struct {
	Output     string
	PackDigest string
}

type Dependencies struct {
	Capture func(context.Context, targetposture.Config) (*targetposture.Posture, error)
}

type Projections struct {
	Report   Report
	JSON     []byte
	Markdown []byte
	SARIF    []byte
}

type VerifiedPack struct {
	Root       string
	PackDigest string
	Files      map[string][]byte
	Projection Projections
}
