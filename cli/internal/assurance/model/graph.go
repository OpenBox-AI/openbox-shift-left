// Package model normalizes passive discovery evidence without upgrading it to
// runtime observation or behavioral judgment.
package model

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/artifact"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/inspect"
)

const maxProvenancePerItem = 64

type NodeType string

const (
	NodeAgent               NodeType = "agent"
	NodeModelRoute          NodeType = "model_route"
	NodeTool                NodeType = "tool"
	NodeMCPServer           NodeType = "mcp_server"
	NodeRetrieval           NodeType = "retrieval"
	NodeMemory              NodeType = "memory"
	NodeCredentialBoundary  NodeType = "credential_boundary"
	NodeApproval            NodeType = "approval"
	NodeFilesystemBoundary  NodeType = "filesystem_boundary"
	NodeProcessBoundary     NodeType = "process_boundary"
	NodeNetworkBoundary     NodeType = "network_boundary"
	NodeTelemetrySink       NodeType = "telemetry_sink"
	NodePersistenceSink     NodeType = "persistence_sink"
	NodeExternalDestination NodeType = "external_destination"
	NodeOpenBoxSDK          NodeType = "openbox_sdk"
)

type Provenance struct {
	Detector   string
	Basis      inspect.Basis
	Confidence inspect.Confidence
	Path       string
	Line       int64
	Column     int64
	Digest     artifact.ContentDigest
}

type Node struct {
	ID         string
	Type       NodeType
	Value      string
	Provenance []Provenance
}

// Signal retains discovery inputs needed by later descriptor and entrypoint
// analysis that are not node types in openbox.project-model/v1. Signals must
// not be projected as invented project-model nodes.
type Signal struct {
	ID    string
	Kind  inspect.FactKind
	Value string
	// DeclaredValueDigest is populated only for dependency declarations.
	DeclaredValueDigest artifact.ContentDigest
	Provenance          []Provenance
}

type Uncertainty struct {
	ID            string
	Subject       string
	Reason        string
	Path          string
	Line          int64
	EvidenceLevel string
}

type Graph struct {
	nodes           []Node
	signals         []Signal
	uncertainties   []Uncertainty
	initializations []inspect.OpenBoxInitialization
}

func (graph Graph) Nodes() []Node     { return cloneNodes(graph.nodes) }
func (graph Graph) Signals() []Signal { return cloneSignals(graph.signals) }
func (graph Graph) Uncertainties() []Uncertainty {
	return append([]Uncertainty(nil), graph.uncertainties...)
}
func (graph Graph) Initializations() []inspect.OpenBoxInitialization {
	return cloneInspectInitializations(graph.initializations)
}

// Normalize creates deterministic semantic identities from passive evidence.
// Equal semantic facts merge provenance; no edge is invented without an
// explicit relationship detector.
func Normalize(detection inspect.Detection) (Graph, error) {
	graph, err := normalizeEvidence(detection.Facts(), detection.Uncertainties(), semanticID)
	if err != nil {
		return Graph{}, err
	}
	graph.initializations = detection.Initializations()
	return graph, nil
}

type identityFunction func(namespace, kind, value string) (string, error)

