package report

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/artifact"
)

const (
	proposalID            = "proposal-ASI02-INDIRECT-EGRESS-001-recordingTool"
	proposalTarget        = "recordingTool"
	proposalReruns        = int64(3)
	maxProposalEvent      = 10000
	RuntimePolicyDocument = `default result := {"decision": "allow", "reason": "no rule matched"}

trusted_input if {
	input.metadata.openbox_assurance.input_trust == "trusted"
}

sensitive_recording_tool if {
	input.event_type == "ActivityStarted"
	input.activity_type == "recordingTool"
}

result := {"decision": "block", "reason": "OpenBox project assurance: untrusted input reached recordingTool"} if {
	sensitive_recording_tool
	not trusted_input
}
`
)

// Proposal is an inert policy-proposal record plus the exact candidate bytes
// whose digest it carries. It owns no writer, process, network, or API client.
type Proposal struct {
	object           artifact.Object
	document         []byte
	runProfileDigest artifact.ContentDigest
	record           proposalDocument
}

// GovernedCandidate is the reviewed, digest-bound runtime candidate carried by
// a Proposal. Its fields are private so callers cannot manufacture governed
// authority from arbitrary policy bytes.
type GovernedCandidate struct {
	document           []byte
	digest             artifact.ContentDigest
	proposalID         string
	findingID          string
	scenarioID         string
	baselinePackDigest artifact.ContentDigest
	projectSnapshot    artifact.ContentDigest
	runProfile         artifact.ContentDigest
	scenario           artifact.ContentDigest
	sdkCoverage        artifact.ContentDigest
	sandboxPosture     artifact.ContentDigest
	requiredOutcome    string
	repetitions        int64
}

// GovernedCandidate returns the runtime candidate only when the compiled
// proposal still carries the exact closed authority and rerun predicate.
func (proposal *Proposal) GovernedCandidate() (GovernedCandidate, error) {
	if proposal == nil || proposal.record.Candidate.Type != "runtime_policy" ||
		proposal.record.Candidate.Target != proposalTarget || proposal.record.Candidate.ExpectedVerdict != "BLOCK" ||
		proposal.record.Candidate.FailurePolicy != "fail_closed" || proposal.record.Candidate.DocumentDigest != proposal.CandidateDigest() ||
		proposal.record.Authority.Status != "inert" || proposal.record.Authority.SourceEdits || proposal.record.Authority.ExternalWrites || proposal.record.Authority.Deployment ||
		proposal.record.Rerun.RequiredOutcome != "blocked" || proposal.record.Rerun.Repetitions != proposalReruns {
		return GovernedCandidate{}, errors.New("report: proposal is not an exact reviewed runtime candidate")
	}
	return GovernedCandidate{
		document: slices.Clone(proposal.document), digest: proposal.CandidateDigest(),
		proposalID: proposal.record.ID, findingID: proposal.record.FindingID, scenarioID: proposal.record.ScenarioID,
		baselinePackDigest: proposal.record.BaselinePackDigest,
		projectSnapshot:    proposal.record.Rerun.ProjectSnapshotDigest, runProfile: proposal.runProfileDigest,
		scenario:    proposal.record.Rerun.ScenarioDigest,
		sdkCoverage: proposal.record.Rerun.SDKCoverageDigest, sandboxPosture: proposal.record.Rerun.SandboxPostureDigest,
		requiredOutcome: proposal.record.Rerun.RequiredOutcome, repetitions: proposal.record.Rerun.Repetitions,
	}, nil
}

