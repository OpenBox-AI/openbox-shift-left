package legacyprofile

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestParseAcceptsHistoricalProfileAndRejectsSemanticDrift(t *testing.T) {
	path := filepath.Join("..", "..", "..", "..", "..", "contracts", "project-assurance", "testdata", "valid", "project-run-profile-v1.json")
	valid, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Parse(valid); err != nil {
		t.Fatalf("historical profile rejected: %v", err)
	}
	mutated := bytes.Replace(valid, []byte(`"mode": "redacted_digests"`), []byte(`"mode": "full_content"`), 1)
	if _, err := Parse(mutated); err == nil {
		t.Fatal("historical profile semantic drift was accepted")
	}
}
