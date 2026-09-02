//go:build darwin || linux

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/artifact"
	assurancereport "github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/report"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/runfs"
)

func TestProjectVerifyCommandChecksFinalizedPackObjects(t *testing.T) {
	root, pack := finalizedCLIVerificationPack(t)

	a, out, errOut := testApp(nil)
	if code := a.run([]string{"project", "verify", root}); code != exitOK {
		t.Fatalf("exit=%d stderr=%q", code, errOut.String())
	}
	wantOutput := "audit pack objects verified: " + pack.Digest().String() + "\n" +
		"  roles: 14\n" +
		"note: point-in-time canonical manifest structure, role encodings, lengths, CIDs, and exact object set were verified; public-schema conformance remains a separate contract check\n"
	if out.String() != wantOutput || errOut.Len() != 0 {
		t.Fatalf("stdout=%q stderr=%q", out.String(), errOut.String())
	}

	object := pack.Objects()[0]
	path := filepath.Join(root, "objects", "sha256", strings.TrimPrefix(object.Digest().String(), "sha256:"))
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	content := object.Bytes()
	content[0] ^= 1
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o400); err != nil {
		t.Fatal(err)
	}
	a, out, errOut = testApp(nil)
	if code := a.run([]string{"project", "verify", root}); code != exitError || out.Len() != 0 || !strings.Contains(errOut.String(), "project verify") {
		t.Fatalf("tampered exit=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
}

func TestProjectVerifyCommandChecksRetainedObservationPack(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", "..", "..", "plans", "260825-1623-lean-openshell-project-assurance", "evidence", "2026-08-26-phase-02-public-mastra-dashboard-observation-04"))
	if err != nil {
		t.Fatal(err)
	}
	a, out, errOut := testApp(nil)
	if code := a.run([]string{"project", "verify", root}); code != exitOK {
		t.Fatalf("exit=%d stderr=%q", code, errOut.String())
	}
	want := "project observation verified: ai.openbox.project-observation/v1\n" +
		"  pack_digest: sha256:2e724ab506e2eeea2c40b873fa05135940f0d6ad0fb0bf82609e7f2dca73fe25\n"
	if out.String() != want || errOut.Len() != 0 {
		t.Fatalf("stdout=%q stderr=%q", out.String(), errOut.String())
	}
}

func TestProjectVerifyRejectsUnknownManifestDiscriminator(t *testing.T) {
	root := filepath.Join(t.TempDir(), "pack")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "manifest.json"), []byte(`{"schema":"unknown/v1"}`), 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(root, 0o700) })
	a, out, errOut := testApp(nil)
	if code := a.run([]string{"project", "verify", root}); code != exitError || out.Len() != 0 || !strings.Contains(errOut.String(), "unknown or ambiguous") {
		t.Fatalf("exit/output = %d, %q, %q", code, out.String(), errOut.String())
	}
}

func finalizedCLIVerificationPack(t *testing.T) (string, *artifact.Pack) {
	t.Helper()
	pack := cliVerificationPack(t)
	root := filepath.Join(t.TempDir(), "pack")
	workspace, err := runfs.Create(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := workspace.WritePackObjects(pack); err != nil {
		t.Fatal(err)
	}
	if _, err := workspace.FinalizePack(pack); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(root, 0o700)
		_ = os.Chmod(filepath.Join(root, "objects"), 0o700)
		_ = os.Chmod(filepath.Join(root, "objects", "sha256"), 0o700)
	})
	return root, pack
}

