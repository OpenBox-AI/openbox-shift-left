//go:build darwin || linux

package runfs

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/artifact"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/snapshot"
	"golang.org/x/sys/unix"
)

const maxContractProbeBytes = 1 << 20

func TestSnapshotOmissionsRemainVisibleInValidatedFinalManifest(t *testing.T) {
	ajvRoot := os.Getenv("OPENBOX_AJV_ROOT")
	if ajvRoot == "" {
		t.Skip("set OPENBOX_AJV_ROOT to an offline Ajv 8.17.1 package root to run contract integration")
	}
	parent := t.TempDir()
	projectRoot := filepath.Join(parent, "project")
	if err := os.Mkdir(projectRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, filepath.Join(projectRoot, "package.json"), []byte(`{"name":"fixture"}`))
	writeFixtureFile(t, filepath.Join(projectRoot, ".env"), []byte("SECRET=must-not-appear"))
	writeFixtureFile(t, filepath.Join(projectRoot, ".cache", "cached"), []byte("cache"))
	writeFixtureFile(t, filepath.Join(projectRoot, ".openbox", "audit", "prior", "manifest.json"), []byte(`{}`))
	external := filepath.Join(parent, "outside")
	writeFixtureFile(t, external, []byte("outside"))
	if err := os.Symlink(external, filepath.Join(projectRoot, "external-link")); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(projectRoot, "named-pipe"), 0o600); err != nil {
		t.Fatal(err)
	}

	snapshotRoot := filepath.Join(parent, "snapshot")
	if err := os.Mkdir(snapshotRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	project, err := snapshot.Resolve(projectRoot, snapshot.Boundaries{
		AuditOutput: filepath.Join(projectRoot, ".openbox", "audit", "current"),
		TempParent:  snapshotRoot,
	})
	if err != nil {
		t.Fatal(err)
	}
	copied, err := project.Copy(snapshotRoot)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { makeWritableForTest(snapshotRoot) })
	wantClasses := map[snapshot.PathClass]bool{
		snapshot.PathClassAuditOutput:     false,
		snapshot.PathClassCache:           false,
		snapshot.PathClassSecret:          false,
		snapshot.PathClassExternalSymlink: false,
		snapshot.PathClassFIFO:            false,
	}
	for _, omission := range copied.Omissions() {
		if _, expected := wantClasses[omission.PathClass]; expected {
			wantClasses[omission.PathClass] = true
		}
	}
	for class, observed := range wantClasses {
		if !observed {
			t.Fatalf("missing actual omission class %q", class)
		}
	}

	documents, fixtures, contractsRoot := loadContractAssets(t)
	projectModel := decodeTestJSON(t, fixtures["openbox.project-model/v1"])
	projectObject := projectModel.(map[string]any)
	projectObject["project"] = map[string]any{
		"name": "mastra-mvp",
		"root": ".",
		"git":  map[string]any{"present": false, "head": nil, "dirty": nil},
	}
	projectObject["snapshot"] = map[string]any{
		"digest":          copied.Digest(),
		"selectionDigest": copied.SelectionDigest(),
		"fileCount":       copied.FileCount(),
		"totalBytes":      copied.TotalBytes(),
		"selectionRules":  copied.SelectionRules(),
		"omissions":       copied.Omissions(),
	}
	projectModelBytes, _, err := artifact.DigestCanonicalJSON(projectObject)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(projectModelBytes, []byte("must-not-appear")) || bytes.Contains(projectModelBytes, []byte(projectRoot)) || bytes.Contains(projectModelBytes, []byte(snapshotRoot)) {
		t.Fatalf("secret or volatile path leaked into project model: %s", projectModelBytes)
	}
	validateContractDocument(t, ajvRoot, contractsRoot, "openbox.project-model/v1", projectModelBytes)
	for class := range wantClasses {
		if !bytes.Contains(projectModelBytes, []byte(`"pathClass":"`+string(class)+`"`)) {
			t.Fatalf("validated project model does not disclose omission class %q", class)
		}
	}

	pack := assembleValidatedFixturePack(t, documents, fixtures, copied.Manifest(), projectObject)
	validateContractDocument(t, ajvRoot, contractsRoot, "openbox.audit-pack/v1", pack.Manifest())
	if !bytes.Contains(pack.Manifest(), []byte(`"project-model"`)) {
		t.Fatal("validated manifest does not reference project-model")
	}
	var packedProjectModel artifact.Object
	for _, object := range pack.Objects() {
		if object.Role() == artifact.RoleProjectModel {
			packedProjectModel = object
			break
		}
	}
	if !bytes.Equal(packedProjectModel.Bytes(), projectModelBytes) || !bytes.Contains(pack.Manifest(), []byte(packedProjectModel.Digest().String())) {
		t.Fatal("manifest project-model reference does not bind the validated omission object")
	}

	packRoot := filepath.Join(parent, "audit-pack")
	workspace, err := Create(packRoot)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = removeOwnedTree(packRoot, workspace.identity) })
	if err := workspace.WritePackObjects(pack); err != nil {
		t.Fatal(err)
	}
	digest, err := workspace.FinalizePack(pack)
	if err != nil {
		t.Fatal(err)
	}
	if digest != pack.Digest() {
		t.Fatalf("final digest = %s, want %s", digest, pack.Digest())
	}
	if state, err := Inspect(packRoot); err != nil || state != StateManifestCommitted {
		t.Fatalf("final state = %q, %v", state, err)
	}
}

