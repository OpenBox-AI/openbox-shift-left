package securityreport

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/artifact"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/observation"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/runfs"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/targetposture"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/securityskill"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

const pinnedSARIFSchemaSHA256 = "c3b4bb2d6093897483348925aaa73af03b3e3f4bd4ca38cef26dcb4212a2682e"

func TestRecommendationCatalogPublicParityAndFrozenDigest(t *testing.T) {
	_, embedded, err := loadCatalog()
	if err != nil {
		t.Fatalf("loadCatalog: %v", err)
	}
	public, err := os.ReadFile(filepath.Join(repoRoot(t), "contracts/project-security-report/recommendation-catalog.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(embedded, public) || artifact.DigestBytes(embedded).String() != RecommendationDigest {
		t.Fatalf("catalog parity/digest drifted: %s", artifact.DigestBytes(embedded))
	}
}

func TestEmbeddedSchemasMatchPublicContracts(t *testing.T) {
	root := repoRoot(t)
	for _, definition := range phaseFourSchemas {
		public, err := os.ReadFile(filepath.Join(root, "contracts/project-security-report/schema", definition.filename))
		if err != nil {
			t.Fatal(err)
		}
		embedded, err := schemaFiles.ReadFile("schema/" + definition.filename)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(public, embedded) {
			t.Fatalf("embedded schema %s drifted from its public contract", definition.filename)
		}
		if _, err := artifact.CanonicalizeJSON(public); err != nil {
			t.Fatalf("public schema %s is invalid JSON: %v", definition.filename, err)
		}
	}
}

func TestPrepareAcceptsBothInstalledHostMastraCandidates(t *testing.T) {
	observationPath := mastraObservation(t)
	for _, name := range []string{"2026-08-27-phase-03-installed-claude-candidate.json", "2026-08-27-phase-03-installed-codex-candidate.json"} {
		t.Run(name, func(t *testing.T) {
			candidate := privateCandidateCopy(t, filepath.Join(repoRoot(t), "plans/260825-1623-lean-openshell-project-assurance/evidence", name))
			prepared, err := Prepare(observationPath, candidate, filepath.Join(t.TempDir(), "report-pack"))
			if err != nil {
				t.Fatalf("Prepare: %v", err)
			}
			if prepared.Candidate.Result != "no_supported_issue" || len(prepared.Issues) != 0 || prepared.PackDigest != "sha256:2e724ab506e2eeea2c40b873fa05135940f0d6ad0fb0bf82609e7f2dca73fe25" {
				t.Fatalf("unexpected preparation: %#v", prepared)
			}
		})
	}
}

func TestPrepareAndRenderAcceptHonestInconclusiveResult(t *testing.T) {
	source, err := os.ReadFile(filepath.Join(repoRoot(t), "plans/260825-1623-lean-openshell-project-assurance/evidence/2026-08-27-phase-03-installed-codex-candidate.json"))
	if err != nil {
		t.Fatal(err)
	}
	var candidate Candidate
	if err := json.Unmarshal(source, &candidate); err != nil {
		t.Fatal(err)
	}
	candidate.Result = "inconclusive"
	content, err := artifact.CanonicalJSON(candidate)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "candidate.json")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	prepared, err := Prepare(mastraObservation(t), path, filepath.Join(t.TempDir(), "report"))
	if err != nil {
		t.Fatal(err)
	}
	projections, _, err := buildProjections(prepared, validPosture(prepared.PackDigest))
	if err != nil {
		t.Fatal(err)
	}
	if projections.Report.Result != "inconclusive" || projections.Report.SecurityPass || len(projections.Report.Issues) != 0 || len(projections.Report.Recommendations) != 0 || !bytes.Contains(projections.Markdown, []byte("inconclusive")) {
		t.Fatalf("inconclusive result was overclaimed: %#v", projections.Report)
	}
}

