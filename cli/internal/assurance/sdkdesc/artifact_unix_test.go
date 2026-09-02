//go:build darwin || linux

package sdkdesc

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/artifact"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/model"
)

func TestBuildInspectionArtifactsAreCanonicalDeterministicAndTruthful(t *testing.T) {
	copied, graph := coverageArtifactsFixture(t, map[string][]byte{
		"package.json": []byte(`{"main":"src/index.ts","dependencies":{"@openbox-ai/openbox-mastra-sdk":"1.0.0"}}`),
		"src/index.ts": []byte(`import { withOpenBox } from "@openbox-ai/openbox-mastra-sdk";
const tool = createTool({ id: "recording-tool" });
fetch("https://sink.invalid/effect");
`),
	})
	project, err := model.BuildProjectArtifacts(copied, graph, model.ProjectIdentity{Name: "mastra-mvp"})
	if err != nil {
		t.Fatal(err)
	}
	coverage, err := DeriveExpectedCoverage(graph, Validate(qualifiedCandidate(qualifiedInitialization())))
	if err != nil {
		t.Fatal(err)
	}
	first, err := BuildCoverageArtifacts(project, coverage)
	if err != nil {
		t.Fatal(err)
	}
	repeatedProject, err := model.BuildProjectArtifacts(copied, graph, model.ProjectIdentity{Name: "mastra-mvp"})
	if err != nil {
		t.Fatal(err)
	}
	repeatedCoverage, err := BuildCoverageArtifacts(repeatedProject, coverage)
	if err != nil {
		t.Fatal(err)
	}
	if project.SnapshotManifest().Role() != artifact.RoleProjectSnapshot || project.ProjectModel().Role() != artifact.RoleProjectModel ||
		first.SDKCoverage.Role() != artifact.RoleSDKCoverage {
		t.Fatalf("inspection objects have wrong roles: %q %q %q", project.SnapshotManifest().Role(), project.ProjectModel().Role(), first.SDKCoverage.Role())
	}
	if !bytes.Equal(project.SnapshotManifest().Bytes(), copied.Manifest()) ||
		!bytes.Equal(project.ProjectModel().Bytes(), repeatedProject.ProjectModel().Bytes()) ||
		!bytes.Equal(first.SDKCoverage.Bytes(), repeatedCoverage.SDKCoverage.Bytes()) ||
		project.ProjectModel().Digest() != repeatedProject.ProjectModel().Digest() || first.SDKCoverage.Digest() != repeatedCoverage.SDKCoverage.Digest() ||
		!reflect.DeepEqual(first.Guidance, repeatedCoverage.Guidance) {
		t.Fatal("repeated inspection artifact projection changed bytes, identity, or guidance")
	}
	if canonical, err := artifact.CanonicalizeJSON(project.ProjectModel().Bytes()); err != nil || !bytes.Equal(canonical, project.ProjectModel().Bytes()) {
		t.Fatalf("project model is not canonical JSON: %v", err)
	}
	if canonical, err := artifact.CanonicalizeJSON(first.SDKCoverage.Bytes()); err != nil || !bytes.Equal(canonical, first.SDKCoverage.Bytes()) {
		t.Fatalf("SDK coverage is not canonical JSON: %v", err)
	}

	var publicModel struct {
		Project struct {
			Root string `json:"root"`
			Git  struct {
				Present bool    `json:"present"`
				Head    *string `json:"head"`
				Dirty   *bool   `json:"dirty"`
			} `json:"git"`
		} `json:"project"`
		Nodes []struct {
			ID         string `json:"id"`
			Provenance []struct {
				Detector string `json:"detector"`
				Path     string `json:"path"`
				Line     int64  `json:"line"`
			} `json:"provenance"`
		} `json:"nodes"`
		Edges []json.RawMessage `json:"edges"`
	}
	if err := json.Unmarshal(project.ProjectModel().Bytes(), &publicModel); err != nil {
		t.Fatal(err)
	}
	if publicModel.Project.Root != "." || publicModel.Project.Git.Present || publicModel.Project.Git.Head != nil || publicModel.Project.Git.Dirty != nil ||
		len(publicModel.Nodes) == 0 || publicModel.Edges == nil || len(publicModel.Edges) != 0 {
		t.Fatalf("project-model projection changed frozen fields: %#v", publicModel)
	}
	for _, node := range publicModel.Nodes {
		if node.ID == "" || len(node.Provenance) == 0 {
			t.Fatalf("project node lacks identity or provenance: %#v", node)
		}
		for _, provenance := range node.Provenance {
			if provenance.Detector == "" || provenance.Path == "" || provenance.Line < 1 {
				t.Fatalf("project provenance is incomplete: %#v", provenance)
			}
		}
	}
	for _, forbidden := range []string{`"value"`, `"confidence"`, `"column"`} {
		if bytes.Contains(project.ProjectModel().Bytes(), []byte(forbidden)) {
			t.Fatalf("public project model widened the frozen provenance/node contract with %s", forbidden)
		}
	}

	var publicCoverage struct {
		ProjectModelDigest artifact.ContentDigest `json:"projectModelDigest"`
		Instrumentation    []struct {
			ActionClass string                     `json:"actionClass"`
			Observation InstrumentationObservation `json:"observation"`
			EventCount  int64                      `json:"eventCount"`
			Evidence    []artifact.ContentDigest   `json:"evidence"`
		} `json:"instrumentation"`
		Readiness struct {
			Status     ReadinessState `json:"status"`
			ProbeCount int64          `json:"probeCount"`
			Reason     string         `json:"reason"`
		} `json:"readiness"`
	}
	if err := json.Unmarshal(first.SDKCoverage.Bytes(), &publicCoverage); err != nil {
		t.Fatal(err)
	}
	if publicCoverage.ProjectModelDigest != project.ProjectModel().Digest() || len(publicCoverage.Instrumentation) != 1 ||
		publicCoverage.Instrumentation[0].ActionClass != RecordingTool || publicCoverage.Instrumentation[0].Observation != ObservationMissing ||
		publicCoverage.Instrumentation[0].EventCount != 0 || len(publicCoverage.Instrumentation[0].Evidence) == 0 ||
		publicCoverage.Readiness.Status != ReadinessInconclusive || publicCoverage.Readiness.ProbeCount != 0 || publicCoverage.Readiness.Reason == "" {
		t.Fatalf("static bytes were upgraded to runtime coverage or lost their binding: %#v", publicCoverage)
	}
	joinedGuidance := strings.Join(first.Guidance.Actions, " ")
	for _, required := range []string{"qualified local openbox-mastra-sdk commit", "sdk-auth", "recording-tool-pre-effect", "never as evidence that no action occurred"} {
		if !strings.Contains(joinedGuidance, required) {
			t.Fatalf("readiness guidance lacks %q: %#v", required, first.Guidance)
		}
	}
}

