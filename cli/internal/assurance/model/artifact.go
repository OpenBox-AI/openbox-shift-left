package model

import (
	"errors"
	"fmt"
	"path"
	"strings"
	"unicode/utf8"

	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/artifact"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/inspect"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/snapshot"
)

// ProjectIdentity supplies stable project metadata that passive snapshot
// selection cannot derive without reading excluded VCS state.
type ProjectIdentity struct {
	Name string
	Git  GitState
}

type GitState struct {
	Present bool
	Head    *string
	Dirty   *bool
}

const gitStatusUnknownReason = "Git repository presence was detected, but HEAD and dirty state were not resolved by filesystem-only inspection."

// ProjectArtifacts are the normalized Phase 02 inputs to a later audit pack.
// SnapshotManifest is the project-snapshot role, not manifest.json: only a
// complete audit pack may publish that root file.
type ProjectArtifacts struct {
	snapshotManifest artifact.Object
	projectModel     artifact.Object
	graphDigest      artifact.ContentDigest
}

func (artifacts ProjectArtifacts) SnapshotManifest() artifact.Object {
	return artifacts.snapshotManifest
}
func (artifacts ProjectArtifacts) ProjectModel() artifact.Object { return artifacts.projectModel }

// GraphDigest binds later coverage projection to the exact rich graph used to
// build ProjectModel. It is an internal assembly identity, not a public field.
func (artifacts ProjectArtifacts) GraphDigest() artifact.ContentDigest { return artifacts.graphDigest }

// BuildProjectArtifacts projects the rich internal graph to the already
// accepted openbox.project-model/v1 shape and binds every projected source
// fact to the immutable snapshot inventory.
func BuildProjectArtifacts(copied *snapshot.Snapshot, graph Graph, identity ProjectIdentity) (ProjectArtifacts, error) {
	if copied == nil {
		return ProjectArtifacts{}, errors.New("model: nil project snapshot")
	}
	graphDigest, err := GraphDigest(graph)
	if err != nil {
		return ProjectArtifacts{}, err
	}
	if err := validateProjectIdentity(identity); err != nil {
		return ProjectArtifacts{}, err
	}
	files := make(map[string]artifact.ContentDigest, len(copied.Files()))
	for _, file := range copied.Files() {
		if _, exists := files[file.Path]; exists {
			return ProjectArtifacts{}, fmt.Errorf("model: duplicate snapshot file %q", file.Path)
		}
		files[file.Path] = file.Digest
	}

	nodes := graph.Nodes()
	if len(nodes) == 0 {
		return ProjectArtifacts{}, errors.New("model: project-model requires at least one node")
	}
	projectedNodes := make([]projectModelNode, len(nodes))
	seenNodeIDs := make(map[string]struct{}, len(nodes))
	for index, node := range nodes {
		if !validName(node.ID) || !validNodeType(node.Type) {
			return ProjectArtifacts{}, fmt.Errorf("model: node %q is not representable by openbox.project-model/v1", node.ID)
		}
		if _, exists := seenNodeIDs[node.ID]; exists {
			return ProjectArtifacts{}, fmt.Errorf("model: duplicate node ID %q", node.ID)
		}
		seenNodeIDs[node.ID] = struct{}{}
		provenance, err := projectProvenance(node.Provenance, files)
		if err != nil {
			return ProjectArtifacts{}, fmt.Errorf("model: project node %q: %w", node.ID, err)
		}
		projectedNodes[index] = projectModelNode{ID: node.ID, Type: node.Type, Provenance: provenance}
	}

	uncertainties := graph.Uncertainties()
	projectedUncertainties := make([]projectModelUncertainty, 0, len(uncertainties)+1)
	for _, current := range uncertainties {
		if !validName(current.Subject) || current.Reason == "" || utf8.RuneCountInString(current.Reason) > 4096 || current.EvidenceLevel != "discovered" {
			return ProjectArtifacts{}, fmt.Errorf("model: uncertainty %q is not representable by openbox.project-model/v1", current.Subject)
		}
		projectedUncertainties = append(projectedUncertainties, projectModelUncertainty{
			Subject: current.Subject, Reason: current.Reason, EvidenceLevel: current.EvidenceLevel,
		})
	}
	if identity.Git.Present && identity.Git.Dirty == nil {
		projectedUncertainties = append(projectedUncertainties, projectModelUncertainty{
			Subject: "git-status", Reason: gitStatusUnknownReason, EvidenceLevel: "discovered",
		})
	}

	value := projectModelDocument{
		APIVersion: "openbox.project-model/v1",
		Kind:       "ProjectModel",
		Project: projectModelProject{
			Name: identity.Name, Root: ".",
			Git: projectModelGit{Present: identity.Git.Present, Head: cloneString(identity.Git.Head), Dirty: cloneBool(identity.Git.Dirty)},
		},
		Snapshot: projectModelSnapshot{
			Digest: copied.Digest(), SelectionDigest: copied.SelectionDigest(), FileCount: copied.FileCount(), TotalBytes: copied.TotalBytes(),
			SelectionRules: copied.SelectionRules(), Omissions: copied.Omissions(),
		},
		Nodes: projectedNodes, Edges: []projectModelEdge{}, Uncertainties: projectedUncertainties,
	}
	modelSchema := "openbox.project-model/v1"
	projectModel, err := artifact.NewCanonicalObject(
		artifact.RoleProjectModel, "application/json", &modelSchema, "normalized", value,
	)
	if err != nil {
		return ProjectArtifacts{}, fmt.Errorf("model: build project-model object: %w", err)
	}
	projectSnapshot, err := artifact.NewExactObject(
		artifact.RoleProjectSnapshot, "application/vnd.openbox.project-snapshot", nil, "normalized", copied.Manifest(),
	)
	if err != nil {
		return ProjectArtifacts{}, fmt.Errorf("model: build project-snapshot object: %w", err)
	}
	return ProjectArtifacts{snapshotManifest: projectSnapshot, projectModel: projectModel, graphDigest: graphDigest}, nil
}

