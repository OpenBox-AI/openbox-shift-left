package observation

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/artifact"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/runfs"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

func TestAssembleFinalizeAndReadExactObservation(t *testing.T) {
	now := time.Date(2026, 8, 26, 6, 0, 0, 0, time.UTC)
	execution, _ := json.Marshal(map[string]any{
		"schema": "old", "evaluation_id": "ev-one", "agent_id": "450999ca-ae2a-409c-8a26-d00a71132440", "started_at": now, "completed_at": now, "duration_ms": 1,
		"image": map[string]any{}, "argv": []any{}, "environment_names": []any{}, "openshell": map[string]any{}, "inference": map[string]any{}, "phases": []any{},
		"core": map[string]any{}, "effects": map[string]any{}, "exit_classification": "success", "logs": map[string]any{}, "coverage_limitations": []any{}, "cleanup": map[string]any{},
	})
	entries := []Entry{
		testEntry(1, "/health", []byte(`{"status":200,"data":"Success"}`)),
		testEntry(2, "/auth/profile", []byte(`{"status":200,"data":{"orgId":"openbox.ai","permissions":["create:agent","read:agent","update:agent","read:agent_session","read:agent_log","read:agent_guardrail","read:agent_policy","read:agent_behavior_rule"],"isApiKeyAuth":true,"setup":{"pending":false}}}`)),
		testEntry(3, "/agent/450999ca-ae2a-409c-8a26-d00a71132440/sessions?page=0&perPage=100&search=ev-one", []byte(`{"data":{"data":[],"limit":100,"start":0,"total":0},"status":200}`)),
	}
	backend := &Result{OrganizationID: "openbox.ai", Session: Session{ID: "ecfd94a0-e4c6-4ae8-96b2-72fc20f5e19a"}, Entries: entries}
	snapshot := &Snapshot{OrganizationID: "openbox.ai", Backend: BackendIdentity{URL: ExactBackendURL, APIContract: DashboardActivityContract}, Entries: entries}
	pack, err := Assemble(PackInput{ExecutionJSON: execution, OpenShellLog: []byte("ready\ncleaned\n"), Snapshot: snapshot, Backend: backend, Window: Window{EvaluationID: "ev-one", StartedAt: now, Deadline: now.Add(time.Minute)}, Effects: map[string]any{
		"safe_sink":        map[string]any{"status": "observed", "attempts": 1, "matching_receipts": 1, "evaluation_id": "ev-one", "matched_at": now.Format(time.RFC3339Nano)},
		"retrieval_poison": map[string]any{"status": "missing", "matching_receipts": 0},
		"model_route":      map[string]any{"provider": "openai-compatible-provider", "model": "granite4.1:3b", "model_digest": "sha256:6fd349357287c7ffc9e38189a93b48ea175d24fc566b38f09cfc564fb7f303eb"},
		"core_relay":       map[string]any{"status": "observed", "matching_validations": 1, "governance_events": 1},
	}, FinalizedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	validateContractSchemas(t, pack)
	var contradictory map[string]any
	if json.Unmarshal(pack.Payloads["effects.json"], &contradictory) != nil {
		t.Fatal("decode effects")
	}
	contradictory["safe_sink"].(map[string]any)["status"] = "missing"
	changed, err := artifact.CanonicalJSON(contradictory)
	if err != nil {
		t.Fatal(err)
	}
	mutated := &Pack{Payloads: map[string][]byte{}, Manifest: pack.Manifest}
	for name, content := range pack.Payloads {
		mutated.Payloads[name] = append([]byte(nil), content...)
	}
	mutated.Payloads["effects.json"] = changed
	if err := Validate(mutated); err == nil {
		t.Fatal("accepted contradictory effect receipt")
	}
	private := filepath.Join(t.TempDir(), "private")
	workspace, err := runfs.Create(private)
	if err != nil {
		t.Fatal(err)
	}
	if err := workspace.WriteObservationPayloads(pack.Payloads); err != nil {
		t.Fatal(err)
	}
	if _, err := workspace.FinalizeObservation(pack.Payloads, pack.Manifest); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(filepath.Dir(private), "output")
	t.Cleanup(func() {
		_ = os.Chmod(output, 0o700)
		for _, name := range append(append([]string(nil), payloadOrder...), "manifest.json") {
			_ = os.Chmod(filepath.Join(output, name), 0o600)
		}
	})
	if err := workspace.PublishTo(output); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(output); err != nil {
		t.Fatal(err)
	}
}

func testEntry(ordinal int, path string, body []byte) Entry {
	representation := "backend_response"
	if strings.Contains(path, "/sessions") {
		representation = "dashboard_public_projection"
	}
	return Entry{Ordinal: ordinal, Method: "GET", Path: path, Status: 200, ContentType: "application/json", BodyBytes: len(body), SHA256: artifact.DigestBytes(body).String(), BodyBase64: base64.StdEncoding.EncodeToString(body), Representation: representation}
}

func TestLiveObservationEvidenceWhenRequested(t *testing.T) {
	root := os.Getenv("OPENBOX_LIVE_OBSERVATION")
	if root == "" {
		t.Skip("set OPENBOX_LIVE_OBSERVATION to validate retained live evidence")
	}
	pack, err := Read(root)
	if err != nil {
		t.Fatal(err)
	}
	validateContractSchemas(t, pack)
}

func validateContractSchemas(t *testing.T, pack *Pack) {
	t.Helper()
	root := filepath.Join("..", "..", "..", "..", "contracts", "project-observation", "schema")
	files := map[string][]byte{
		"manifest.schema.json": pack.Manifest,
		"run.schema.json":      pack.Payloads["run.json"],
		"backend.schema.json":  pack.Payloads["backend.json"],
		"effects.schema.json":  pack.Payloads["effects.json"],
		"behavior.schema.json": pack.Payloads["behavior.json"],
		"coverage.schema.json": pack.Payloads["coverage.json"],
	}
	for name, content := range files {
		compiler := jsonschema.NewCompiler()
		document, err := os.Open(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := jsonschema.UnmarshalJSON(document)
		document.Close()
		if err != nil {
			t.Fatal(err)
		}
		if err := compiler.AddResource(name, decoded); err != nil {
			t.Fatal(err)
		}
		schema, err := compiler.Compile(name)
		if err != nil {
			t.Fatal(err)
		}
		var value any
		if json.Unmarshal(content, &value) != nil {
			t.Fatal("invalid generated JSON")
		}
		if err := schema.Validate(value); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	compiler := jsonschema.NewCompiler()
	document, err := os.Open(filepath.Join(root, "openshell-record.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := jsonschema.UnmarshalJSON(document)
	document.Close()
	if err != nil {
		t.Fatal(err)
	}
	if err := compiler.AddResource("openshell-record.schema.json", decoded); err != nil {
		t.Fatal(err)
	}
	schema, err := compiler.Compile("openshell-record.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(pack.Payloads["openshell.jsonl"])), "\n") {
		var value any
		if json.Unmarshal([]byte(line), &value) != nil {
			t.Fatal("invalid generated JSONL")
		}
		if err := schema.Validate(value); err != nil {
			t.Fatal(err)
		}
	}
}
