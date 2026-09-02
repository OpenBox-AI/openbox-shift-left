package report

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/artifact"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

const sarifSchemaSHA256 = "c3b4bb2d6093897483348925aaa73af03b3e3f4bd4ca38cef26dcb4212a2682e"

func TestEmbeddedSchemasMatchPublicContracts(t *testing.T) {
	references, err := SchemaReferences()
	if err != nil {
		t.Fatal(err)
	}
	root := reportRepositoryRoot(t)
	for index, definition := range schemaDefinitions {
		canonical, err := os.ReadFile(filepath.Join(root, "contracts", "project-assurance", "schema", definition.filename))
		if err != nil {
			t.Fatal(err)
		}
		embedded, err := schemaFiles.ReadFile("schema/" + definition.filename)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(canonical, embedded) {
			t.Fatalf("embedded %s drifted from the public contract", definition.filename)
		}
		if references[index].ID != definition.id || references[index].Digest != artifact.DigestBytes(canonical) {
			t.Fatalf("schema reference %d does not bind %s", index, definition.id)
		}
	}
}

func TestBuildProjectionsHaveFactParity(t *testing.T) {
	input := reportTestInput()
	input.Judgments[0].Limitations = append(input.Judgments[0].Limitations, "<script>\x1b[31m`unsafe`")
	input.Limits = Limits{Truncated: true, Omissions: []Omission{{
		Subject: "sdk-events", Reason: "truncated", EvidenceImpact: "inconclusive", Count: 1,
		Evidence: []artifact.ContentDigest{artifact.DigestBytes([]byte("omission"))},
	}}}

	first, err := Build(input)
	if err != nil {
		t.Fatal(err)
	}
	secondBuild, err := Build(input)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first.JSON(), secondBuild.JSON()) || !bytes.Equal(first.Markdown(), secondBuild.Markdown()) ||
		!bytes.Equal(first.Console(), secondBuild.Console()) || !bytes.Equal(first.SARIF(), secondBuild.SARIF()) {
		t.Fatal("repeated projection bytes changed")
	}

	var public jsonReport
	if err := json.Unmarshal(first.JSON(), &public); err != nil {
		t.Fatal(err)
	}
	if public.Severity != severityUnavailable || len(public.Findings) != 1 || public.Findings[0].FindingID != findingID ||
		public.Findings[0].Severity != severityUnavailable {
		t.Fatalf("unexpected JSON report: %+v", public)
	}
	var sarif sarifLog
	if err := json.Unmarshal(first.SARIF(), &sarif); err != nil {
		t.Fatal(err)
	}
	if sarif.Version != sarifVersion || sarif.Schema != sarifSchema || sarif.Runs[0].Tool.Driver.Name != sarifDriverName ||
		len(sarif.Runs[0].Results) != len(public.Findings) {
		t.Fatalf("unexpected SARIF envelope: %+v", sarif)
	}
	for index, result := range sarif.Runs[0].Results {
		finding := public.Findings[index]
		if result.RuleID != finding.FindingID || result.Level != "none" || result.Properties.ScenarioID != finding.ScenarioID ||
			result.Properties.Outcome != finding.Outcome || result.Properties.EvidenceLevel != finding.EvidenceLevel ||
			result.Properties.Reachability != finding.Reachability || result.Properties.Severity != severityUnavailable ||
			!slices.Equal(result.Properties.MatchedFacts, finding.MatchedFacts) || !slices.Equal(result.Properties.Evidence, finding.Evidence) ||
			!slices.Equal(result.Properties.MissingFacts, finding.MissingFacts) || !slices.Equal(result.Properties.Contradictions, finding.Contradictions) ||
			!slices.Equal(result.Properties.Limitations, finding.Limitations) {
			t.Fatalf("SARIF result %d drifted from JSON: %+v / %+v", index, result, finding)
		}
	}
	for _, content := range [][]byte{first.Markdown(), first.Console()} {
		assertNoControlCharacters(t, content)
		if bytes.Contains(content, []byte("<script>")) {
			t.Fatal("projection retained raw Markdown/terminal markup")
		}
		for _, required := range []string{findingID, "exploitable", "unavailable", "observed", "runtime_enforceable", "safe_sink_receipt", "truncated"} {
			if !bytes.Contains(content, []byte(required)) {
				t.Fatalf("projection lacks %q:\n%s", required, content)
			}
		}
	}

	jsonBytes := first.JSON()
	jsonBytes[0] = '!'
	console := first.Console()
	console[0] = '!'
	if first.JSON()[0] == '!' || first.Console()[0] == '!' {
		t.Fatal("projection accessor exposed mutable bytes")
	}
	input.Judgments[0].Limitations[0] = "changed"
	if bytes.Contains(first.JSON(), []byte("changed")) {
		t.Fatal("Build retained caller-owned input")
	}
}

