package securityskill

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

func TestBundleIdentityAndPublicReferenceParity(t *testing.T) {
	manifest, files, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Name != Name || manifest.Version != Version || manifest.Digest != "sha256:c519954fa2eca7fb735b36af76cf09c99a23ef5f78dc8b867f187336fce5a094" {
		t.Fatalf("manifest identity = %#v", manifest)
	}
	if lines := bytes.Count(files["SKILL.md"], []byte("\n")); lines >= 500 {
		t.Fatalf("SKILL.md has %d lines, want < 500", lines)
	}
	root := filepath.Join("..", "..", "..", "contracts", "project-security-analysis")
	for bundled, public := range map[string]string{
		"references/candidate.schema.json": filepath.Join(root, "schema", "candidate.schema.json"),
		"references/standards.json":        filepath.Join(root, "standards.json"),
	} {
		content, err := os.ReadFile(public)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(files[bundled], content) {
			t.Errorf("%s differs from %s", bundled, public)
		}
	}
}

func TestCandidateContractValidAndAdversarialFixtures(t *testing.T) {
	root := filepath.Join("..", "..", "..", "contracts", "project-security-analysis", "testdata")
	for _, name := range []string{"issues.json", "no-supported-issue.json", "inconclusive.json"} {
		content, err := os.ReadFile(filepath.Join(root, "valid", name))
		if err != nil {
			t.Fatal(err)
		}
		if err := ValidateCandidate(content); err != nil {
			t.Errorf("valid %s: %v", name, err)
		}
	}
	for _, name := range []string{"forbidden-recommendation.json", "issues-result-empty.json", "no-issue-result-nonempty.json"} {
		content, err := os.ReadFile(filepath.Join(root, "invalid", name))
		if err != nil {
			t.Fatal(err)
		}
		if err := ValidateCandidate(content); err == nil {
			t.Errorf("accepted invalid %s", name)
		}
	}
	duplicate := []byte(`{"schema":"ai.openbox.project-security-analysis/v1","schema":"ai.openbox.project-security-analysis/v1"}`)
	if err := ValidateCandidate(duplicate); err == nil {
		t.Fatal("accepted duplicate candidate key")
	}
}

func TestStandardsCatalogSchemaSelectionAndSourceDigests(t *testing.T) {
	root := filepath.Join("..", "..", "..", "contracts", "project-security-analysis")
	schemaBytes, err := os.ReadFile(filepath.Join(root, "schema", "standards.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	compiler.AssertFormat()
	document, err := jsonschema.UnmarshalJSON(bytes.NewReader(schemaBytes))
	if err != nil {
		t.Fatal(err)
	}
	if err := compiler.AddResource("standards.schema.json", document); err != nil {
		t.Fatal(err)
	}
	schema, err := compiler.Compile("standards.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	catalogBytes, err := os.ReadFile(filepath.Join(root, "standards.json"))
	if err != nil {
		t.Fatal(err)
	}
	catalogDocument, err := jsonschema.UnmarshalJSON(bytes.NewReader(catalogBytes))
	if err != nil {
		t.Fatal(err)
	}
	if err := schema.Validate(catalogDocument); err != nil {
		t.Fatal(err)
	}
	var catalog struct {
		Version string `json:"version"`
		Sources []struct {
			Catalog         string `json:"catalog"`
			LocalSource     string `json:"local_source"`
			UpstreamVersion string `json:"upstream_version"`
			UpstreamSHA256  string `json:"upstream_sha256"`
		} `json:"sources"`
		Entries []struct {
			Catalog string `json:"catalog"`
			Version string `json:"version"`
			ID      string `json:"id"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(catalogBytes, &catalog); err != nil {
		t.Fatal(err)
	}
	// No count is pinned. The catalog ships whole upstream corpora, so a fixed
	// number would mean editing a test every time a standard is added — and the
	// bundle manifest already digest-pins these exact bytes.
	if catalog.Version != CatalogVersion || len(catalog.Entries) == 0 || len(catalog.Sources) == 0 {
		t.Fatalf("catalog identity = %s, entries=%d sources=%d", catalog.Version, len(catalog.Entries), len(catalog.Sources))
	}

	// What actually matters: every index entry resolves to a shipped source that
	// really contains it. An index naming an id no corpus defines would let the
	// analyst cite a standard nobody can look up.
	defined := map[string]bool{}
	for _, source := range catalog.Sources {
		content, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(source.LocalSource)))
		if err != nil {
			t.Fatalf("source %s: %v", source.LocalSource, err)
		}
		var payload struct {
			Catalog         string `json:"catalog"`
			UpstreamVersion string `json:"upstream_version"`
			UpstreamSHA256  string `json:"upstream_sha256"`
			Entries         []struct {
				ID          string `json:"id"`
				Title       string `json:"title"`
				Description string `json:"description"`
			} `json:"entries"`
		}
		if err := json.Unmarshal(content, &payload); err != nil {
			t.Fatalf("source %s: %v", source.LocalSource, err)
		}
		if payload.Catalog != source.Catalog || payload.UpstreamVersion != source.UpstreamVersion ||
			payload.UpstreamSHA256 != source.UpstreamSHA256 || !strings.HasPrefix(source.UpstreamSHA256, "sha256:") {
			t.Errorf("%s provenance disagrees with the index", source.LocalSource)
		}
		for _, entry := range payload.Entries {
			if entry.Title == "" || entry.Description == "" {
				t.Errorf("%s entry %s has no title or description", source.LocalSource, entry.ID)
			}
			defined[payload.Catalog+"/"+payload.UpstreamVersion+"/"+entry.ID] = true
		}
	}
	for _, entry := range catalog.Entries {
		if !defined[entry.Catalog+"/"+entry.Version+"/"+entry.ID] {
			t.Errorf("index cites %s/%s/%s, which no shipped source defines", entry.Catalog, entry.Version, entry.ID)
		}
	}
}

func TestSkillContainsExplicitInstructionIsolation(t *testing.T) {
	_, files, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	skill := string(files["SKILL.md"])
	for _, required := range []string{
		"disable-model-invocation: true", "captured", "never instructions",
		"openbox project verify", "no_supported_issue", "inconclusive",
		"openbox project finalize", "do not run", "severity: unavailable",
	} {
		if !strings.Contains(strings.ToLower(skill), strings.ToLower(required)) {
			t.Errorf("SKILL.md missing %q", required)
		}
	}
}
