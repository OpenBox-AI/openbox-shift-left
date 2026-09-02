package sdkdesc

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/artifact"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/inspect"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/model"
)

type ExpectedState string

const (
	ExpectedEnabled     ExpectedState = "enabled"
	ExpectedUnknown     ExpectedState = "unknown"
	ExpectedNotRunnable ExpectedState = "not_runnable"
)

type GapClassification string

const (
	GapMissing     GapClassification = "missing"
	GapDisabled    GapClassification = "disabled"
	GapBypassed    GapClassification = "bypassed"
	GapUnknown     GapClassification = "unknown"
	GapUnsupported GapClassification = "unsupported"
)

type ReadinessState string

const (
	ReadinessReady        ReadinessState = "ready"
	ReadinessInconclusive ReadinessState = "inconclusive"
	ReadinessNotRunnable  ReadinessState = "not_runnable"
)

type InstrumentationObservation string

const (
	ObservationMissing     InstrumentationObservation = "missing"
	ObservationNotRunnable InstrumentationObservation = "not_runnable"
)

// IntegrationExpectation describes the static middleware integration shape,
// separately from any semantic action-class observation.
type IntegrationExpectation struct {
	State    ExpectedState
	Evidence []artifact.ContentDigest
	Reason   string
}

// InstrumentationExpectation is schema-projectable static planning evidence.
// Observation can only be missing or not_runnable; it never means observed and
// carries no event count.
type InstrumentationExpectation struct {
	ActionClass string
	Required    bool
	State       ExpectedState
	Observation InstrumentationObservation
	Evidence    []artifact.ContentDigest
	Reason      string
}

type SurfaceGap struct {
	Surface        string
	Classification GapClassification
	Reason         string
	Evidence       []artifact.ContentDigest
}

type ReadinessExpectation struct {
	State    ReadinessState
	Probes   []ReadinessProbe
	Evidence []artifact.ContentDigest
	Reason   string
}

// ExpectedCoverage is deterministic static planning data for SE-02-07. It is
// not an openbox.sdk-coverage/v1 runtime observation.
type ExpectedCoverage struct {
	descriptorID    string
	graphDigest     artifact.ContentDigest
	integration     IntegrationExpectation
	instrumentation []InstrumentationExpectation
	exclusions      []SurfaceGap
	gaps            []SurfaceGap
	readiness       ReadinessExpectation
}

func (coverage ExpectedCoverage) DescriptorID() string { return coverage.descriptorID }
func (coverage ExpectedCoverage) Integration() IntegrationExpectation {
	result := coverage.integration
	result.Evidence = append([]artifact.ContentDigest(nil), result.Evidence...)
	return result
}
func (coverage ExpectedCoverage) Instrumentation() []InstrumentationExpectation {
	result := append([]InstrumentationExpectation(nil), coverage.instrumentation...)
	for index := range result {
		result[index].Evidence = append([]artifact.ContentDigest(nil), result[index].Evidence...)
	}
	return result
}
func (coverage ExpectedCoverage) Exclusions() []SurfaceGap { return cloneGaps(coverage.exclusions) }
func (coverage ExpectedCoverage) Gaps() []SurfaceGap       { return cloneGaps(coverage.gaps) }
func (coverage ExpectedCoverage) Readiness() ReadinessExpectation {
	result := coverage.readiness
	result.Probes = append([]ReadinessProbe(nil), result.Probes...)
	result.Evidence = append([]artifact.ContentDigest(nil), result.Evidence...)
	return result
}