func assembleValidatedFixturePack(
	t *testing.T,
	documents map[string][]byte,
	fixtures map[string][]byte,
	projectSnapshot []byte,
	projectModel any,
) *artifact.Pack {
	t.Helper()
	ids := []string{
		"openbox.project-model/v1",
		"openbox.project-run-profile/v1",
		"openbox.sdk-coverage/v1",
		"openbox.sandbox-posture/v1",
		"openbox.security-test/v1",
		"openbox.audit-pack/v1",
		"openbox.policy-proposal/v1",
	}
	schemas := make([]artifact.SchemaReference, len(ids))
	for index, identifier := range ids {
		schemas[index] = artifact.SchemaReference{ID: identifier, Digest: artifact.DigestBytes(documents[identifier])}
	}
	pointer := func(value string) *string { return &value }
	canonical := func(role artifact.Role, schema *string, value any) artifact.Object {
		t.Helper()
		object, err := artifact.NewCanonicalObject(role, expectedTestMediaType(role), schema, "normalized", value)
		if err != nil {
			t.Fatal(err)
		}
		return object
	}
	exact := func(role artifact.Role, schema *string, retention string, content []byte) artifact.Object {
		t.Helper()
		object, err := artifact.NewExactObject(role, expectedTestMediaType(role), schema, retention, content)
		if err != nil {
			t.Fatal(err)
		}
		return object
	}
	securityTest := mustCanonicalTestJSON(t, decodeTestJSON(t, fixtures["openbox.security-test/v1"]))
	auditFixture := decodeTestJSON(t, fixtures["openbox.audit-pack/v1"]).(map[string]any)
	input := artifact.ManifestInput{
		RunID:   "run-phase01-conformance",
		Mode:    "baseline",
		Schemas: schemas,
		Objects: artifact.ManifestObjects{
			ProjectSnapshot: exact(artifact.RoleProjectSnapshot, nil, "normalized", projectSnapshot),
			ProjectModel:    canonical(artifact.RoleProjectModel, pointer(ids[0]), projectModel),
			RunProfile:      exact(artifact.RoleRunProfile, pointer(ids[1]), "normalized", mustCanonicalTestJSON(t, decodeTestJSON(t, fixtures[ids[1]]))),
			SDKCoverage:     exact(artifact.RoleSDKCoverage, pointer(ids[2]), "normalized", mustCanonicalTestJSON(t, decodeTestJSON(t, fixtures[ids[2]]))),
			SandboxPosture:  exact(artifact.RoleSandboxPosture, pointer(ids[3]), "normalized", mustCanonicalTestJSON(t, decodeTestJSON(t, fixtures[ids[3]]))),
			Scenarios:       exact(artifact.RoleScenarios, pointer(ids[4]), "normalized", append(securityTest, '\n')),
			SDKEvents:       exact(artifact.RoleSDKEvents, nil, "redacted", nil),
			FixtureEvents:   exact(artifact.RoleFixtureEvents, nil, "redacted", nil),
			EffectEvents:    exact(artifact.RoleEffectEvents, nil, "redacted", nil),
			CleanupReceipt:  canonical(artifact.RoleCleanupReceipt, nil, map[string]any{"clean": true}),
			ReportJSON:      exact(artifact.RoleReportJSON, nil, "public_projection", []byte(`{}`)),
			ReportMarkdown:  exact(artifact.RoleReportMarkdown, nil, "public_projection", []byte("# Report\n")),
			ReportSARIF:     exact(artifact.RoleReportSARIF, nil, "public_projection", []byte(`{}`)),
		},
		Judgments:     auditFixture["judgments"],
		Limits:        auditFixture["limits"],
		RunnerVersion: "0.0.0-phase01-test",
		Runtime: artifact.RuntimeEnvelope{
			CapturedAt:   "2026-08-20T12:00:00Z",
			SnapshotRoot: "/private/tmp/openbox-phase01-test",
		},
	}
	pack, err := artifact.AssemblePack(input)
	if err != nil {
		t.Fatal(err)
	}
	return pack
}

