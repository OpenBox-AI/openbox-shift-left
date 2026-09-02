//go:build darwin || linux

package sdkdesc

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/inspect"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/model"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/snapshot"
)

func TestDeriveExpectedCoverageClassifiesEnabledDisabledBypassedAndUnknown(t *testing.T) {
	sentinel := filepath.Join(t.TempDir(), "must-not-run")
	graph := coverageGraphFixture(t, map[string][]byte{
		"package.json": []byte(fmt.Sprintf(`{
  "main":"src/index.ts",
  "scripts":{"postinstall":"touch %s"},
  "dependencies":{"@openbox-ai/openbox-mastra-sdk":"1.0.0"}
}`, sentinel)),
		"src/index.ts": []byte(`import { withOpenBox } from "@openbox-ai/openbox-mastra-sdk";
const agent = createAgent({});
const tool = createTool({ id: "recording-tool" });
fetch("https://sink.invalid/path?secret=removed");
readFile("fixture.txt");
query("select safe");
spawn("worker");
import(packageName);
`),
	})
	compatibility := Validate(qualifiedCandidate(qualifiedInitialization()))
	first, err := DeriveExpectedCoverage(graph, compatibility)
	if err != nil {
		t.Fatal(err)
	}
	second, err := DeriveExpectedCoverage(graph, compatibility)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("repeated expected-coverage derivation changed output")
	}
	if first.DescriptorID() != MastraDescriptorID {
		t.Fatalf("descriptor = %q", first.DescriptorID())
	}
	integration := first.Integration()
	if integration.State != ExpectedEnabled || len(integration.Evidence) == 0 ||
		!strings.Contains(integration.Reason, "installed consumer bytes") {
		t.Fatalf("exact declared integration expectation was widened or lost: %#v", integration)
	}
	instrumentation := first.Instrumentation()
	if len(instrumentation) != 1 || instrumentation[0].ActionClass != RecordingTool || !instrumentation[0].Required ||
		instrumentation[0].State != ExpectedUnknown || instrumentation[0].Observation != ObservationMissing ||
		len(instrumentation[0].Evidence) == 0 || strings.Contains(strings.ToLower(instrumentation[0].Reason), "observed") {
		t.Fatalf("static required expectation was widened or lost: %#v", instrumentation)
	}
	readiness := first.Readiness()
	if readiness.State != ReadinessInconclusive || len(readiness.Probes) != 2 || len(readiness.Evidence) == 0 ||
		!strings.Contains(readiness.Reason, "unexecuted") {
		t.Fatalf("static evidence incorrectly became ready: %#v", readiness)
	}
	for _, surface := range []string{"database", "file", "http"} {
		assertSurface(t, first.Exclusions(), surface, GapDisabled)
	}
	for _, surface := range []string{"external-destination", "subprocess"} {
		assertSurface(t, first.Gaps(), surface, GapBypassed)
	}
	assertSurface(t, first.Gaps(), "agent", GapUnsupported)
	assertSurface(t, first.Gaps(), "passive-discovery", GapUnknown)
	if _, err := os.Lstat(sentinel); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("coverage derivation executed package code or sentinel lookup failed: %v", err)
	}
}

func TestDeriveExpectedCoverageFailsClosedForCompatibilityAndMissingEvidence(t *testing.T) {
	complete := coverageGraphFixture(t, map[string][]byte{
		"package.json": []byte(`{"main":"src/index.ts","dependencies":{"@openbox-ai/openbox-mastra-sdk":"1.0.0"}}`),
		"src/index.ts": []byte(`import "@openbox-ai/openbox-mastra-sdk"; createTool({});`),
	})
	candidate := qualifiedCandidate(qualifiedInitialization())
	candidate.Initializations[0].Validate = ControlBinding{Shape: BindingLiteral, Literal: "false"}
	coverage, err := DeriveExpectedCoverage(complete, Validate(candidate))
	if err != nil {
		t.Fatal(err)
	}
	if coverage.Integration().State != ExpectedNotRunnable || coverage.Instrumentation()[0].State != ExpectedNotRunnable ||
		coverage.Instrumentation()[0].Observation != ObservationNotRunnable || coverage.Readiness().State != ReadinessNotRunnable {
		t.Fatalf("incompatible descriptor became runnable: %#v", coverage)
	}
	assertSurface(t, coverage.Gaps(), "sdk-compatibility", GapMissing)

	incomplete := coverageGraphFixture(t, map[string][]byte{
		"package.json": []byte(`{"dependencies":{"@openbox-ai/openbox-mastra-sdk":"1.0.0"}}`),
		"src/tool.ts":  []byte(`createTool({});`),
	})
	coverage, err = DeriveExpectedCoverage(incomplete, Validate(qualifiedCandidate(qualifiedInitialization())))
	if err != nil {
		t.Fatal(err)
	}
	if coverage.Integration().State != ExpectedUnknown || coverage.Instrumentation()[0].State != ExpectedUnknown ||
		coverage.Instrumentation()[0].Observation != ObservationMissing || coverage.Readiness().State != ReadinessInconclusive {
		t.Fatalf("incomplete passive evidence became enabled: %#v", coverage)
	}
	assertSurface(t, coverage.Gaps(), "mastra-sdk-import", GapUnknown)
	assertSurface(t, coverage.Gaps(), "project-entrypoint", GapMissing)
}

