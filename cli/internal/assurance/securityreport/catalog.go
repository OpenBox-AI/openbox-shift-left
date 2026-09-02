package securityreport

import (
	"embed"
	"encoding/json"
	"errors"
	"sort"
	"sync"

	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/artifact"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/targetposture"
)

//go:embed resources/recommendation-catalog.json
var resourceFiles embed.FS

type catalogEntry struct {
	ID            string   `json:"id"`
	Kind          string   `json:"kind"`
	StandardIDs   []string `json:"standard_ids"`
	ActionClasses []string `json:"action_classes"`
	Permission    string   `json:"permission"`
	Route         string   `json:"route"`
	Constraint    string   `json:"constraint"`
	Limitations   []string `json:"limitations"`
}

type recommendationCatalog struct {
	Schema       string         `json:"schema"`
	Version      string         `json:"version"`
	ReadContract string         `json:"read_contract"`
	Entries      []catalogEntry `json:"entries"`
}

var (
	catalogOnce    sync.Once
	catalogValue   recommendationCatalog
	catalogContent []byte
	catalogErr     error
)

func loadCatalog() (recommendationCatalog, []byte, error) {
	catalogOnce.Do(func() {
		catalogContent, catalogErr = resourceFiles.ReadFile("resources/recommendation-catalog.json")
		if catalogErr != nil {
			return
		}
		_, err := artifact.CanonicalizeJSON(catalogContent)
		if err != nil || artifact.DigestBytes(catalogContent).String() != RecommendationDigest {
			catalogErr = errors.New("security report: recommendation catalog digest or canonical bytes drifted")
			return
		}
		if err := json.Unmarshal(catalogContent, &catalogValue); err != nil {
			catalogErr = err
			return
		}
		if err := validatePhaseFourSchema(RecommendationSchema, catalogContent); err != nil {
			catalogErr = err
			return
		}
		if catalogValue.Schema != RecommendationSchema || catalogValue.Version != RecommendationVersion || catalogValue.ReadContract != targetposture.ReadContract || len(catalogValue.Entries) != 6 {
			catalogErr = errors.New("security report: recommendation catalog identity is invalid")
			return
		}
		seen := make(map[string]bool)
		for _, entry := range catalogValue.Entries {
			if entry.ID == "" || seen[entry.ID] || !sort.StringsAreSorted(entry.StandardIDs) || !sort.StringsAreSorted(entry.ActionClasses) {
				catalogErr = errors.New("security report: recommendation catalog entries are invalid")
				return
			}
			seen[entry.ID] = true
		}
	})
	return catalogValue, append([]byte(nil), catalogContent...), catalogErr
}

func mapRecommendations(issues []Issue, posture *targetposture.Posture) ([]Issue, []Recommendation, error) {
	catalog, _, err := loadCatalog()
	if err != nil {
		return nil, nil, err
	}
	normalizedIssues := append([]Issue(nil), issues...)
	recommendations := make([]Recommendation, 0)
	for issueIndex := range normalizedIssues {
		issue := &normalizedIssues[issueIndex]
		matched := false
		available := false
		for _, entry := range catalog.Entries {
			if !entryMatches(entry, *issue) {
				continue
			}
			matched = true
			recommendation := recommendationFor(entry, *issue, posture)
			if recommendation.Status != "unavailable" {
				available = true
			}
			canonical, err := artifact.CanonicalJSON(recommendation)
			if err != nil {
				return nil, nil, err
			}
			recommendation.RecommendationID = "recommendation:" + stringsTrimDigest(artifact.DigestBytes(canonical).String())
			recommendations = append(recommendations, recommendation)
		}
		switch {
		case available:
			issue.RecommendationMapping = "available"
		case matched:
			issue.RecommendationMapping = "unavailable"
		default:
			issue.RecommendationMapping = "not_applicable"
		}
	}
	sort.Slice(recommendations, func(left, right int) bool {
		return recommendations[left].RecommendationID < recommendations[right].RecommendationID
	})
	return normalizedIssues, recommendations, nil
}

func entryMatches(entry catalogEntry, issue Issue) bool {
	standardMatch := false
	for _, standard := range issue.Standards {
		if containsString(entry.StandardIDs, standard.ID) {
			standardMatch = true
			break
		}
	}
	if !standardMatch {
		return false
	}
	if entry.ID == "observation-gap" {
		return len(issue.CoverageGapIDs) > 0
	}
	return issue.Action == nil || containsString(entry.ActionClasses, issue.Action.Class)
}

