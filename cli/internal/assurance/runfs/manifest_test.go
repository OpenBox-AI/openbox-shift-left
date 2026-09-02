package runfs

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/artifact"
)

func TestWritePackObjectsThenFinalizeManifestLast(t *testing.T) {
	pack := testAssembledPack(t)
	root := filepath.Join(t.TempDir(), "run-pack")
	workspace, err := Create(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = removeOwnedTree(root, workspace.identity) })

	if err := workspace.WritePackObjects(pack); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(root, ManifestName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("manifest exists before finalization: %v", err)
	}

	unique := make(map[string][]byte)
	for _, object := range pack.Objects() {
		name := object.Digest().String()[len("sha256:"):]
		unique[name] = object.Bytes()
	}
	objectDirectory := filepath.Join(root, "objects", "sha256")
	entries, err := os.ReadDir(objectDirectory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(unique) {
		t.Fatalf("stored object count = %d, want %d", len(entries), len(unique))
	}
	for name, want := range unique {
		path := filepath.Join(objectDirectory, name)
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("object %s changed", name)
		}
		assertMode(t, path, 0o600)
	}

	digest, err := workspace.FinalizePack(pack)
	if err != nil {
		t.Fatal(err)
	}
	if digest != pack.Digest() {
		t.Fatalf("finalized digest = %s, want %s", digest, pack.Digest())
	}
	for name := range unique {
		assertMode(t, filepath.Join(objectDirectory, name), 0o400)
	}
	manifest, err := os.ReadFile(filepath.Join(root, ManifestName))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(manifest, pack.Manifest()) {
		t.Fatal("finalized manifest bytes changed")
	}
}