// DeriveExpectedCoverage maps passive graph evidence and an exact descriptor
// compatibility result into expected instrumentation, exclusions, gaps, and
// required probes. It cannot produce observed, missing-at-runtime, or ready.
func DeriveExpectedCoverage(graph model.Graph, compatibility Compatibility) (ExpectedCoverage, error) {
	if compatibility.DescriptorID != MastraDescriptorID {
		return ExpectedCoverage{}, errors.New("sdkdesc: compatibility belongs to an unknown descriptor")
	}
	switch compatibility.Status {
	case Compatible:
		if len(compatibility.Problems) != 0 {
			return ExpectedCoverage{}, errors.New("sdkdesc: compatible result contains problems")
		}
	case NotRunnable:
		if len(compatibility.Problems) == 0 {
			return ExpectedCoverage{}, errors.New("sdkdesc: not_runnable result lacks a problem")
		}
	default:
		return ExpectedCoverage{}, fmt.Errorf("sdkdesc: unsupported compatibility status %q", compatibility.Status)
	}
	descriptor := MastraMVP()
	unsupported := make(map[string]struct{}, len(descriptor.UnsupportedActionClasses))
	for _, actionClass := range descriptor.UnsupportedActionClasses {
		unsupported[actionClass] = struct{}{}
	}
	for _, actionClass := range []string{"agent", "approval", "database", "file", "http", "mcp", "model", "retrieval"} {
		if _, ok := unsupported[actionClass]; !ok {
			return ExpectedCoverage{}, fmt.Errorf("sdkdesc: descriptor does not classify required unsupported action %q", actionClass)
		}
	}
	for _, control := range []struct{ option, literal string }{
		{option: "httpCapture", literal: "false"},
		{option: "instrumentDatabases", literal: "false"},
		{option: "instrumentFileIo", literal: "false"},
	} {
		if !descriptorRequiresLiteral(descriptor, control.option, control.literal) {
			return ExpectedCoverage{}, fmt.Errorf("sdkdesc: descriptor does not fix %s=%s", control.option, control.literal)
		}
	}
	descriptorEvidence, err := descriptorDigest(descriptor)
	if err != nil {
		return ExpectedCoverage{}, err
	}
	graphDigest, err := model.GraphDigest(graph)
	if err != nil {
		return ExpectedCoverage{}, err
	}

	nodes := graph.Nodes()
	signals := graph.Signals()
	uncertainties := graph.Uncertainties()
	if len(nodes) == 0 {
		return ExpectedCoverage{}, errors.New("sdkdesc: normalized graph has no nodes")
	}

	sdkEvidence := nodeEvidence(nodes, model.NodeOpenBoxSDK)
	toolEvidence := nodeEvidence(nodes, model.NodeTool)
	dependencyEvidence, exactDependencyEvidence, dependencyMismatch := dependencyEvidence(signals, MastraPackage, "1.0.0")
	importEvidence := signalEvidence(signals, inspect.FactPackageImport, MastraPackage)
	entrypointEvidence := signalEvidence(signals, inspect.FactEntrypoint, "")
	requiredEvidence, requiredEvidenceTruncated := boundedEvidence(
		[]artifact.ContentDigest{descriptorEvidence}, sdkEvidence, toolEvidence, dependencyEvidence, importEvidence, entrypointEvidence,
	)

	integrationComplete := compatibility.Status == Compatible && len(sdkEvidence) != 0 &&
		len(exactDependencyEvidence) != 0 && !dependencyMismatch &&
		len(importEvidence) != 0 && len(entrypointEvidence) != 0
	integration := IntegrationExpectation{Evidence: requiredEvidence}
	switch {
	case compatibility.Status != Compatible:
		integration.State = ExpectedNotRunnable
		integration.Reason = "The exact SDK tuple or withOpenBox initialization is incompatible; no behavioral run may start."
	case integrationComplete:
		integration.State = ExpectedEnabled
		integration.Reason = "The exact declared SDK version, import, entrypoint, and compatible local-clone withOpenBox candidate make the middleware integration statically expected; installed consumer bytes and execution remain unobserved."
	default:
		integration.State = ExpectedUnknown
		integration.Reason = "Passive evidence is insufficient to establish the middleware integration; runtime absence is not inferred."
	}
	if requiredEvidenceTruncated {
		integration.Reason += " Evidence digests are capped at 64."
	}
	instrumentation := InstrumentationExpectation{
		ActionClass: RecordingTool, Required: true, Evidence: requiredEvidence,
	}
	switch {
	case compatibility.Status != Compatible:
		instrumentation.State = ExpectedNotRunnable
		instrumentation.Observation = ObservationNotRunnable
		instrumentation.Reason = "The exact SDK tuple or withOpenBox initialization is incompatible; recordingTool cannot be probed."
	default:
		instrumentation.State = ExpectedUnknown
		instrumentation.Observation = ObservationMissing
		instrumentation.Reason = "The passive detector does not resolve createTool object IDs, so the scenario's exact recording-tool to recordingTool binding remains unknown until a startup probe observes it."
	}
	if requiredEvidenceTruncated {
		instrumentation.Reason += " Evidence digests are capped at 64."
	}

	gapBuilder := newSurfaceBuilder()
	for _, current := range compatibility.Problems {
		digest, digestErr := problemDigest(current)
		if digestErr != nil {
			return ExpectedCoverage{}, digestErr
		}
		gapBuilder.add(false, "sdk-compatibility", GapMissing,
			"The exact local SDK tuple or initialization contract is incompatible, so automatic behavioral readiness is unavailable.", digest)
	}
	if len(dependencyEvidence) == 0 {
		gapBuilder.add(false, "mastra-sdk-dependency", GapMissing,
			"No exact Mastra SDK package dependency was found in passive evidence.", descriptorEvidence)
	} else if len(exactDependencyEvidence) == 0 || dependencyMismatch {
		gapBuilder.add(false, "mastra-sdk-version", GapUnsupported,
			"The declared Mastra SDK dependency does not consistently match the sole local-clone MVP version 1.0.0; automatic behavioral readiness is unavailable.", dependencyEvidence...)
	}
	if len(importEvidence) == 0 {
		gapBuilder.add(false, "mastra-sdk-import", GapUnknown,
			"No exact Mastra SDK import was found; dynamic or runtime loading is not treated as absence.", descriptorEvidence)
	}
	if len(toolEvidence) == 0 {
		gapBuilder.add(false, RecordingTool, GapUnknown,
			"No statically classified tool declaration supports the required recordingTool expectation; runtime registration remains unknown.", descriptorEvidence)
	}
	if len(entrypointEvidence) == 0 {
		gapBuilder.add(false, "project-entrypoint", GapMissing,
			"No declarative or lexical project entrypoint was found for later readiness probing.", descriptorEvidence)
	}

	for _, node := range nodes {
		evidence := provenanceDigests(node.Provenance)
		switch node.Type {
		case model.NodeAgent:
			gapBuilder.add(false, "agent", GapUnsupported, unsupportedReason("agent"), evidence...)
		case model.NodeModelRoute:
			gapBuilder.add(false, "model", GapUnsupported, unsupportedReason("model"), evidence...)
		case model.NodeMCPServer:
			gapBuilder.add(false, "mcp", GapUnsupported, unsupportedReason("MCP"), evidence...)
		case model.NodeRetrieval, model.NodeMemory:
			gapBuilder.add(false, "retrieval", GapUnsupported, unsupportedReason("retrieval and memory"), evidence...)
		case model.NodeApproval:
			gapBuilder.add(false, "approval", GapUnsupported, unsupportedReason("approval"), evidence...)
		case model.NodeFilesystemBoundary:
			gapBuilder.add(true, "file", GapDisabled,
				"File-I/O instrumentation was disabled in the only qualified SDK configuration and is not expected coverage.", evidence...)
		case model.NodeNetworkBoundary:
			gapBuilder.add(true, "http", GapDisabled,
				"HTTP capture was disabled in the only qualified SDK configuration and is not expected coverage.", evidence...)
		case model.NodePersistenceSink:
			gapBuilder.add(true, "database", GapDisabled,
				"Database instrumentation was disabled in the only qualified SDK configuration and is not expected coverage.", evidence...)
		case model.NodeProcessBoundary:
			gapBuilder.add(false, "subprocess", GapBypassed,
				"A passive subprocess call surface lies outside the sole qualified recordingTool semantic gate; reachability remains unobserved.", evidence...)
		case model.NodeExternalDestination:
			gapBuilder.add(false, "external-destination", GapBypassed,
				"A passive external-destination surface is not correlated to the sole qualified recordingTool gate; reachability remains unobserved.", evidence...)
		case model.NodeTelemetrySink:
			gapBuilder.add(false, "telemetry", GapUnsupported, unsupportedReason("telemetry"), evidence...)
		case model.NodeCredentialBoundary:
			gapBuilder.add(false, "credential-boundary", GapUnknown,
				"A credential-name boundary was found, but values and runtime injection are deliberately not inspected.", evidence...)
		case model.NodeTool, model.NodeOpenBoxSDK:
			// These feed the required expectation above.
		default:
			return ExpectedCoverage{}, fmt.Errorf("sdkdesc: unsupported normalized node type %q", node.Type)
		}
	}

	if len(uncertainties) != 0 {
		evidence := make([]artifact.ContentDigest, 0, min(len(uncertainties), 64))
		for _, uncertainty := range uncertainties {
			digest, digestErr := uncertaintyDigest(uncertainty)
			if digestErr != nil {
				return ExpectedCoverage{}, digestErr
			}
			evidence = append(evidence, digest)
		}
		reason := "Passive discovery retained dynamic, opaque, ambiguous, truncated, or unsupported evidence; missing SDK events must remain unknown."
		gapBuilder.add(false, "passive-discovery", GapUnknown, reason, evidence...)
	}

	exclusions, gaps := gapBuilder.finish()
	readiness := ReadinessExpectation{
		Probes: append([]ReadinessProbe(nil), descriptor.ReadinessProbes...), Evidence: requiredEvidence,
	}
	switch {
	case compatibility.Status != Compatible:
		readiness.State = ReadinessNotRunnable
		readiness.Reason = "Descriptor compatibility failed; probes and behavioral execution must not start."
	case !integrationComplete:
		readiness.State = ReadinessInconclusive
		readiness.Reason = "Required passive SDK, exact-version dependency, import, or entrypoint evidence is incomplete or conflicting."
	default:
		readiness.State = ReadinessInconclusive
		readiness.Reason = "Static integration expectations are complete, but both descriptor readiness probes and the exact recording-tool binding remain unexecuted."
	}
	if requiredEvidenceTruncated {
		readiness.Reason += " Evidence digests are capped at 64."
	}
	return ExpectedCoverage{
		descriptorID: descriptor.ID, graphDigest: graphDigest, integration: integration, instrumentation: []InstrumentationExpectation{instrumentation},
		exclusions: exclusions, gaps: gaps, readiness: readiness,
	}, nil
}

