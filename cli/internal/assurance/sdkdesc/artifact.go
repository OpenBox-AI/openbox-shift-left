package sdkdesc

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/artifact"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/model"
)

// CoverageArtifacts contains the public static coverage object plus guidance
// for the next explicit execution boundary. Guidance is not evidence and is
// deliberately not embedded in the schema object.
type CoverageArtifacts struct {
	SDKCoverage artifact.Object
	Guidance    ReadinessGuidance
}

type ReadinessGuidance struct {
	Status  ReadinessState
	Summary string
	Actions []string
}

// BuildCoverageArtifacts projects expected coverage without inventing runtime
// event counts or probe results. It accepts only a project-model object so the
// public digest relationship cannot be bound to an unrelated role.
func BuildCoverageArtifacts(project model.ProjectArtifacts, coverage ExpectedCoverage) (CoverageArtifacts, error) {
	projectModel := project.ProjectModel()
	if projectModel.Role() != artifact.RoleProjectModel {
		return CoverageArtifacts{}, errors.New("sdkdesc: SDK coverage requires a project-model object")
	}
	if project.GraphDigest() == (artifact.ContentDigest{}) || project.GraphDigest() != coverage.graphDigest {
		return CoverageArtifacts{}, errors.New("sdkdesc: expected coverage belongs to a different normalized graph")
	}
	if coverage.descriptorID != MastraDescriptorID {
		return CoverageArtifacts{}, errors.New("sdkdesc: expected coverage has an unknown descriptor")
	}
	if err := validateExpectedCoverage(coverage); err != nil {
		return CoverageArtifacts{}, err
	}
	descriptor := MastraMVP()
	frameworkAdapter, ok := descriptorComponent(descriptor, MastraPackage)
	if !ok {
		return CoverageArtifacts{}, errors.New("sdkdesc: descriptor lacks the Mastra adapter component")
	}
	baseSDK, ok := descriptorComponent(descriptor, BaseAlias)
	if !ok {
		return CoverageArtifacts{}, errors.New("sdkdesc: descriptor lacks the base SDK component")
	}
	framework, ok := descriptorComponent(descriptor, MastraCore)
	if !ok {
		return CoverageArtifacts{}, errors.New("sdkdesc: descriptor lacks the Mastra Core component")
	}

	instrumentation := coverage.instrumentation[0]
	value := sdkCoverageDocument{
		APIVersion:         "openbox.sdk-coverage/v1",
		Kind:               "SDKCoverage",
		ProjectModelDigest: projectModel.Digest(),
		Descriptor: sdkCoverageDescriptor{
			ID:               descriptor.ID,
			FrameworkAdapter: sdkCoverageComponent{Name: frameworkAdapter.Resolved, Version: frameworkAdapter.Version},
			BaseSDK:          sdkCoverageComponent{Name: baseSDK.Resolved, Version: baseSDK.Version},
			Framework:        sdkCoverageComponent{Name: framework.Resolved, Version: framework.Version},
		},
		Instrumentation: []sdkCoverageInstrumentation{{
			ActionClass: instrumentation.ActionClass, Expectation: "required", Observation: instrumentation.Observation,
			EventCount: 0, Evidence: append([]artifact.ContentDigest(nil), instrumentation.Evidence...),
		}},
		Exclusions: projectGaps(coverage.exclusions),
		Gaps:       projectGaps(coverage.gaps),
		Readiness: sdkCoverageReadiness{
			Status: coverage.readiness.State, ProbeCount: 0,
			Evidence: append([]artifact.ContentDigest(nil), coverage.readiness.Evidence...), Reason: coverageReason(coverage.readiness.Reason),
		},
	}
	coverageSchema := "openbox.sdk-coverage/v1"
	object, err := artifact.NewCanonicalObject(
		artifact.RoleSDKCoverage, "application/json", &coverageSchema, "normalized", value,
	)
	if err != nil {
		return CoverageArtifacts{}, fmt.Errorf("sdkdesc: build SDK-coverage object: %w", err)
	}
	return CoverageArtifacts{SDKCoverage: object, Guidance: readinessGuidance(coverage)}, nil
}

type sdkCoverageDocument struct {
	APIVersion         string                       `json:"apiVersion"`
	Kind               string                       `json:"kind"`
	ProjectModelDigest artifact.ContentDigest       `json:"projectModelDigest"`
	Descriptor         sdkCoverageDescriptor        `json:"descriptor"`
	Instrumentation    []sdkCoverageInstrumentation `json:"instrumentation"`
	Exclusions         []sdkCoverageGap             `json:"exclusions"`
	Gaps               []sdkCoverageGap             `json:"gaps"`
	Readiness          sdkCoverageReadiness         `json:"readiness"`
}

type sdkCoverageDescriptor struct {
	ID               string               `json:"id"`
	FrameworkAdapter sdkCoverageComponent `json:"frameworkAdapter"`
	BaseSDK          sdkCoverageComponent `json:"baseSDK"`
	Framework        sdkCoverageComponent `json:"framework"`
}

