package securityreport

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/artifact"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/observation"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/runfs"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/safety"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/securityskill"
)

var lexicalAction = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

type behaviorRecord struct {
	ID        string `json:"id"`
	Type      string `json:"type"`
	Timestamp string `json:"timestamp,omitempty"`
	Authority string `json:"authority"`
	Source    struct {
		File            string `json:"file"`
		ResponseOrdinal int    `json:"response_ordinal,omitempty"`
		RecordOrdinal   int    `json:"record_ordinal,omitempty"`
		Record          string `json:"record,omitempty"`
	} `json:"source"`
}

type coverageRecord struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

// Prepare performs the complete offline gate. It reads no credential, makes no
// request, and creates no output.
func Prepare(evaluationPath, candidatePath, outputPath string) (*Prepared, error) {
	evaluation, err := filepath.Abs(evaluationPath)
	if err != nil {
		return nil, fmt.Errorf("security report: resolve observation path: %w", err)
	}
	candidateFile, err := filepath.Abs(candidatePath)
	if err != nil {
		return nil, fmt.Errorf("security report: resolve candidate path: %w", err)
	}
	output, err := resolveAbsentOutput(outputPath)
	if err != nil {
		return nil, err
	}
	pack, err := observation.Read(evaluation)
	if err != nil {
		return nil, fmt.Errorf("security report: observation verification: %w", err)
	}
	candidateBytes, err := runfs.ReadPrivateFile(candidateFile, 0o600, MaxCandidateBytes)
	if err != nil {
		return nil, fmt.Errorf("security report: candidate read: %w", err)
	}
	validated, standards, issues, err := validateCandidate(pack, candidateBytes)
	if err != nil {
		return nil, err
	}
	packDigest, _ := pack.PackDigest()
	var run struct {
		AgentID        string `json:"agent_id"`
		OrganizationID string `json:"organization_id"`
	}
	if err := json.Unmarshal(pack.Payloads["run.json"], &run); err != nil || !safeIdentity(run.AgentID) || !safeIdentity(run.OrganizationID) {
		return nil, errors.New("security report: observation target identity is invalid")
	}
	return &Prepared{
		ObservationPath: evaluation, CandidatePath: candidateFile, OutputPath: output,
		Observation: pack, Candidate: validated, CandidateBytes: candidateBytes,
		StandardsBytes: standards, PackDigest: packDigest,
		AgentID: run.AgentID, OrganizationID: run.OrganizationID, Issues: issues,
	}, nil
}

