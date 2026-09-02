package snapshot

import (
	"errors"
	"os"

	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/artifact"
)

const maxContractInteger = int64(9007199254740991)

// File records one regular file copied into an executable snapshot. Only the
// executable bit is semantically preserved; the copied tree itself is sealed
// read-only after construction.
type File struct {
	Path       string                 `json:"path"`
	Digest     artifact.ContentDigest `json:"digest"`
	Size       int64                  `json:"size"`
	Executable bool                   `json:"executable"`
}

// Snapshot is an immutable copied tree plus its normalized identities. Root is
// volatile run state and is deliberately absent from the canonical manifest.
type Snapshot struct {
	root            string
	rootIdentity    os.FileInfo
	sourceIdentity  os.FileInfo
	manifest        []byte
	digest          artifact.ContentDigest
	selectionDigest artifact.ContentDigest
	files           []File
	totalBytes      int64
	rules           []Rule
	omissions       []Omission
	dependencies    bool
}

// Root returns the volatile absolute path of the read-only copied tree.
func (snapshot *Snapshot) Root() string { return snapshot.root }

// Manifest returns a copy of the normalized per-file manifest bytes.
func (snapshot *Snapshot) Manifest() []byte { return append([]byte(nil), snapshot.manifest...) }

// Digest returns the content ID of Manifest.
func (snapshot *Snapshot) Digest() artifact.ContentDigest { return snapshot.digest }

// SelectionDigest identifies the selected source entries, including classified
// non-regular entries and the exact digests of selected regular files.
func (snapshot *Snapshot) SelectionDigest() artifact.ContentDigest { return snapshot.selectionDigest }

// Files returns a copy of the deterministic regular-file inventory.
func (snapshot *Snapshot) Files() []File { return append([]File(nil), snapshot.files...) }

// FileCount returns the number of copied regular files.
func (snapshot *Snapshot) FileCount() int64 { return int64(len(snapshot.files)) }

// TotalBytes returns the exact sum of copied regular-file byte lengths.
func (snapshot *Snapshot) TotalBytes() int64 { return snapshot.totalBytes }

// SelectionRules returns the closed rules applied before copying.
func (snapshot *Snapshot) SelectionRules() []Rule { return append([]Rule(nil), snapshot.rules...) }

// Omissions returns bounded path-only omission records. Omitted contents are
// never retained by this package.
func (snapshot *Snapshot) Omissions() []Omission {
	result := make([]Omission, len(snapshot.omissions))
	for index, omission := range snapshot.omissions {
		result[index] = omission
		result[index].Examples = make([]string, len(omission.Examples))
		copy(result[index].Examples, omission.Examples)
	}
	return result
}

// Copy copies every selected regular file into an existing empty private
// directory owned by the caller. Destination lifecycle and cleanup remain with
// the caller; this package never removes a caller-supplied path.
func (project *Project) Copy(destination string) (*Snapshot, error) {
	return project.copy(destination, false)
}

// CopyWithDependencies creates the dependency-complete snapshot used only by
// a byte-pinned trusted testbed. All ordinary project snapshots keep excluding
// dependency caches.
func (project *Project) CopyWithDependencies(destination string) (*Snapshot, error) {
	return project.copy(destination, true)
}

func (project *Project) copy(destination string, dependencies bool) (*Snapshot, error) {
	selection, err := project.selection(dependencies)
	if err != nil {
		return nil, err
	}
	if err := validateSelectedByteBounds(selection.entries); err != nil {
		return nil, err
	}
	snapshot, err := copySnapshot(project, destination, selection)
	if err != nil {
		return nil, err
	}
	snapshot.dependencies = dependencies
	if project.afterCopy != nil {
		if err := project.afterCopy(); err != nil {
			return nil, err
		}
	}
	if err := project.Verify(snapshot); err != nil {
		return nil, errors.Join(errors.New("snapshot: source changed while copying"), err)
	}
	if err := sealSnapshot(snapshot); err != nil {
		return nil, err
	}
	return snapshot, nil
}