type surfaceBuilder struct {
	exclusions map[string]*SurfaceGap
	gaps       map[string]*SurfaceGap
}

func newSurfaceBuilder() *surfaceBuilder {
	return &surfaceBuilder{exclusions: make(map[string]*SurfaceGap), gaps: make(map[string]*SurfaceGap)}
}

func (builder *surfaceBuilder) add(exclusion bool, surface string, classification GapClassification, reason string, evidence ...artifact.ContentDigest) {
	target := builder.gaps
	if exclusion {
		target = builder.exclusions
	}
	key := string(classification) + "\x00" + surface
	current, exists := target[key]
	if !exists {
		current = &SurfaceGap{Surface: surface, Classification: classification, Reason: reason}
		target[key] = current
	}
	var truncated bool
	current.Evidence, truncated = boundedEvidence(current.Evidence, evidence)
	if truncated && !strings.Contains(current.Reason, "Evidence digests are capped at 64.") {
		current.Reason += " Evidence digests are capped at 64."
	}
}

func (builder *surfaceBuilder) finish() ([]SurfaceGap, []SurfaceGap) {
	return sortedGaps(builder.exclusions), sortedGaps(builder.gaps)
}

func sortedGaps(source map[string]*SurfaceGap) []SurfaceGap {
	result := make([]SurfaceGap, 0, len(source))
	for _, current := range source {
		copyOf := *current
		copyOf.Evidence = append([]artifact.ContentDigest(nil), current.Evidence...)
		result = append(result, copyOf)
	}
	sort.Slice(result, func(left, right int) bool {
		if result[left].Surface != result[right].Surface {
			return result[left].Surface < result[right].Surface
		}
		return result[left].Classification < result[right].Classification
	})
	return result
}