func loadContractAssets(t *testing.T) (map[string][]byte, map[string][]byte, string) {
	t.Helper()
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate contract integration test")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(source), "..", "..", "..", "..", "contracts", "project-assurance"))
	files := map[string]string{
		"openbox.project-model/v1":       "project-model-v1",
		"openbox.project-run-profile/v1": "project-run-profile-v1",
		"openbox.sdk-coverage/v1":        "sdk-coverage-v1",
		"openbox.sandbox-posture/v1":     "sandbox-posture-v1",
		"openbox.security-test/v1":       "security-test-v1",
		"openbox.audit-pack/v1":          "audit-pack-v1",
		"openbox.policy-proposal/v1":     "policy-proposal-v1",
	}
	documents := make(map[string][]byte, len(files))
	fixtures := make(map[string][]byte, len(files))
	for identifier, base := range files {
		var err error
		documents[identifier], err = os.ReadFile(filepath.Join(root, "schema", base+".schema.json"))
		if err != nil {
			t.Fatal(err)
		}
		fixtures[identifier], err = os.ReadFile(filepath.Join(root, "testdata", "valid", base+".json"))
		if err != nil {
			t.Fatal(err)
		}
	}
	return documents, fixtures, root
}

func validateContractDocument(t *testing.T, ajvRoot, contractsRoot, identifier string, document []byte) {
	t.Helper()
	if len(document) > maxContractProbeBytes {
		t.Fatalf("%s document exceeds %d bytes", identifier, maxContractProbeBytes)
	}
	temporary, err := os.CreateTemp(t.TempDir(), "contract-*.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := temporary.Write(document); err != nil {
		_ = temporary.Close()
		t.Fatal(err)
	}
	if err := temporary.Close(); err != nil {
		t.Fatal(err)
	}
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate contract integration test")
	}
	repositoryRoot := filepath.Clean(filepath.Join(filepath.Dir(source), "..", "..", "..", ".."))
	probe := filepath.Join(repositoryRoot, "plans", "260819-1600-project-security-evaluation", "evidence", "probes", "validate_project_assurance_schemas.mjs")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "node", probe, ajvRoot, contractsRoot, identifier, temporary.Name())
	output := boundedContractProbeOutput{remaining: maxContractProbeBytes}
	command.Stdout = &output
	command.Stderr = &output
	err = command.Run()
	if ctx.Err() != nil {
		t.Fatalf("%s validation exceeded 30 seconds: %v", identifier, ctx.Err())
	}
	if output.truncated {
		t.Fatalf("%s validation exceeded %d output bytes", identifier, maxContractProbeBytes)
	}
	if err != nil {
		t.Fatalf("%s validation failed: %v\n%s", identifier, err, output.buffer.Bytes())
	}
	var result struct {
		Status             string `json:"status"`
		Validator          string `json:"validator"`
		DocumentValidation struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"documentValidation"`
	}
	if err := json.Unmarshal(output.buffer.Bytes(), &result); err != nil {
		t.Fatalf("decode %s validation output: %v\n%s", identifier, err, output.buffer.Bytes())
	}
	if result.Status != "passed" || result.Validator != "ajv@8.17.1" || result.DocumentValidation.ID != identifier || result.DocumentValidation.Status != "passed" {
		t.Fatalf("unexpected %s validation result: %+v", identifier, result)
	}
}

type boundedContractProbeOutput struct {
	buffer    bytes.Buffer
	remaining int
	truncated bool
}

func (output *boundedContractProbeOutput) Write(content []byte) (int, error) {
	written := len(content)
	if len(content) > output.remaining {
		content = content[:output.remaining]
		output.truncated = true
	}
	_, _ = output.buffer.Write(content)
	output.remaining -= len(content)
	return written, nil
}

func expectedTestMediaType(role artifact.Role) string {
	switch role {
	case artifact.RoleProjectSnapshot:
		return "application/vnd.openbox.project-snapshot"
	case artifact.RoleScenarios, artifact.RoleSDKEvents, artifact.RoleFixtureEvents, artifact.RoleEffectEvents, artifact.RolePolicyProposals:
		return "application/x-ndjson"
	case artifact.RoleReportMarkdown:
		return "text/markdown"
	case artifact.RoleReportSARIF:
		return "application/sarif+json"
	default:
		return "application/json"
	}
}

func decodeTestJSON(t *testing.T, raw []byte) any {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		t.Fatal(err)
	}
	return value
}

func mustCanonicalTestJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := artifact.CanonicalizeJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	return canonical
}

func writeFixtureFile(t *testing.T, path string, content []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
}

func makeWritableForTest(root string) {
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
