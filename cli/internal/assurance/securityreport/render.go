package securityreport

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html"
	"sort"
	"strings"
	"unicode"

	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/artifact"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/targetposture"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/securityskill"
)

func buildProjections(prepared *Prepared, posture *targetposture.Posture) (Projections, []byte, error) {
	var projections Projections
	if prepared == nil || posture == nil {
		return projections, nil, fmt.Errorf("security report: incomplete projection input")
	}
	postureBytes, err := artifact.CanonicalJSON(posture)
	if err != nil {
		return projections, nil, err
	}
	if err := validatePhaseFourSchema(targetposture.Schema, postureBytes); err != nil {
		return projections, nil, err
	}
	issues, recommendations, err := mapRecommendations(prepared.Issues, posture)
	if err != nil {
		return projections, nil, err
	}
	if prepared.Candidate.Result != "issues" {
		issues = []Issue{}
		recommendations = []Recommendation{}
	}
	report := Report{
		Schema:                ReportSchema,
		Observation:           SchemaIdentity{Schema: observationSchema, Digest: prepared.PackDigest},
		Analysis:              SchemaIdentity{Schema: securityskill.CandidateSchema, Digest: artifact.DigestBytes(prepared.CandidateBytes).String()},
		Standards:             VersionIdentity{Version: securityskill.CatalogVersion, Digest: artifact.DigestBytes(prepared.StandardsBytes).String()},
		RecommendationCatalog: VersionIdentity{Version: RecommendationVersion, Digest: RecommendationDigest},
		TargetPosture:         SchemaIdentity{Schema: targetposture.Schema, Digest: artifact.DigestBytes(postureBytes).String()},
		Result:                prepared.Candidate.Result, SecurityPass: false,
		CoverageGapIDs: append([]string(nil), prepared.Candidate.CoverageGapIDs...),
		Issues:         issues, Recommendations: recommendations,
		Limitations: reportLimitations(prepared.Candidate.Result),
	}
	jsonBytes, err := artifact.CanonicalJSON(report)
	if err != nil {
		return projections, nil, err
	}
	if err := validatePhaseFourSchema(ReportSchema, jsonBytes); err != nil {
		return projections, nil, err
	}
	markdown := renderMarkdown(report)
	sarif, err := renderSARIF(report)
	if err != nil {
		return projections, nil, err
	}
	projections = Projections{Report: report, JSON: jsonBytes, Markdown: markdown, SARIF: sarif}
	return projections, postureBytes, nil
}

const observationSchema = "ai.openbox.project-observation/v1"

func reportLimitations(result string) []string {
	limitations := []string{
		"This report is advisory and untrusted as an enforcement decision; it applies no OpenBox control or approval.",
		"The target posture was captured after the observation and can differ from controls present during the evaluated run.",
		"Observed GET seams and current control identities do not prove semantic coverage, application, or effectiveness.",
		"The local dashboard read contract exposes no backend build identity, so the catalog is bound to its named read contract and digest.",
	}
	switch result {
	case "no_supported_issue":
		limitations = append(limitations, "No supported issue in this bounded observation is not a security pass or customer security assessment.")
	case "inconclusive":
		limitations = append(limitations, "The result is inconclusive because relevant evidence authority is missing, contradictory, or truncated.")
	default:
		limitations = append(limitations, "Candidate prose is preserved as an analyzer assertion; only cited retained records establish observed facts.")
	}
	return limitations
}

