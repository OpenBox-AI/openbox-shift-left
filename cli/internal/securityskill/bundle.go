package securityskill

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"sync"

	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/artifact"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

const (
	Name               = "openbox-security-evaluation"
	Version            = "1.0.0"
	BundleSchema       = "ai.openbox.security-skill-bundle/v1"
	CandidateSchema    = "ai.openbox.project-security-analysis/v1"
	CatalogSchema      = "ai.openbox.security-standards-catalog/v1"
	CatalogVersion     = "2026-08-26-mvp1"
	RepositoryPath     = "cli/internal/securityskill/bundles/openbox-security-evaluation/1.0.0"
	bundleRoot         = "bundles/openbox-security-evaluation/1.0.0"
	BundleManifestName = "bundle.json"
	MaxCandidateBytes  = 4 << 20
)

var payloadPaths = [...]string{
	"SKILL.md",
	"references/candidate.schema.json",
	"references/evidence-authority.md",
	"references/standards.json",
	"scripts/publish-candidate.sh",
}

//go:embed bundles/openbox-security-evaluation/1.0.0
var bundleFiles embed.FS

type Descriptor struct {
	Bytes  int    `json:"bytes"`
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

type Manifest struct {
	Digest  string       `json:"digest"`
	Files   []Descriptor `json:"files"`
	Name    string       `json:"name"`
	Schema  string       `json:"schema"`
	Version string       `json:"version"`
}

var (
	bundleOnce     sync.Once
	bundleManifest Manifest
	bundlePayloads map[string][]byte
	bundleErr      error
	candidateOnce  sync.Once
	candidateValue *jsonschema.Schema
	candidateErr   error
)

// Load returns independently owned bytes after verifying the embedded exact
// file set, per-file descriptors, and canonical bundle digest.
func Load() (Manifest, map[string][]byte, error) {
	bundleOnce.Do(loadBundle)
	if bundleErr != nil {
		return Manifest{}, nil, bundleErr
	}
	manifest := bundleManifest
	manifest.Files = append([]Descriptor(nil), bundleManifest.Files...)
	files := make(map[string][]byte, len(bundlePayloads))
	for path, content := range bundlePayloads {
		files[path] = append([]byte(nil), content...)
	}
	return manifest, files, nil
}

func loadBundle() {
	entries := make([]string, 0, len(payloadPaths)+1)
	err := fs.WalkDir(bundleFiles, bundleRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		relative := strings.TrimPrefix(path, bundleRoot+"/")
		entries = append(entries, relative)
		return nil
	})
	if err != nil {
		bundleErr = fmt.Errorf("security skill: enumerate embedded bundle: %w", err)
		return
	}
	sort.Strings(entries)
	wantEntries := append([]string(nil), payloadPaths[:]...)
	wantEntries = append(wantEntries, BundleManifestName)
	sort.Strings(wantEntries)
	if !equalStrings(entries, wantEntries) {
		bundleErr = errors.New("security skill: embedded bundle file set is not exact")
		return
	}
	manifestBytes, err := bundleFiles.ReadFile(bundleRoot + "/" + BundleManifestName)
	if err != nil {
		bundleErr = err
		return
	}
	if _, err := artifact.CanonicalizeJSON(manifestBytes); err != nil {
		bundleErr = errors.New("security skill: bundle manifest is invalid JSON")
		return
	}
	if err := json.Unmarshal(manifestBytes, &bundleManifest); err != nil {
		bundleErr = fmt.Errorf("security skill: decode bundle manifest: %w", err)
		return
	}
	if bundleManifest.Schema != BundleSchema || bundleManifest.Name != Name || bundleManifest.Version != Version {
		bundleErr = errors.New("security skill: bundle identity is invalid")
		return
	}
	bundlePayloads = make(map[string][]byte, len(payloadPaths)+1)
	descriptors := make([]Descriptor, 0, len(payloadPaths))
	for _, path := range payloadPaths {
		content, err := bundleFiles.ReadFile(bundleRoot + "/" + path)
		if err != nil {
			bundleErr = err
			return
		}
		descriptors = append(descriptors, Descriptor{Bytes: len(content), Path: path, SHA256: artifact.DigestBytes(content).String()})
		bundlePayloads[path] = content
	}
	descriptorBytes, err := artifact.CanonicalJSON(descriptors)
	if err != nil {
		bundleErr = err
		return
	}
	digest := artifact.DigestBytes(descriptorBytes).String()
	if !equalDescriptors(descriptors, bundleManifest.Files) || bundleManifest.Digest != digest {
		bundleErr = fmt.Errorf("security skill: bundle descriptors or digest drifted (computed %s, manifest %s)", digest, bundleManifest.Digest)
		return
	}
	bundlePayloads[BundleManifestName] = manifestBytes
}

func ValidateCandidate(content []byte) error {
	if len(content) > MaxCandidateBytes {
		return errors.New("security skill: candidate exceeds 4 MiB")
	}
	_, err := artifact.CanonicalizeJSON(content)
	if err != nil {
		return fmt.Errorf("security skill: candidate JSON: %w", err)
	}
	candidateOnce.Do(func() {
		_, files, err := Load()
		if err != nil {
			candidateErr = err
			return
		}
		compiler := jsonschema.NewCompiler()
		compiler.DefaultDraft(jsonschema.Draft2020)
		document, err := jsonschema.UnmarshalJSON(bytes.NewReader(files["references/candidate.schema.json"]))
		if err != nil {
			candidateErr = err
			return
		}
		if err := compiler.AddResource("candidate.schema.json", document); err != nil {
			candidateErr = err
			return
		}
		candidateValue, candidateErr = compiler.Compile("candidate.schema.json")
	})
	if candidateErr != nil {
		return fmt.Errorf("security skill: compile candidate schema: %w", candidateErr)
	}
	document, err := jsonschema.UnmarshalJSON(bytes.NewReader(content))
	if err != nil {
		return fmt.Errorf("security skill: decode candidate: %w", err)
	}
	if err := candidateValue.Validate(document); err != nil {
		return fmt.Errorf("security skill: candidate schema validation failed: %w", err)
	}
	return nil
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func equalDescriptors(left, right []Descriptor) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