func TestWritePackObjectsFailsClosed(t *testing.T) {
	t.Run("nil pack", func(t *testing.T) {
		workspace, err := Create(filepath.Join(t.TempDir(), "run-nil"))
		if err != nil {
			t.Fatal(err)
		}
		defer cleanupIncomplete(t, workspace)
		if err := workspace.WritePackObjects(nil); err == nil {
			t.Fatal("expected nil pack rejection")
		}
	})

	t.Run("existing object", func(t *testing.T) {
		pack := testAssembledPack(t)
		workspace, err := Create(filepath.Join(t.TempDir(), "run-existing-object"))
		if err != nil {
			t.Fatal(err)
		}
		defer cleanupIncomplete(t, workspace)
		if err := workspace.WritePackObjects(pack); err != nil {
			t.Fatal(err)
		}
		if err := workspace.WritePackObjects(pack); err == nil {
			t.Fatal("expected exclusive object rewrite rejection")
		}
		if state, err := Inspect(workspace.Root()); err != nil || state != StateIncomplete {
			t.Fatalf("state = %q, %v", state, err)
		}
	})

	t.Run("mutated object before manifest", func(t *testing.T) {
		pack := testAssembledPack(t)
		workspace, err := Create(filepath.Join(t.TempDir(), "run-mutated-object"))
		if err != nil {
			t.Fatal(err)
		}
		defer cleanupIncomplete(t, workspace)
		if err := workspace.WritePackObjects(pack); err != nil {
			t.Fatal(err)
		}
		object := pack.Objects()[0]
		name := object.Digest().String()[len("sha256:"):]
		path := filepath.Join(workspace.Root(), "objects", "sha256", name)
		if err := os.WriteFile(path, []byte(`{"tampered":true}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := workspace.FinalizePack(pack); err == nil {
			t.Fatal("expected mutated object rejection")
		}
		assertIncompleteWithoutManifest(t, workspace)
	})

	t.Run("extra object before manifest", func(t *testing.T) {
		pack := testAssembledPack(t)
		workspace, err := Create(filepath.Join(t.TempDir(), "run-extra-object"))
		if err != nil {
			t.Fatal(err)
		}
		defer cleanupIncomplete(t, workspace)
		if err := workspace.WritePackObjects(pack); err != nil {
			t.Fatal(err)
		}
		extraName := "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
		path := filepath.Join(workspace.Root(), "objects", "sha256", extraName)
		if err := os.WriteFile(path, []byte(`{}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := workspace.FinalizePack(pack); err == nil {
			t.Fatal("expected extra object rejection")
		}
		assertIncompleteWithoutManifest(t, workspace)
	})
}

func assertIncompleteWithoutManifest(t *testing.T, workspace *Workspace) {
	t.Helper()
	if _, err := os.Lstat(filepath.Join(workspace.Root(), ManifestName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("manifest exists after object verification failure: %v", err)
	}
	if state, err := Inspect(workspace.Root()); err != nil || state != StateIncomplete {
		t.Fatalf("state = %q, %v", state, err)
	}
}

func testAssembledPack(t *testing.T) *artifact.Pack {
	t.Helper()
	schemaIDs := []string{
		"openbox.project-model/v1",
		"openbox.project-run-profile/v1",
		"openbox.sdk-coverage/v1",
		"openbox.sandbox-posture/v1",
		"openbox.security-test/v1",
		"openbox.audit-pack/v1",
		"openbox.policy-proposal/v1",
	}
	schemas := make([]artifact.SchemaReference, len(schemaIDs))
	for index, id := range schemaIDs {
		schemas[index] = artifact.SchemaReference{ID: id, Digest: artifact.DigestBytes([]byte(id))}
	}
	exact := func(role artifact.Role, mediaType, retention string, schema *string, content string) artifact.Object {
		t.Helper()
		object, err := artifact.NewExactObject(role, mediaType, schema, retention, []byte(content))
		if err != nil {
			t.Fatal(err)
		}
		return object
	}
	canonical := func(role artifact.Role, schema *string, value any) artifact.Object {
		t.Helper()
		object, err := artifact.NewCanonicalObject(role, "application/json", schema, "normalized", value)
		if err != nil {
			t.Fatal(err)
		}
		return object
	}
	pointer := func(value string) *string { return &value }
	input := artifact.ManifestInput{
		RunID:   "run-test",
		Mode:    "baseline",
		Schemas: schemas,
		Objects: artifact.ManifestObjects{
			ProjectSnapshot: exact(artifact.RoleProjectSnapshot, "application/vnd.openbox.project-snapshot", "normalized", nil, `{"files":[]}`),
			ProjectModel:    canonical(artifact.RoleProjectModel, pointer(schemaIDs[0]), map[string]any{"kind": "ProjectModel"}),
			RunProfile:      canonical(artifact.RoleRunProfile, pointer(schemaIDs[1]), map[string]any{"profile": "mvp"}),
			SDKCoverage:     canonical(artifact.RoleSDKCoverage, pointer(schemaIDs[2]), map[string]any{"sdk": "mastra"}),
			SandboxPosture:  canonical(artifact.RoleSandboxPosture, pointer(schemaIDs[3]), map[string]any{"sandbox": "qualified"}),
			Scenarios:       exact(artifact.RoleScenarios, "application/x-ndjson", "normalized", pointer(schemaIDs[4]), "{}\n"),
			SDKEvents:       exact(artifact.RoleSDKEvents, "application/x-ndjson", "redacted", nil, "{}\n"),
			FixtureEvents:   exact(artifact.RoleFixtureEvents, "application/x-ndjson", "redacted", nil, "{}\n"),
			EffectEvents:    exact(artifact.RoleEffectEvents, "application/x-ndjson", "redacted", nil, "{}\n"),
			CleanupReceipt:  canonical(artifact.RoleCleanupReceipt, nil, map[string]any{"clean": true}),
			ReportJSON:      exact(artifact.RoleReportJSON, "application/json", "public_projection", nil, "{}"),
			ReportMarkdown:  exact(artifact.RoleReportMarkdown, "text/markdown", "public_projection", nil, "report\n"),
			ReportSARIF:     exact(artifact.RoleReportSARIF, "application/sarif+json", "public_projection", nil, "{}"),
		},
		Judgments:     []any{map[string]any{"outcome": "inconclusive"}},
		Limits:        map[string]any{"truncated": false, "omissions": []any{}},
		RunnerVersion: "0.0.0-test",
		Runtime:       artifact.RuntimeEnvelope{CapturedAt: "2026-08-20T11:00:00Z", SnapshotRoot: "/private/tmp/openbox-test"},
	}
	pack, err := artifact.AssemblePack(input)
	if err != nil {
		t.Fatal(err)
	}
	return pack
}
