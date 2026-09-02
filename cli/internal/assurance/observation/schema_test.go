package observation

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestEmbeddedObservationSchemasMatchPublicContractsByteForByte(t *testing.T) {
	root := filepath.Join("..", "..", "..", "..", "contracts", "project-observation", "schema")
	for _, definition := range schemaDefinitions {
		embedded, err := schemaFiles.ReadFile("schema/" + definition.name)
		if err != nil {
			t.Fatal(err)
		}
		public, err := os.ReadFile(filepath.Join(root, definition.name))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(embedded, public) {
			t.Errorf("embedded %s differs from public contract", definition.name)
		}
	}
}

func TestEmbeddedObservationSchemasCompileOnce(t *testing.T) {
	first, err := observationSchemas()
	if err != nil {
		t.Fatal(err)
	}
	second, err := observationSchemas()
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != len(schemaDefinitions) || len(second) != len(schemaDefinitions) {
		t.Fatalf("compiled schema count = %d, %d", len(first), len(second))
	}
}
