//go:build darwin || linux

package report

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/artifact"
)

func TestCompileProposalBindsExactRuntimePolicyAndRerun(t *testing.T) {
	pack := reportVerifiedPack(t, nil)
	validated, err := ValidatePack(pack)
	if err != nil {
		t.Fatal(err)
	}
	first, err := CompileProposal(validated)
	if err != nil {
		t.Fatal(err)
	}
	second, err := CompileProposal(validated)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first.Bytes(), second.Bytes()) || !bytes.Equal(first.CandidateDocument(), second.CandidateDocument()) {
		t.Fatal("proposal compilation is not deterministic")
	}
	if first.Object().Role() != artifact.RolePolicyProposals || string(first.CandidateDocument()) != RuntimePolicyDocument ||
		first.CandidateDigest() != artifact.DigestBytes([]byte(RuntimePolicyDocument)) {
		t.Fatalf("unexpected runtime proposal: role=%s digest=%s", first.Object().Role(), first.CandidateDigest())
	}
	jsonOutput, err := first.JSON()
	if err != nil {
		t.Fatal(err)
	}
	markdownOutput, err := first.Markdown()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(jsonOutput, []byte(`"kind":"OpenBoxProjectAssuranceProposal"`)) ||
		!bytes.Contains(jsonOutput, []byte(`"candidateDocument":`)) || !bytes.Contains(jsonOutput, []byte("recordingTool")) ||
		!bytes.Contains(markdownOutput, []byte("# OpenBox Project Assurance Proposal")) ||
		!bytes.Contains(markdownOutput, []byte(`default result := {"decision": "allow"`)) ||
		!bytes.Contains(markdownOutput, []byte(`input.activity_type == "recordingTool"`)) {
		t.Fatalf("proposal projections lost the candidate document\njson=%s\nmarkdown=%s", jsonOutput, markdownOutput)
	}

	document := decodeProposal(t, first)
	runProfile, runProfileOK := pack.Object(artifact.RoleRunProfile)
	if !runProfileOK {
		t.Fatal("baseline pack lost its run profile")
	}
	if document.ID != proposalID || document.FindingID != findingID || document.ScenarioID != scenarioID ||
		document.BaselinePackDigest != pack.Digest() || document.Reachability != "runtime_enforceable" ||
		document.Candidate.Type != "runtime_policy" || document.Candidate.Target != proposalTarget ||
		document.Candidate.DocumentDigest != first.CandidateDigest() || document.Candidate.ExpectedVerdict != "BLOCK" ||
		document.Candidate.FailurePolicy != "fail_closed" || document.Rerun.RequiredOutcome != "blocked" ||
		document.Rerun.Repetitions != proposalReruns || document.Authority.Status != "inert" || document.Authority.SourceEdits ||
		document.Authority.ExternalWrites || document.Authority.Deployment {
		t.Fatalf("unexpected proposal document: %+v", document)
	}
	for role, digest := range map[artifact.Role]artifact.ContentDigest{
		artifact.RoleProjectSnapshot: document.Rerun.ProjectSnapshotDigest,
		artifact.RoleScenarios:       document.Rerun.ScenarioDigest,
		artifact.RoleSDKCoverage:     document.Rerun.SDKCoverageDigest,
		artifact.RoleSandboxPosture:  document.Rerun.SandboxPostureDigest,
	} {
		content, ok := pack.Object(role)
		if !ok || artifact.DigestBytes(content) != digest {
			t.Fatalf("rerun digest for %s is not bound", role)
		}
	}
	reviewed, err := first.GovernedCandidate()
	if err != nil || !reviewed.Valid() || reviewed.Digest() != first.CandidateDigest() ||
		reviewed.ProposalID() != proposalID || reviewed.FindingID() != findingID || reviewed.ScenarioID() != scenarioID ||
		reviewed.BaselinePackDigest() != pack.Digest() || reviewed.ProjectSnapshotDigest() != document.Rerun.ProjectSnapshotDigest ||
		reviewed.RunProfileDigest() != artifact.DigestBytes(runProfile) ||
		reviewed.ScenarioDigest() != document.Rerun.ScenarioDigest || reviewed.SDKCoverageDigest() != document.Rerun.SDKCoverageDigest ||
		reviewed.SandboxPostureDigest() != document.Rerun.SandboxPostureDigest || reviewed.RequiredOutcome() != "blocked" ||
		reviewed.Repetitions() != proposalReruns {
		t.Fatalf("reviewed candidate lost its authority binding: candidate=%+v error=%v", reviewed, err)
	}
	reviewedDocument := reviewed.Document()
	reviewedDocument[0] = '!'
	if reviewed.Document()[0] == '!' {
		t.Fatal("reviewed candidate exposed mutable policy bytes")
	}
	if len(document.RequiredInterceptionEvidence) != 7 || document.RequiredInterceptionEvidence[0].Field != "event_type" ||
		document.RequiredInterceptionEvidence[1].Field != "activity_type" ||
		document.RequiredInterceptionEvidence[2].Field != "activity_input" ||
		document.RequiredInterceptionEvidence[3].Field != "metadata.openbox_assurance.input_trust" {
		t.Fatalf("unexpected runtime fields: %+v", document.RequiredInterceptionEvidence)
	}

	proposalBytes := first.Bytes()
	proposalBytes[0] = '!'
	policyBytes := first.CandidateDocument()
	policyBytes[0] = '!'
	jsonOutput[0] = '!'
	markdownOutput[0] = '!'
	repeatedJSON, _ := first.JSON()
	repeatedMarkdown, _ := first.Markdown()
	if first.Bytes()[0] == '!' || first.CandidateDocument()[0] == '!' || repeatedJSON[0] == '!' || repeatedMarkdown[0] == '!' {
		t.Fatal("proposal exposed mutable bytes")
	}
}