func validateCandidate(pack *observation.Pack, content []byte) (Candidate, []byte, []Issue, error) {
	var candidate Candidate
	if _, err := artifact.CanonicalizeJSON(content); err != nil {
		return candidate, nil, nil, fmt.Errorf("security report: candidate JSON: %w", err)
	}
	containsSecret, err := safety.JSONContainsCredentialMaterial(content)
	if err != nil || containsSecret {
		return candidate, nil, nil, errors.New("security report: candidate contains unsafe credential-derived material")
	}
	if err := securityskill.ValidateCandidate(content); err != nil {
		return candidate, nil, nil, fmt.Errorf("security report: candidate contract: %w", err)
	}
	var raw any
	if err := decodeExactJSON(content, &raw); err != nil {
		return candidate, nil, nil, fmt.Errorf("security report: decode candidate: %w", err)
	}
	if field := findForbiddenField(raw); field != "" {
		return candidate, nil, nil, fmt.Errorf("security report: candidate contains forbidden field %q", field)
	}
	if err := decodeExactJSON(content, &candidate); err != nil {
		return candidate, nil, nil, err
	}
	bundle, files, err := securityskill.Load()
	if err != nil {
		return candidate, nil, nil, err
	}
	if candidate.Skill.Name != bundle.Name || candidate.Skill.Version != bundle.Version || candidate.Skill.Digest != bundle.Digest {
		return candidate, nil, nil, errors.New("security report: candidate skill identity does not match the canonical bundle")
	}
	standards := files["references/standards.json"]
	standardsIndex, err := indexStandards(standards)
	if err != nil {
		return candidate, nil, nil, err
	}
	packDigest, err := pack.PackDigest()
	if err != nil || candidate.Observation.Schema != observation.Schema || candidate.Observation.PackDigest != packDigest {
		return candidate, nil, nil, errors.New("security report: candidate observation identity is forged or stale")
	}
	behavior, err := indexBehavior(pack.Payloads["behavior.json"])
	if err != nil {
		return candidate, nil, nil, err
	}
	coverage, truncated, contradictions, err := indexCoverage(pack.Payloads["coverage.json"])
	if err != nil {
		return candidate, nil, nil, err
	}
	if err := validateGapIDs(candidate.CoverageGapIDs, coverage); err != nil {
		return candidate, nil, nil, fmt.Errorf("security report: top-level coverage gaps: %w", err)
	}
	if candidate.Result == "inconclusive" && len(candidate.CoverageGapIDs) == 0 && !truncated && contradictions == 0 {
		return candidate, nil, nil, errors.New("security report: inconclusive candidate names no limiting evidence")
	}
	priorID := ""
	issues := make([]Issue, 0, len(candidate.Issues))
	for _, candidateIssue := range candidate.Issues {
		if priorID != "" && candidateIssue.CandidateID <= priorID {
			return candidate, nil, nil, errors.New("security report: candidate issue IDs are duplicate or not sorted")
		}
		priorID = candidateIssue.CandidateID
		if err := validateGapIDs(candidateIssue.CoverageGapIDs, coverage); err != nil {
			return candidate, nil, nil, fmt.Errorf("security report: issue %s coverage gaps: %w", candidateIssue.CandidateID, err)
		}
		for _, gap := range candidateIssue.CoverageGapIDs {
			if !containsSorted(candidate.CoverageGapIDs, gap) {
				return candidate, nil, nil, fmt.Errorf("security report: issue %s gap %s is absent from the top-level gaps", candidateIssue.CandidateID, gap)
			}
		}
		facts, action, err := validateEvidence(pack, candidateIssue, behavior, coverage)
		if err != nil {
			return candidate, nil, nil, err
		}
		if err := validateStandards(candidateIssue, standardsIndex); err != nil {
			return candidate, nil, nil, err
		}
		normalized, err := artifact.CanonicalJSON(struct {
			Observation string         `json:"observation_pack_digest"`
			Issue       CandidateIssue `json:"candidate_issue"`
		}{Observation: packDigest, Issue: candidateIssue})
		if err != nil {
			return candidate, nil, nil, err
		}
		issues = append(issues, Issue{
			IssueID:     "issue:" + strings.TrimPrefix(artifact.DigestBytes(normalized).String(), "sha256:"),
			CandidateID: candidateIssue.CandidateID, Title: candidateIssue.Title,
			ObservedBehaviorAssertion: candidateIssue.ObservedBehavior,
			CrossedBoundaryAssertion:  candidateIssue.CrossedBoundary,
			RationaleAssertion:        candidateIssue.Rationale,
			Inference:                 candidateIssue.Inference, Confidence: candidateIssue.Confidence, Severity: candidateIssue.Severity,
			ObservedFacts: facts, Action: action, Evidence: append([]EvidenceReference(nil), candidateIssue.Evidence...),
			Standards:      append([]StandardReference(nil), candidateIssue.Standards...),
			CoverageGapIDs: append([]string(nil), candidateIssue.CoverageGapIDs...), RecommendationMapping: "not_applicable",
		})
	}
	return candidate, standards, issues, nil
}

func validateEvidence(pack *observation.Pack, issue CandidateIssue, behavior map[string]behaviorRecord, coverage map[string]coverageRecord) ([]ObservedFact, *Action, error) {
	roles := map[string]string{"backend": "semantic_behavior", "independent_receipt": "external_effect", "openshell": "runtime_context", "model_receipt": "model_route"}
	seen := make(map[string]bool)
	facts := make([]ObservedFact, 0, len(issue.Evidence))
	var action *Action
	hasBackend := false
	for _, reference := range issue.Evidence {
		key := reference.Index + "\x00" + reference.ID + "\x00" + reference.Role
		if seen[key] {
			return nil, nil, fmt.Errorf("security report: issue %s has a duplicate evidence reference", issue.CandidateID)
		}
		seen[key] = true
		switch reference.Index {
		case "behavior":
			record, ok := behavior[reference.ID]
			if !ok || roles[record.Authority] != reference.Role {
				return nil, nil, fmt.Errorf("security report: issue %s behavior reference %s has the wrong authority role", issue.CandidateID, reference.ID)
			}
			facts = append(facts, ObservedFact{EvidenceID: record.ID, Authority: record.Authority, Type: record.Type, Timestamp: record.Timestamp})
			if record.Authority == "backend" {
				hasBackend = true
				candidateAction, actionErr := actionFromBackend(pack.Payloads["backend.json"], record)
				if actionErr != nil {
					return nil, nil, fmt.Errorf("security report: issue %s backend action: %w", issue.CandidateID, actionErr)
				}
				if action == nil && candidateAction != nil {
					action = candidateAction
				}
			}
		case "coverage":
			record, ok := coverage[reference.ID]
			if !ok || !isGap(record.Status) || reference.Role != "limitation" || !containsSorted(issue.CoverageGapIDs, reference.ID) {
				return nil, nil, fmt.Errorf("security report: issue %s coverage reference %s is not an honest limitation", issue.CandidateID, reference.ID)
			}
		default:
			return nil, nil, fmt.Errorf("security report: issue %s cites unknown evidence index %s", issue.CandidateID, reference.Index)
		}
	}
	if !hasBackend {
		return nil, nil, fmt.Errorf("security report: issue %s cites no backend semantic behavior", issue.CandidateID)
	}
	for _, gap := range issue.CoverageGapIDs {
		found := false
		for _, reference := range issue.Evidence {
			found = found || (reference.Index == "coverage" && reference.ID == gap && reference.Role == "limitation")
		}
		if !found {
			return nil, nil, fmt.Errorf("security report: issue %s gap %s lacks a limitation citation", issue.CandidateID, gap)
		}
	}
	return facts, action, nil
}

