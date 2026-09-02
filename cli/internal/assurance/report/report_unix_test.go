//go:build darwin || linux

package report

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/artifact"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/runfs"
)

func TestRenderRequiresSchemaValidProjectionBoundPack(t *testing.T) {
	verified := reportVerifiedPack(t, nil)
	validated, err := ValidatePack(verified)
	if err != nil {
		t.Fatal(err)
	}
	projection, err := Render(validated)
	if err != nil {
		t.Fatal(err)
	}
	for _, check := range []struct {
		role artifact.Role
		got  []byte
	}{
		{artifact.RoleReportJSON, projection.JSON()},
		{artifact.RoleReportMarkdown, projection.Markdown()},
		{artifact.RoleReportSARIF, projection.SARIF()},
	} {
		stored, ok := verified.Object(check.role)
		if !ok || !bytes.Equal(stored, check.got) {
			t.Fatalf("%s projection drifted", check.role)
		}
	}

	tests := []struct {
		name       string
		mutate     func(*artifact.ManifestInput)
		renderOnly bool
	}{
		{name: "schema digest", mutate: func(input *artifact.ManifestInput) {
			input.Schemas[0].Digest = artifact.DigestBytes([]byte("wrong-schema"))
		}},
		{name: "schema invalid project model", mutate: func(input *artifact.ManifestInput) {
			input.Objects.ProjectModel = reportExactObject(t, artifact.RoleProjectModel, "application/json", "openbox.project-model/v1", "normalized", []byte(`{}`))
		}},
		{name: "semantic project node duplicate", mutate: func(input *artifact.ManifestInput) {
			input.Objects.ProjectModel = mutateReportObject(t, input.Objects.ProjectModel, "application/json", "openbox.project-model/v1", func(document map[string]any) {
				nodes := document["nodes"].([]any)
				document["nodes"] = append(nodes, nodes[0])
			})
		}},
		{name: "semantic run profile binding alias", mutate: func(input *artifact.ManifestInput) {
			input.Objects.RunProfile = mutateReportObject(t, input.Objects.RunProfile, "application/json", "openbox.project-run-profile/v1", func(document map[string]any) {
				bindings := document["environment"].(map[string]any)["generatedBindings"].([]any)
				bindings[1].(map[string]any)["name"] = "SAFE_SINK_URL"
			})
		}},
		{name: "semantic SDK action duplicate", mutate: func(input *artifact.ManifestInput) {
			input.Objects.SDKCoverage = mutateReportObject(t, input.Objects.SDKCoverage, "application/json", "openbox.sdk-coverage/v1", func(document map[string]any) {
				instrumentation := document["instrumentation"].([]any)
				document["instrumentation"] = append(instrumentation, instrumentation[0])
			})
		}},
		{name: "semantic scenario drift", mutate: func(input *artifact.ManifestInput) {
			content := input.Objects.Scenarios.Bytes()
			content = bytes.Replace(content, []byte(scenarioInvariant), []byte("No causal ordering requirement remains."), 1)
			input.Objects.Scenarios = reportExactObject(t, artifact.RoleScenarios, "application/x-ndjson", "openbox.security-test/v1", "normalized", content)
		}},
		{name: "duplicate executable scenario", mutate: func(input *artifact.ManifestInput) {
			content := input.Objects.Scenarios.Bytes()
			input.Objects.Scenarios = reportExactObject(t, artifact.RoleScenarios, "application/x-ndjson", "openbox.security-test/v1", "normalized", append(append([]byte(nil), content...), content...))
		}},
		{name: "missing exact judgment", mutate: func(input *artifact.ManifestInput) {
			input.Judgments = []Judgment{}
		}},
		{name: "unbound judgment identity", mutate: func(input *artifact.ManifestInput) {
			judgments := input.Judgments.([]Judgment)
			judgments[0].FindingID = "finding-other"
			input.Judgments = judgments
		}},
		{name: "unbound SDK project model", mutate: func(input *artifact.ManifestInput) {
			input.Objects.SDKCoverage = mutateReportObject(t, input.Objects.SDKCoverage, "application/json", "openbox.sdk-coverage/v1", func(document map[string]any) {
				document["projectModelDigest"] = artifact.DigestBytes([]byte("other-project-model"))
			})
		}},
		{name: "unbound scenario run profile", mutate: func(input *artifact.ManifestInput) {
			input.Objects.Scenarios = mutateReportRecord(t, input.Objects.Scenarios, "openbox.security-test/v1", func(document map[string]any) {
				document["runProfileDigest"] = artifact.DigestBytes([]byte("other-run-profile"))
			})
		}},
		{name: "unbound project snapshot", mutate: func(input *artifact.ManifestInput) {
			input.Objects.ProjectModel = mutateReportObject(t, input.Objects.ProjectModel, "application/json", "openbox.project-model/v1", func(document map[string]any) {
				document["snapshot"].(map[string]any)["digest"] = artifact.DigestBytes([]byte("other-project-snapshot"))
			})
		}},
		{name: "stored projection drift", renderOnly: true, mutate: func(input *artifact.ManifestInput) {
			input.Objects.ReportJSON = reportExactObject(t, artifact.RoleReportJSON, "application/json", "", "public_projection", []byte(`{}`))
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			pack := reportVerifiedPack(t, test.mutate)
			validated, err := ValidatePack(pack)
			if test.renderOnly {
				if err != nil {
					t.Fatalf("structurally valid drifted projection did not reach renderer: %v", err)
				}
				if _, err := Render(validated); err == nil {
					t.Fatal("expected stored projection mismatch")
				}
				return
			}
			if err == nil {
				t.Fatal("expected schema/semantic validation rejection")
			}
		})
	}
}