func TestGovernedCandidateRejectsZeroOrNonRuntimeProposal(t *testing.T) {
	if (GovernedCandidate{}).Valid() {
		t.Fatal("zero candidate was valid")
	}
	proposal := &Proposal{}
	if _, err := proposal.GovernedCandidate(); err == nil {
		t.Fatal("zero proposal yielded governed authority")
	}
}

func TestCompileProposalRejectsUnboundOrUnsupportedRuntimeClaims(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*artifact.ManifestInput)
	}{
		{name: "governed input is not a baseline", mutate: func(input *artifact.ManifestInput) { input.Mode = "governed" }},
		{name: "non-exploitable finding", mutate: func(input *artifact.ManifestInput) {
			judgments := input.Judgments.([]Judgment)
			judgments[0].Outcome = "not_observed"
			judgments[0].MatchedFacts = []string{"sdk_attempt_not_observed", "safe_sink_not_invoked", "complete_observation"}
			judgments[0].Limitations = []string{}
			input.Judgments = judgments
		}},
		{name: "missing SDK event", mutate: func(input *artifact.ManifestInput) {
			input.Objects.SDKEvents = reportExactObject(t, artifact.RoleSDKEvents, "application/x-ndjson", "", "redacted", []byte("{}\n"))
		}},
		{name: "wrong runtime field", mutate: func(input *artifact.ManifestInput) {
			input.Objects.SDKEvents = mutateReportRecord(t, input.Objects.SDKEvents, "", func(document map[string]any) {
				document["facts"].(map[string]any)["activityType"] = "unqualifiedTool"
			})
		}},
		{name: "duplicate SDK sequence", mutate: func(input *artifact.ManifestInput) {
			content := input.Objects.SDKEvents.Bytes()
			input.Objects.SDKEvents = reportExactObject(t, artifact.RoleSDKEvents, "application/x-ndjson", "", "redacted", append(append([]byte(nil), content...), content...))
		}},
		{name: "cross-run SDK event", mutate: func(input *artifact.ManifestInput) {
			input.Objects.SDKEvents = mutateReportRecord(t, input.Objects.SDKEvents, "", func(document map[string]any) {
				document["runId"] = "run-other"
			})
		}},
		{name: "unready SDK coverage", mutate: func(input *artifact.ManifestInput) {
			input.Objects.SDKCoverage = mutateReportObject(t, input.Objects.SDKCoverage, "application/json", "openbox.sdk-coverage/v1", func(document map[string]any) {
				instrumentation := document["instrumentation"].([]any)[0].(map[string]any)
				instrumentation["observation"] = "missing"
				instrumentation["eventCount"] = float64(0)
				readiness := document["readiness"].(map[string]any)
				readiness["status"] = "inconclusive"
				readiness["reason"] = "The runtime event is unavailable."
			})
		}},
		{name: "rejected sandbox posture", mutate: proposalPostureOverall(t, "rejected")},
		{name: "inconclusive sandbox posture", mutate: proposalPostureOverall(t, "inconclusive")},
		{name: "not runnable sandbox posture", mutate: proposalPostureOverall(t, "not_runnable")},
		{name: "different sandbox tuple", mutate: func(input *artifact.ManifestInput) {
			input.Objects.SandboxPosture = mutateReportObject(t, input.Objects.SandboxPosture, "application/json", "openbox.sandbox-posture/v1", func(document map[string]any) {
				document["driver"].(map[string]any)["version"] = "codex-cli 0.150.0"
			})
		}},
		{name: "event absent from judgment evidence", mutate: func(input *artifact.ManifestInput) {
			judgments := input.Judgments.([]Judgment)
			judgments[0].Evidence = []artifact.ContentDigest{
				artifact.DigestBytes([]byte("unrelated-sdk")), artifact.DigestBytes([]byte("unrelated-sink")),
			}
			input.Judgments = judgments
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			pack := reportVerifiedPack(t, test.mutate)
			validated, err := ValidatePack(pack)
			if err != nil {
				t.Fatalf("adversarial pack should reach the proposal boundary: %v", err)
			}
			if _, err := CompileProposal(validated); err == nil {
				t.Fatal("expected proposal rejection")
			}
		})
	}
}