func nodeEvidence(nodes []model.Node, nodeType model.NodeType) []artifact.ContentDigest {
	var evidence []artifact.ContentDigest
	for _, node := range nodes {
		if node.Type == nodeType {
			evidence = append(evidence, provenanceDigests(node.Provenance)...)
		}
	}
	return mergeEvidence(evidence)
}

func signalEvidence(signals []model.Signal, kind inspect.FactKind, exactValue string) []artifact.ContentDigest {
	var evidence []artifact.ContentDigest
	for _, signal := range signals {
		if signal.Kind == kind && (exactValue == "" || signal.Value == exactValue) {
			evidence = append(evidence, provenanceDigests(signal.Provenance)...)
		}
	}
	return mergeEvidence(evidence)
}

func dependencyEvidence(signals []model.Signal, name, version string) ([]artifact.ContentDigest, []artifact.ContentDigest, bool) {
	var all []artifact.ContentDigest
	var exact []artifact.ContentDigest
	var mismatch bool
	expected := artifact.DigestBytes([]byte(version))
	for _, signal := range signals {
		if signal.Kind != inspect.FactPackageDependency || signal.Value != name {
			continue
		}
		evidence := provenanceDigests(signal.Provenance)
		all = append(all, evidence...)
		if signal.DeclaredValueDigest == expected {
			exact = append(exact, evidence...)
		} else {
			mismatch = true
		}
	}
	return mergeEvidence(all), mergeEvidence(exact), mismatch
}