func actionFromBackend(content []byte, behavior behaviorRecord) (*Action, error) {
	if behavior.Source.File != "backend.json" || behavior.Source.ResponseOrdinal < 1 || behavior.Source.RecordOrdinal < 0 {
		return nil, errors.New("behavior source does not resolve to retained backend evidence")
	}
	var document struct {
		Entries []observation.Entry `json:"entries"`
	}
	if err := json.Unmarshal(content, &document); err != nil {
		return nil, err
	}
	var body []byte
	for _, entry := range document.Entries {
		if entry.Ordinal == behavior.Source.ResponseOrdinal {
			decoded, err := base64.StdEncoding.DecodeString(entry.BodyBase64)
			if err != nil {
				return nil, err
			}
			body = decoded
			break
		}
	}
	if body == nil {
		return nil, errors.New("backend response ordinal is unresolved")
	}
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, err
	}
	var page struct {
		Data []json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(envelope.Data, &page); err != nil || behavior.Source.RecordOrdinal >= len(page.Data) {
		return nil, errors.New("backend record ordinal is unresolved")
	}
	var record map[string]json.RawMessage
	if err := json.Unmarshal(page.Data[behavior.Source.RecordOrdinal], &record); err != nil {
		return nil, err
	}
	var spans any
	if rawSpans, ok := record["spans"]; ok {
		_ = json.Unmarshal(rawSpans, &spans)
	}
	if semanticType, name := findSemanticAction(spans, 0); semanticType != "" {
		if name == "" {
			name = semanticType
		}
		return &Action{Class: semanticType, Name: boundActionName(name), SourceEvidenceID: behavior.ID}, nil
	}
	var eventType, activityType string
	_ = json.Unmarshal(record["event_type"], &eventType)
	_ = json.Unmarshal(record["activity_type"], &activityType)
	if (eventType == "ActivityStarted" || eventType == "ActivityCompleted") && safeActionName(activityType) {
		return &Action{Class: "tool_activity", Name: boundActionName(activityType), SourceEvidenceID: behavior.ID}, nil
	}
	return nil, nil
}

func findSemanticAction(value any, depth int) (string, string) {
	if depth > 8 {
		return "", ""
	}
	switch typed := value.(type) {
	case map[string]any:
		semantic, _ := typed["semantic_type"].(string)
		semantic = strings.ToLower(semantic)
		if lexicalAction.MatchString(semantic) {
			for _, key := range []string{"name", "tool_name", "activity_type"} {
				if name, ok := typed[key].(string); ok && safeActionName(name) {
					return semantic, name
				}
			}
			return semantic, semantic
		}
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if semantic, name := findSemanticAction(typed[key], depth+1); semantic != "" {
				return semantic, name
			}
		}
	case []any:
		for _, child := range typed {
			if semantic, name := findSemanticAction(child, depth+1); semantic != "" {
				return semantic, name
			}
		}
	}
	return "", ""
}

func indexBehavior(content []byte) (map[string]behaviorRecord, error) {
	var document struct {
		Entries []behaviorRecord `json:"entries"`
	}
	if err := json.Unmarshal(content, &document); err != nil {
		return nil, err
	}
	result := make(map[string]behaviorRecord, len(document.Entries))
	for _, record := range document.Entries {
		if record.ID == "" || result[record.ID].ID != "" {
			return nil, errors.New("security report: duplicate or empty behavior ID")
		}
		result[record.ID] = record
	}
	return result, nil
}

