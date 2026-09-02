package artifact

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestAssemblePackSeparatesNormalizedObjectsFromRunProvenance(t *testing.T) {
	first := testManifestInput(t, "run-001", "1.2.3", false)
	second := testManifestInput(t, "run-001", "1.2.3", true)

	firstPack, err := AssemblePack(first)
	if err != nil {
		t.Fatal(err)
	}
	secondPack, err := AssemblePack(second)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(firstPack.Manifest(), secondPack.Manifest()) || firstPack.Digest() != secondPack.Digest() {
		t.Fatalf("insertion order changed manifest identity:\n%s\n%s", firstPack.Manifest(), secondPack.Manifest())
	}
	assertSameObjects(t, firstPack.Objects(), secondPack.Objects())

	volatile := testManifestInput(t, "run-002", "1.2.4", false)
	volatilePack, err := AssemblePack(volatile)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(firstPack.Manifest(), volatilePack.Manifest()) || firstPack.Digest() == volatilePack.Digest() {
		t.Fatal("run provenance did not change manifest identity")
	}
	assertSameObjects(t, firstPack.Objects(), volatilePack.Objects())
	for _, object := range volatilePack.Objects() {
		content := string(object.Bytes())
		if strings.Contains(content, "run-002") || strings.Contains(content, "1.2.4") || strings.Contains(content, t.TempDir()) {
			t.Fatalf("volatile provenance leaked into %q object", object.Role())
		}
	}

	runtimeOnly := testManifestInput(t, "run-001", "1.2.3", false)
	runtimeOnly.Runtime = RuntimeEnvelope{CapturedAt: "2026-08-20T11:00:00Z", SnapshotRoot: "/private/tmp/openbox-run-2"}
	runtimePack, err := AssemblePack(runtimeOnly)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(firstPack.Manifest(), runtimePack.Manifest()) || firstPack.Digest() != runtimePack.Digest() {
		t.Fatal("volatile timestamp/temp path perturbed retained manifest identity")
	}
	assertSameObjects(t, firstPack.Objects(), runtimePack.Objects())
	if firstPack.RuntimeEnvelope() == runtimePack.RuntimeEnvelope() {
		t.Fatal("changed timestamp/temp path did not change runtime envelope")
	}
}