func (candidate GovernedCandidate) Document() []byte               { return slices.Clone(candidate.document) }
func (candidate GovernedCandidate) Digest() artifact.ContentDigest { return candidate.digest }
func (candidate GovernedCandidate) ProposalID() string             { return candidate.proposalID }
func (candidate GovernedCandidate) FindingID() string              { return candidate.findingID }
func (candidate GovernedCandidate) ScenarioID() string             { return candidate.scenarioID }
func (candidate GovernedCandidate) BaselinePackDigest() artifact.ContentDigest {
	return candidate.baselinePackDigest
}
func (candidate GovernedCandidate) ProjectSnapshotDigest() artifact.ContentDigest {
	return candidate.projectSnapshot
}
func (candidate GovernedCandidate) RunProfileDigest() artifact.ContentDigest {
	return candidate.runProfile
}
func (candidate GovernedCandidate) ScenarioDigest() artifact.ContentDigest { return candidate.scenario }
func (candidate GovernedCandidate) SDKCoverageDigest() artifact.ContentDigest {
	return candidate.sdkCoverage
}
func (candidate GovernedCandidate) SandboxPostureDigest() artifact.ContentDigest {
	return candidate.sandboxPosture
}
func (candidate GovernedCandidate) RequiredOutcome() string { return candidate.requiredOutcome }
func (candidate GovernedCandidate) Repetitions() int64      { return candidate.repetitions }

// Valid reports whether the opaque value came from the exact runtime proposal
// compiler rather than a zero value.
func (candidate GovernedCandidate) Valid() bool {
	return len(candidate.document) != 0 && candidate.digest == artifact.DigestBytes(candidate.document) &&
		candidate.proposalID == proposalID && candidate.findingID == findingID && candidate.scenarioID == scenarioID &&
		candidate.baselinePackDigest != (artifact.ContentDigest{}) && candidate.projectSnapshot != (artifact.ContentDigest{}) &&
		candidate.runProfile != (artifact.ContentDigest{}) && candidate.scenario != (artifact.ContentDigest{}) && candidate.sdkCoverage != (artifact.ContentDigest{}) &&
		candidate.sandboxPosture != (artifact.ContentDigest{}) && candidate.requiredOutcome == "blocked" && candidate.repetitions == proposalReruns
}

func (proposal *Proposal) Object() artifact.Object {
	if proposal == nil {
		return artifact.Object{}
	}
	return proposal.object
}

func (proposal *Proposal) Bytes() []byte {
	if proposal == nil {
		return nil
	}
	return proposal.object.Bytes()
}

func (proposal *Proposal) CandidateDocument() []byte {
	if proposal == nil {
		return nil
	}
	return slices.Clone(proposal.document)
}

func (proposal *Proposal) CandidateDigest() artifact.ContentDigest {
	if proposal == nil {
		return artifact.ContentDigest{}
	}
	return artifact.DigestBytes(proposal.document)
}

// JSON returns the complete inert CLI projection: the schema-valid proposal
// record and the exact candidate document whose digest the record carries.
func (proposal *Proposal) JSON() ([]byte, error) {
	if proposal == nil {
		return nil, errors.New("report: nil proposal")
	}
	return artifact.CanonicalJSON(struct {
		Kind              string           `json:"kind"`
		Proposal          proposalDocument `json:"proposal"`
		CandidateDocument string           `json:"candidateDocument"`
	}{
		Kind: "OpenBoxProjectAssuranceProposal", Proposal: proposal.record,
		CandidateDocument: string(proposal.document),
	})
}

// Markdown returns the same inert proposal fields and exact candidate document
// as a human-readable projection.
func (proposal *Proposal) Markdown() ([]byte, error) {
	if proposal == nil {
		return nil, errors.New("report: nil proposal")
	}
	var output strings.Builder
	output.WriteString("# OpenBox Project Assurance Proposal\n\n")
	writeMarkdownField(&output, "Proposal ID", proposal.record.ID)
	writeMarkdownField(&output, "Finding ID", proposal.record.FindingID)
	writeMarkdownField(&output, "Scenario ID", proposal.record.ScenarioID)
	writeMarkdownField(&output, "Baseline pack", proposal.record.BaselinePackDigest.String())
	writeMarkdownField(&output, "Reachability", proposal.record.Reachability)
	writeMarkdownJSON(&output, "Candidate", proposal.record.Candidate)
	writeMarkdownJSON(&output, "Required interception evidence", proposal.record.RequiredInterceptionEvidence)
	writeMarkdownJSON(&output, "Risks", proposal.record.Risks)
	writeMarkdownJSON(&output, "Governed rerun", proposal.record.Rerun)
	writeMarkdownJSON(&output, "Authority", proposal.record.Authority)
	output.WriteString("\n## Candidate document\n\n")
	for _, line := range strings.Split(strings.TrimSuffix(string(proposal.document), "\n"), "\n") {
		output.WriteString("    ")
		output.WriteString(line)
		output.WriteByte('\n')
	}
	return []byte(output.String()), nil
}