type sdkCoverageComponent struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type sdkCoverageInstrumentation struct {
	ActionClass string                     `json:"actionClass"`
	Expectation string                     `json:"expectation"`
	Observation InstrumentationObservation `json:"observation"`
	EventCount  int64                      `json:"eventCount"`
	Evidence    []artifact.ContentDigest   `json:"evidence"`
}

type sdkCoverageGap struct {
	Surface        string                   `json:"surface"`
	Classification GapClassification        `json:"classification"`
	Reason         string                   `json:"reason"`
	Evidence       []artifact.ContentDigest `json:"evidence"`
}

type sdkCoverageReadiness struct {
	Status     ReadinessState           `json:"status"`
	ProbeCount int64                    `json:"probeCount"`
	Evidence   []artifact.ContentDigest `json:"evidence"`
	Reason     *string                  `json:"reason"`
}

func coverageReason(value string) *string { return &value }

func validateExpectedCoverage(coverage ExpectedCoverage) error {
	if coverage.integration.State != ExpectedEnabled && coverage.integration.State != ExpectedUnknown && coverage.integration.State != ExpectedNotRunnable {
		return errors.New("sdkdesc: invalid integration expectation")
	}
	if len(coverage.instrumentation) != 1 {
		return errors.New("sdkdesc: expected exactly one instrumentation row")
	}
	instrumentation := coverage.instrumentation[0]
	validInstrumentationState := instrumentation.State == ExpectedUnknown && instrumentation.Observation == ObservationMissing ||
		instrumentation.State == ExpectedNotRunnable && instrumentation.Observation == ObservationNotRunnable
	if instrumentation.ActionClass != RecordingTool || !instrumentation.Required || !validInstrumentationState ||
		!validEvidence(instrumentation.Evidence) {
		return errors.New("sdkdesc: required instrumentation is not schema-projectable")
	}
	if coverage.readiness.State != ReadinessInconclusive && coverage.readiness.State != ReadinessNotRunnable ||
		coverage.readiness.State == ReadinessNotRunnable != (instrumentation.Observation == ObservationNotRunnable) ||
		!validEvidence(coverage.readiness.Evidence) ||
		coverage.readiness.Reason == "" || utf8.RuneCountInString(coverage.readiness.Reason) > 4096 {
		return errors.New("sdkdesc: readiness is not schema-projectable")
	}
	for _, group := range [][]SurfaceGap{coverage.exclusions, coverage.gaps} {
		for _, gap := range group {
			if gap.Surface == "" || utf8.RuneCountInString(gap.Surface) > 256 ||
				!validGapClassification(gap.Classification) || gap.Reason == "" || utf8.RuneCountInString(gap.Reason) > 4096 ||
				!validEvidence(gap.Evidence) {
				return fmt.Errorf("sdkdesc: surface %q is not schema-projectable", gap.Surface)
			}
		}
	}
	return nil
}

func validEvidence(evidence []artifact.ContentDigest) bool {
	if len(evidence) == 0 || len(evidence) > 64 {
		return false
	}
	previous := ""
	for _, digest := range evidence {
		if digest == (artifact.ContentDigest{}) || digest.String() <= previous {
			return false
		}
		previous = digest.String()
	}
	return true
}

func descriptorComponent(descriptor Descriptor, requested string) (Component, bool) {
	for _, component := range descriptor.Components {
		if component.Requested == requested {
			return component, true
		}
	}
	return Component{}, false
}

func projectGaps(source []SurfaceGap) []sdkCoverageGap {
	result := make([]sdkCoverageGap, len(source))
	for index, current := range source {
		result[index] = sdkCoverageGap{
			Surface: current.Surface, Classification: current.Classification, Reason: current.Reason,
			Evidence: append([]artifact.ContentDigest(nil), current.Evidence...),
		}
	}
	return result
}

func validGapClassification(classification GapClassification) bool {
	switch classification {
	case GapMissing, GapDisabled, GapBypassed, GapUnknown, GapUnsupported:
		return true
	default:
		return false
	}
}

func readinessGuidance(coverage ExpectedCoverage) ReadinessGuidance {
	guidance := ReadinessGuidance{Status: coverage.readiness.State, Summary: coverage.readiness.Reason}
	if coverage.readiness.State == ReadinessNotRunnable {
		guidance.Actions = []string{"Correct the exact local Mastra tuple or withOpenBox configuration before a future evaluation; do not start the project process."}
		return guidance
	}
	if coverage.integration.State == ExpectedEnabled {
		guidance.Actions = append(guidance.Actions, "Bind the project to the qualified local openbox-mastra-sdk commit and verify installed bytes before launch; an exact declaration alone is insufficient.")
	} else {
		guidance.Actions = append(guidance.Actions, "Resolve the missing or conflicting SDK dependency, import, entrypoint, and initialization evidence before launch.")
	}
	probeIDs := make([]string, len(coverage.readiness.Probes))
	for index, probe := range coverage.readiness.Probes {
		probeIDs[index] = probe.ID
	}
	sort.Strings(probeIDs)
	guidance.Actions = append(guidance.Actions,
		"Run the required startup probes before a future evaluation: "+strings.Join(probeIDs, ", ")+".",
		"Treat a missing recordingTool event as missing coverage, never as evidence that no action occurred.",
	)
	return guidance
}
