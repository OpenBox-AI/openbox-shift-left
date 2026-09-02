//go:build darwin || linux

package runfs

import (
	"os"
	"path/filepath"
	"testing"
)

func TestObservationTransactionRejectsMissingExtraAndHardLinkedPayloads(t *testing.T) {
	payloads := observationTestPayloads()
	t.Run("missing", func(t *testing.T) {
		workspace, err := Create(filepath.Join(t.TempDir(), "run"))
		if err != nil {
			t.Fatal(err)
		}
		delete(payloads, "coverage.json")
		if err := workspace.WriteObservationPayloads(payloads); err == nil {
			t.Fatal("accepted missing payload")
		}
	})
	t.Run("extra", func(t *testing.T) {
		payloads := observationTestPayloads()
		workspace, err := Create(filepath.Join(t.TempDir(), "run"))
		if err != nil {
			t.Fatal(err)
		}
		if err := workspace.WriteObservationPayloads(payloads); err != nil {
			t.Fatal(err)
		}
		if err := workspace.WritePrivateFile("extra.json", []byte("{}")); err != nil {
			t.Fatal(err)
		}
		if _, err := workspace.FinalizeObservation(payloads, []byte("{}")); err == nil {
			t.Fatal("accepted extra payload")
		}
	})
	t.Run("hard-link", func(t *testing.T) {
		root := t.TempDir()
		payloads := observationTestPayloads()
		workspace, err := Create(filepath.Join(root, "run"))
		if err != nil {
			t.Fatal(err)
		}
		if err := workspace.WriteObservationPayloads(payloads); err != nil {
			t.Fatal(err)
		}
		external := filepath.Join(root, "external")
		if err := os.WriteFile(external, payloads["run.json"], 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(filepath.Join(workspace.Root(), "run.json")); err != nil {
			t.Fatal(err)
		}
		if err := os.Link(external, filepath.Join(workspace.Root(), "run.json")); err != nil {
			t.Fatal(err)
		}
		if _, err := workspace.FinalizeObservation(payloads, []byte("{}")); err == nil {
			t.Fatal("accepted hard-linked payload")
		}
	})
}

func TestObservationReaderRejectsPermissionMutation(t *testing.T) {
	root := t.TempDir()
	payloads := observationTestPayloads()
	workspace, err := Create(filepath.Join(root, "private"))
	if err != nil {
		t.Fatal(err)
	}
	if err := workspace.WriteObservationPayloads(payloads); err != nil {
		t.Fatal(err)
	}
	if _, err := workspace.FinalizeObservation(payloads, []byte("{}")); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(root, "output")
	if err := workspace.PublishTo(output); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(output, 0o700)
		for _, name := range append(append([]string(nil), observationPayloadNames...), ManifestName) {
			_ = os.Chmod(filepath.Join(output, name), 0o600)
		}
	})
	if err := os.Chmod(filepath.Join(output, "coverage.json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadObservation(output); err == nil {
		t.Fatal("accepted widened payload mode")
	}
}

func observationTestPayloads() map[string][]byte {
	result := make(map[string][]byte, len(observationPayloadNames))
	for _, name := range observationPayloadNames {
		if name == "openshell.jsonl" {
			result[name] = []byte("{}\n")
		} else {
			result[name] = []byte("{}")
		}
	}
	return result
}