// GraphDigest identifies the complete normalized in-process graph, including
// private signals that inform SDK coverage but are not public project nodes.
func GraphDigest(graph Graph) (artifact.ContentDigest, error) {
	_, digest, err := artifact.DigestCanonicalJSON(struct {
		Nodes           []Node                          `json:"nodes"`
		Signals         []Signal                        `json:"signals"`
		Uncertainties   []Uncertainty                   `json:"uncertainties"`
		Initializations []inspect.OpenBoxInitialization `json:"initializations"`
	}{Nodes: graph.Nodes(), Signals: graph.Signals(), Uncertainties: graph.Uncertainties(), Initializations: graph.Initializations()})
	if err != nil {
		return artifact.ContentDigest{}, fmt.Errorf("model: canonical graph binding: %w", err)
	}
	return digest, nil
}

type projectModelDocument struct {
	APIVersion    string                    `json:"apiVersion"`
	Kind          string                    `json:"kind"`
	Project       projectModelProject       `json:"project"`
	Snapshot      projectModelSnapshot      `json:"snapshot"`
	Nodes         []projectModelNode        `json:"nodes"`
	Edges         []projectModelEdge        `json:"edges"`
	Uncertainties []projectModelUncertainty `json:"uncertainties"`
}

type projectModelProject struct {
	Name string          `json:"name"`
	Root string          `json:"root"`
	Git  projectModelGit `json:"git"`
}

type projectModelGit struct {
	Present bool    `json:"present"`
	Head    *string `json:"head"`
	Dirty   *bool   `json:"dirty"`
}

type projectModelSnapshot struct {
	Digest          artifact.ContentDigest `json:"digest"`
	SelectionDigest artifact.ContentDigest `json:"selectionDigest"`
	FileCount       int64                  `json:"fileCount"`
	TotalBytes      int64                  `json:"totalBytes"`
	SelectionRules  []snapshot.Rule        `json:"selectionRules"`
	Omissions       []snapshot.Omission    `json:"omissions"`
}

type projectModelNode struct {
	ID         string                   `json:"id"`
	Type       NodeType                 `json:"type"`
	Provenance []projectModelProvenance `json:"provenance"`
}

type projectModelProvenance struct {
	Detector string        `json:"detector"`
	Basis    inspect.Basis `json:"basis"`
	Path     string        `json:"path"`
	Line     int64         `json:"line"`
}