func provenanceDigests(provenance []model.Provenance) []artifact.ContentDigest {
	result := make([]artifact.ContentDigest, 0, len(provenance))
	for _, current := range provenance {
		result = append(result, current.Digest)
	}
	return mergeEvidence(result)
}

func mergeEvidence(groups ...[]artifact.ContentDigest) []artifact.ContentDigest {
	unique := make(map[string]artifact.ContentDigest)
	for _, group := range groups {
		for _, digest := range group {
			unique[digest.String()] = digest
		}
	}
	keys := make([]string, 0, len(unique))
	for key := range unique {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]artifact.ContentDigest, len(keys))
	for index, key := range keys {
		result[index] = unique[key]
	}
	return result
}

func boundedEvidence(groups ...[]artifact.ContentDigest) ([]artifact.ContentDigest, bool) {
	result := mergeEvidence(groups...)
	if len(result) <= 64 {
		return result, false
	}
	return append([]artifact.ContentDigest(nil), result[:64]...), true
}

func descriptorDigest(descriptor Descriptor) (artifact.ContentDigest, error) {
	_, digest, err := artifact.DigestCanonicalJSON(descriptor)
	if err != nil {
		return artifact.ContentDigest{}, fmt.Errorf("sdkdesc: canonical descriptor evidence: %w", err)
	}
	return digest, nil
}

func problemDigest(problem Problem) (artifact.ContentDigest, error) {
	_, digest, err := artifact.DigestCanonicalJSON(problem)
	if err != nil {
		return artifact.ContentDigest{}, fmt.Errorf("sdkdesc: canonical compatibility evidence: %w", err)
	}
	return digest, nil
}

func uncertaintyDigest(uncertainty model.Uncertainty) (artifact.ContentDigest, error) {
	prefix := "uncertainty:"
	if strings.HasPrefix(uncertainty.ID, prefix) {
		if digest, err := artifact.ParseContentDigest("sha256:" + strings.TrimPrefix(uncertainty.ID, prefix)); err == nil {
			return digest, nil
		}
	}
	_, digest, err := artifact.DigestCanonicalJSON(uncertainty)
	if err != nil {
		return artifact.ContentDigest{}, fmt.Errorf("sdkdesc: canonical uncertainty evidence: %w", err)
	}
	return digest, nil
}

func unsupportedReason(surface string) string {
	return "The qualified MVP descriptor does not establish " + surface + " semantic coverage; missing events remain unknown."
}

func descriptorRequiresLiteral(descriptor Descriptor, option, literal string) bool {
	for _, rule := range descriptor.Configuration {
		if rule.Option == option {
			return rule.RequiredLiteral == literal && rule.AllowLiteral && !rule.AllowOmission && !rule.AllowEnvironmentRef
		}
	}
	return false
}

func cloneGaps(source []SurfaceGap) []SurfaceGap {
	result := append([]SurfaceGap(nil), source...)
	for index := range result {
		result[index].Evidence = append([]artifact.ContentDigest(nil), result[index].Evidence...)
	}
	return result
}