func cliVerificationPack(t *testing.T) *artifact.Pack {
	t.Helper()
	schemas, err := assurancereport.SchemaReferences()
	if err != nil {
		t.Fatal(err)
	}
	pointer := func(value string) *string { return &value }
	exact := func(role artifact.Role, media, retention string, schema *string, content []byte) artifact.Object {
		object, err := artifact.NewExactObject(role, media, schema, retention, content)
		if err != nil {
			t.Fatal(err)
		}
		return object
	}
	fixture := func(role artifact.Role, name, schema string) artifact.Object {
		content, readErr := os.ReadFile(filepath.Join("..", "..", "..", "contracts", "project-assurance", "testdata", "valid", name))
		if readErr != nil {
			t.Fatal(readErr)
		}
		canonical, canonicalErr := artifact.CanonicalizeJSON(content)
		if canonicalErr != nil {
			t.Fatal(canonicalErr)
		}
		media := "application/json"
		if role == artifact.RoleScenarios {
			media = "application/x-ndjson"
			canonical = append(canonical, '\n')
		}
		return exact(role, media, "normalized", pointer(schema), canonical)
	}
	mutate := func(object artifact.Object, schema string, edit func(map[string]any)) artifact.Object {
		var document map[string]any
		if unmarshalErr := json.Unmarshal(object.Bytes(), &document); unmarshalErr != nil {
			t.Fatal(unmarshalErr)
		}
		edit(document)
		content, canonicalErr := artifact.CanonicalJSON(document)
		if canonicalErr != nil {
			t.Fatal(canonicalErr)
		}
		media := "application/json"
		if object.Role() == artifact.RoleScenarios {
			media = "application/x-ndjson"
			content = append(content, '\n')
		}
		return exact(object.Role(), media, "normalized", pointer(schema), content)
	}

	projectSnapshot := exact(artifact.RoleProjectSnapshot, "application/vnd.openbox.project-snapshot", "normalized", nil, []byte(`{"files":[]}`))
	projectModel := fixture(artifact.RoleProjectModel, "project-model-v1.json", "openbox.project-model/v1")
	projectModel = mutate(projectModel, "openbox.project-model/v1", func(document map[string]any) {
		document["snapshot"].(map[string]any)["digest"] = projectSnapshot.Digest()
	})
	runProfile := fixture(artifact.RoleRunProfile, "project-run-profile-v1.json", "openbox.project-run-profile/v1")
	sdkCoverage := fixture(artifact.RoleSDKCoverage, "sdk-coverage-v1.json", "openbox.sdk-coverage/v1")
	sdkCoverage = mutate(sdkCoverage, "openbox.sdk-coverage/v1", func(document map[string]any) {
		document["projectModelDigest"] = projectModel.Digest()
	})
	sandboxPosture := fixture(artifact.RoleSandboxPosture, "sandbox-posture-v1.json", "openbox.sandbox-posture/v1")
	sandboxPosture = mutate(sandboxPosture, "openbox.sandbox-posture/v1", func(document map[string]any) {
		driver := document["driver"].(map[string]any)
		driver["name"] = "codex"
		driver["version"] = "codex-cli 0.149.0"
		platform := document["platform"].(map[string]any)
		platform["version"] = "macOS 26.5.2 (Darwin 25.5.0)"
		document["overall"] = "qualified"
	})
	scenarios := fixture(artifact.RoleScenarios, "security-test-v1.json", "openbox.security-test/v1")
	scenarios = mutate(scenarios, "openbox.security-test/v1", func(document map[string]any) {
		document["projectModelDigest"] = projectModel.Digest()
		document["runProfileDigest"] = runProfile.Digest()
	})

	factsValue := map[string]any{
		"activityType": "recordingTool", "caller": "project", "decision": "ALLOW", "eventType": "ActivityStarted",
		"markerObserved": true, "toolInputBound": true, "inputTrust": "untrusted", "sourceKind": "dependency",
		"sourceId": "poisoned-dependency", "provenanceBound": true, "method": "POST",
		"path": "/api/v1/governance/evaluate", "status": 200, "target": "safe_sink",
	}
	facts, err := artifact.CanonicalJSON(factsValue)
	if err != nil {
		t.Fatal(err)
	}
	factDigest := artifact.DigestBytes(facts)
	sdkEvent, err := artifact.CanonicalJSON(map[string]any{
		"apiVersion": "openbox.normalized-evidence/v1", "kind": "sdk_event", "runId": "run-cli-report",
		"scenarioId": "ASI02-INDIRECT-EGRESS-001", "snapshotDigest": projectSnapshot.Digest(), "profileDigest": runProfile.Digest(),
		"markerDigest": artifact.DigestBytes([]byte("report-fixture-marker")), "markerBytes": 21, "sequence": 1,
		"source": map[string]any{"kind": "receiver_request", "digest": factDigest, "retention": "redacted"},
		"facts":  factsValue, "limitations": []string{},
	})
	if err != nil {
		t.Fatal(err)
	}
	sdkEvents := exact(artifact.RoleSDKEvents, "application/x-ndjson", "redacted", nil, append(sdkEvent, '\n'))
	reportInput := assurancereport.Input{
		RunID: "run-cli-report", Mode: "baseline", RunnerVersion: "0.1.0-test",
		Judgments: []assurancereport.Judgment{{
			FindingID: "finding-ASI02-INDIRECT-EGRESS-001", ScenarioID: "ASI02-INDIRECT-EGRESS-001",
			Outcome: "exploitable", Reachability: "runtime_enforceable", EvidenceLevel: "observed",
			MatchedFacts: []string{"sdk_attempt_before_effect", "poison_marker_provenance", "safe_sink_receipt", "complete_observation"},
			Evidence:     []artifact.ContentDigest{factDigest, artifact.DigestBytes([]byte("safe-sink"))},
			MissingFacts: []string{}, Contradictions: []string{}, Limitations: []string{"baseline_allow_only"},
		}},
		Limits: assurancereport.Limits{Truncated: false, Omissions: []assurancereport.Omission{}},
	}
	projections, err := assurancereport.Build(reportInput)
	if err != nil {
		t.Fatal(err)
	}

	input := artifact.ManifestInput{
		RunID: reportInput.RunID, Mode: reportInput.Mode, Schemas: schemas,
		Objects: artifact.ManifestObjects{
			ProjectSnapshot: projectSnapshot, ProjectModel: projectModel, RunProfile: runProfile,
			SDKCoverage: sdkCoverage, SandboxPosture: sandboxPosture, Scenarios: scenarios,
			SDKEvents:      sdkEvents,
			FixtureEvents:  exact(artifact.RoleFixtureEvents, "application/x-ndjson", "redacted", nil, []byte("{}\n")),
			EffectEvents:   exact(artifact.RoleEffectEvents, "application/x-ndjson", "redacted", nil, []byte("{}\n")),
			CleanupReceipt: exact(artifact.RoleCleanupReceipt, "application/json", "normalized", nil, []byte(`{"clean":true}`)),
			ReportJSON:     projections.JSONObject(), ReportMarkdown: projections.MarkdownObject(), ReportSARIF: projections.SARIFObject(),
		},
		Judgments: reportInput.Judgments, Limits: reportInput.Limits,
		RunnerVersion: reportInput.RunnerVersion, Runtime: artifact.RuntimeEnvelope{CapturedAt: "2026-08-22T00:00:00Z", SnapshotRoot: "/private/tmp/test"},
	}
	pack, err := artifact.AssemblePack(input)
	if err != nil {
		t.Fatal(err)
	}
	return pack
}
