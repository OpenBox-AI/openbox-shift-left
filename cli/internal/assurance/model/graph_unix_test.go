//go:build darwin || linux

package model

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/inspect"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/snapshot"
)

func TestDetectNormalizeIntegrationStaysPassiveAndDeterministic(t *testing.T) {
	projectRoot := t.TempDir()
	sentinel := filepath.Join(t.TempDir(), "must-not-exist")
	files := map[string][]byte{
		"package.json": []byte(fmt.Sprintf(`{"main":"src/index.ts","scripts":{"postinstall":"touch %s"},"dependencies":{"@openbox-ai/openbox-mastra-sdk":"1.0.0"}}`, sentinel)),
		"src/index.ts": []byte(`import { withOpenBox } from "@openbox-ai/openbox-mastra-sdk";
const tool = createTool({ id: "recordingTool" });
const key = process.env.OPENBOX_API_KEY;
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
	t.Cleanup(func() { makeGraphFixtureWritable(destination) })
	detection, err := inspect.Detect(copied)
	if err != nil {
		t.Fatal(err)
	}
	first, err := Normalize(detection)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Normalize(detection)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("repeated normalization changed graph identities or order")
	}
	if _, err := os.Lstat(sentinel); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("normalization executed project code or script: %v", err)
	}
	if len(first.Nodes()) < 3 || len(first.Signals()) < 2 {
		t.Fatalf("detector evidence was lost: nodes=%#v signals=%#v", first.Nodes(), first.Signals())
	}
	for _, node := range first.Nodes() {
		if node.ID == "" || node.Value == "" || len(node.Provenance) == 0 {
			t.Fatalf("node lacks stable identity or provenance: %#v", node)
		}
		for _, provenance := range node.Provenance {
			if provenance.Basis == inspect.Basis("observed") {
				t.Fatalf("static evidence was upgraded to observed: %#v", provenance)
			}
		}
	}
}

func makeGraphFixtureWritable(root string) {
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
