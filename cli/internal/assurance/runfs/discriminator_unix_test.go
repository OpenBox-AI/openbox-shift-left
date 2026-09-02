//go:build darwin || linux

package runfs

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCommittedManifestDiscriminatorRejectsAmbiguityAndReplacement(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "pack")
	writeDiscriminatorFixture(t, root, []byte(`{"pack_schema":"ai.openbox.project-observation/v1","schema":"ai.openbox.project-observation.manifest/v1"}`))
	selected, err := ReadCommittedManifestDiscriminator(root)
	if err != nil {
		t.Fatal(err)
	}
	if selected.PackSchema() != ObservationPackSchema {
		t.Fatalf("schema = %q", selected.PackSchema())
	}
	replaced := filepath.Join(parent, "replaced")
	if err := os.Rename(root, replaced); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(replaced, 0o700) })
	writeDiscriminatorFixture(t, root, []byte(`{"pack_schema":"ai.openbox.project-observation/v1","schema":"ai.openbox.project-observation.manifest/v1"}`))
	if err := RecheckCommittedManifest(root, selected); err == nil {
		t.Fatal("accepted replaced pack root")
	}

	ambiguous := filepath.Join(parent, "ambiguous")
	writeDiscriminatorFixture(t, ambiguous, []byte(`{"pack_schema":"ai.openbox.project-observation/v1","schema":"openbox.audit-pack/v1"}`))
	if _, err := ReadCommittedManifestDiscriminator(ambiguous); err == nil {
		t.Fatal("accepted ambiguous discriminator")
	}
}

func writeDiscriminatorFixture(t *testing.T, root string, manifest []byte) {
	t.Helper()
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ManifestName), manifest, 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(root, 0o700) })
}