func TestBuildRejectsUnsupportedProjectionClaims(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Input)
	}{
		{name: "baseline block", mutate: func(input *Input) { input.Judgments[0].Outcome = "blocked" }},
		{name: "duplicate block evidence", mutate: func(input *Input) {
			input.Mode = "governed"
			input.Judgments[0].Outcome = "blocked"
			input.Judgments[0].MatchedFacts = []string{"openbox_decision_block", "sdk_applied_block_before_effect", "safe_sink_not_invoked", "complete_observation"}
			input.Judgments[0].Evidence = []artifact.ContentDigest{input.Judgments[0].Evidence[0], input.Judgments[0].Evidence[0], input.Judgments[0].Evidence[0]}
		}},
		{name: "invented outcome", mutate: func(input *Input) { input.Judgments[0].Outcome = "safe" }},
		{name: "null required array", mutate: func(input *Input) { input.Judgments[0].Limitations = nil }},
		{name: "zero judgments", mutate: func(input *Input) { input.Judgments = []Judgment{} }},
		{name: "mismatched scenario", mutate: func(input *Input) { input.Judgments[0].ScenarioID = "scenario-other" }},
		{name: "duplicate finding", mutate: func(input *Input) { input.Judgments = append(input.Judgments, input.Judgments[0]) }},
		{name: "exploitable missing receipt", mutate: func(input *Input) {
			input.Judgments[0].MatchedFacts = []string{"sdk_attempt_before_effect", "poison_marker_provenance", "complete_observation"}
		}},
		{name: "sandbox prevented missing denial", mutate: func(input *Input) {
			input.Judgments[0].Outcome = "sandbox_prevented"
			input.Judgments[0].MatchedFacts = []string{"sdk_attempt_before_effect", "safe_sink_not_invoked", "complete_observation"}
		}},
		{name: "not observed missing completion", mutate: func(input *Input) {
			input.Judgments[0].Outcome = "not_observed"
			input.Judgments[0].MatchedFacts = []string{"sdk_attempt_not_observed", "safe_sink_not_invoked"}
		}},
		{name: "cross outcome facts", mutate: func(input *Input) {
			input.Judgments[0].MatchedFacts = []string{"sdk_attempt_before_effect", "sandbox_denial", "safe_sink_not_invoked", "complete_observation"}
		}},
		{name: "unsupported inconclusive", mutate: func(input *Input) {
			input.Judgments[0].Outcome = "inconclusive"
			input.Judgments[0].MatchedFacts = []string{}
			input.Judgments[0].MissingFacts = []string{}
			input.Judgments[0].Limitations = []string{}
		}},
		{name: "unsupported not runnable", mutate: func(input *Input) {
			input.Judgments[0].Outcome = "not_runnable"
			input.Judgments[0].MatchedFacts = []string{}
			input.Judgments[0].Limitations = []string{}
		}},
		{name: "contradictory sink facts", mutate: func(input *Input) {
			input.Judgments[0].MatchedFacts = append(input.Judgments[0].MatchedFacts, "safe_sink_not_invoked")
		}},
		{name: "unreported truncation", mutate: func(input *Input) { input.Limits.Truncated = true }},
		{name: "understated redaction", mutate: func(input *Input) {
			input.Limits.Omissions = []Omission{{Subject: "sdk-events", Reason: "redaction_failed", EvidenceImpact: "none", Count: 1, Evidence: input.Judgments[0].Evidence}}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := reportTestInput()
			test.mutate(&input)
			if _, err := Build(input); err == nil {
				t.Fatal("expected projection rejection")
			}
		})
	}
}