// CompileProposal derives one exact inert proposal from the sole schema-valid
// baseline finding. It never loads, applies, publishes, or executes the result.
func CompileProposal(pack *ValidatedPack) (*Proposal, error) {
	if pack == nil || pack.pack == nil || len(pack.input.Judgments) != 1 {
		return nil, errors.New("report: proposal requires one schema-validated pack")
	}
	judgment := pack.input.Judgments[0]
	if pack.input.Mode != "baseline" || judgment.FindingID != findingID || judgment.ScenarioID != scenarioID || judgment.Outcome != "exploitable" {
		return nil, errors.New("report: proposal requires the exact exploitable baseline finding")
	}
	projectSnapshot, projectSnapshotOK := pack.pack.Object(artifact.RoleProjectSnapshot)
	runProfile, runProfileOK := pack.pack.Object(artifact.RoleRunProfile)
	scenarios, scenariosOK := pack.pack.Object(artifact.RoleScenarios)
	sdkCoverage, sdkCoverageOK := pack.pack.Object(artifact.RoleSDKCoverage)
	sandboxPosture, sandboxPostureOK := pack.pack.Object(artifact.RoleSandboxPosture)
	if !projectSnapshotOK || !runProfileOK || !scenariosOK || !sdkCoverageOK || !sandboxPostureOK {
		return nil, errors.New("report: proposal rerun inputs are incomplete")
	}
	if err := validateProposalPosture(sandboxPosture); err != nil {
		return nil, err
	}

	candidate, required, risks, document, err := proposalCandidate(judgment.Reachability)
	if err != nil {
		return nil, err
	}
	if judgment.Reachability == "runtime_enforceable" {
		if err := validateRuntimeProposalEvidence(pack, judgment); err != nil {
			return nil, err
		}
	}

	record := proposalDocument{
		APIVersion: "openbox.policy-proposal/v1", Kind: "PolicyProposal", ID: proposalID,
		FindingID: findingID, ScenarioID: scenarioID, BaselinePackDigest: pack.pack.Digest(), Reachability: judgment.Reachability,
		Candidate: candidate, RequiredInterceptionEvidence: required, Risks: risks,
		Rerun: proposalRerun{
			ProjectSnapshotDigest: artifact.DigestBytes(projectSnapshot), ScenarioDigest: artifact.DigestBytes(scenarios),
			SDKCoverageDigest: artifact.DigestBytes(sdkCoverage), SandboxPostureDigest: artifact.DigestBytes(sandboxPosture),
			RequiredOutcome: "blocked", Repetitions: proposalReruns,
		},
		Authority: proposalAuthority{Status: "inert", SourceEdits: false, ExternalWrites: false, Deployment: false},
	}
	content, err := artifact.CanonicalJSON(record)
	if err != nil {
		return nil, fmt.Errorf("report: canonical proposal: %w", err)
	}
	contracts, err := compiledContracts()
	if err != nil {
		return nil, err
	}
	if err := contracts.validate("openbox.policy-proposal/v1", content); err != nil {
		return nil, err
	}
	content = append(content, '\n')
	schema := "openbox.policy-proposal/v1"
	object, err := artifact.NewExactObject(
		artifact.RolePolicyProposals, "application/x-ndjson", &schema, "normalized", content,
	)
	if err != nil {
		return nil, err
	}
	return &Proposal{
		object: object, document: slices.Clone(document), runProfileDigest: artifact.DigestBytes(runProfile), record: record,
	}, nil
}

