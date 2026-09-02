// Package report renders immutable projections and inert proposals from
// schema-validated audit packs. It has no project, process, network, or
// control-plane authority.
package report

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/artifact"
	assuranceevidence "github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/evidence"
)

const (
	reportKind                = "OpenBoxProjectAssuranceReport"
	severityUnavailable       = "unavailable"
	sarifVersion              = "2.1.0"
	sarifSchema               = "https://docs.oasis-open.org/sarif/sarif/v2.1.0/errata01/os/schemas/sarif-schema-2.1.0.json"
	sarifDriverName           = "openbox-project-assurance"
	maxProjectionBytes        = 16 << 20
	maxSigned53         int64 = 9007199254740991
)

var reportIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,255}$`)

// Judgment is the exact public v1 judgment projection. Severity is absent from
// the authority and is therefore emitted only as the literal "unavailable".
type Judgment struct {
	FindingID      string                   `json:"findingId"`
	ScenarioID     string                   `json:"scenarioId"`
	Outcome        string                   `json:"outcome"`
	Reachability   string                   `json:"reachability"`
	EvidenceLevel  string                   `json:"evidenceLevel"`
	MatchedFacts   []string                 `json:"matchedFacts"`
	Evidence       []artifact.ContentDigest `json:"evidence"`
	MissingFacts   []string                 `json:"missingFacts"`
	Contradictions []string                 `json:"contradictions"`
	Limitations    []string                 `json:"limitations"`
}

type Omission struct {
	Subject        string                   `json:"subject"`
	Reason         string                   `json:"reason"`
	EvidenceImpact string                   `json:"evidenceImpact"`
	Count          int64                    `json:"count"`
	Evidence       []artifact.ContentDigest `json:"evidence"`
}

type Limits struct {
	Truncated bool       `json:"truncated"`
	Omissions []Omission `json:"omissions"`
}

// Input is the producer-side mirror of the retained manifest fields. Render
// consumes only ValidatedPack; Build exists so those addressed projection
// objects can be created before the manifest-last assembly step.
type Input struct {
	RunID         string
	Mode          string
	RunnerVersion string
	Judgments     []Judgment
	Limits        Limits
}

// Projections contains the three addressed report objects plus the plain-text
// console rendering. All accessors return defensive copies.
type Projections struct {
	jsonObject     artifact.Object
	markdownObject artifact.Object
	sarifObject    artifact.Object
	console        []byte
}

func (projections *Projections) JSON() []byte {
	if projections == nil {
		return nil
	}
	return projections.jsonObject.Bytes()
}

func (projections *Projections) Markdown() []byte {
	if projections == nil {
		return nil
	}
	return projections.markdownObject.Bytes()
}

func (projections *Projections) SARIF() []byte {
	if projections == nil {
		return nil
	}
	return projections.sarifObject.Bytes()
}

func (projections *Projections) Console() []byte {
	if projections == nil {
		return nil
	}
	return slices.Clone(projections.console)
}

func (projections *Projections) JSONObject() artifact.Object     { return projections.jsonObject }
func (projections *Projections) MarkdownObject() artifact.Object { return projections.markdownObject }
func (projections *Projections) SARIFObject() artifact.Object    { return projections.sarifObject }

// Build deterministically creates all four projections from one typed model.
func Build(input Input) (*Projections, error) {
	if err := validateInput(input); err != nil {
		return nil, err
	}
	if projectionInputSize(input) > maxProjectionBytes {
		return nil, fmt.Errorf("report: projection input exceeds %d bytes", maxProjectionBytes)
	}
	input = cloneInput(input)

	jsonValue := jsonReport{
		Kind: reportKind, RunID: input.RunID, Mode: input.Mode, RunnerVersion: input.RunnerVersion,
		Severity: severityUnavailable, Findings: make([]jsonFinding, len(input.Judgments)), Limits: input.Limits,
	}
	for index, judgment := range input.Judgments {
		jsonValue.Findings[index] = jsonFinding{Judgment: judgment, Severity: severityUnavailable}
	}
	jsonBytes, err := artifact.CanonicalJSON(jsonValue)
	if err != nil {
		return nil, fmt.Errorf("report: canonical JSON projection: %w", err)
	}
	markdown, err := renderMarkdown(jsonValue)
	if err != nil {
		return nil, err
	}
	console, err := renderConsole(jsonValue)
	if err != nil {
		return nil, err
	}
	sarifBytes, err := renderSARIF(jsonValue)
	if err != nil {
		return nil, err
	}
	for name, content := range map[string][]byte{"json": jsonBytes, "markdown": markdown, "console": console, "sarif": sarifBytes} {
		if len(content) > maxProjectionBytes {
			return nil, fmt.Errorf("report: %s projection exceeds %d bytes", name, maxProjectionBytes)
		}
	}
	jsonObject, err := artifact.NewExactObject(artifact.RoleReportJSON, "application/json", nil, "public_projection", jsonBytes)
	if err != nil {
		return nil, err
	}
	markdownObject, err := artifact.NewExactObject(artifact.RoleReportMarkdown, "text/markdown", nil, "public_projection", markdown)
	if err != nil {
		return nil, err
	}
	sarifObject, err := artifact.NewExactObject(artifact.RoleReportSARIF, "application/sarif+json", nil, "public_projection", sarifBytes)
	if err != nil {
		return nil, err
	}
	return &Projections{jsonObject: jsonObject, markdownObject: markdownObject, sarifObject: sarifObject, console: console}, nil
}

// Render recomputes every projection from authoritative validated manifest
// fields and requires byte equality with the three addressed report roles.
func Render(pack *ValidatedPack) (*Projections, error) {
	if pack == nil || pack.pack == nil {
		return nil, errors.New("report: nil schema-validated pack")
	}
	projections, err := Build(pack.input)
	if err != nil {
		return nil, err
	}
	checks := []struct {
		role artifact.Role
		want []byte
	}{
		{artifact.RoleReportJSON, projections.JSON()},
		{artifact.RoleReportMarkdown, projections.Markdown()},
		{artifact.RoleReportSARIF, projections.SARIF()},
	}
	for _, check := range checks {
		stored, ok := pack.pack.Object(check.role)
		if !ok || !bytes.Equal(stored, check.want) {
			return nil, fmt.Errorf("report: addressed %q is not the authoritative projection", check.role)
		}
	}
	return projections, nil
}

type jsonFinding struct {
	Judgment
	Severity string `json:"severity"`
}

type jsonReport struct {
	Kind          string        `json:"kind"`
	RunID         string        `json:"runId"`
	Mode          string        `json:"mode"`
	RunnerVersion string        `json:"runnerVersion"`
	Severity      string        `json:"severity"`
	Findings      []jsonFinding `json:"findings"`
	Limits        Limits        `json:"limits"`
}

func renderMarkdown(report jsonReport) ([]byte, error) {
	var output strings.Builder
	output.WriteString("# OpenBox Project Assurance Report\n\n")
	writeMarkdownField(&output, "Run ID", report.RunID)
	writeMarkdownField(&output, "Mode", report.Mode)
	writeMarkdownField(&output, "Runner version", report.RunnerVersion)
	writeMarkdownField(&output, "Severity", report.Severity)
	writeMarkdownJSON(&output, "Truncated", report.Limits.Truncated)
	for _, finding := range report.Findings {
		output.WriteString("\n## Finding ")
		output.WriteString(finding.FindingID)
		output.WriteString("\n\n")
		writeMarkdownField(&output, "Scenario", finding.ScenarioID)
		writeMarkdownField(&output, "Outcome", finding.Outcome)
		writeMarkdownField(&output, "Severity", finding.Severity)
		writeMarkdownField(&output, "Evidence level", finding.EvidenceLevel)
		writeMarkdownField(&output, "Reachability", finding.Reachability)
		writeMarkdownJSON(&output, "Matched facts", finding.MatchedFacts)
		writeMarkdownJSON(&output, "Evidence", finding.Evidence)
		writeMarkdownJSON(&output, "Missing facts", finding.MissingFacts)
		writeMarkdownJSON(&output, "Contradictions", finding.Contradictions)
		writeMarkdownJSON(&output, "Limitations", finding.Limitations)
	}
	output.WriteString("\n## Omissions\n")
	if len(report.Limits.Omissions) == 0 {
		output.WriteString("\n    []\n")
	} else {
		for _, omission := range report.Limits.Omissions {
			writeMarkdownJSON(&output, "Omission", omission)
		}
	}
	return []byte(output.String()), nil
}

func writeMarkdownField(output *strings.Builder, label, value string) {
	encoded := safeTextJSON(value)
	output.WriteString("- ")
	output.WriteString(label)
	output.WriteString(":\n\n    ")
	output.Write(encoded)
	output.WriteByte('\n')
}

func writeMarkdownJSON(output *strings.Builder, label string, value any) {
	encoded := safeTextJSON(value)
	output.WriteString("- ")
	output.WriteString(label)
	output.WriteString(":\n\n    ")
	output.Write(encoded)
	output.WriteByte('\n')
}

func renderConsole(report jsonReport) ([]byte, error) {
	var output strings.Builder
	writeConsoleJSON(&output, "run_id", report.RunID)
	writeConsoleJSON(&output, "mode", report.Mode)
	writeConsoleJSON(&output, "runner_version", report.RunnerVersion)
	writeConsoleJSON(&output, "severity", report.Severity)
	writeConsoleJSON(&output, "truncated", report.Limits.Truncated)
	for index, finding := range report.Findings {
		prefix := fmt.Sprintf("finding[%d].", index)
		writeConsoleJSON(&output, prefix+"finding_id", finding.FindingID)
		writeConsoleJSON(&output, prefix+"scenario_id", finding.ScenarioID)
		writeConsoleJSON(&output, prefix+"outcome", finding.Outcome)
		writeConsoleJSON(&output, prefix+"severity", finding.Severity)
		writeConsoleJSON(&output, prefix+"evidence_level", finding.EvidenceLevel)
		writeConsoleJSON(&output, prefix+"reachability", finding.Reachability)
		writeConsoleJSON(&output, prefix+"matched_facts", finding.MatchedFacts)
		writeConsoleJSON(&output, prefix+"evidence", finding.Evidence)
		writeConsoleJSON(&output, prefix+"missing_facts", finding.MissingFacts)
		writeConsoleJSON(&output, prefix+"contradictions", finding.Contradictions)
		writeConsoleJSON(&output, prefix+"limitations", finding.Limitations)
	}
	writeConsoleJSON(&output, "omissions", report.Limits.Omissions)
	return []byte(output.String()), nil
}

func writeConsoleJSON(output *strings.Builder, name string, value any) {
	encoded := safeTextJSON(value)
	output.WriteString(name)
	output.WriteByte('=')
	output.Write(encoded)
	output.WriteByte('\n')
}

func safeTextJSON(value any) []byte {
	encoded, _ := artifact.CanonicalJSON(value)
	encoded = bytes.ReplaceAll(encoded, []byte("<"), []byte(`\u003c`))
	encoded = bytes.ReplaceAll(encoded, []byte(">"), []byte(`\u003e`))
	return bytes.ReplaceAll(encoded, []byte("&"), []byte(`\u0026`))
}

type sarifLog struct {
	Schema  string     `json:"$schema"`
	Version string     `json:"version"`
	Runs    []sarifRun `json:"runs"`
}

type sarifRun struct {
	Tool       sarifTool          `json:"tool"`
	Results    []sarifResult      `json:"results"`
	Properties sarifRunProperties `json:"properties"`
}

type sarifTool struct {
	Driver sarifDriverComponent `json:"driver"`
}
type sarifDriverComponent struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}
type sarifMessage struct {
	Text string `json:"text"`
}

type sarifRunProperties struct {
	RunID     string     `json:"openboxRunId"`
	Mode      string     `json:"openboxMode"`
	Severity  string     `json:"openboxSeverity"`
	Truncated bool       `json:"openboxTruncated"`
	Omissions []Omission `json:"openboxOmissions"`
}

type sarifResult struct {
	RuleID     string                `json:"ruleId"`
	Level      string                `json:"level"`
	Message    sarifMessage          `json:"message"`
	Properties sarifResultProperties `json:"properties"`
}

type sarifResultProperties struct {
	ScenarioID     string                   `json:"openboxScenarioId"`
	Outcome        string                   `json:"openboxOutcome"`
	Severity       string                   `json:"openboxSeverity"`
	EvidenceLevel  string                   `json:"openboxEvidenceLevel"`
	Reachability   string                   `json:"openboxReachability"`
	MatchedFacts   []string                 `json:"openboxMatchedFacts"`
	Evidence       []artifact.ContentDigest `json:"openboxEvidence"`
	MissingFacts   []string                 `json:"openboxMissingFacts"`
	Contradictions []string                 `json:"openboxContradictions"`
	Limitations    []string                 `json:"openboxLimitations"`
}

func renderSARIF(report jsonReport) ([]byte, error) {
	run := sarifRun{
		Tool:       sarifTool{Driver: sarifDriverComponent{Name: sarifDriverName, Version: report.RunnerVersion}},
		Results:    make([]sarifResult, len(report.Findings)),
		Properties: sarifRunProperties{RunID: report.RunID, Mode: report.Mode, Severity: severityUnavailable, Truncated: report.Limits.Truncated, Omissions: cloneOmissions(report.Limits.Omissions)},
	}
	for index, finding := range report.Findings {
		run.Results[index] = sarifResult{
			RuleID: finding.FindingID, Level: "none",
			Message: sarifMessage{Text: "OpenBox project assurance outcome: " + finding.Outcome},
			Properties: sarifResultProperties{
				ScenarioID: finding.ScenarioID, Outcome: finding.Outcome, Severity: severityUnavailable,
				EvidenceLevel: finding.EvidenceLevel, Reachability: finding.Reachability,
				MatchedFacts: slices.Clone(finding.MatchedFacts), Evidence: slices.Clone(finding.Evidence),
				MissingFacts: slices.Clone(finding.MissingFacts), Contradictions: slices.Clone(finding.Contradictions),
				Limitations: slices.Clone(finding.Limitations),
			},
		}
	}
	content, err := artifact.CanonicalJSON(sarifLog{Schema: sarifSchema, Version: sarifVersion, Runs: []sarifRun{run}})
	if err != nil {
		return nil, fmt.Errorf("report: canonical SARIF projection: %w", err)
	}
	if err := validateSARIF(content, len(report.Findings)); err != nil {
		return nil, err
	}
	return content, nil
}

func validateSARIF(content []byte, findingCount int) error {
	var report sarifLog
	if err := artifactJSON(content, &report); err != nil || report.Version != sarifVersion || report.Schema != sarifSchema || len(report.Runs) != 1 ||
		report.Runs[0].Tool.Driver.Name != sarifDriverName || len(report.Runs[0].Results) != findingCount {
		return errors.New("report: generated SARIF does not match the frozen 2.1.0 envelope")
	}
	for _, result := range report.Runs[0].Results {
		if result.RuleID == "" || result.Level != "none" || result.Message.Text == "" || result.Properties.Severity != severityUnavailable {
			return errors.New("report: generated SARIF result is invalid")
		}
	}
	return nil
}

func artifactJSON(content []byte, destination any) error {
	canonical, err := artifact.CanonicalizeJSON(content)
	if err != nil || !bytes.Equal(canonical, content) {
		return errors.New("noncanonical JSON")
	}
	return json.Unmarshal(content, destination)
}

func validateInput(input Input) error {
	if !reportIDPattern.MatchString(input.RunID) || (input.Mode != "baseline" && input.Mode != "governed") ||
		input.RunnerVersion == "" || utf8.RuneCountInString(input.RunnerVersion) > 128 || input.Judgments == nil || input.Limits.Omissions == nil ||
		len(input.Judgments) != 1 || len(input.Limits.Omissions) > 10000 {
		return errors.New("report: projection input envelope is invalid")
	}
	if input.Judgments[0].FindingID != findingID || input.Judgments[0].ScenarioID != scenarioID {
		return errors.New("report: v1 judgment does not match the exact executable scenario")
	}
	seen := make(map[string]struct{}, len(input.Judgments))
	for _, judgment := range input.Judgments {
		key := judgment.FindingID + "\x00" + judgment.ScenarioID
		if _, duplicate := seen[key]; duplicate {
			return errors.New("report: duplicate finding/scenario judgment")
		}
		seen[key] = struct{}{}
		if err := validateJudgment(input.Mode, judgment); err != nil {
			return err
		}
	}
	truncation := false
	for _, omission := range input.Limits.Omissions {
		if err := validateOmission(omission); err != nil {
			return err
		}
		if omission.Reason == "truncated" {
			truncation = true
		}
	}
	if truncation != input.Limits.Truncated {
		return errors.New("report: truncation omission and flag disagree")
	}
	return nil
}

func projectionInputSize(input Input) int {
	size := len(input.RunID) + len(input.Mode) + len(input.RunnerVersion)
	addStringList := func(values []string) {
		for _, value := range values {
			size += len(value) + 4
		}
	}
	for _, judgment := range input.Judgments {
		size += len(judgment.FindingID) + len(judgment.ScenarioID) + len(judgment.Outcome) + len(judgment.Reachability) + len(judgment.EvidenceLevel) + 256
		addStringList(judgment.MatchedFacts)
		addStringList(judgment.MissingFacts)
		addStringList(judgment.Contradictions)
		addStringList(judgment.Limitations)
		size += len(judgment.Evidence) * 72
		if size > maxProjectionBytes {
			return size
		}
	}
	for _, omission := range input.Limits.Omissions {
		size += len(omission.Subject) + len(omission.Reason) + len(omission.EvidenceImpact) + len(omission.Evidence)*72 + 128
		if size > maxProjectionBytes {
			return size
		}
	}
	return size
}

func validateJudgment(mode string, judgment Judgment) error {
	if !reportIDPattern.MatchString(judgment.FindingID) || !reportIDPattern.MatchString(judgment.ScenarioID) ||
		!oneOf(judgment.Outcome, "exploitable", "blocked", "sandbox_prevented", "not_observed", "inconclusive", "not_runnable") ||
		!oneOf(judgment.Reachability, "runtime_enforceable", "host_enforceable", "observable_only", "code_change_required", "blind") ||
		!oneOf(judgment.EvidenceLevel, "documented", "discovered", "callable", "observed", "repeated", "release_qualified") ||
		judgment.MatchedFacts == nil || judgment.Evidence == nil || judgment.MissingFacts == nil || judgment.Contradictions == nil || judgment.Limitations == nil ||
		len(judgment.MatchedFacts) > 64 || len(judgment.Evidence) < 1 || len(judgment.Evidence) > 256 {
		return errors.New("report: judgment identity or classification is invalid")
	}
	matchedFacts, err := parseFacts(judgment.MatchedFacts)
	if err != nil {
		return err
	}
	missingFacts, err := parseFacts(judgment.MissingFacts)
	if err != nil {
		return err
	}
	if judgment.Outcome == "blocked" && mode == "baseline" {
		return errors.New("report: baseline cannot contain a blocked judgment")
	}
	uniqueEvidence := make(map[artifact.ContentDigest]struct{}, len(judgment.Evidence))
	for _, digest := range judgment.Evidence {
		if _, duplicate := uniqueEvidence[digest]; duplicate {
			return errors.New("report: judgment repeats an evidence digest")
		}
		uniqueEvidence[digest] = struct{}{}
	}
	if err := assuranceevidence.ValidateRetainedJudgment(
		assuranceevidence.Outcome(judgment.Outcome), matchedFacts, missingFacts,
		judgment.Contradictions, judgment.Limitations, len(uniqueEvidence),
	); err != nil {
		return fmt.Errorf("report: retained judgment predicate: %w", err)
	}
	return validateStringLists(judgment.Contradictions, judgment.Limitations)
}

func parseFacts(values []string) ([]assuranceevidence.Fact, error) {
	allowed := map[assuranceevidence.Fact]struct{}{
		assuranceevidence.FactSDKAttemptBeforeEffect: {}, assuranceevidence.FactSDKAttemptNotObserved: {},
		assuranceevidence.FactPoisonMarkerProvenance: {}, assuranceevidence.FactSafeSinkReceipt: {},
		assuranceevidence.FactSandboxDenial: {}, assuranceevidence.FactOpenBoxDecisionBlock: {},
		assuranceevidence.FactSDKAppliedBlock: {}, assuranceevidence.FactSafeSinkNotInvoked: {},
		assuranceevidence.FactCompleteObservation: {},
	}
	result := make([]assuranceevidence.Fact, len(values))
	seen := make(map[assuranceevidence.Fact]struct{}, len(values))
	for index, value := range values {
		fact := assuranceevidence.Fact(value)
		if _, known := allowed[fact]; !known {
			return nil, errors.New("report: judgment contains an unknown fact")
		}
		if _, duplicate := seen[fact]; duplicate {
			return nil, errors.New("report: judgment contains a duplicate fact")
		}
		seen[fact] = struct{}{}
		result[index] = fact
	}
	return result, nil
}

func validateOmission(omission Omission) error {
	if omission.Subject == "" || utf8.RuneCountInString(omission.Subject) > 512 || omission.Count < 0 || omission.Count > maxSigned53 ||
		!oneOf(omission.Reason, "source_excluded", "raw_not_retained", "redaction_failed", "truncated", "unsupported", "unavailable", "budget_exceeded") ||
		!oneOf(omission.EvidenceImpact, "none", "inconclusive", "not_runnable") || len(omission.Evidence) < 1 || len(omission.Evidence) > 64 {
		return errors.New("report: omission is invalid")
	}
	if (omission.Reason == "redaction_failed" || omission.Reason == "truncated") && omission.EvidenceImpact != "inconclusive" {
		return errors.New("report: omission understates evidence impact")
	}
	return nil
}

func validateStringLists(lists ...[]string) error {
	for _, list := range lists {
		if len(list) > 10000 {
			return errors.New("report: judgment string list exceeds its bound")
		}
		for _, value := range list {
			if value == "" || !utf8.ValidString(value) || utf8.RuneCountInString(value) > 4096 {
				return errors.New("report: judgment string is invalid")
			}
		}
	}
	return nil
}

func oneOf(value string, allowed ...string) bool { return slices.Contains(allowed, value) }

func cloneInput(input Input) Input {
	input.Judgments = cloneJudgments(input.Judgments)
	input.Limits = cloneLimits(input.Limits)
	return input
}

func cloneJudgments(source []Judgment) []Judgment {
	result := slices.Clone(source)
	for index := range result {
		result[index].MatchedFacts = slices.Clone(result[index].MatchedFacts)
		result[index].Evidence = slices.Clone(result[index].Evidence)
		result[index].MissingFacts = slices.Clone(result[index].MissingFacts)
		result[index].Contradictions = slices.Clone(result[index].Contradictions)
		result[index].Limitations = slices.Clone(result[index].Limitations)
	}
	return result
}

func cloneLimits(source Limits) Limits {
	source.Omissions = cloneOmissions(source.Omissions)
	return source
}
func cloneOmissions(source []Omission) []Omission {
	result := slices.Clone(source)
	for index := range result {
		result[index].Evidence = slices.Clone(result[index].Evidence)
	}
	return result
}
