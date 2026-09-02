package model

import (
	"reflect"
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/artifact"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/inspect"
)

func TestNormalizeDeterministicGolden(t *testing.T) {
	digestA := artifact.DigestBytes([]byte("a"))
	digestB := artifact.DigestBytes([]byte("b"))
	facts := []inspect.Fact{
		{Kind: inspect.FactOpenBoxSDK, Value: "@openbox-ai/openbox-mastra-sdk", Evidence: inspect.Evidence{Detector: "source-openbox-sdk", Basis: inspect.BasisInferred, Confidence: inspect.ConfidenceHigh, Path: "src/z.ts", Line: 9, Column: 3, Digest: digestB}},
		{Kind: inspect.FactPackageImport, Value: "@openbox-ai/openbox-mastra-sdk", Evidence: inspect.Evidence{Detector: "javascript-import", Basis: inspect.BasisInferred, Confidence: inspect.ConfidenceHigh, Path: "src/index.ts", Line: 1, Column: 29, Digest: digestA}},
		{Kind: inspect.FactOpenBoxSDK, Value: "@openbox-ai/openbox-mastra-sdk", Evidence: inspect.Evidence{Detector: "package-json-openbox-sdk", Basis: inspect.BasisDeclared, Confidence: inspect.ConfidenceHigh, Path: "package.json", Line: 2, Column: 1, Digest: digestA}},
	}
	uncertainties := []inspect.Uncertainty{
		{Subject: "runtime-registration", Reason: "Runtime registration is unknown."},
		{Subject: "dynamic-import", Reason: "Computed target.", Path: "src/index.ts", Line: 4},
	}

	first, err := normalizeEvidence(facts, uncertainties, semanticID)
	if err != nil {
		t.Fatal(err)
	}
	reversedFacts := append([]inspect.Fact(nil), facts...)
	reversedFacts[0], reversedFacts[2] = reversedFacts[2], reversedFacts[0]
	reversedUncertainties := append([]inspect.Uncertainty(nil), uncertainties...)
	reversedUncertainties[0], reversedUncertainties[1] = reversedUncertainties[1], reversedUncertainties[0]
	second, err := normalizeEvidence(reversedFacts, reversedUncertainties, semanticID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("input order changed graph:\nfirst=%#v\nsecond=%#v", first, second)
	}

	nodes := first.Nodes()
	if len(nodes) != 1 || nodes[0].ID != "openbox_sdk:00111b53b2a4f50f65f8994e4f6467e52efc6b2b881b44faf0a34c3c01fd9dc7" || nodes[0].Type != NodeOpenBoxSDK || nodes[0].Value != "@openbox-ai/openbox-mastra-sdk" {
		t.Fatalf("node golden changed: %#v", nodes)
	}
	if len(nodes[0].Provenance) != 2 || nodes[0].Provenance[0].Path != "package.json" || nodes[0].Provenance[1].Path != "src/z.ts" {
		t.Fatalf("provenance was not merged and sorted: %#v", nodes[0].Provenance)
	}
	signals := first.Signals()
	if len(signals) != 1 || signals[0].ID != "package_import:39c2f1d360135b04cdd6e9e9bb4ecb7e6031ac21c5ee6be0718c6381e89f7df1" {
		t.Fatalf("signal golden changed: %#v", signals)
	}
	if len(first.Uncertainties()) != 2 {
		t.Fatalf("uncertainty normalization changed: %#v", first.Uncertainties())
	}
}

