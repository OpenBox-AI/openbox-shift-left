package artifact

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

const ajvRootEnvironment = "OPENBOX_AJV_ROOT"
const maxConformanceOutputBytes = 1 << 20

func TestPinnedProjectAssuranceContractConformance(t *testing.T) {
	ajvRoot := os.Getenv(ajvRootEnvironment)
	if ajvRoot == "" {
		t.Skip("set OPENBOX_AJV_ROOT to an offline Ajv 8.17.1 package root to run contract conformance")
	}
	repositoryRoot := conformanceRepositoryRoot(t)

	var publicResult struct {
		Status         string         `json:"status"`
		Validator      string         `json:"validator"`
		SchemaCount    int            `json:"schemaCount"`
		ValidCount     int            `json:"validCount"`
		NegativeCounts map[string]int `json:"negativeCounts"`
	}
	runConformanceProbe(t, &publicResult,
		filepath.Join(repositoryRoot, "plans", "260819-1600-project-security-evaluation", "evidence", "probes", "validate_project_assurance_schemas.mjs"),
		ajvRoot,
		filepath.Join(repositoryRoot, "contracts", "project-assurance"),
	)
	if publicResult.Status != "passed" || publicResult.Validator != "ajv@8.17.1" || publicResult.SchemaCount != 7 || publicResult.ValidCount != 7 {
		t.Fatalf("unexpected public conformance result: %+v", publicResult)
	}
	wantNegativeCounts := map[string]int{
		"missingRequired": 7, "unknownProperty": 7, "invalidEnum": 7,
		"unsafeNumber": 7, "malformedDigest": 6, "wrongApiVersion": 7,
		"wrongType": 7, "adversarial": 26, "semanticAdversarial": 16,
	}
	for name, want := range wantNegativeCounts {
		if got := publicResult.NegativeCounts[name]; got != want {
			t.Fatalf("%s negatives = %d, want %d", name, got, want)
		}
	}

	var profileResult struct {
		Validator struct {
			Package string `json:"package"`
			Version string `json:"version"`
		} `json:"validator"`
		Positive struct {
			SchemaValid            bool `json:"schemaValid"`
			SemanticValid          bool `json:"semanticValid"`
			TrustedRelayTupleValid bool `json:"trustedRelayTupleValid"`
		} `json:"positive"`
		SchemaNegatives   []json.RawMessage `json:"schemaNegatives"`
		SemanticNegatives []json.RawMessage `json:"semanticNegatives"`
	}
	contractsRoot := filepath.Join(repositoryRoot, "contracts", "project-assurance")
	runConformanceProbe(t, &profileResult,
		filepath.Join(repositoryRoot, "plans", "260819-1600-project-security-evaluation", "evidence", "probes", "validate_run_profile_draft.mjs"),
		ajvRoot,
		filepath.Join(contractsRoot, "schema", "project-run-profile-v1.schema.json"),
		filepath.Join(contractsRoot, "testdata", "valid", "project-run-profile-v1.json"),
	)
	if profileResult.Validator.Package != "ajv" || profileResult.Validator.Version != "8.17.1" ||
		!profileResult.Positive.SchemaValid || !profileResult.Positive.SemanticValid || !profileResult.Positive.TrustedRelayTupleValid ||
		len(profileResult.SchemaNegatives) != 16 || len(profileResult.SemanticNegatives) != 18 {
		t.Fatalf("unexpected run-profile conformance result: %+v", profileResult)
	}
}

func runConformanceProbe(t *testing.T, result any, probe string, arguments ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "node", append([]string{probe}, arguments...)...)
	output := boundedProbeOutput{remaining: maxConformanceOutputBytes}
	command.Stdout = &output
	command.Stderr = &output
	err := command.Run()
	if ctx.Err() != nil {
		t.Fatalf("contract conformance exceeded 30 seconds: %v", ctx.Err())
	}
	if output.truncated {
		t.Fatalf("contract conformance exceeded %d output bytes", maxConformanceOutputBytes)
	}
	if err != nil {
		t.Fatalf("contract conformance failed: %v\n%s", err, output.buffer.Bytes())
	}
	if err := json.Unmarshal(output.buffer.Bytes(), result); err != nil {
		t.Fatalf("decode contract conformance output: %v\n%s", err, output.buffer.Bytes())
	}
}

type boundedProbeOutput struct {
	buffer    bytes.Buffer
	remaining int
	truncated bool
}

func (output *boundedProbeOutput) Write(content []byte) (int, error) {
	written := len(content)
	if len(content) > output.remaining {
		content = content[:output.remaining]
		output.truncated = true
	}
	_, _ = output.buffer.Write(content)
	output.remaining -= len(content)
	return written, nil
}

func conformanceRepositoryRoot(t *testing.T) string {
	t.Helper()
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate contract conformance test")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(source), "..", "..", "..", ".."))
}
