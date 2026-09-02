package inspect

import "testing"

func TestManifestKindClosedInventory(t *testing.T) {
	tests := map[string]ManifestKind{
		"package.json":                KindPackageJSON,
		"packages/api/package.json":   KindPackageJSON,
		"package-lock.json":           KindPackageLock,
		"npm-shrinkwrap.json":         KindNPMShrinkwrap,
		"yarn.lock":                   KindYarnLock,
		"pnpm-lock.yaml":              KindPNPMLock,
		"pyproject.toml":              KindPyprojectTOML,
		"requirements.txt":            KindRequirements,
		"requirements-dev.txt":        KindRequirements,
		"requirements/test.txt":       KindRequirements,
		"config/requirements/dev.txt": KindRequirements,
		"poetry.lock":                 KindPoetryLock,
		"uv.lock":                     KindUVLock,
		"Pipfile":                     KindPipfile,
		"Pipfile.lock":                KindPipfileLock,
		"pdm.lock":                    KindPDMLock,
	}
	for relative, want := range tests {
		got, matched := manifestKind(relative)
		if !matched || got != want {
			t.Fatalf("manifestKind(%q) = %q, %v; want %q, true", relative, got, matched, want)
		}
	}
	for _, relative := range []string{"README.md", "unknown.lock", "PACKAGE.JSON", "requirements", "requirements-.yaml", "requirements/dev.toml"} {
		if got, matched := manifestKind(relative); matched {
			t.Fatalf("manifestKind(%q) = %q, true; want closed rejection", relative, got)
		}
	}
}

func TestReadManifestsRejectsNilSnapshot(t *testing.T) {
	if _, err := ReadManifests(nil); err == nil {
		t.Fatal("nil snapshot was accepted")
	}
}