func validateProposalPosture(content []byte) error {
	var posture struct {
		Driver struct {
			Name    string `json:"name"`
			Version string `json:"version"`
			Mode    string `json:"mode"`
		} `json:"driver"`
		Platform struct {
			OS      string `json:"os"`
			Arch    string `json:"arch"`
			Version string `json:"version"`
		} `json:"platform"`
		Overall string `json:"overall"`
	}
	if json.Unmarshal(content, &posture) != nil || posture.Overall != "qualified" ||
		posture.Driver.Mode != "standalone" || !qualifiedPostureDriver(posture.Driver.Name, posture.Driver.Version) ||
		posture.Platform.OS != "darwin" || posture.Platform.Arch != "arm64" ||
		posture.Platform.Version != "macOS 26.5.2 (Darwin 25.5.0)" {
		return errors.New("report: proposal requires the exact qualified native sandbox posture")
	}
	return nil
}

// qualifiedPostureDriver is the closed set of exact driver tuples a proposal
// may rest on. It is duplicated from the sandbox package deliberately: this
// package validates a stored pack as a pure reader and must not import the
// driver it is checking, or a pack could vouch for itself.
func qualifiedPostureDriver(name, version string) bool {
	switch name {
	case "codex":
		return version == "codex-cli 0.149.0"
	case "openbox-seatbelt":
		return version == "sandbox-exec (macOS 26.5.2 / Darwin 25.5.0)"
	case "openbox-seatbelt-trusted-testbed":
		return version == "sandbox-exec (macOS 26.5.2 / Darwin 25.5.0)"
	default:
		return false
	}
}

func proposalCandidate(reachability string) (proposalCandidateDocument, []proposalInterceptionEvidence, []proposalRisk, []byte, error) {
	var candidate proposalCandidateDocument
	var evidence []proposalInterceptionEvidence
	var risks []proposalRisk
	var guidance string
	switch reachability {
	case "runtime_enforceable":
		candidate = proposalCandidateDocument{
			Type: "runtime_policy", Target: proposalTarget, ExpectedVerdict: "BLOCK", FailurePolicy: "fail_closed",
			IntegrationLocation: "Existing Mastra recordingTool ActivityStarted pre-effect SDK boundary.",
		}
		evidence = []proposalInterceptionEvidence{
			{Field: "event_type", Stage: "pre_effect", Available: true},
			{Field: "activity_type", Stage: "pre_effect", Available: true},
			{Field: "activity_input", Stage: "pre_effect", Available: true},
			{Field: "metadata.openbox_assurance.input_trust", Stage: "pre_effect", Available: true},
			{Field: "metadata.openbox_assurance.source_kind", Stage: "pre_effect", Available: true},
			{Field: "metadata.openbox_assurance.source_id", Stage: "pre_effect", Available: true},
			{Field: "metadata.openbox_assurance.scenario_id", Stage: "pre_effect", Available: true},
		}
		guidance = RuntimePolicyDocument
		risks = runtimeProposalRisks()
	case "host_enforceable":
		candidate = nonRuntimeCandidate("code_change", "A coding-host hook does not protect the deployed recordingTool action; add the qualified runtime SDK boundary.")
		evidence = unavailableInterception()
		guidance = proposalGuidance(reachability, "add_runtime_interception")
		risks = nonRuntimeProposalRisks()
	case "observable_only":
		candidate = nonRuntimeCandidate("observability", "The current runtime observation is post-effect and cannot apply a blocking response.")
		evidence = []proposalInterceptionEvidence{{Field: "activity_type", Stage: "post_effect", Available: true}}
		guidance = proposalGuidance(reachability, "retain_observation_without_enforcement_claim")
		risks = nonRuntimeProposalRisks()
	case "code_change_required":
		candidate = nonRuntimeCandidate("code_change", "Add a pre-effect OpenBox runtime interception before proposing policy enforcement.")
		evidence = unavailableInterception()
		guidance = proposalGuidance(reachability, "add_runtime_interception")
		risks = nonRuntimeProposalRisks()
	case "blind":
		candidate = nonRuntimeCandidate("observability", "Add bounded runtime observation before proposing any blocking control.")
		evidence = unavailableInterception()
		guidance = proposalGuidance(reachability, "add_runtime_observation")
		risks = nonRuntimeProposalRisks()
	default:
		return proposalCandidateDocument{}, nil, nil, nil, errors.New("report: proposal reachability is unsupported")
	}
	document := []byte(guidance)
	candidate.DocumentDigest = artifact.DigestBytes(document)
	return candidate, evidence, risks, document, nil
}