func recommendationFor(entry catalogEntry, issue Issue, posture *targetposture.Posture) Recommendation {
	recommendation := Recommendation{
		IssueID: issue.IssueID, CatalogEntryID: entry.ID,
		Catalog: VersionIdentity{Version: RecommendationVersion, Digest: RecommendationDigest},
		Kind:    entry.Kind, Target: RecommendationTarget{AgentID: posture.Agent.ID},
		CurrentControlIDs: currentControls(entry.Kind, posture), IntendedConstraint: entry.Constraint,
		ExpectedProtectedBehavior: expectedBehavior(entry.Constraint),
		FutureVerification: FutureVerification{
			ExpectedBehavior:           expectedBehavior(entry.Constraint),
			AuthoritativeEvidenceRoles: []string{"semantic_behavior", "external_effect"},
			SuccessCriteria:            "A separately authorized rerun records the intended protected behavior in its authoritative evidence.",
			RefusalCriteria:            "Refuse an effectiveness claim when evidence is missing, contradictory, truncated, or cannot be attributed to the target action.",
		},
		Limitations: append([]string(nil), entry.Limitations...),
	}
	if issue.Action != nil {
		recommendation.Target.ActionClass = issue.Action.Class
		recommendation.Target.ActionName = issue.Action.Name
	}
	seam := seamFor(entry.Kind, posture)
	switch {
	case entry.ID != "observation-gap" && issue.Action == nil:
		recommendation.Status = "unavailable"
		recommendation.Limitations = append(recommendation.Limitations, "The cited backend evidence does not resolve a structured action target.")
	case seam.Status != "observed" || seam.Permission != entry.Permission || seam.Route != entry.Route:
		recommendation.Status = "unavailable"
		recommendation.Limitations = append(recommendation.Limitations, "The checked-in catalog seam was not observed exactly in the sealed target posture.")
	case len(recommendation.CurrentControlIDs) > 0:
		recommendation.Status = "review_existing"
	default:
		recommendation.Status = "new_gap"
	}
	return recommendation
}

func seamFor(kind string, posture *targetposture.Posture) targetposture.Seam {
	switch kind {
	case "guardrail":
		return posture.Seams.Guardrail
	case "policy":
		return posture.Seams.Policy
	case "behavior_rule":
		return posture.Seams.BehaviorRule
	case "approval_requirement":
		return posture.Seams.ApprovalRequirement
	case "sdk_integration":
		return posture.Seams.SDKIntegration
	default:
		return targetposture.Seam{Status: "unavailable"}
	}
}

func currentControls(kind string, posture *targetposture.Posture) []string {
	// The public report schema requires an array even when the target has no
	// matching current control. Keep the empty value non-nil so canonical JSON
	// emits [] rather than null on the common new-gap path.
	ids := make([]string, 0)
	switch kind {
	case "guardrail":
		for _, control := range posture.Guardrails {
			if control.Active {
				ids = append(ids, control.ID)
			}
		}
	case "policy":
		for _, control := range posture.Policies {
			if control.Active {
				ids = append(ids, control.ID)
			}
		}
	case "behavior_rule":
		for _, control := range posture.BehaviorRules {
			if control.Active {
				ids = append(ids, control.ID)
			}
		}
	case "approval_requirement":
		for _, control := range posture.BehaviorRules {
			if control.Active && (control.Verdict == "REQUIRE_APPROVAL" || control.Verdict == "require_approval") {
				ids = append(ids, control.ID)
			}
		}
	}
	sort.Strings(ids)
	return ids
}

func expectedBehavior(constraint string) string {
	switch constraint {
	case "neutralize_untrusted_prompt_input":
		return "Untrusted prompt content does not redirect the agent into the cited action."
	case "validate_model_output_before_effect":
		return "Model output is validated before it can cause the cited external action."
	case "constrain_effect_sequence":
		return "The cited action occurs only after the required safe predecessor state."
	case "limit_action_authority":
		return "The target agent has only the authority required for the cited action."
	case "require_human_authorization":
		return "The cited effect waits for a separately governed human authorization."
	default:
		return "The missing semantic authority is recorded explicitly before a security conclusion is made."
	}
}

func containsString(values []string, value string) bool {
	index := sort.SearchStrings(values, value)
	return index < len(values) && values[index] == value
}

func stringsTrimDigest(value string) string {
	if len(value) > len("sha256:") {
		return value[len("sha256:"):]
	}
	return value
}