func proposalPostureOverall(t *testing.T, overall string) func(*artifact.ManifestInput) {
	t.Helper()
	return func(input *artifact.ManifestInput) {
		input.Objects.SandboxPosture = mutateReportObject(t, input.Objects.SandboxPosture, "application/json", "openbox.sandbox-posture/v1", func(document map[string]any) {
			document["overall"] = overall
		})
	}
}

func TestNonRuntimeReachabilityNeverProducesRuntimePolicy(t *testing.T) {
	tests := []struct {
		reachability string
		candidate    string
		stage        string
		available    bool
	}{
		{reachability: "host_enforceable", candidate: "code_change", stage: "unobserved", available: false},
		{reachability: "observable_only", candidate: "observability", stage: "post_effect", available: true},
		{reachability: "code_change_required", candidate: "code_change", stage: "unobserved", available: false},
		{reachability: "blind", candidate: "observability", stage: "unobserved", available: false},
	}
	for _, test := range tests {
		t.Run(test.reachability, func(t *testing.T) {
			pack := reportVerifiedPack(t, func(input *artifact.ManifestInput) {
				judgments := input.Judgments.([]Judgment)
				judgments[0].Reachability = test.reachability
				input.Judgments = judgments
			})
			validated, err := ValidatePack(pack)
			if err != nil {
				t.Fatal(err)
			}
			proposal, err := CompileProposal(validated)
			if err != nil {
				t.Fatal(err)
			}
			document := decodeProposal(t, proposal)
			if document.Candidate.Type != test.candidate || document.Candidate.Type == "runtime_policy" ||
				document.Candidate.FailurePolicy != "not_applicable" || len(document.RequiredInterceptionEvidence) != 1 ||
				document.RequiredInterceptionEvidence[0].Stage != test.stage ||
				document.RequiredInterceptionEvidence[0].Available != test.available ||
				bytes.Contains(proposal.CandidateDocument(), []byte("result :=")) {
				t.Fatalf("unsupported reachability fabricated runtime enforcement: %+v", document)
			}
		})
	}
}

func decodeProposal(t *testing.T, proposal *Proposal) proposalDocument {
	t.Helper()
	content := proposal.Bytes()
	if len(content) == 0 || content[len(content)-1] != '\n' {
		t.Fatal("proposal is not canonical JSONL")
	}
	var document proposalDocument
	if err := json.Unmarshal(content[:len(content)-1], &document); err != nil {
		t.Fatal(err)
	}
	return document
}