func indexCoverage(content []byte) (map[string]coverageRecord, bool, int, error) {
	var document struct {
		Channels       []coverageRecord  `json:"channels"`
		Truncated      bool              `json:"truncated"`
		Contradictions []json.RawMessage `json:"contradictions"`
	}
	if err := json.Unmarshal(content, &document); err != nil {
		return nil, false, 0, err
	}
	result := make(map[string]coverageRecord, len(document.Channels))
	for _, record := range document.Channels {
		if record.ID == "" || result[record.ID].ID != "" {
			return nil, false, 0, errors.New("security report: duplicate or empty coverage ID")
		}
		result[record.ID] = record
	}
	return result, document.Truncated, len(document.Contradictions), nil
}

func indexStandards(content []byte) (map[string]bool, error) {
	if _, err := artifact.CanonicalizeJSON(content); err != nil {
		return nil, fmt.Errorf("security report: standards catalog JSON: %w", err)
	}
	var catalog struct {
		Schema  string `json:"schema"`
		Version string `json:"version"`
		Entries []struct {
			Catalog string `json:"catalog"`
			Version string `json:"version"`
			ID      string `json:"id"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(content, &catalog); err != nil || catalog.Schema != securityskill.CatalogSchema || catalog.Version != securityskill.CatalogVersion || len(catalog.Entries) != 7 {
		return nil, errors.New("security report: standards catalog identity is invalid")
	}
	result := make(map[string]bool, len(catalog.Entries))
	for _, entry := range catalog.Entries {
		key := entry.Catalog + "/" + entry.Version + "/" + entry.ID
		if result[key] {
			return nil, errors.New("security report: duplicate standard")
		}
		result[key] = true
	}
	return result, nil
}

func validateStandards(issue CandidateIssue, standards map[string]bool) error {
	prior := ""
	for _, reference := range issue.Standards {
		key := reference.Catalog + "/" + reference.Version + "/" + reference.ID
		if !standards[key] {
			return fmt.Errorf("security report: issue %s cites unknown standard %s", issue.CandidateID, key)
		}
		if prior != "" && key <= prior {
			return fmt.Errorf("security report: issue %s standards are duplicate or not sorted", issue.CandidateID)
		}
		prior = key
	}
	return nil
}

func validateGapIDs(gaps []string, coverage map[string]coverageRecord) error {
	if !sort.StringsAreSorted(gaps) {
		return errors.New("gap IDs are not lexically sorted")
	}
	prior := ""
	for _, gap := range gaps {
		record, ok := coverage[gap]
		if !ok || !isGap(record.Status) {
			return fmt.Errorf("%s is unknown or observed", gap)
		}
		if gap == prior {
			return fmt.Errorf("duplicate gap %s", gap)
		}
		prior = gap
	}
	return nil
}

func isGap(status string) bool {
	switch status {
	case "missing", "opaque", "truncated", "unsupported":
		return true
	default:
		return false
	}
}

func resolveAbsentOutput(path string) (string, error) {
	if path == "" {
		return "", errors.New("security report: output path is required")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("security report: resolve output path: %w", err)
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(absolute))
	if err != nil {
		return "", fmt.Errorf("security report: resolve output parent: %w", err)
	}
	absolute = filepath.Join(parent, filepath.Base(absolute))
	if _, err := os.Lstat(absolute); err == nil {
		return "", errors.New("security report: output already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("security report: inspect output: %w", err)
	}
	return absolute, nil
}

func decodeExactJSON(content []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return errors.New("JSON contains trailing data")
	}
	return nil
}

func containsSorted(values []string, value string) bool {
	index := sort.SearchStrings(values, value)
	return index < len(values) && values[index] == value
}

func safeIdentity(value string) bool {
	return safeActionName(value) && !strings.ContainsAny(value, "/?&#")
}

func safeActionName(value string) bool {
	if value == "" || len(value) > 256 || !utf8.ValidString(value) {
		return false
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return false
		}
	}
	return true
}

func boundActionName(value string) string {
	if len(value) <= 160 {
		return value
	}
	return value[:160]
}

var forbiddenFields = map[string]bool{
	"recommendation": true, "recommendations": true, "control": true, "controls": true,
	"capability": true, "capabilities": true, "enforcement": true, "verification_recipe": true,
	"endpoint": true, "endpoints": true, "credential": true, "credentials": true,
	"command": true, "commands": true, "patch": true, "apply": true, "approval": true, "approvals": true,
}

func findForbiddenField(value any) string {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if forbiddenFields[strings.ToLower(key)] {
				return key
			}
			if found := findForbiddenField(child); found != "" {
				return found
			}
		}
	case []any:
		for _, child := range typed {
			if found := findForbiddenField(child); found != "" {
				return found
			}
		}
	}
	return ""
}