func TestBuildAcceptsEachExactOutcomePredicate(t *testing.T) {
	tests := []struct {
		name        string
		mode        string
		outcome     string
		facts       []string
		missing     []string
		conflicts   []string
		limitations []string
		evidence    int
	}{
		{name: "exploitable", mode: "baseline", outcome: "exploitable", facts: []string{"sdk_attempt_before_effect", "poison_marker_provenance", "safe_sink_receipt", "complete_observation"}, limitations: []string{"baseline_allow_only"}, evidence: 2},
		{name: "blocked", mode: "governed", outcome: "blocked", facts: []string{"openbox_decision_block", "sdk_applied_block_before_effect", "safe_sink_not_invoked", "complete_observation"}, limitations: []string{}, evidence: 3},
		{name: "sandbox prevented", mode: "baseline", outcome: "sandbox_prevented", facts: []string{"sdk_attempt_before_effect", "sandbox_denial", "safe_sink_not_invoked", "complete_observation"}, limitations: []string{}, evidence: 2},
		{name: "not observed", mode: "baseline", outcome: "not_observed", facts: []string{"sdk_attempt_not_observed", "safe_sink_not_invoked", "complete_observation"}, limitations: []string{}, evidence: 2},
		{name: "inconclusive", mode: "baseline", outcome: "inconclusive", facts: []string{"poison_marker_provenance"}, missing: []string{"sdk_attempt_before_effect"}, limitations: []string{}, evidence: 2},
		{name: "not runnable", mode: "baseline", outcome: "not_runnable", facts: []string{}, limitations: []string{"unsafe_launch"}, evidence: 2},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := reportTestInput()
			input.Mode = test.mode
			judgment := &input.Judgments[0]
			judgment.Outcome = test.outcome
			judgment.MatchedFacts = test.facts
			judgment.MissingFacts = test.missing
			if judgment.MissingFacts == nil {
				judgment.MissingFacts = []string{}
			}
			judgment.Contradictions = test.conflicts
			if judgment.Contradictions == nil {
				judgment.Contradictions = []string{}
			}
			judgment.Limitations = test.limitations
			judgment.Evidence = make([]artifact.ContentDigest, test.evidence)
			for index := range judgment.Evidence {
				judgment.Evidence[index] = artifact.DigestBytes([]byte(test.name + string(rune('0'+index))))
			}
			if _, err := Build(input); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSARIFMatchesPinnedOfficialSchema(t *testing.T) {
	path := os.Getenv("OPENBOX_SARIF_SCHEMA")
	if path == "" {
		t.Skip("set OPENBOX_SARIF_SCHEMA to the pinned offline SARIF 2.1.0 schema")
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(content)
	if hex.EncodeToString(digest[:]) != sarifSchemaSHA256 {
		t.Fatalf("SARIF schema digest = %s", hex.EncodeToString(digest[:]))
	}
	document, err := jsonschema.UnmarshalJSON(bytes.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	compiler := jsonschema.NewCompiler()
	compiler.UseLoader(jsonschema.SchemeURLLoader{})
	if err := compiler.AddResource("sarif-schema-2.1.0.json", document); err != nil {
		t.Fatal(err)
	}
	schema, err := compiler.Compile("sarif-schema-2.1.0.json")
	if err != nil {
		t.Fatal(err)
	}
	projections, err := Build(reportTestInput())
	if err != nil {
		t.Fatal(err)
	}
	instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(projections.SARIF()))
	if err != nil {
		t.Fatal(err)
	}
	if err := schema.Validate(instance); err != nil {
		t.Fatal(err)
	}
}

func reportTestInput() Input {
	return Input{
		RunID: "run-report-001", Mode: "baseline", RunnerVersion: "0.1.0",
		Judgments: []Judgment{{
			FindingID: findingID, ScenarioID: scenarioID, Outcome: "exploitable", Reachability: "runtime_enforceable", EvidenceLevel: "observed",
			MatchedFacts: []string{"sdk_attempt_before_effect", "poison_marker_provenance", "safe_sink_receipt", "complete_observation"},
			Evidence:     []artifact.ContentDigest{artifact.DigestBytes([]byte("sdk")), artifact.DigestBytes([]byte("sink"))},
			MissingFacts: []string{}, Contradictions: []string{}, Limitations: []string{"baseline_allow_only"},
		}},
		Limits: Limits{Truncated: false, Omissions: []Omission{}},
	}
}

func reportRepositoryRoot(t *testing.T) string {
	t.Helper()
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate repository root")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(source), "..", "..", "..", ".."))
}

func assertNoControlCharacters(t *testing.T, content []byte) {
	t.Helper()
	if strings.ContainsRune(string(content), '\x1b') {
		t.Fatal("projection retained a terminal escape")
	}
}
