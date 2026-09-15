package securityskill

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/artifact"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

func TestBundleIdentityAndPublicReferenceParity(t *testing.T) {
	manifest, files, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Name != Name || manifest.Version != Version || manifest.Digest != "sha256:817e35e1db637d3c9a68ea7b0adf444aa1b5e9c2ad3eaa75c22496506ce0fe13" {
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
			LocalSource       string `json:"local_source"`
			LocalSourceDigest string `json:"local_source_digest"`
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
	if catalog.Version != CatalogVersion || len(catalog.Entries) != 7 {
		t.Fatalf("catalog identity/count = %s/%d", catalog.Version, len(catalog.Entries))
	}
	for _, source := range catalog.Sources {
		content, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(source.LocalSource)))
		if err != nil {
			t.Fatal(err)
		}
		if got := artifact.DigestBytes(content).String(); got != source.LocalSourceDigest {
			t.Errorf("%s digest = %s, want %s", source.LocalSource, got, source.LocalSourceDigest)
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