func renderMarkdown(report Report) []byte {
	var output strings.Builder
	output.WriteString("# OpenBox project security report\n\n")
	fmt.Fprintf(&output, "Result: `%s`  \nSecurity pass: `false`\n\n", report.Result)
	output.WriteString("This is a sealed advisory report. It does not apply controls, decide approvals, or establish enforcement.\n\n")
	output.WriteString("## Authority chain\n\n")
	output.WriteString("Observed behavior -> validated issue -> standard -> evidence-derived OpenBox target/action -> observed read seam/current posture -> inert recommendation or unavailable mapping -> expected protected behavior -> future verification criteria.\n\n")
	if len(report.CoverageGapIDs) > 0 {
		output.WriteString("## Coverage gaps\n\n")
		for _, gap := range report.CoverageGapIDs {
			fmt.Fprintf(&output, "- `%s`\n", escapeMarkdown(gap))
		}
		output.WriteByte('\n')
	}
	output.WriteString("## Issues\n\n")
	if len(report.Issues) == 0 {
		if report.Result == "inconclusive" {
			output.WriteString("No issue conclusion was published because the bounded evidence is inconclusive. This is not a pass.\n\n")
		} else {
			output.WriteString("The bounded observation supports no catalog issue. This is not a security pass.\n\n")
		}
	}
	for _, issue := range report.Issues {
		fmt.Fprintf(&output, "### %s\n\n", escapeMarkdown(issue.Title))
		fmt.Fprintf(&output, "- Issue ID: `%s`\n", issue.IssueID)
		fmt.Fprintf(&output, "- Candidate provenance: `%s`\n", escapeMarkdown(issue.CandidateID))
		fmt.Fprintf(&output, "- Confidence: `%s`; severity: `unavailable`; inference: `%t`\n", issue.Confidence, issue.Inference)
		fmt.Fprintf(&output, "- Recommendation mapping: `%s`\n", issue.RecommendationMapping)
		if issue.Action != nil {
			fmt.Fprintf(&output, "- Evidence-derived action: `%s` / `%s` from `%s`\n", escapeMarkdown(issue.Action.Class), escapeMarkdown(issue.Action.Name), escapeMarkdown(issue.Action.SourceEvidenceID))
		}
		output.WriteString("\nAnalyzer assertions:\n\n")
		fmt.Fprintf(&output, "> Observed behavior assertion: %s\n>\n> Crossed-boundary assertion: %s\n>\n> Rationale assertion: %s\n\n", escapeMarkdown(issue.ObservedBehaviorAssertion), escapeMarkdown(issue.CrossedBoundaryAssertion), escapeMarkdown(issue.RationaleAssertion))
		output.WriteString("Observed facts:\n\n")
		for _, fact := range issue.ObservedFacts {
			fmt.Fprintf(&output, "- `%s` — authority `%s`, type `%s`", escapeMarkdown(fact.EvidenceID), fact.Authority, escapeMarkdown(fact.Type))
			if fact.Timestamp != "" {
				fmt.Fprintf(&output, ", timestamp `%s`", escapeMarkdown(fact.Timestamp))
			}
			output.WriteByte('\n')
		}
		output.WriteString("\nStandards:\n\n")
		for _, standard := range issue.Standards {
			fmt.Fprintf(&output, "- `%s/%s/%s`\n", standard.Catalog, escapeMarkdown(standard.Version), standard.ID)
		}
		output.WriteByte('\n')
	}
	output.WriteString("## Inert recommendations\n\n")
	if len(report.Recommendations) == 0 {
		output.WriteString("No recommendation was generated.\n\n")
	}
	for _, recommendation := range report.Recommendations {
		fmt.Fprintf(&output, "### `%s`\n\n", recommendation.RecommendationID)
		fmt.Fprintf(&output, "- Issue: `%s`\n- Kind: `%s`\n- Mapping: `%s`\n- Catalog entry: `%s`\n", recommendation.IssueID, recommendation.Kind, recommendation.Status, recommendation.CatalogEntryID)
		fmt.Fprintf(&output, "- Target agent: `%s`\n", escapeMarkdown(recommendation.Target.AgentID))
		if recommendation.Target.ActionClass != "" {
			fmt.Fprintf(&output, "- Target action: `%s` / `%s`\n", escapeMarkdown(recommendation.Target.ActionClass), escapeMarkdown(recommendation.Target.ActionName))
		}
		fmt.Fprintf(&output, "- Intended constraint: `%s`\n- Expected protected behavior: %s\n", recommendation.IntendedConstraint, escapeMarkdown(recommendation.ExpectedProtectedBehavior))
		if len(recommendation.CurrentControlIDs) > 0 {
			output.WriteString("- Current control identities for review: ")
			for index, id := range recommendation.CurrentControlIDs {
				if index > 0 {
					output.WriteString(", ")
				}
				fmt.Fprintf(&output, "`%s`", escapeMarkdown(id))
			}
			output.WriteByte('\n')
		}
		fmt.Fprintf(&output, "- Future success criterion: %s\n- Future refusal criterion: %s\n\n", escapeMarkdown(recommendation.FutureVerification.SuccessCriteria), escapeMarkdown(recommendation.FutureVerification.RefusalCriteria))
	}
	output.WriteString("## Limitations\n\n")
	for _, limitation := range report.Limitations {
		fmt.Fprintf(&output, "- %s\n", escapeMarkdown(limitation))
	}
	return []byte(output.String())
}