func TestNormalizeMapsClosedNodeAndSignalKinds(t *testing.T) {
	nodes := map[inspect.FactKind]NodeType{
		inspect.FactAgent: NodeAgent, inspect.FactModelRoute: NodeModelRoute,
		inspect.FactTool: NodeTool, inspect.FactMCPServer: NodeMCPServer,
		inspect.FactRetrieval: NodeRetrieval, inspect.FactMemory: NodeMemory,
		inspect.FactCredentialBoundary: NodeCredentialBoundary, inspect.FactApproval: NodeApproval,
		inspect.FactFilesystemBoundary: NodeFilesystemBoundary, inspect.FactProcessBoundary: NodeProcessBoundary,
		inspect.FactNetworkBoundary: NodeNetworkBoundary, inspect.FactTelemetrySink: NodeTelemetrySink,
		inspect.FactPersistenceSink: NodePersistenceSink, inspect.FactExternalDestination: NodeExternalDestination,
		inspect.FactOpenBoxSDK: NodeOpenBoxSDK,
	}
	for kind, want := range nodes {
		if got, ok := factNodeType(kind); !ok || got != want {
			t.Fatalf("factNodeType(%q) = %q, %v; want %q, true", kind, got, ok, want)
		}
	}
	for _, kind := range []inspect.FactKind{inspect.FactPackageDependency, inspect.FactPackageImport, inspect.FactEntrypoint, inspect.FactEnvironmentReference} {
		if !signalFact(kind) {
			t.Fatalf("%q is not retained as a descriptor signal", kind)
		}
	}
	if _, ok := factNodeType(inspect.FactKind("invented")); ok || signalFact(inspect.FactKind("invented")) {
		t.Fatal("unknown fact kind entered the closed graph vocabulary")
	}
}

func TestNormalizeRejectsIdentityCollisionsAndInvalidEvidence(t *testing.T) {
	valid := func(kind inspect.FactKind, value string) inspect.Fact {
		return inspect.Fact{Kind: kind, Value: value, Evidence: inspect.Evidence{
			Detector: "fixture", Basis: inspect.BasisInferred, Confidence: inspect.ConfidenceMedium,
			Path: "src/index.ts", Line: 1, Column: 1, Digest: artifact.DigestBytes([]byte(value)),
		}}
	}
	collision := func(_, _, _ string) (string, error) { return "collision", nil }
	if _, err := normalizeEvidence([]inspect.Fact{valid(inspect.FactTool, "a"), valid(inspect.FactTool, "b")}, nil, collision); err == nil || !strings.Contains(err.Error(), "identity collision") {
		t.Fatalf("node collision error = %v", err)
	}
	if _, err := normalizeEvidence([]inspect.Fact{valid(inspect.FactPackageImport, "a"), valid(inspect.FactPackageImport, "b")}, nil, collision); err == nil || !strings.Contains(err.Error(), "identity collision") {
		t.Fatalf("signal collision error = %v", err)
	}
	uncertainties := []inspect.Uncertainty{{Subject: "dynamic-import", Reason: "a"}, {Subject: "dynamic-import", Reason: "b"}}
	if _, err := normalizeEvidence(nil, uncertainties, collision); err == nil || !strings.Contains(err.Error(), "identity collision") {
		t.Fatalf("uncertainty collision error = %v", err)
	}
	if _, err := normalizeEvidence([]inspect.Fact{valid(inspect.FactKind("invented"), "a")}, nil, semanticID); err == nil || !strings.Contains(err.Error(), "unsupported fact kind") {
		t.Fatalf("unknown-kind error = %v", err)
	}
	badBasis := valid(inspect.FactTool, "a")
	badBasis.Evidence.Basis = inspect.Basis("observed")
	if _, err := normalizeEvidence([]inspect.Fact{badBasis}, nil, semanticID); err == nil || !strings.Contains(err.Error(), "forbidden basis") {
		t.Fatalf("observed-basis error = %v", err)
	}
	badLocation := valid(inspect.FactTool, "a")
	badLocation.Evidence.Line = 0
	if _, err := normalizeEvidence([]inspect.Fact{badLocation}, nil, semanticID); err == nil || !strings.Contains(err.Error(), "positive source location") {
		t.Fatalf("location error = %v", err)
	}
}

func TestNormalizeSeparatesSiteScopedEntitiesAndRejectsNodeLessGraph(t *testing.T) {
	tool := func(path string, line int64) inspect.Fact {
		return inspect.Fact{Kind: inspect.FactTool, Value: "createTool", Evidence: inspect.Evidence{
			Detector: "source-call", Basis: inspect.BasisInferred, Confidence: inspect.ConfidenceMedium,
			Path: path, Line: line, Column: 1, Digest: artifact.DigestBytes([]byte(path)),
		}}
	}
	graph, err := normalizeEvidence([]inspect.Fact{tool("src/a.ts", 1), tool("src/b.ts", 1)}, nil, semanticID)
	if err != nil {
		t.Fatal(err)
	}
	if len(graph.Nodes()) != 2 || graph.Nodes()[0].ID == graph.Nodes()[1].ID {
		t.Fatalf("distinct tool declarations collapsed: %#v", graph.Nodes())
	}
	if _, err := normalizeEvidence(nil, nil, semanticID); err == nil || !strings.Contains(err.Error(), "no node evidence") {
		t.Fatalf("empty graph error = %v", err)
	}
	signalOnly := inspect.Fact{Kind: inspect.FactPackageImport, Value: "package", Evidence: tool("package.json", 1).Evidence}
	if _, err := normalizeEvidence([]inspect.Fact{signalOnly}, nil, semanticID); err == nil || !strings.Contains(err.Error(), "no node evidence") {
		t.Fatalf("signal-only graph error = %v", err)
	}
}