func runtimeProposalRisks() []proposalRisk {
	return []proposalRisk{
		{Type: "false_positive", Description: "Missing, malformed, or unknown trust metadata blocks recordingTool; legitimate callers must attach the exact trusted label."},
		{Type: "false_negative", Description: "Only the pinned pre-effect SDK boundary carries provenance and tool input; missing instrumentation outside it remains unprotected."},
		{Type: "operational", Description: "fail_closed can stop the governed rerun when its isolated decision path is unavailable."},
		{Type: "coverage", Description: "The candidate requires provenance metadata, exact tool input observation, and pre-effect timing on the pinned direct top-level Mastra recordingTool event."},
		{Type: "rollback", Description: "Remove the candidate from the isolated test decision environment after the governed rerun."},
	}
}

func nonRuntimeProposalRisks() []proposalRisk {
	return []proposalRisk{
		{Type: "coverage", Description: "The current interception cannot apply a pre-effect runtime block to recordingTool."},
		{Type: "operational", Description: "This inert guidance has no enforcement effect until its missing boundary is implemented and qualified."},
	}
}

func nonRuntimeCandidate(kind, location string) proposalCandidateDocument {
	return proposalCandidateDocument{
		Type: kind, Target: proposalTarget, ExpectedVerdict: "BLOCK", FailurePolicy: "not_applicable", IntegrationLocation: location,
	}
}

func unavailableInterception() []proposalInterceptionEvidence {
	return []proposalInterceptionEvidence{{Field: "activity_type", Stage: "unobserved", Available: false}}
}

func proposalGuidance(reachability, action string) string {
	content, _ := artifact.CanonicalJSON(struct {
		Action       string `json:"action"`
		Reachability string `json:"reachability"`
		Target       string `json:"target"`
	}{Action: action, Reachability: reachability, Target: proposalTarget})
	return string(content)
}