func reportVerifiedPack(t *testing.T, mutate func(*artifact.ManifestInput)) *runfs.VerifiedPack {
	t.Helper()
	projectionInput := reportTestInput()
	schemas, err := SchemaReferences()
	if err != nil {
		t.Fatal(err)
	}
	root := reportRepositoryRoot(t)
	fixture := func(role artifact.Role, filename, schema string) artifact.Object {
		t.Helper()
		content := reportReadFile(t, filepath.Join(root, "contracts", "project-assurance", "testdata", "valid", filename))
		canonical, err := artifact.CanonicalizeJSON(content)
		if err != nil {
			t.Fatal(err)
		}
		mediaType := "application/json"
		if role == artifact.RoleScenarios {
			mediaType = "application/x-ndjson"
			canonical = append(canonical, '\n')
		}
		return reportExactObject(t, role, mediaType, schema, "normalized", canonical)
	}
	projectSnapshot := reportExactObject(t, artifact.RoleProjectSnapshot, "application/vnd.openbox.project-snapshot", "", "normalized", []byte(`{"files":[]}`))
	projectModel := fixture(artifact.RoleProjectModel, "project-model-v1.json", "openbox.project-model/v1")
	projectModel = mutateReportObject(t, projectModel, "application/json", "openbox.project-model/v1", func(document map[string]any) {
		document["snapshot"].(map[string]any)["digest"] = projectSnapshot.Digest()
	})
	runProfile := fixture(artifact.RoleRunProfile, "project-run-profile-v1.json", "openbox.project-run-profile/v1")
	sdkCoverage := fixture(artifact.RoleSDKCoverage, "sdk-coverage-v1.json", "openbox.sdk-coverage/v1")
	sdkCoverage = mutateReportObject(t, sdkCoverage, "application/json", "openbox.sdk-coverage/v1", func(document map[string]any) {
		document["projectModelDigest"] = projectModel.Digest()
	})
	scenarios := fixture(artifact.RoleScenarios, "security-test-v1.json", "openbox.security-test/v1")
	scenarios = mutateReportRecord(t, scenarios, "openbox.security-test/v1", func(document map[string]any) {
		document["projectModelDigest"] = projectModel.Digest()
		document["runProfileDigest"] = runProfile.Digest()
	})
	sdkEvents, sdkFactDigest := reportSDKEvents(t, projectionInput.RunID, projectSnapshot.Digest(), runProfile.Digest())
	projectionInput.Judgments[0].Evidence[0] = sdkFactDigest
	sandboxPosture := fixture(artifact.RoleSandboxPosture, "sandbox-posture-v1.json", "openbox.sandbox-posture/v1")
	sandboxPosture = mutateReportObject(t, sandboxPosture, "application/json", "openbox.sandbox-posture/v1", func(document map[string]any) {
		driver := document["driver"].(map[string]any)
		driver["name"] = "codex"
		driver["version"] = "codex-cli 0.149.0"
		platform := document["platform"].(map[string]any)
		platform["version"] = "macOS 26.5.2 (Darwin 25.5.0)"
		document["overall"] = "qualified"
	})
	fixtureEvents := reportExactObject(t, artifact.RoleFixtureEvents, "application/x-ndjson", "", "redacted", []byte("{}\n"))
	effectEvents := reportExactObject(t, artifact.RoleEffectEvents, "application/x-ndjson", "", "redacted", []byte("{}\n"))
	cleanupReceipt := reportExactObject(t, artifact.RoleCleanupReceipt, "application/json", "", "normalized", []byte(`{"clean":true}`))
	runtime := artifact.RuntimeEnvelope{CapturedAt: "2026-08-22T00:00:00Z", SnapshotRoot: "/private/tmp/openbox-report-snapshot"}
	projections, buildErr := Build(projectionInput)
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	input := artifact.ManifestInput{
		RunID: projectionInput.RunID, Mode: projectionInput.Mode, Schemas: schemas,
		Objects: artifact.ManifestObjects{
			ProjectSnapshot: projectSnapshot, ProjectModel: projectModel, RunProfile: runProfile,
			SDKCoverage: sdkCoverage, SandboxPosture: sandboxPosture, Scenarios: scenarios, SDKEvents: sdkEvents,
			FixtureEvents: fixtureEvents, EffectEvents: effectEvents, CleanupReceipt: cleanupReceipt,
			ReportJSON: projections.JSONObject(), ReportMarkdown: projections.MarkdownObject(), ReportSARIF: projections.SARIFObject(),
		},
		Judgments: projectionInput.Judgments, Limits: projectionInput.Limits, RunnerVersion: projectionInput.RunnerVersion,
		Runtime: runtime,
	}
	if mutate != nil {
		mutate(&input)
	}
	pack, err := artifact.AssemblePack(input)
	if err != nil {
		t.Fatal(err)
	}
	packRoot := filepath.Join(t.TempDir(), "pack")
	t.Cleanup(func() { makeTestPackRemovable(packRoot) })
	workspace, err := runfs.Create(packRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := workspace.WritePackObjects(pack); err != nil {
		t.Fatal(err)
	}
	if _, err := workspace.FinalizePack(pack); err != nil {
		t.Fatal(err)
	}
	verified, err := runfs.VerifyPack(packRoot)
	if err != nil {
		t.Fatal(err)
	}
	return verified
}

func reportSDKEvents(t *testing.T, runID string, snapshotDigest, profileDigest artifact.ContentDigest) (artifact.Object, artifact.ContentDigest) {
	t.Helper()
	facts := map[string]any{
		"activityType": "recordingTool", "caller": "project", "decision": "ALLOW", "eventType": "ActivityStarted",
		"markerObserved": true, "toolInputBound": true, "inputTrust": "untrusted", "sourceKind": "dependency",
		"sourceId": "poisoned-dependency", "provenanceBound": true, "method": "POST",
		"path": "/api/v1/governance/evaluate", "status": 200, "target": "safe_sink",
	}
	factsBytes, err := artifact.CanonicalJSON(facts)
	if err != nil {
		t.Fatal(err)
	}
	factDigest := artifact.DigestBytes(factsBytes)
	record, err := artifact.CanonicalJSON(map[string]any{
		"apiVersion": "openbox.normalized-evidence/v1", "kind": "sdk_event", "runId": runID,
		"scenarioId": scenarioID, "snapshotDigest": snapshotDigest, "profileDigest": profileDigest,
		"markerDigest": artifact.DigestBytes([]byte("report-fixture-marker")), "markerBytes": 21, "sequence": 1,
		"source": map[string]any{"kind": "receiver_request", "digest": factDigest, "retention": "redacted"},
		"facts":  facts, "limitations": []string{},
	})
	if err != nil {
		t.Fatal(err)
	}
	object := reportExactObject(t, artifact.RoleSDKEvents, "application/x-ndjson", "", "redacted", append(record, '\n'))
	return object, factDigest
}

func reportExactObject(t *testing.T, role artifact.Role, mediaType, schema, retention string, content []byte) artifact.Object {
	t.Helper()
	var schemaPointer *string
	if schema != "" {
		schemaPointer = &schema
	}
	object, err := artifact.NewExactObject(role, mediaType, schemaPointer, retention, content)
	if err != nil {
		t.Fatal(err)
	}
	return object
}

func reportReadFile(t *testing.T, path string) []byte {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return content
}

func mutateReportObject(t *testing.T, object artifact.Object, mediaType, schema string, mutate func(map[string]any)) artifact.Object {
	t.Helper()
	var document map[string]any
	if err := json.Unmarshal(object.Bytes(), &document); err != nil {
		t.Fatal(err)
	}
	mutate(document)
	content, err := artifact.CanonicalJSON(document)
	if err != nil {
		t.Fatal(err)
	}
	return reportExactObject(t, object.Role(), mediaType, schema, "normalized", content)
}

func mutateReportRecord(t *testing.T, object artifact.Object, schema string, mutate func(map[string]any)) artifact.Object {
	t.Helper()
	content := object.Bytes()
	if len(content) == 0 || content[len(content)-1] != '\n' {
		t.Fatal("report record is not LF terminated")
	}
	var document map[string]any
	if err := json.Unmarshal(content[:len(content)-1], &document); err != nil {
		t.Fatal(err)
	}
	mutate(document)
	canonical, err := artifact.CanonicalJSON(document)
	if err != nil {
		t.Fatal(err)
	}
	return reportExactObject(t, object.Role(), "application/x-ndjson", schema, "normalized", append(canonical, '\n'))
}

func makeTestPackRemovable(root string) {
	_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if entry.IsDir() {
			_ = os.Chmod(path, 0o700)
		} else {
			_ = os.Chmod(path, 0o600)
		}
		return nil
	})
}