func TestNormalizeKeepsConflictingDependencyDeclarationsDistinct(t *testing.T) {
	evidence := inspect.Evidence{
		Detector: "fixture", Basis: inspect.BasisDeclared, Confidence: inspect.ConfidenceHigh,
		Path: "package.json", Line: 1, Column: 1, Digest: artifact.DigestBytes([]byte("manifest")),
	}
	facts := []inspect.Fact{
		{Kind: inspect.FactOpenBoxSDK, Value: "@openbox-ai/openbox-mastra-sdk", Evidence: evidence},
		{Kind: inspect.FactPackageDependency, Value: "@openbox-ai/openbox-mastra-sdk", DeclaredValueDigest: artifact.DigestBytes([]byte("1.0.0")), Evidence: evidence},
		{Kind: inspect.FactPackageDependency, Value: "@openbox-ai/openbox-mastra-sdk", DeclaredValueDigest: artifact.DigestBytes([]byte("99.0.0")), Evidence: evidence},
	}
	graph, err := normalizeEvidence(facts, nil, semanticID)
	if err != nil {
		t.Fatal(err)
	}
	if len(graph.Signals()) != 2 || graph.Signals()[0].ID == graph.Signals()[1].ID {
		t.Fatalf("conflicting dependency declarations collapsed: %#v", graph.Signals())
	}
}

func TestNormalizeCapsProvenanceWithExplicitUncertainty(t *testing.T) {
	facts := make([]inspect.Fact, maxProvenancePerItem+1)
	for index := range facts {
		facts[index] = inspect.Fact{Kind: inspect.FactOpenBoxSDK, Value: "@openbox-ai/openbox-mastra-sdk", Evidence: inspect.Evidence{
			Detector: "source-call", Basis: inspect.BasisInferred, Confidence: inspect.ConfidenceMedium,
			Path: "src/index.ts", Line: int64(index + 1), Column: 1, Digest: artifact.DigestBytes([]byte("source")),
		}}
	}
	graph, err := normalizeEvidence(facts, nil, semanticID)
	if err != nil {
		t.Fatal(err)
	}
	if len(graph.Nodes()) != 1 || len(graph.Nodes()[0].Provenance) != maxProvenancePerItem {
		t.Fatalf("provenance cap failed: %#v", graph.Nodes())
	}
	found := false
	for _, uncertainty := range graph.Uncertainties() {
		found = found || uncertainty.Subject == "provenance-truncated"
	}
	if !found {
		t.Fatalf("provenance truncation was silent: %#v", graph.Uncertainties())
	}
}

func TestGraphAccessorsReturnDefensiveCopies(t *testing.T) {
	fact := inspect.Fact{Kind: inspect.FactTool, Value: "createTool", Evidence: inspect.Evidence{
		Detector: "source-call", Basis: inspect.BasisInferred, Confidence: inspect.ConfidenceMedium,
		Path: "src/index.ts", Line: 1, Column: 1, Digest: artifact.DigestBytes([]byte("source")),
	}}
	graph, err := normalizeEvidence([]inspect.Fact{fact}, nil, semanticID)
	if err != nil {
		t.Fatal(err)
	}
	nodes := graph.Nodes()
	nodes[0].Value = "mutated"
	nodes[0].Provenance[0].Path = "mutated"
	if graph.Nodes()[0].Value != "createTool" || graph.Nodes()[0].Provenance[0].Path != "src/index.ts" {
		t.Fatal("Graph.Nodes exposed retained storage")
	}
}