func TestPrepareRejectsUnsafeCandidateFilesAndJSONShapes(t *testing.T) {
	source, err := os.ReadFile(filepath.Join(repoRoot(t), "plans/260825-1623-lean-openshell-project-assurance/evidence/2026-08-27-phase-03-installed-codex-candidate.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name  string
		setup func(*testing.T, string) string
	}{
		{name: "non-private mode", setup: func(t *testing.T, root string) string {
			path := filepath.Join(root, "candidate.json")
			if err := os.WriteFile(path, source, 0o644); err != nil {
				t.Fatal(err)
			}
			return path
		}},
		{name: "directory", setup: func(t *testing.T, root string) string {
			path := filepath.Join(root, "candidate.json")
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
			return path
		}},
		{name: "symbolic link", setup: func(t *testing.T, root string) string {
			target := filepath.Join(root, "target.json")
			if err := os.WriteFile(target, source, 0o600); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, "candidate.json")
			if err := os.Symlink(target, path); err != nil {
				t.Fatal(err)
			}
			return path
		}},
		{name: "multiple links", setup: func(t *testing.T, root string) string {
			path := filepath.Join(root, "candidate.json")
			if err := os.WriteFile(path, source, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Link(path, filepath.Join(root, "alias.json")); err != nil {
				t.Fatal(err)
			}
			return path
		}},
		{name: "oversize", setup: func(t *testing.T, root string) string {
			path := filepath.Join(root, "candidate.json")
			if err := os.WriteFile(path, bytes.Repeat([]byte{' '}, int(MaxCandidateBytes)+1), 0o600); err != nil {
				t.Fatal(err)
			}
			return path
		}},
		{name: "duplicate key", setup: func(t *testing.T, root string) string {
			path := filepath.Join(root, "candidate.json")
			content := append([]byte(`{"schema":"duplicate",`), source[1:]...)
			if err := os.WriteFile(path, content, 0o600); err != nil {
				t.Fatal(err)
			}
			return path
		}},
		{name: "trailing JSON", setup: func(t *testing.T, root string) string {
			path := filepath.Join(root, "candidate.json")
			content := append(append([]byte(nil), source...), []byte(`{}`)...)
			if err := os.WriteFile(path, content, 0o600); err != nil {
				t.Fatal(err)
			}
			return path
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			candidate := test.setup(t, root)
			output := filepath.Join(root, "report")
			if _, err := Prepare(mastraObservation(t), candidate, output); err == nil {
				t.Fatal("Prepare accepted an unsafe candidate")
			}
			if _, err := os.Lstat(output); !os.IsNotExist(err) {
				t.Fatalf("offline failure created output: %v", err)
			}
		})
	}
}

func TestCandidateValidationAndMappingUseRetainedBehavior(t *testing.T) {
	pack, err := observation.Read(mastraObservation(t))
	if err != nil {
		t.Fatal(err)
	}
	bundle, _, err := securityskill.Load()
	if err != nil {
		t.Fatal(err)
	}
	packDigest, _ := pack.PackDigest()
	candidate := Candidate{
		Schema:         securityskill.CandidateSchema,
		Skill:          Skill{Name: bundle.Name, Version: bundle.Version, Digest: bundle.Digest},
		Result:         "issues",
		CoverageGapIDs: []string{"coverage:retrieval_poison", "coverage:signed_request_attribution"},
	}
	candidate.Observation.Schema = observation.Schema
	candidate.Observation.PackDigest = packDigest
	candidate.Issues = []CandidateIssue{{
		CandidateID: "candidate:bounded-tool-boundary", Title: "A validated tool boundary issue",
		ObservedBehavior: "Analyzer assertion grounded by the cited backend activity and external receipt.",
		CrossedBoundary:  "Analyzer assertion about a tool boundary.", Rationale: "Analyzer rationale; retained records remain authoritative.",
		Inference: true, Confidence: "medium", Severity: "unavailable",
		Evidence: []EvidenceReference{
			{Index: "behavior", ID: "82782612-7b86-41e9-a488-b465b4c77b61", Role: "semantic_behavior"},
			{Index: "behavior", ID: "effect:safe_sink:ev-656213e0d3ddc7400f324769", Role: "external_effect"},
			{Index: "coverage", ID: "coverage:retrieval_poison", Role: "limitation"},
			{Index: "coverage", ID: "coverage:signed_request_attribution", Role: "limitation"},
		},
		Standards: []StandardReference{
			{Catalog: "CWE", Version: "4.20", ID: "CWE-1426"},
			{Catalog: "CWE", Version: "4.20", ID: "CWE-1427"},
			{Catalog: "MITRE_ATLAS", Version: "2026-08-26", ID: "AML.T0051"},
			{Catalog: "OWASP_AGENTIC", Version: "2026", ID: "ASI01"},
			{Catalog: "OWASP_AGENTIC", Version: "2026", ID: "ASI02"},
			{Catalog: "OWASP_LLM", Version: "2025", ID: "LLM01"},
			{Catalog: "OWASP_LLM", Version: "2025", ID: "LLM06"},
		},
		CoverageGapIDs: []string{"coverage:retrieval_poison", "coverage:signed_request_attribution"},
	}}
	content, err := artifact.CanonicalJSON(candidate)
	if err != nil {
		t.Fatal(err)
	}
	validated, _, issues, err := validateCandidate(pack, content)
	if err != nil {
		t.Fatalf("validateCandidate: %v\n%s", err, content)
	}
	if validated.Result != "issues" || len(issues) != 1 || issues[0].Action == nil || issues[0].Action.Class != "tool_activity" || issues[0].Action.Name != "recordingTool" {
		t.Fatalf("action was not derived from cited backend record: %#v", issues)
	}
	posture := validPosture(packDigest)
	mapped, recommendations, err := mapRecommendations(issues, posture)
	if err != nil {
		t.Fatal(err)
	}
	if mapped[0].RecommendationMapping != "available" || len(recommendations) != 6 {
		t.Fatalf("expected all six deterministic catalog paths, got %#v %#v", mapped, recommendations)
	}
	for _, recommendation := range recommendations {
		if recommendation.Status != "new_gap" || recommendation.CurrentControlIDs == nil || recommendation.Target.AgentID != "450999ca-ae2a-409c-8a26-d00a71132440" || strings.Contains(strings.ToLower(recommendation.ExpectedProtectedBehavior), "curl ") {
			t.Fatalf("unsafe or incorrect recommendation: %#v", recommendation)
		}
	}
}

func TestRecommendationMappingDistinguishesExistingAndUnavailableTargets(t *testing.T) {
	posture := validPosture("sha256:1111111111111111111111111111111111111111111111111111111111111111")
	posture.Guardrails = []targetposture.Guardrail{{ID: "guardrail-1", VersionHash: "guardrail-v1", Active: true, Opaque: true}}
	posture.Policies = []targetposture.Policy{{ID: "policy-1", VersionHash: "policy-v1", Active: true, Current: true, Opaque: true}}
	posture.BehaviorRules = []targetposture.BehaviorRule{{ID: "behavior-1", VersionHash: "behavior-v1", BaseRuleID: "base-1", Trigger: "tool_activity", Verdict: "REQUIRE_APPROVAL", Priority: 80, Active: true, Current: true, Opaque: true}}
	issue := Issue{
		IssueID: "issue:1111111111111111111111111111111111111111111111111111111111111111",
		Action:  &Action{Class: "tool_activity", Name: "recordingTool", SourceEvidenceID: "backend-1"},
		Standards: []StandardReference{
			{Catalog: "CWE", Version: "4.20", ID: "CWE-1426"},
			{Catalog: "CWE", Version: "4.20", ID: "CWE-1427"},
			{Catalog: "OWASP_AGENTIC", Version: "2026", ID: "ASI02"},
		},
	}
	mapped, recommendations, err := mapRecommendations([]Issue{issue}, posture)
	if err != nil {
		t.Fatal(err)
	}
	if mapped[0].RecommendationMapping != "available" || len(recommendations) != 5 {
		t.Fatalf("existing-control mapping = %#v %#v", mapped, recommendations)
	}
	for _, recommendation := range recommendations {
		if recommendation.Status != "review_existing" || len(recommendation.CurrentControlIDs) == 0 {
			t.Fatalf("existing control was not retained only for review: %#v", recommendation)
		}
	}

	unresolved := issue
	unresolved.Action = nil
	unresolved.Standards = []StandardReference{{Catalog: "CWE", Version: "4.20", ID: "CWE-1426"}}
	mapped, recommendations, err = mapRecommendations([]Issue{unresolved}, validPosture(posture.Observation.PackDigest))
	if err != nil {
		t.Fatal(err)
	}
	if mapped[0].RecommendationMapping != "unavailable" || len(recommendations) != 1 || recommendations[0].Status != "unavailable" || len(recommendations[0].CurrentControlIDs) != 0 {
		t.Fatalf("unresolved action was overclaimed: %#v %#v", mapped, recommendations)
	}
}

func TestAuthoritativeCandidateValidatorRejectsForgedOrDishonestAuthority(t *testing.T) {
	pack, err := observation.Read(mastraObservation(t))
	if err != nil {
		t.Fatal(err)
	}
	bundle, _, err := securityskill.Load()
	if err != nil {
		t.Fatal(err)
	}
	packDigest, _ := pack.PackDigest()
	for _, test := range []struct {
		name   string
		mutate func(*Candidate)
	}{
		{name: "forged observation", mutate: func(candidate *Candidate) {
			candidate.Observation.PackDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		}},
		{name: "wrong role", mutate: func(candidate *Candidate) { candidate.Issues[0].Evidence[0].Role = "runtime_context" }},
		{name: "missing backend authority", mutate: func(candidate *Candidate) {
			candidate.Issues[0].Evidence[0] = EvidenceReference{Index: "behavior", ID: "openshell:1:sha256:e4ae26964d84fa698320846e6589b814831f445a66b6e30e28ac372a58e3ac81", Role: "runtime_context"}
		}},
		{name: "observed limitation", mutate: func(candidate *Candidate) {
			candidate.CoverageGapIDs = []string{"coverage:backend_lifecycle"}
			candidate.Issues[0].CoverageGapIDs = []string{"coverage:backend_lifecycle"}
			candidate.Issues[0].Evidence = append(candidate.Issues[0].Evidence[:2], EvidenceReference{Index: "coverage", ID: "coverage:backend_lifecycle", Role: "limitation"})
		}},
		{name: "unsorted standards", mutate: func(candidate *Candidate) {
			candidate.Issues[0].Standards[0], candidate.Issues[0].Standards[1] = candidate.Issues[0].Standards[1], candidate.Issues[0].Standards[0]
		}},
		{name: "credential-derived analyzer", mutate: func(candidate *Candidate) {
			candidate.Analyzer = &Analyzer{Product: "obx_secret-derived"}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate := controlledCandidate(bundle, packDigest)
			test.mutate(&candidate)
			content, err := artifact.CanonicalJSON(candidate)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, _, err := validateCandidate(pack, content); err == nil {
				t.Fatalf("validator accepted %s", test.name)
			}
		})
	}
	t.Run("forbidden recommendation field", func(t *testing.T) {
		candidate := controlledCandidate(bundle, packDigest)
		content, err := artifact.CanonicalJSON(candidate)
		if err != nil {
			t.Fatal(err)
		}
		var document map[string]any
		if err := json.Unmarshal(content, &document); err != nil {
			t.Fatal(err)
		}
		document["recommendations"] = []any{}
		content, err = artifact.CanonicalJSON(document)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, _, err := validateCandidate(pack, content); err == nil {
			t.Fatal("validator accepted a candidate-carried recommendation field")
		}
	})
}

func TestFinalizeSealsVerifiesAndRendersOffline(t *testing.T) {
	candidate := privateCandidateCopy(t, filepath.Join(repoRoot(t), "plans/260825-1623-lean-openshell-project-assurance/evidence/2026-08-27-phase-03-installed-codex-candidate.json"))
	output := filepath.Join(t.TempDir(), "report-pack")
	prepared, err := Prepare(mastraObservation(t), candidate, output)
	if err != nil {
		t.Fatal(err)
	}
	output = prepared.OutputPath
	posture := validPosture(prepared.PackDigest)
	captureCalls := 0
	result, err := Finalize(context.Background(), prepared, RuntimeInput{}, Dependencies{Capture: func(context.Context, targetposture.Config) (*targetposture.Posture, error) {
		captureCalls++
		return posture, nil
	}})
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	makePackRemovable(t, output)
	if captureCalls != 1 || result.Output != output || result.PackDigest == "" {
		t.Fatalf("unexpected finalization result: %#v calls=%d", result, captureCalls)
	}
	verified, err := Verify(output)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if verified.PackDigest != result.PackDigest || verified.Projection.Report.Result != "no_supported_issue" || len(verified.Projection.Report.Issues) != 0 || len(verified.Projection.Report.Recommendations) != 0 || verified.Projection.Report.SecurityPass {
		t.Fatalf("untruthful verified report: %#v", verified.Projection.Report)
	}
	discriminator, err := runfs.ReadCommittedManifestDiscriminator(output)
	if err != nil || discriminator.PackSchema() != runfs.SecurityReportPackSchema {
		t.Fatalf("report discriminator: %q %v", discriminator.PackSchema(), err)
	}
	if !bytes.Contains(verified.Projection.Markdown, []byte("not a security pass")) || !bytes.Contains(verified.Projection.SARIF, []byte(`"results":[]`)) {
		t.Fatal("projections do not express the bounded non-pass result")
	}
	if _, err := Finalize(context.Background(), prepared, RuntimeInput{}, Dependencies{Capture: func(context.Context, targetposture.Config) (*targetposture.Posture, error) { return posture, nil }}); err == nil {
		t.Fatal("existing report target was replaced")
	}
}

func TestFinalizeIsByteStableForIdenticalInputs(t *testing.T) {
	candidate := privateCandidateCopy(t, filepath.Join(repoRoot(t), "plans/260825-1623-lean-openshell-project-assurance/evidence/2026-08-27-phase-03-installed-codex-candidate.json"))
	root := t.TempDir()
	outputs := []string{filepath.Join(root, "report-a"), filepath.Join(root, "report-b")}
	verified := make([]*VerifiedPack, 0, len(outputs))
	manifests := make([][]byte, 0, len(outputs))
	for _, output := range outputs {
		prepared, err := Prepare(mastraObservation(t), candidate, output)
		if err != nil {
			t.Fatal(err)
		}
		posture := validPosture(prepared.PackDigest)
		result, err := Finalize(context.Background(), prepared, RuntimeInput{}, Dependencies{Capture: func(context.Context, targetposture.Config) (*targetposture.Posture, error) {
			return posture, nil
		}})
		if err != nil {
			t.Fatal(err)
		}
		pack, err := Verify(result.Output)
		if err != nil {
			t.Fatal(err)
		}
		verified = append(verified, pack)
		manifestBytes, err := runfs.ReadPrivateFile(filepath.Join(result.Output, "manifest.json"), 0o400, 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		manifests = append(manifests, manifestBytes)
		makePackRemovable(t, result.Output)
	}
	if verified[0].PackDigest != verified[1].PackDigest || !bytes.Equal(manifests[0], manifests[1]) {
		t.Fatal("identical finalization inputs did not produce identical pack identity")
	}
	for _, expected := range reportInventory {
		if !bytes.Equal(verified[0].Files[expected.Path], verified[1].Files[expected.Path]) {
			t.Fatalf("payload %s is not byte stable", expected.Path)
		}
	}
}

func TestVerifyRejectsAnyProjectionMutation(t *testing.T) {
	candidate := privateCandidateCopy(t, filepath.Join(repoRoot(t), "plans/260825-1623-lean-openshell-project-assurance/evidence/2026-08-27-phase-03-installed-codex-candidate.json"))
	output := filepath.Join(t.TempDir(), "report-pack")
	prepared, err := Prepare(mastraObservation(t), candidate, output)
	if err != nil {
		t.Fatal(err)
	}
	output = prepared.OutputPath
	posture := validPosture(prepared.PackDigest)
	if _, err := Finalize(context.Background(), prepared, RuntimeInput{}, Dependencies{Capture: func(context.Context, targetposture.Config) (*targetposture.Posture, error) { return posture, nil }}); err != nil {
		t.Fatal(err)
	}
	makePackRemovable(t, output)
	reportPath := filepath.Join(output, "report.json")
	content, _ := os.ReadFile(reportPath)
	if err := os.Chmod(reportPath, 0o600); err != nil {
		t.Fatal(err)
	}
	content[0] = '!'
	if err := os.WriteFile(reportPath, content, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(reportPath, 0o400); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(output); err == nil {
		t.Fatal("Verify accepted a mutated projection")
	}
}

func TestFinalizeDoesNotReplaceTargetCreatedAfterOfflinePreflight(t *testing.T) {
	candidate := privateCandidateCopy(t, filepath.Join(repoRoot(t), "plans/260825-1623-lean-openshell-project-assurance/evidence/2026-08-27-phase-03-installed-codex-candidate.json"))
	prepared, err := Prepare(mastraObservation(t), candidate, filepath.Join(t.TempDir(), "report-pack"))
	if err != nil {
		t.Fatal(err)
	}
	sentinel := []byte("belongs to another writer\n")
	posture := validPosture(prepared.PackDigest)
	_, err = Finalize(context.Background(), prepared, RuntimeInput{}, Dependencies{Capture: func(context.Context, targetposture.Config) (*targetposture.Posture, error) {
		if writeErr := os.WriteFile(prepared.OutputPath, sentinel, 0o600); writeErr != nil {
			t.Fatal(writeErr)
		}
		return posture, nil
	}})
	if err == nil {
		t.Fatal("Finalize replaced a target created after offline preflight")
	}
	content, readErr := os.ReadFile(prepared.OutputPath)
	if readErr != nil || !bytes.Equal(content, sentinel) {
		t.Fatalf("existing target changed: %q %v", content, readErr)
	}
	matches, globErr := filepath.Glob(filepath.Join(filepath.Dir(prepared.OutputPath), "."+filepath.Base(prepared.OutputPath)+".security-report-*"))
	if globErr != nil || len(matches) != 0 {
		t.Fatalf("staging residue: %v %v", matches, globErr)
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
	if hex.EncodeToString(digest[:]) != pinnedSARIFSchemaSHA256 {
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
	candidate := privateCandidateCopy(t, filepath.Join(repoRoot(t), "plans/260825-1623-lean-openshell-project-assurance/evidence/2026-08-27-phase-03-installed-codex-candidate.json"))
	prepared, err := Prepare(mastraObservation(t), candidate, filepath.Join(t.TempDir(), "report"))
	if err != nil {
		t.Fatal(err)
	}
	projections, _, err := buildProjections(prepared, validPosture(prepared.PackDigest))
	if err != nil {
		t.Fatal(err)
	}
	instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(projections.SARIF))
	if err != nil {
		t.Fatal(err)
	}
	if err := schema.Validate(instance); err != nil {
		t.Fatal(err)
	}
}

func validPosture(packDigest string) *targetposture.Posture {
	permissions := append([]string(nil), observation.RequiredPermissions...)
	sort.Strings(permissions)
	return &targetposture.Posture{
		Schema: targetposture.Schema, ReadContract: targetposture.ReadContract,
		Catalog:       targetposture.Identity{Version: RecommendationVersion, Digest: RecommendationDigest},
		Observation:   targetposture.ObservationIdentity{PackDigest: packDigest, AgentID: "450999ca-ae2a-409c-8a26-d00a71132440", OrganizationID: "openbox.ai"},
		CaptureWindow: targetposture.CaptureWindow{StartedAt: "2026-08-27T00:00:00Z", CompletedAt: "2026-08-27T00:00:01Z", Passes: 2},
		Permissions:   permissions,
		Agent:         targetposture.Agent{ID: "450999ca-ae2a-409c-8a26-d00a71132440", OrganizationID: "openbox.ai", Status: int64(0), UpdatedAt: "2026-08-27T00:00:00Z"},
		Seams: targetposture.Seams{
			Guardrail:           targetposture.Seam{Status: "observed", Permission: "read:agent_guardrail", Route: "/agent/{agentId}/guardrails"},
			Policy:              targetposture.Seam{Status: "observed", Permission: "read:agent_policy", Route: "/agent/{agentId}/policies"},
			BehaviorRule:        targetposture.Seam{Status: "observed", Permission: "read:agent_behavior_rule", Route: "/agent/{agentId}/behavior-rule"},
			ApprovalRequirement: targetposture.Seam{Status: "observed", Permission: "read:agent_behavior_rule", Route: "/agent/{agentId}/behavior-rule"},
			SDKIntegration:      targetposture.Seam{Status: "observed", Permission: "read:agent", Route: "/agent/{agentId}"},
		},
		Guardrails: []targetposture.Guardrail{}, GuardrailAggregate: &targetposture.Aggregate{VersionHash: "empty-guardrails", Count: 0},
		Policies: []targetposture.Policy{}, BehaviorRules: []targetposture.BehaviorRule{}, BehaviorRuleAggregate: &targetposture.Aggregate{VersionHash: "empty-behavior", Count: 0},
	}
}

func controlledCandidate(bundle securityskill.Manifest, packDigest string) Candidate {
	candidate := Candidate{
		Schema:         securityskill.CandidateSchema,
		Skill:          Skill{Name: bundle.Name, Version: bundle.Version, Digest: bundle.Digest},
		Result:         "issues",
		CoverageGapIDs: []string{"coverage:retrieval_poison", "coverage:signed_request_attribution"},
	}
	candidate.Observation.Schema = observation.Schema
	candidate.Observation.PackDigest = packDigest
	candidate.Issues = []CandidateIssue{{
		CandidateID: "candidate:bounded-tool-boundary", Title: "A validated tool boundary issue",
		ObservedBehavior: "Analyzer assertion grounded by the cited backend activity and external receipt.",
		CrossedBoundary:  "Analyzer assertion about a tool boundary.", Rationale: "Analyzer rationale; retained records remain authoritative.",
		Inference: true, Confidence: "medium", Severity: "unavailable",
		Evidence: []EvidenceReference{
			{Index: "behavior", ID: "82782612-7b86-41e9-a488-b465b4c77b61", Role: "semantic_behavior"},
			{Index: "behavior", ID: "effect:safe_sink:ev-656213e0d3ddc7400f324769", Role: "external_effect"},
			{Index: "coverage", ID: "coverage:retrieval_poison", Role: "limitation"},
			{Index: "coverage", ID: "coverage:signed_request_attribution", Role: "limitation"},
		},
		Standards: []StandardReference{
			{Catalog: "CWE", Version: "4.20", ID: "CWE-1426"}, {Catalog: "CWE", Version: "4.20", ID: "CWE-1427"},
			{Catalog: "MITRE_ATLAS", Version: "2026-08-26", ID: "AML.T0051"},
			{Catalog: "OWASP_AGENTIC", Version: "2026", ID: "ASI01"}, {Catalog: "OWASP_AGENTIC", Version: "2026", ID: "ASI02"},
			{Catalog: "OWASP_LLM", Version: "2025", ID: "LLM01"}, {Catalog: "OWASP_LLM", Version: "2025", ID: "LLM06"},
		},
		CoverageGapIDs: []string{"coverage:retrieval_poison", "coverage:signed_request_attribution"},
	}}
	return candidate
}

func privateCandidateCopy(t *testing.T, source string) string {
	t.Helper()
	content, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "candidate.json")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func mastraObservation(t *testing.T) string {
	t.Helper()
	return filepath.Join(repoRoot(t), "plans/260825-1623-lean-openshell-project-assurance/evidence/2026-08-26-phase-02-public-mastra-dashboard-observation-04")
}

func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("../../../..")
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func makePackRemovable(t *testing.T, root string) {
	t.Helper()
	t.Cleanup(func() {
		_ = os.Chmod(filepath.Join(root, "observation"), 0o700)
		_ = os.Chmod(root, 0o700)
	})
}