func TestBuildInspectionArtifactsRejectsUnboundAndUnprojectableInputs(t *testing.T) {
	projectArtifactsType := reflect.TypeOf(model.ProjectArtifacts{})
	for index := 0; index < projectArtifactsType.NumField(); index++ {
		if projectArtifactsType.Field(index).IsExported() {
			t.Fatalf("project artifact binding field %q is caller-replaceable", projectArtifactsType.Field(index).Name)
		}
	}
	copied, graph := coverageArtifactsFixture(t, map[string][]byte{
		"package.json": []byte(`{"main":"src/index.ts","dependencies":{"@openbox-ai/openbox-mastra-sdk":"1.0.0"}}`),
		"src/index.ts": []byte(`import "@openbox-ai/openbox-mastra-sdk"; createTool({});`),
	})
	unknownGit, err := model.BuildProjectArtifacts(copied, graph, model.ProjectIdentity{
		Name: "fixture", Git: model.GitState{Present: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	var unknownGitModel struct {
		Project struct {
			Git struct {
				Present bool    `json:"present"`
				Head    *string `json:"head"`
				Dirty   *bool   `json:"dirty"`
			} `json:"git"`
		} `json:"project"`
		Uncertainties []struct {
			Subject       string `json:"subject"`
			Reason        string `json:"reason"`
			EvidenceLevel string `json:"evidenceLevel"`
		} `json:"uncertainties"`
	}
	if err := json.Unmarshal(unknownGit.ProjectModel().Bytes(), &unknownGitModel); err != nil {
		t.Fatal(err)
	}
	if !unknownGitModel.Project.Git.Present || unknownGitModel.Project.Git.Head != nil || unknownGitModel.Project.Git.Dirty != nil ||
		len(unknownGitModel.Uncertainties) == 0 || unknownGitModel.Uncertainties[len(unknownGitModel.Uncertainties)-1].Subject != "git-status" {
		t.Fatalf("unknown Git state lacks explicit uncertainty: %#v", unknownGitModel)
	}
	if _, err := model.BuildProjectArtifacts(nil, graph, model.ProjectIdentity{Name: "fixture"}); err == nil {
		t.Fatal("nil snapshot was accepted")
	}
	for _, identity := range []model.ProjectIdentity{
		{Name: ""},
		{Name: "bad name"},
		{Name: "fixture", Git: model.GitState{Present: false, Dirty: boolPointer(false)}},
		{Name: "fixture", Git: model.GitState{Present: true, Head: stringPointer("0123456789abcdef0123456789abcdef01234567")}},
		{Name: "fixture", Git: model.GitState{Present: true, Head: stringPointer("abc"), Dirty: boolPointer(false)}},
	} {
		if _, err := model.BuildProjectArtifacts(copied, graph, identity); err == nil {
			t.Fatalf("invalid project identity was accepted: %#v", identity)
		}
	}
	different, differentGraph := coverageArtifactsFixture(t, map[string][]byte{
		"package.json": []byte(`{"main":"src/index.ts","dependencies":{"@openbox-ai/openbox-mastra-sdk":"1.0.0"}}`),
		"src/index.ts": []byte(`import "@openbox-ai/openbox-mastra-sdk"; createTool({ changed: true });`),
	})
	if _, err := model.BuildProjectArtifacts(different, graph, model.ProjectIdentity{Name: "fixture"}); err == nil || !strings.Contains(err.Error(), "does not resolve") {
		t.Fatalf("graph-to-snapshot mismatch error = %v", err)
	}
	differentProject, err := model.BuildProjectArtifacts(different, differentGraph, model.ProjectIdentity{Name: "fixture"})
	if err != nil {
		t.Fatal(err)
	}

	project, err := model.BuildProjectArtifacts(copied, graph, model.ProjectIdentity{Name: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	coverage, err := DeriveExpectedCoverage(graph, Validate(qualifiedCandidate(qualifiedInitialization())))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := BuildCoverageArtifacts(differentProject, coverage); err == nil || !strings.Contains(err.Error(), "different normalized graph") {
		t.Fatalf("coverage-to-project graph mismatch error = %v", err)
	}
	if _, err := BuildCoverageArtifacts(model.ProjectArtifacts{}, coverage); err == nil || !strings.Contains(err.Error(), "project-model object") {
		t.Fatalf("wrong-role project digest error = %v", err)
	}
	malformed := coverage
	malformed.instrumentation = nil
	if _, err := BuildCoverageArtifacts(project, malformed); err == nil || !strings.Contains(err.Error(), "one instrumentation") {
		t.Fatalf("missing instrumentation error = %v", err)
	}
	malformed = coverage
	malformed.readiness.State = ReadinessState("ready")
	if _, err := BuildCoverageArtifacts(project, malformed); err == nil || !strings.Contains(err.Error(), "readiness") {
		t.Fatalf("ready static coverage error = %v", err)
	}
	malformed = coverage
	malformed.instrumentation[0].State = ExpectedEnabled
	if _, err := BuildCoverageArtifacts(project, malformed); err == nil || !strings.Contains(err.Error(), "instrumentation") {
		t.Fatalf("enabled instrumentation projection error = %v", err)
	}
	malformed = coverage
	malformed.instrumentation = append([]InstrumentationExpectation(nil), coverage.instrumentation...)
	malformed.instrumentation[0].Evidence = []artifact.ContentDigest{{}}
	if _, err := BuildCoverageArtifacts(project, malformed); err == nil || !strings.Contains(err.Error(), "instrumentation") {
		t.Fatalf("zero evidence projection error = %v", err)
	}
}

func boolPointer(value bool) *bool       { return &value }
func stringPointer(value string) *string { return &value }