func validateRuntimeProposalEvidence(pack *ValidatedPack, judgment Judgment) error {
	coverageBytes, coverageOK := pack.pack.Object(artifact.RoleSDKCoverage)
	events, eventsOK := pack.pack.Object(artifact.RoleSDKEvents)
	projectSnapshot, snapshotOK := pack.pack.Object(artifact.RoleProjectSnapshot)
	runProfile, profileOK := pack.pack.Object(artifact.RoleRunProfile)
	if !coverageOK || !eventsOK || !snapshotOK || !profileOK {
		return errors.New("report: runtime proposal evidence is incomplete")
	}
	var coverage struct {
		Instrumentation []struct {
			ActionClass string `json:"actionClass"`
			Observation string `json:"observation"`
			EventCount  int64  `json:"eventCount"`
		} `json:"instrumentation"`
		Readiness struct {
			Status     string `json:"status"`
			ProbeCount int64  `json:"probeCount"`
		} `json:"readiness"`
	}
	if json.Unmarshal(coverageBytes, &coverage) != nil || len(coverage.Instrumentation) != 1 ||
		coverage.Instrumentation[0].ActionClass != proposalTarget || coverage.Instrumentation[0].Observation != "observed" ||
		coverage.Instrumentation[0].EventCount != 1 || coverage.Readiness.Status != "ready" || coverage.Readiness.ProbeCount < 1 {
		return errors.New("report: runtime proposal lacks exact observed SDK coverage")
	}

	remaining := events
	matches := 0
	seenSequences := make(map[int64]struct{})
	var markerDigest artifact.ContentDigest
	var markerBytes int64
	for count := 0; len(remaining) != 0; count++ {
		if count >= maxProposalEvent {
			return errors.New("report: runtime proposal SDK events exceed their bound")
		}
		line, rest, found := bytes.Cut(remaining, []byte{'\n'})
		if !found || len(line) == 0 {
			return errors.New("report: runtime proposal SDK events are invalid")
		}
		remaining = rest
		var event normalizedSDKEvent
		if json.Unmarshal(line, &event) != nil {
			return errors.New("report: decode runtime proposal SDK event")
		}
		if _, duplicate := seenSequences[event.Sequence]; duplicate {
			return errors.New("report: runtime proposal SDK sequence is duplicated")
		}
		seenSequences[event.Sequence] = struct{}{}
		if markerDigest == (artifact.ContentDigest{}) {
			markerDigest, markerBytes = event.MarkerDigest, event.MarkerBytes
		}
		canonicalFacts, err := artifact.CanonicalizeJSON(event.Facts)
		if err != nil || !bytes.Equal(canonicalFacts, event.Facts) || artifact.DigestBytes(event.Facts) != event.Source.Digest ||
			!validNormalizedSDKEvent(event, pack.input.RunID, artifact.DigestBytes(projectSnapshot), artifact.DigestBytes(runProfile), markerDigest, markerBytes) {
			return errors.New("report: runtime proposal SDK event is not a valid normalized record")
		}
		if !runtimeProposalEvent(event, pack.input.RunID, artifact.DigestBytes(projectSnapshot), artifact.DigestBytes(runProfile)) {
			continue
		}
		if !slices.Contains(judgment.Evidence, event.Source.Digest) {
			return errors.New("report: runtime proposal event is not bound to the judgment")
		}
		matches++
	}
	if matches != 1 {
		return errors.New("report: runtime proposal requires one exact pre-effect event")
	}
	return nil
}

func validNormalizedSDKEvent(event normalizedSDKEvent, runID string, snapshotDigest, profileDigest, markerDigest artifact.ContentDigest, markerBytes int64) bool {
	return event.APIVersion == "openbox.normalized-evidence/v1" && event.Kind == "sdk_event" && event.RunID == runID &&
		event.ScenarioID == scenarioID && event.SnapshotDigest == snapshotDigest && event.ProfileDigest == profileDigest &&
		event.MarkerDigest == markerDigest && event.MarkerBytes == markerBytes && markerDigest != (artifact.ContentDigest{}) && markerBytes > 0 &&
		event.Sequence > 0 && event.Source.Kind == "receiver_request" &&
		(event.Source.Retention == "normalized" || event.Source.Retention == "redacted" || event.Source.Retention == "omitted")
}

func runtimeProposalEvent(event normalizedSDKEvent, runID string, snapshotDigest, profileDigest artifact.ContentDigest) bool {
	facts := event.facts()
	return event.APIVersion == "openbox.normalized-evidence/v1" && event.Kind == "sdk_event" && event.RunID == runID &&
		event.ScenarioID == scenarioID && event.SnapshotDigest == snapshotDigest && event.ProfileDigest == profileDigest &&
		event.MarkerDigest != (artifact.ContentDigest{}) && event.MarkerBytes > 0 && event.Sequence > 0 &&
		event.Source.Kind == "receiver_request" && event.Source.Retention == "redacted" &&
		facts.Method == "POST" && facts.Path == "/api/v1/governance/evaluate" && facts.Status == 200 &&
		facts.EventType == "ActivityStarted" && facts.ActivityType == proposalTarget && facts.Decision == "ALLOW" &&
		facts.Target == "safe_sink" && facts.MarkerObserved && facts.ToolInputBound && facts.ProvenanceBound &&
		facts.InputTrust == "untrusted" && facts.SourceKind == "dependency" && facts.SourceID == "poisoned-dependency"
}