// Verify re-selects and re-hashes the source and fails if its normalized
// identity differs from the bytes copied into snapshot.
func (project *Project) Verify(snapshot *Snapshot) error {
	if snapshot == nil {
		return errors.New("snapshot: nil copied snapshot")
	}
	if !os.SameFile(project.identity, snapshot.sourceIdentity) {
		return errors.New("snapshot: copied snapshot belongs to a different source root")
	}
	selection, err := project.selection(snapshot.dependencies)
	if err != nil {
		return err
	}
	if err := validateSelectedByteBounds(selection.entries); err != nil {
		return err
	}
	files, err := hashSelectedFiles(project, selection.entries)
	if err != nil {
		return err
	}
	_, digest, selectionDigest, _, err := normalizedIdentities(files, selection)
	if err != nil {
		return err
	}
	if digest != snapshot.digest || selectionDigest != snapshot.selectionDigest {
		return errors.New("snapshot: source selection or bytes changed")
	}
	return nil
}

func validateSelectedByteBounds(entries []Entry) error {
	var total int64
	for _, entry := range entries {
		if entry.Kind != KindRegular {
			continue
		}
		if entry.Size < 0 || entry.Size > maxContractInteger-total {
			return errors.New("snapshot: selected byte total exceeds the v1 integer bound")
		}
		total += entry.Size
	}
	return nil
}

type snapshotManifest struct {
	Files []File `json:"files"`
}

type selectionManifest struct {
	Entries   []selectionRecord `json:"entries"`
	Rules     []Rule            `json:"rules"`
	Omissions []Omission        `json:"omissions"`
}

type selectionRecord struct {
	Path       string `json:"path"`
	Kind       Kind   `json:"kind"`
	Digest     string `json:"digest,omitempty"`
	Size       int64  `json:"size,omitempty"`
	Executable bool   `json:"executable,omitempty"`
	LinkTarget string `json:"linkTarget,omitempty"`
}

func normalizedIdentities(files []File, selection selectedSource) ([]byte, artifact.ContentDigest, artifact.ContentDigest, int64, error) {
	fileByPath := make(map[string]File, len(files))
	var total int64
	for _, file := range files {
		if _, exists := fileByPath[file.Path]; exists {
			return nil, artifact.ContentDigest{}, artifact.ContentDigest{}, 0, errors.New("snapshot: duplicate copied file path")
		}
		if file.Size < 0 || file.Size > maxContractInteger-total {
			return nil, artifact.ContentDigest{}, artifact.ContentDigest{}, 0, errors.New("snapshot: file byte total exceeds the v1 integer bound")
		}
		total += file.Size
		fileByPath[file.Path] = file
	}
	records := make([]selectionRecord, 0, len(selection.entries))
	for _, entry := range selection.entries {
		record := selectionRecord{Path: entry.Path, Kind: entry.Kind, LinkTarget: entry.LinkTarget}
		if entry.Kind == KindRegular {
			file, ok := fileByPath[entry.Path]
			if !ok {
				return nil, artifact.ContentDigest{}, artifact.ContentDigest{}, 0, errors.New("snapshot: selected regular file has no copied digest")
			}
			record.Digest = file.Digest.String()
			record.Size = file.Size
			record.Executable = file.Executable
		}
		records = append(records, record)
	}
	manifest, digest, err := artifact.DigestCanonicalJSON(snapshotManifest{Files: files})
	if err != nil {
		return nil, artifact.ContentDigest{}, artifact.ContentDigest{}, 0, err
	}
	_, selectionDigest, err := artifact.DigestCanonicalJSON(selectionManifest{
		Entries:   records,
		Rules:     selection.rules,
		Omissions: selection.omissions,
	})
	if err != nil {
		return nil, artifact.ContentDigest{}, artifact.ContentDigest{}, 0, err
	}
	return manifest, digest, selectionDigest, total, nil
}