func TestAssemblePackChangedObjectChangesOnlyItsCIDAndManifest(t *testing.T) {
	firstInput := testManifestInput(t, "run-001", "1.2.3", false)
	secondInput := testManifestInput(t, "run-001", "1.2.3", false)
	changed, err := NewExactObject(RoleProjectSnapshot, "application/vnd.openbox.project-snapshot", nil, "normalized", []byte(`{"files":[{"path":"changed"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	secondInput.Objects.ProjectSnapshot = changed

	first, err := AssemblePack(firstInput)
	if err != nil {
		t.Fatal(err)
	}
	second, err := AssemblePack(secondInput)
	if err != nil {
		t.Fatal(err)
	}
	if first.Digest() == second.Digest() {
		t.Fatal("changed object did not change manifest identity")
	}
	differences := 0
	for index, left := range first.Objects() {
		right := second.Objects()[index]
		if left.Digest() != right.Digest() {
			differences++
			if left.Role() != RoleProjectSnapshot || right.Role() != RoleProjectSnapshot {
				t.Fatalf("unexpected changed role %q/%q", left.Role(), right.Role())
			}
		}
	}
	if differences != 1 {
		t.Fatalf("changed object altered %d object identities, want 1", differences)
	}
}

func TestAssemblePackDerivesJudgmentsObject(t *testing.T) {
	input := testManifestInput(t, "run-001", "1.2.3", false)
	pack, err := AssemblePack(input)
	if err != nil {
		t.Fatal(err)
	}
	canonicalJudgments, _, err := DigestCanonicalJSON(input.Judgments)
	if err != nil {
		t.Fatal(err)
	}
	var judgmentObject Object
	for _, object := range pack.Objects() {
		if object.Role() == RoleJudgments {
			judgmentObject = object
			break
		}
	}
	if !bytes.Equal(judgmentObject.Bytes(), canonicalJudgments) {
		t.Fatalf("judgments object = %s, want %s", judgmentObject.Bytes(), canonicalJudgments)
	}

	var manifest map[string]any
	if err := json.Unmarshal(pack.Manifest(), &manifest); err != nil {
		t.Fatal(err)
	}
	objects := manifest["objects"].(map[string]any)
	judgmentReference := objects[string(RoleJudgments)].(map[string]any)
	if judgmentReference["cid"] != judgmentObject.Digest().String() || judgmentReference["bytes"] != float64(len(canonicalJudgments)) {
		t.Fatalf("judgment reference = %#v", judgmentReference)
	}
}

func TestAssemblePackOptionalPolicyProposal(t *testing.T) {
	input := testManifestInput(t, "run-001", "1.2.3", false)
	proposal, err := NewExactObject(
		RolePolicyProposals,
		"application/x-ndjson",
		stringPointer(schemaIDs[6]),
		"normalized",
		[]byte(`{"apiVersion":"openbox.policy-proposal/v1","kind":"PolicyProposal"}`+"\n"),
	)
	if err != nil {
		t.Fatal(err)
	}
	input.Objects.PolicyProposals = &proposal
	pack, err := AssemblePack(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(pack.Objects()) != len(requiredRoles)+1 || !bytes.Contains(pack.Manifest(), []byte(`"policy-proposals"`)) {
		t.Fatalf("optional proposal role was not assembled: %s", pack.Manifest())
	}
}

func TestAssemblePackRejectsRoleAndSchemaContradictions(t *testing.T) {
	t.Run("wrong role in fixed field", func(t *testing.T) {
		input := testManifestInput(t, "run-001", "1.2.3", false)
		input.Objects.ProjectSnapshot = input.Objects.ProjectModel
		if _, err := AssemblePack(input); err == nil || !strings.Contains(err.Error(), "project-snapshot") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("schema inventory order", func(t *testing.T) {
		input := testManifestInput(t, "run-001", "1.2.3", false)
		input.Schemas[0], input.Schemas[1] = input.Schemas[1], input.Schemas[0]
		if _, err := AssemblePack(input); err == nil || !strings.Contains(err.Error(), "schema inventory") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("role schema mapping", func(t *testing.T) {
		wrong := "openbox.project-model/v1"
		if _, err := NewExactObject(RoleSDKEvents, "application/x-ndjson", &wrong, "redacted", []byte("{}\n")); err == nil {
			t.Fatal("expected role/schema contradiction")
		}
	})

	t.Run("retention omission correlation", func(t *testing.T) {
		if _, err := NewExactObject(RoleSDKEvents, "application/x-ndjson", nil, "omitted", []byte("{}")); err == nil {
			t.Fatal("expected payload-bearing omitted retention rejection")
		}
	})

	t.Run("media encoding", func(t *testing.T) {
		if _, err := NewExactObject(RoleProjectModel, "text/plain", stringPointer(schemaIDs[0]), "normalized", []byte(`{}`)); err == nil {
			t.Fatal("expected role media-type rejection")
		}
		if _, err := NewExactObject(RoleProjectModel, "application/json", stringPointer(schemaIDs[0]), "normalized", []byte(`{ "kind": "ProjectModel" }`)); err == nil {
			t.Fatal("expected non-canonical JSON rejection")
		}
		if _, err := NewExactObject(RoleSDKEvents, "application/x-ndjson", nil, "redacted", []byte(`{}`)); err == nil {
			t.Fatal("expected JSONL final-LF rejection")
		}
		if _, err := NewExactObject(RoleSDKEvents, "application/x-ndjson", nil, "redacted", []byte("{ \"b\":2, \"a\":1 }\n")); err == nil {
			t.Fatal("expected non-canonical JSONL record rejection")
		}
	})
}

func TestPackAccessorsAreDefensive(t *testing.T) {
	pack, err := AssemblePack(testManifestInput(t, "run-001", "1.2.3", false))
	if err != nil {
		t.Fatal(err)
	}
	manifest := pack.Manifest()
	manifest[0] = '!'
	objects := pack.Objects()
	content := objects[0].Bytes()
	content[0] = '!'
	if pack.Manifest()[0] == '!' || pack.Objects()[0].Bytes()[0] == '!' {
		t.Fatal("pack accessor exposed mutable internal bytes")
	}
}

func testManifestInput(t *testing.T, runID, runnerVersion string, reverseMaps bool) ManifestInput {
	t.Helper()
	canonicalObject := func(role Role, schema *string, value any) Object {
		t.Helper()
		object, err := NewCanonicalObject(role, "application/json", schema, "normalized", value)
		if err != nil {
			t.Fatal(err)
		}
		return object
	}
	exactObject := func(role Role, mediaType, retention, value string) Object {
		t.Helper()
		object, err := NewExactObject(role, mediaType, nil, retention, []byte(value))
		if err != nil {
			t.Fatal(err)
		}
		return object
	}

	projectModelValue := map[string]any{"kind": "ProjectModel", "apiVersion": "openbox.project-model/v1"}
	limits := map[string]any{"truncated": false, "omissions": []any{}}
	judgment := map[string]any{"findingId": "finding-1", "outcome": "inconclusive"}
	if reverseMaps {
		projectModelValue = map[string]any{"apiVersion": "openbox.project-model/v1", "kind": "ProjectModel"}
		limits = map[string]any{"omissions": []any{}, "truncated": false}
		judgment = map[string]any{"outcome": "inconclusive", "findingId": "finding-1"}
	}

	return ManifestInput{
		RunID: runID,
		Mode:  "baseline",
		Schemas: []SchemaReference{
			{ID: schemaIDs[0], Digest: DigestBytes([]byte("schema-0"))},
			{ID: schemaIDs[1], Digest: DigestBytes([]byte("schema-1"))},
			{ID: schemaIDs[2], Digest: DigestBytes([]byte("schema-2"))},
			{ID: schemaIDs[3], Digest: DigestBytes([]byte("schema-3"))},
			{ID: schemaIDs[4], Digest: DigestBytes([]byte("schema-4"))},
			{ID: schemaIDs[5], Digest: DigestBytes([]byte("schema-5"))},
			{ID: schemaIDs[6], Digest: DigestBytes([]byte("schema-6"))},
		},
		Objects: ManifestObjects{
			ProjectSnapshot: exactObject(RoleProjectSnapshot, "application/vnd.openbox.project-snapshot", "normalized", `{"files":[]}`),
			ProjectModel:    canonicalObject(RoleProjectModel, stringPointer(schemaIDs[0]), projectModelValue),
			RunProfile:      canonicalObject(RoleRunProfile, stringPointer(schemaIDs[1]), map[string]any{"profile": "mvp"}),
			SDKCoverage:     canonicalObject(RoleSDKCoverage, stringPointer(schemaIDs[2]), map[string]any{"sdk": "mastra"}),
			SandboxPosture:  canonicalObject(RoleSandboxPosture, stringPointer(schemaIDs[3]), map[string]any{"sandbox": "qualified"}),
			Scenarios:       exactSchemaObject(t, RoleScenarios, "application/x-ndjson", schemaIDs[4], "{\"scenario\":\"one\"}\n"),
			SDKEvents:       exactObject(RoleSDKEvents, "application/x-ndjson", "redacted", "{}\n"),
			FixtureEvents:   exactObject(RoleFixtureEvents, "application/x-ndjson", "redacted", "{}\n"),
			EffectEvents:    exactObject(RoleEffectEvents, "application/x-ndjson", "redacted", "{}\n"),
			CleanupReceipt:  canonicalObject(RoleCleanupReceipt, nil, map[string]any{"clean": true}),
			ReportJSON:      exactObject(RoleReportJSON, "application/json", "public_projection", "{}"),
			ReportMarkdown:  exactObject(RoleReportMarkdown, "text/markdown", "public_projection", "report\n"),
			ReportSARIF:     exactObject(RoleReportSARIF, "application/sarif+json", "public_projection", "{}"),
		},
		Judgments:     []any{judgment},
		Limits:        limits,
		RunnerVersion: runnerVersion,
		Runtime:       RuntimeEnvelope{CapturedAt: "2026-08-20T10:00:00Z", SnapshotRoot: "/private/tmp/openbox-run-1"},
	}
}

func exactSchemaObject(t *testing.T, role Role, mediaType, schema, content string) Object {
	t.Helper()
	object, err := NewExactObject(role, mediaType, stringPointer(schema), "normalized", []byte(content))
	if err != nil {
		t.Fatal(err)
	}
	return object
}

func assertSameObjects(t *testing.T, left, right []Object) {
	t.Helper()
	if len(left) != len(right) {
		t.Fatalf("object count = %d/%d", len(left), len(right))
	}
	for index := range left {
		if left[index].Role() != right[index].Role() || left[index].Digest() != right[index].Digest() || !bytes.Equal(left[index].Bytes(), right[index].Bytes()) {
			t.Fatalf("object %d differs: %q/%q", index, left[index].Role(), right[index].Role())
		}
	}
}
