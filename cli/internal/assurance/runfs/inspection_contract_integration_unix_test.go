//go:build darwin || linux

package runfs

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/inspect"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/model"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/sdkdesc"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/snapshot"
)

func TestGeneratedInspectionArtifactsConformToPinnedSchemas(t *testing.T) {
	ajvRoot := os.Getenv("OPENBOX_AJV_ROOT")
	if ajvRoot == "" {
		t.Skip("set OPENBOX_AJV_ROOT to the qualified Ajv 8.17.1 package root")
	}
	projectRoot := t.TempDir()
	sourceSentinel := filepath.Join(projectRoot, "must-not-run")
	files := map[string][]byte{
		"package.json": []byte(`{
  "main":"src/index.ts",
  "scripts":{"postinstall":"touch must-not-run"},
  "dependencies":{"@openbox-ai/openbox-mastra-sdk":"1.0.0"}
}`),
		"src/index.ts": []byte(`import { withOpenBox } from "@openbox-ai/openbox-mastra-sdk";
const tool = createTool({ id: "recording-tool" });
fetch("https://sink.invalid/effect");
`),
	}
	for relative, content := range files {
		full := filepath.Join(projectRoot, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	destinationParent := t.TempDir()
	destination := filepath.Join(destinationParent, "snapshot")
	snapshotSentinel := filepath.Join(destination, "must-not-run")
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
	t.Cleanup(func() { makeInspectionContractWritable(destination) })
	detection, err := inspect.Detect(copied)
	if err != nil {
		t.Fatal(err)
	}
	graph, err := model.Normalize(detection)
	if err != nil {
		t.Fatal(err)
	}
	projectArtifacts, err := model.BuildProjectArtifacts(copied, graph, model.ProjectIdentity{Name: "mastra-mvp"})
	if err != nil {
		t.Fatal(err)
	}
	coverage, err := sdkdesc.DeriveExpectedCoverage(graph, exactMastraCompatibility())
	if err != nil {
		t.Fatal(err)
	}
	coverageArtifacts, err := sdkdesc.BuildCoverageArtifacts(projectArtifacts, coverage)
	if err != nil {
		t.Fatal(err)
	}
	gotDigests := []string{
		projectArtifacts.SnapshotManifest().Digest().String(),
		projectArtifacts.ProjectModel().Digest().String(),
		coverageArtifacts.SDKCoverage.Digest().String(),
	}
	wantDigests := []string{
		"sha256:272d9e7d953f7c74f328c6a4bf4afc467b89135153d4d02a199ea0db77ba6bef",
		"sha256:b2763fbfa0cfd60a148239b69934051f4ac1595a0fdc6eb40fc81a18cc5132df",
		"sha256:7a00e3c7e0bd6653c95230b78fe92d6da08ec78e25d31109a8643825b31c5265",
	}
	if !reflect.DeepEqual(gotDigests, wantDigests) {
		t.Fatalf("generated artifact digests = %v, want %v", gotDigests, wantDigests)
	}
	_, _, contractsRoot := loadContractAssets(t)
	validateContractDocument(t, ajvRoot, contractsRoot, "openbox.project-model/v1", projectArtifacts.ProjectModel().Bytes())
	validateContractDocument(t, ajvRoot, contractsRoot, "openbox.sdk-coverage/v1", coverageArtifacts.SDKCoverage.Bytes())
	for _, sentinel := range []string{sourceSentinel, snapshotSentinel} {
		if _, err := os.Lstat(sentinel); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("inspection executed package code or sentinel lookup failed for %s: %v", sentinel, err)
		}
	}
}

func exactMastraCompatibility() sdkdesc.Compatibility {
	descriptor := sdkdesc.MastraMVP()
	packages := make([]sdkdesc.PackageResolution, len(descriptor.Components))
	for index, component := range descriptor.Components {
		packages[index] = sdkdesc.PackageResolution{
			Requested: component.Requested, Resolved: component.Resolved, Version: component.Version,
			ResolvedURI: component.ResolvedURI, Integrity: component.Integrity,
		}
	}
	return sdkdesc.Validate(sdkdesc.Candidate{
		Source: descriptor.Source, Packages: packages,
		Initializations: []sdkdesc.Initialization{{
			Function: sdkdesc.PublicFactory, Target: sdkdesc.MastraTarget,
			EvaluateMaxRetries:  sdkdesc.ControlBinding{Shape: sdkdesc.BindingLiteral, Literal: "0"},
			GovernanceTimeout:   sdkdesc.ControlBinding{Shape: sdkdesc.BindingLiteral, Literal: "5"},
			HITLEnabled:         sdkdesc.ControlBinding{Shape: sdkdesc.BindingLiteral, Literal: "true"},
			HTTPCapture:         sdkdesc.ControlBinding{Shape: sdkdesc.BindingLiteral, Literal: "false"},
			InstrumentDatabases: sdkdesc.ControlBinding{Shape: sdkdesc.BindingLiteral, Literal: "false"},
			InstrumentFileIO:    sdkdesc.ControlBinding{Shape: sdkdesc.BindingLiteral, Literal: "false"},
		}},
	})
}

func makeInspectionContractWritable(root string) {
	_ = filepath.Walk(root, func(current string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if info.IsDir() {
			_ = os.Chmod(current, 0o700)
		} else {
			_ = os.Chmod(current, 0o600)
		}
		return nil
	})
}