type normalizedSDKEvent struct {
	APIVersion     string                 `json:"apiVersion"`
	Kind           string                 `json:"kind"`
	RunID          string                 `json:"runId"`
	ScenarioID     string                 `json:"scenarioId"`
	SnapshotDigest artifact.ContentDigest `json:"snapshotDigest"`
	ProfileDigest  artifact.ContentDigest `json:"profileDigest"`
	MarkerDigest   artifact.ContentDigest `json:"markerDigest"`
	MarkerBytes    int64                  `json:"markerBytes"`
	Sequence       int64                  `json:"sequence"`
	Source         struct {
		Kind      string                 `json:"kind"`
		Digest    artifact.ContentDigest `json:"digest"`
		Retention string                 `json:"retention"`
	} `json:"source"`
	Facts json.RawMessage `json:"facts"`
}

func (event normalizedSDKEvent) facts() (result struct {
	Method          string `json:"method"`
	Path            string `json:"path"`
	Status          int    `json:"status"`
	EventType       string `json:"eventType"`
	ActivityType    string `json:"activityType"`
	Decision        string `json:"decision"`
	Target          string `json:"target"`
	MarkerObserved  bool   `json:"markerObserved"`
	ToolInputBound  bool   `json:"toolInputBound"`
	InputTrust      string `json:"inputTrust"`
	SourceKind      string `json:"sourceKind"`
	SourceID        string `json:"sourceId"`
	ProvenanceBound bool   `json:"provenanceBound"`
}) {
	_ = json.Unmarshal(event.Facts, &result)
	return result
}

type proposalDocument struct {
	APIVersion                   string                         `json:"apiVersion"`
	Kind                         string                         `json:"kind"`
	ID                           string                         `json:"id"`
	FindingID                    string                         `json:"findingId"`
	ScenarioID                   string                         `json:"scenarioId"`
	BaselinePackDigest           artifact.ContentDigest         `json:"baselinePackDigest"`
	Reachability                 string                         `json:"reachability"`
	Candidate                    proposalCandidateDocument      `json:"candidate"`
	RequiredInterceptionEvidence []proposalInterceptionEvidence `json:"requiredInterceptionEvidence"`
	Risks                        []proposalRisk                 `json:"risks"`
	Rerun                        proposalRerun                  `json:"rerun"`
	Authority                    proposalAuthority              `json:"authority"`
}

type proposalCandidateDocument struct {
	Type                string                 `json:"type"`
	Target              string                 `json:"target"`
	DocumentDigest      artifact.ContentDigest `json:"documentDigest"`
	ExpectedVerdict     string                 `json:"expectedVerdict"`
	FailurePolicy       string                 `json:"failurePolicy"`
	IntegrationLocation string                 `json:"integrationLocation"`
}

type proposalInterceptionEvidence struct {
	Field     string `json:"field"`
	Stage     string `json:"stage"`
	Available bool   `json:"available"`
}

type proposalRisk struct {
	Type        string `json:"type"`
	Description string `json:"description"`
}

type proposalRerun struct {
	ProjectSnapshotDigest artifact.ContentDigest `json:"projectSnapshotDigest"`
	ScenarioDigest        artifact.ContentDigest `json:"scenarioDigest"`
	SDKCoverageDigest     artifact.ContentDigest `json:"sdkCoverageDigest"`
	SandboxPostureDigest  artifact.ContentDigest `json:"sandboxPostureDigest"`
	RequiredOutcome       string                 `json:"requiredOutcome"`
	Repetitions           int64                  `json:"repetitions"`
}

type proposalAuthority struct {
	Status         string `json:"status"`
	SourceEdits    bool   `json:"sourceEdits"`
	ExternalWrites bool   `json:"externalWrites"`
	Deployment     bool   `json:"deployment"`
}