func TestDeriveExpectedCoverageRejectsUnsupportedDeclaredSDKVersion(t *testing.T) {
	graph := coverageGraphFixture(t, map[string][]byte{
		"package.json": []byte(`{"main":"src/index.ts","dependencies":{"@openbox-ai/openbox-mastra-sdk":"99.0.0"}}`),
		"src/index.ts": []byte(`import "@openbox-ai/openbox-mastra-sdk"; createTool({ id: "recording-tool" });`),
	})
	coverage, err := DeriveExpectedCoverage(graph, Validate(qualifiedCandidate(qualifiedInitialization())))
	if err != nil {
		t.Fatal(err)
	}
	if coverage.Integration().State != ExpectedUnknown || coverage.Readiness().State != ReadinessInconclusive {
		t.Fatalf("unsupported declared version became an expected runnable integration: %#v", coverage)
	}
	assertSurface(t, coverage.Gaps(), "mastra-sdk-version", GapUnsupported)

	conflicting := coverageGraphFixture(t, map[string][]byte{
		"package.json": []byte(`{
  "main":"src/index.ts",
  "dependencies":{"@openbox-ai/openbox-mastra-sdk":"1.0.0"},
  "devDependencies":{"@openbox-ai/openbox-mastra-sdk":"99.0.0"}
}`),
		"src/index.ts": []byte(`import "@openbox-ai/openbox-mastra-sdk"; createTool({ id: "recording-tool" });`),
	})
	coverage, err = DeriveExpectedCoverage(conflicting, Validate(qualifiedCandidate(qualifiedInitialization())))
	if err != nil {
		t.Fatal(err)
	}
	if coverage.Integration().State != ExpectedUnknown || coverage.Readiness().State != ReadinessInconclusive {
		t.Fatalf("conflicting declared versions became an expected runnable integration: %#v", coverage)
	}
	assertSurface(t, coverage.Gaps(), "mastra-sdk-version", GapUnsupported)
}

func TestExpectedCoverageCapsUncertaintyEvidenceAndReturnsDefensiveCopies(t *testing.T) {
	var source strings.Builder
	source.WriteString(`import "@openbox-ai/openbox-mastra-sdk"; createTool({});`)
	for index := 0; index < 80; index++ {
		fmt.Fprintf(&source, "\nimport(dynamic%d);", index)
	}
	graph := coverageGraphFixture(t, map[string][]byte{
		"package.json": []byte(`{"main":"src/index.ts","dependencies":{"@openbox-ai/openbox-mastra-sdk":"1.0.0"}}`),
		"src/index.ts": []byte(source.String()),
	})
	coverage, err := DeriveExpectedCoverage(graph, Validate(qualifiedCandidate(qualifiedInitialization())))
	if err != nil {
		t.Fatal(err)
	}
	unknown := findSurface(t, coverage.Gaps(), "passive-discovery", GapUnknown)
	if len(unknown.Evidence) != 64 || !strings.Contains(unknown.Reason, "capped at 64") {
		t.Fatalf("uncertainty evidence cap is not explicit: %#v", unknown)
	}
	instrumentation := coverage.Instrumentation()
	integration := coverage.Integration()
	exclusions := coverage.Exclusions()
	gaps := coverage.Gaps()
	readiness := coverage.Readiness()
	integration.Evidence[0][0] ^= 0xff
	instrumentation[0].Evidence[0][0] ^= 0xff
	if len(exclusions) != 0 {
		exclusions[0].Evidence[0][0] ^= 0xff
	}
	gaps[0].Evidence[0][0] ^= 0xff
	readiness.Probes[0].ID = "changed"
	readiness.Evidence[0][0] ^= 0xff
	if reflect.DeepEqual(integration, coverage.Integration()) || reflect.DeepEqual(instrumentation, coverage.Instrumentation()) ||
		reflect.DeepEqual(gaps, coverage.Gaps()) || reflect.DeepEqual(readiness, coverage.Readiness()) {
		t.Fatal("expected coverage exposed retained nested storage")
	}
}