func normalizeEvidence(facts []inspect.Fact, uncertainties []inspect.Uncertainty, identify identityFunction) (Graph, error) {
	if identify == nil {
		return Graph{}, errors.New("model: nil identity function")
	}
	graph := Graph{}
	nodeIndexes := make(map[string]int)
	nodeSemantics := make(map[string]string)
	signalIndexes := make(map[string]int)
	signalSemantics := make(map[string]string)
	for _, fact := range facts {
		provenance, err := normalizeProvenance(fact.Evidence)
		if err != nil {
			return Graph{}, err
		}
		if nodeType, ok := factNodeType(fact.Kind); ok {
			semantic, err := nodeSemantic(nodeType, fact)
			if err != nil {
				return Graph{}, err
			}
			id, err := identify("node", string(nodeType), semantic)
			if err != nil {
				return Graph{}, err
			}
			if index, exists := nodeIndexes[id]; exists {
				node := &graph.nodes[index]
				if node.Type != nodeType || node.Value != fact.Value || nodeSemantics[id] != semantic {
					return Graph{}, fmt.Errorf("model: node identity collision at %q", id)
				}
				appendProvenance(&node.Provenance, provenance)
				continue
			}
			nodeIndexes[id] = len(graph.nodes)
			nodeSemantics[id] = semantic
			graph.nodes = append(graph.nodes, Node{ID: id, Type: nodeType, Value: fact.Value, Provenance: []Provenance{provenance}})
			continue
		}
		if !signalFact(fact.Kind) {
			return Graph{}, fmt.Errorf("model: unsupported fact kind %q", fact.Kind)
		}
		semantic, err := signalSemantic(fact)
		if err != nil {
			return Graph{}, err
		}
		id, err := identify("signal", string(fact.Kind), semantic)
		if err != nil {
			return Graph{}, err
		}
		if index, exists := signalIndexes[id]; exists {
			signal := &graph.signals[index]
			if signal.Kind != fact.Kind || signal.Value != fact.Value || signal.DeclaredValueDigest != fact.DeclaredValueDigest || signalSemantics[id] != semantic {
				return Graph{}, fmt.Errorf("model: signal identity collision at %q", id)
			}
			appendProvenance(&signal.Provenance, provenance)
			continue
		}
		signalIndexes[id] = len(graph.signals)
		signalSemantics[id] = semantic
		graph.signals = append(graph.signals, Signal{
			ID: id, Kind: fact.Kind, Value: fact.Value, DeclaredValueDigest: fact.DeclaredValueDigest,
			Provenance: []Provenance{provenance},
		})
	}

	for index := range graph.nodes {
		node := &graph.nodes[index]
		sortProvenance(node.Provenance)
		if len(node.Provenance) > maxProvenancePerItem {
			node.Provenance = append([]Provenance(nil), node.Provenance[:maxProvenancePerItem]...)
			uncertainties = append(uncertainties, inspect.Uncertainty{Subject: "provenance-truncated", Reason: fmt.Sprintf("Node %s has more than %d distinct passive evidence locations.", node.ID, maxProvenancePerItem)})
		}
	}
	for index := range graph.signals {
		signal := &graph.signals[index]
		sortProvenance(signal.Provenance)
		if len(signal.Provenance) > maxProvenancePerItem {
			signal.Provenance = append([]Provenance(nil), signal.Provenance[:maxProvenancePerItem]...)
			uncertainties = append(uncertainties, inspect.Uncertainty{Subject: "provenance-truncated", Reason: fmt.Sprintf("Signal %s has more than %d distinct passive evidence locations.", signal.ID, maxProvenancePerItem)})
		}
	}
	if err := normalizeUncertainties(&graph, uncertainties, identify); err != nil {
		return Graph{}, err
	}
	if len(graph.nodes) == 0 {
		return Graph{}, errors.New("model: no node evidence satisfies openbox.project-model/v1")
	}
	sort.Slice(graph.nodes, func(left, right int) bool { return graph.nodes[left].ID < graph.nodes[right].ID })
	sort.Slice(graph.signals, func(left, right int) bool { return graph.signals[left].ID < graph.signals[right].ID })
	return graph, nil
}

func normalizeProvenance(evidence inspect.Evidence) (Provenance, error) {
	if evidence.Detector == "" || evidence.Path == "" || evidence.Line < 1 || evidence.Column < 1 {
		return Provenance{}, errors.New("model: fact lacks detector, path, or positive source location")
	}
	if evidence.Basis != inspect.BasisDeclared && evidence.Basis != inspect.BasisInferred {
		return Provenance{}, fmt.Errorf("model: passive fact has forbidden basis %q", evidence.Basis)
	}
	if evidence.Confidence != inspect.ConfidenceHigh && evidence.Confidence != inspect.ConfidenceMedium {
		return Provenance{}, fmt.Errorf("model: fact has unsupported confidence %q", evidence.Confidence)
	}
	return Provenance{
		Detector: evidence.Detector, Basis: evidence.Basis, Confidence: evidence.Confidence,
		Path: evidence.Path, Line: evidence.Line, Column: evidence.Column, Digest: evidence.Digest,
	}, nil
}

func factNodeType(kind inspect.FactKind) (NodeType, bool) {
	switch kind {
	case inspect.FactAgent:
		return NodeAgent, true
	case inspect.FactModelRoute:
		return NodeModelRoute, true
	case inspect.FactTool:
		return NodeTool, true
	case inspect.FactMCPServer:
		return NodeMCPServer, true
	case inspect.FactRetrieval:
		return NodeRetrieval, true
	case inspect.FactMemory:
		return NodeMemory, true
	case inspect.FactCredentialBoundary:
		return NodeCredentialBoundary, true
	case inspect.FactApproval:
		return NodeApproval, true
	case inspect.FactFilesystemBoundary:
		return NodeFilesystemBoundary, true
	case inspect.FactProcessBoundary:
		return NodeProcessBoundary, true
	case inspect.FactNetworkBoundary:
		return NodeNetworkBoundary, true
	case inspect.FactTelemetrySink:
		return NodeTelemetrySink, true
	case inspect.FactPersistenceSink:
		return NodePersistenceSink, true
	case inspect.FactExternalDestination:
		return NodeExternalDestination, true
	case inspect.FactOpenBoxSDK:
		return NodeOpenBoxSDK, true
	default:
		return "", false
	}
}