type projectModelEdge struct{}

type projectModelUncertainty struct {
	Subject       string `json:"subject"`
	Reason        string `json:"reason"`
	EvidenceLevel string `json:"evidenceLevel"`
}

func validateProjectIdentity(identity ProjectIdentity) error {
	if !validName(identity.Name) {
		return errors.New("model: project name is not representable by openbox.project-model/v1")
	}
	if !identity.Git.Present {
		if identity.Git.Head != nil || identity.Git.Dirty != nil {
			return errors.New("model: absent Git state requires null head and dirty")
		}
		return nil
	}
	if identity.Git.Head != nil && !validGitHead(*identity.Git.Head) {
		return errors.New("model: Git head must be a lowercase SHA-1 or SHA-256 object ID")
	}
	if identity.Git.Head != nil && identity.Git.Dirty == nil {
		return errors.New("model: unknown Git state requires null head and dirty")
	}
	return nil
}

func projectProvenance(source []Provenance, files map[string]artifact.ContentDigest) ([]projectModelProvenance, error) {
	if len(source) == 0 || len(source) > maxProvenancePerItem {
		return nil, errors.New("provenance count is outside the public contract")
	}
	result := make([]projectModelProvenance, len(source))
	for index, current := range source {
		if !validName(current.Detector) || !validRelativePath(current.Path) || current.Line < 1 ||
			(current.Basis != inspect.BasisDeclared && current.Basis != inspect.BasisInferred) {
			return nil, errors.New("provenance has an invalid detector, basis, path, or line")
		}
		digest, exists := files[current.Path]
		if !exists || digest != current.Digest {
			return nil, fmt.Errorf("provenance path %q does not resolve to its snapshot digest", current.Path)
		}
		result[index] = projectModelProvenance{Detector: current.Detector, Basis: current.Basis, Path: current.Path, Line: current.Line}
	}
	return result, nil
}

func validNodeType(candidate NodeType) bool {
	_, valid := map[NodeType]struct{}{
		NodeAgent: {}, NodeModelRoute: {}, NodeTool: {}, NodeMCPServer: {}, NodeRetrieval: {}, NodeMemory: {},
		NodeCredentialBoundary: {}, NodeApproval: {}, NodeFilesystemBoundary: {}, NodeProcessBoundary: {},
		NodeNetworkBoundary: {}, NodeTelemetrySink: {}, NodePersistenceSink: {}, NodeExternalDestination: {}, NodeOpenBoxSDK: {},
	}[candidate]
	return valid
}

func validName(candidate string) bool {
	if candidate == "" || utf8.RuneCountInString(candidate) > 256 {
		return false
	}
	for index, current := range candidate {
		if current >= 'A' && current <= 'Z' || current >= 'a' && current <= 'z' || current >= '0' && current <= '9' ||
			index > 0 && strings.ContainsRune("._:/@+-", current) {
			continue
		}
		return false
	}
	return true
}

func validRelativePath(candidate string) bool {
	if candidate == "" || candidate == "." || candidate == ".." || path.IsAbs(candidate) || path.Clean(candidate) != candidate ||
		strings.Contains(candidate, "\\") || utf8.RuneCountInString(candidate) > 4096 ||
		len(candidate) >= 2 && ((candidate[0] >= 'A' && candidate[0] <= 'Z') || (candidate[0] >= 'a' && candidate[0] <= 'z')) && candidate[1] == ':' {
		return false
	}
	for _, current := range candidate {
		if current <= 0x1f || current == 0x7f {
			return false
		}
	}
	return true
}

func validGitHead(candidate string) bool {
	if len(candidate) != 40 && len(candidate) != 64 {
		return false
	}
	for _, current := range candidate {
		if current < '0' || current > '9' {
			if current < 'a' || current > 'f' {
				return false
			}
		}
	}
	return true
}

func cloneString(value *string) *string {
	if value == nil {
		return nil
	}
	copyOf := *value
	return &copyOf
}

func cloneBool(value *bool) *bool {
	if value == nil {
		return nil
	}
	copyOf := *value
	return &copyOf
}