func TestDeriveExpectedCoverageRejectsUnknownDescriptorAndEmptyGraph(t *testing.T) {
	compatibility := Validate(qualifiedCandidate(qualifiedInitialization()))
	compatibility.DescriptorID = "unknown"
	if _, err := DeriveExpectedCoverage(model.Graph{}, compatibility); err == nil || !strings.Contains(err.Error(), "unknown descriptor") {
		t.Fatalf("unknown descriptor error = %v", err)
	}
	compatibility.DescriptorID = MastraDescriptorID
	if _, err := DeriveExpectedCoverage(model.Graph{}, compatibility); err == nil || !strings.Contains(err.Error(), "no nodes") {
		t.Fatalf("empty graph error = %v", err)
	}
	compatibility.Status = Status("invented")
	if _, err := DeriveExpectedCoverage(model.Graph{}, compatibility); err == nil || !strings.Contains(err.Error(), "unsupported compatibility status") {
		t.Fatalf("unknown compatibility status error = %v", err)
	}
	compatibility.Status = Compatible
	compatibility.Problems = []Problem{{Code: "contradiction"}}
	if _, err := DeriveExpectedCoverage(model.Graph{}, compatibility); err == nil || !strings.Contains(err.Error(), "contains problems") {
		t.Fatalf("contradictory compatible result error = %v", err)
	}
	compatibility.Status = NotRunnable
	compatibility.Problems = nil
	if _, err := DeriveExpectedCoverage(model.Graph{}, compatibility); err == nil || !strings.Contains(err.Error(), "lacks a problem") {
		t.Fatalf("problem-free not_runnable error = %v", err)
	}
}

func TestDeriveExpectedCoverageDoesNotTreatSelectedSDKSourceAsIntegratedProject(t *testing.T) {
	projectRoot := os.Getenv("OPENBOX_MASTRA_SDK_ROOT")
	if projectRoot == "" {
		t.Skip("set OPENBOX_MASTRA_SDK_ROOT to the qualified local clone for the MVP integration")
	}
	graph := coverageGraphFromRoot(t, projectRoot)
	coverage, err := DeriveExpectedCoverage(graph, Validate(qualifiedCandidate(qualifiedInitialization())))
	if err != nil {
		t.Fatal(err)
	}
	if coverage.Integration().State != ExpectedUnknown || coverage.Instrumentation()[0].State != ExpectedUnknown ||
		coverage.Instrumentation()[0].Observation != ObservationMissing || coverage.Readiness().State != ReadinessInconclusive {
		t.Fatalf("SDK source checkout was mistaken for an integrated runnable project: %#v", coverage)
	}
	assertSurface(t, coverage.Gaps(), "mastra-sdk-dependency", GapMissing)
	assertSurface(t, coverage.Gaps(), "mastra-sdk-import", GapUnknown)
}

func coverageGraphFixture(t *testing.T, files map[string][]byte) model.Graph {
	t.Helper()
	_, graph := coverageArtifactsFixture(t, files)
	return graph
}

func coverageArtifactsFixture(t *testing.T, files map[string][]byte) (*snapshot.Snapshot, model.Graph) {
	t.Helper()
	projectRoot := t.TempDir()
	for relative, content := range files {
		full := filepath.Join(projectRoot, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return coverageArtifactsFromRoot(t, projectRoot)
}

func coverageGraphFromRoot(t *testing.T, projectRoot string) model.Graph {
	t.Helper()
	_, graph := coverageArtifactsFromRoot(t, projectRoot)
	return graph
}

func coverageArtifactsFromRoot(t *testing.T, projectRoot string) (*snapshot.Snapshot, model.Graph) {
	t.Helper()
	destinationParent := t.TempDir()
	destination := filepath.Join(destinationParent, "snapshot")
	if err := os.Mkdir(destination, 0o700); err != nil {
		t.Fatal(err)
	}
	project, err := snapshot.Resolve(projectRoot, snapshot.Boundaries{
		AuditOutput: filepath.Join(projectRoot, ".openbox", "audit", "current"),
		TempParent:  destinationParent,
	})
	if err != nil {
		t.Fatal(err)
	}
	copied, err := project.Copy(destination)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { makeFixtureWritable(destination) })
	detection, err := inspect.Detect(copied)
	if err != nil {
		t.Fatal(err)
	}
	graph, err := model.Normalize(detection)
	if err != nil {
		t.Fatal(err)
	}
	return copied, graph
}

func assertSurface(t *testing.T, gaps []SurfaceGap, surface string, classification GapClassification) {
	t.Helper()
	_ = findSurface(t, gaps, surface, classification)
}

func findSurface(t *testing.T, gaps []SurfaceGap, surface string, classification GapClassification) SurfaceGap {
	t.Helper()
	for _, gap := range gaps {
		if gap.Surface == surface && gap.Classification == classification {
			if gap.Reason == "" || len(gap.Evidence) == 0 {
				t.Fatalf("surface %s lacks reason or evidence: %#v", surface, gap)
			}
			return gap
		}
	}
	t.Fatalf("surface %s/%s absent from %#v", surface, classification, gaps)
	return SurfaceGap{}
}