func signalFact(kind inspect.FactKind) bool {
	return kind == inspect.FactPackageDependency || kind == inspect.FactPackageImport || kind == inspect.FactEntrypoint || kind == inspect.FactEnvironmentReference
}

func nodeSemantic(nodeType NodeType, fact inspect.Fact) (string, error) {
	switch nodeType {
	case NodeAgent, NodeModelRoute, NodeTool, NodeMCPServer, NodeRetrieval, NodeMemory, NodeApproval:
		return anchoredSemantic(fact)
	default:
		return fact.Value, nil
	}
}

func signalSemantic(fact inspect.Fact) (string, error) {
	if fact.Kind == inspect.FactEntrypoint {
		return anchoredSemantic(fact)
	}
	if fact.Kind == inspect.FactPackageDependency {
		return fact.Value + "\x00" + fact.DeclaredValueDigest.String(), nil
	}
	return fact.Value, nil
}

func anchoredSemantic(fact inspect.Fact) (string, error) {
	canonical, err := artifact.CanonicalJSON(map[string]any{
		"column": fact.Evidence.Column,
		"line":   fact.Evidence.Line,
		"path":   fact.Evidence.Path,
		"value":  fact.Value,
	})
	if err != nil {
		return "", fmt.Errorf("model: canonical source anchor: %w", err)
	}
	return string(canonical), nil
}

func semanticID(namespace, kind, value string) (string, error) {
	canonical, err := artifact.CanonicalJSON(map[string]any{"kind": kind, "namespace": namespace, "value": value})
	if err != nil {
		return "", fmt.Errorf("model: canonical identity: %w", err)
	}
	hexDigest := strings.TrimPrefix(artifact.DigestBytes(canonical).String(), "sha256:")
	return kind + ":" + hexDigest, nil
}

func appendProvenance(target *[]Provenance, candidate Provenance) {
	for _, existing := range *target {
		if existing == candidate {
			return
		}
	}
	*target = append(*target, candidate)
}

func sortProvenance(provenance []Provenance) {
	sort.Slice(provenance, func(left, right int) bool {
		a, b := provenance[left], provenance[right]
		if a.Detector != b.Detector {
			return a.Detector < b.Detector
		}
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		if a.Column != b.Column {
			return a.Column < b.Column
		}
		if a.Basis != b.Basis {
			return a.Basis < b.Basis
		}
		if a.Confidence != b.Confidence {
			return a.Confidence < b.Confidence
		}
		return a.Digest.String() < b.Digest.String()
	})
}

func normalizeUncertainties(graph *Graph, source []inspect.Uncertainty, identify identityFunction) error {
	byID := make(map[string]Uncertainty)
	for _, item := range source {
		if item.Subject == "" || item.Reason == "" || len(item.Reason) > 4096 || item.Line < 0 {
			return errors.New("model: invalid uncertainty")
		}
		identityBytes, err := artifact.CanonicalJSON(map[string]any{
			"line": item.Line, "path": item.Path, "reason": item.Reason, "subject": item.Subject,
		})
		if err != nil {
			return fmt.Errorf("model: canonical uncertainty identity: %w", err)
		}
		id, err := identify("uncertainty", item.Subject, string(identityBytes))
		if err != nil {
			return err
		}
		candidate := Uncertainty{ID: id, Subject: item.Subject, Reason: item.Reason, Path: item.Path, Line: item.Line, EvidenceLevel: "discovered"}
		if existing, found := byID[id]; found && existing != candidate {
			return fmt.Errorf("model: uncertainty identity collision at %q", id)
		}
		byID[id] = candidate
	}
	if len(byID) > 10000 {
		return errors.New("model: uncertainties exceed 10000")
	}
	graph.uncertainties = make([]Uncertainty, 0, len(byID))
	for _, item := range byID {
		graph.uncertainties = append(graph.uncertainties, item)
	}
	sort.Slice(graph.uncertainties, func(left, right int) bool { return graph.uncertainties[left].ID < graph.uncertainties[right].ID })
	return nil
}

func cloneNodes(source []Node) []Node {
	result := append([]Node(nil), source...)
	for index := range result {
		result[index].Provenance = append([]Provenance(nil), result[index].Provenance...)
	}
	return result
}

func cloneSignals(source []Signal) []Signal {
	result := append([]Signal(nil), source...)
	for index := range result {
		result[index].Provenance = append([]Provenance(nil), result[index].Provenance...)
	}
	return result
}

func cloneInspectInitializations(source []inspect.OpenBoxInitialization) []inspect.OpenBoxInitialization {
	result := append([]inspect.OpenBoxInitialization(nil), source...)
	for index := range result {
		result[index].Options = append([]inspect.InitializationOption(nil), result[index].Options...)
	}
	return result
}