func renderSARIF(report Report) ([]byte, error) {
	type message struct {
		Text string `json:"text"`
	}
	type rule struct {
		ID               string  `json:"id"`
		Name             string  `json:"name"`
		ShortDescription message `json:"shortDescription"`
		HelpURI          string  `json:"helpUri,omitempty"`
		Properties       any     `json:"properties"`
	}
	type result struct {
		RuleID              string            `json:"ruleId"`
		Level               string            `json:"level"`
		Message             message           `json:"message"`
		PartialFingerprints map[string]string `json:"partialFingerprints"`
		Properties          any               `json:"properties"`
	}
	rules := make([]rule, 0, len(report.Issues))
	results := make([]result, 0, len(report.Issues))
	for _, issue := range report.Issues {
		standards := make([]string, 0, len(issue.Standards))
		for _, reference := range issue.Standards {
			standards = append(standards, reference.Catalog+"/"+reference.Version+"/"+reference.ID)
		}
		rules = append(rules, rule{ID: issue.IssueID, Name: sarifName(issue.CandidateID), ShortDescription: message{Text: issue.Title}, Properties: map[string]any{"standards": standards, "severity": "unavailable"}})
		results = append(results, result{RuleID: issue.IssueID, Level: "none", Message: message{Text: issue.Title}, PartialFingerprints: map[string]string{"openboxIssue/v1": issue.IssueID}, Properties: map[string]any{"candidateId": issue.CandidateID, "confidence": issue.Confidence, "inference": issue.Inference, "recommendationMapping": issue.RecommendationMapping, "severity": "unavailable"}})
	}
	document := map[string]any{
		"$schema": "https://json.schemastore.org/sarif-2.1.0.json",
		"version": "2.1.0",
		"runs": []any{map[string]any{
			"tool":       map[string]any{"driver": map[string]any{"name": "OpenBox Project Assurance", "informationUri": "https://openbox.ai", "rules": rules, "semanticVersion": "1.0.0"}},
			"results":    results,
			"properties": map[string]any{"result": report.Result, "securityPass": false, "observationPackDigest": report.Observation.Digest},
		}},
	}
	return artifact.CanonicalJSON(document)
}

func escapeMarkdown(value string) string {
	value = strings.Map(func(character rune) rune {
		if unicode.IsControl(character) && character != '\n' && character != '\t' {
			return -1
		}
		return character
	}, value)
	value = html.EscapeString(value)
	var output strings.Builder
	for _, character := range value {
		if strings.ContainsRune(`\`+"`*_{}[]()#+-.!|>", character) {
			output.WriteByte('\\')
		}
		output.WriteRune(character)
	}
	return output.String()
}

func sarifName(candidateID string) string {
	name := strings.TrimPrefix(candidateID, "candidate:")
	name = strings.Map(func(character rune) rune {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || (character >= '0' && character <= '9') || character == '_' || character == '-' || character == '.' {
			return character
		}
		return '-'
	}, name)
	if name == "" {
		return "openbox-security-issue"
	}
	return name
}

func equalProjections(left, right Projections) bool {
	return bytes.Equal(left.JSON, right.JSON) && bytes.Equal(left.Markdown, right.Markdown) && bytes.Equal(left.SARIF, right.SARIF)
}

func sortedStrings(values []string) []string {
	result := append([]string(nil), values...)
	sort.Strings(result)
	return result
}

func decodeReport(content []byte) (Report, error) {
	var report Report
	if err := json.Unmarshal(content, &report); err != nil {
		return report, err
	}
	return report, nil
}
