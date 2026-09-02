// Package runfs owns the local project-assurance run-directory lifecycle.
package runfs

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/artifact"
)

const (
	// IncompleteMarkerName identifies a run directory that is not an audit pack.
	IncompleteMarkerName = ".incomplete"
	// ManifestName is the final, manifest-last audit-pack entry.
	ManifestName = "manifest.json"

	incompleteMarker  = "openbox project-assurance run v1: incomplete\n"
	finalizeLockName  = ".finalizing"
	finalizeLock      = "openbox project-assurance run v1: finalizing\n"
	cleanupMarkerName = ".cleaning"
	cleanupMarker     = "openbox project-assurance run v1: cleaning\n"
)

// State is the conservative on-disk classification of a run directory.
type State string

const (
	StateAbsent     State = "absent"
	StateIncomplete State = "incomplete"
	// StateOrphaned is a markerless crash residue. It never grants normal
	// workspace authority; CleanupOrphan requires a separate explicit call and
	// removes only the closed orphan shape.
	StateOrphaned State = "orphaned"
	// StateManifestCommitted means only that the filesystem transaction
	// completed. Audit-pack schema, role, object, and digest validation remain a
	// separate reader boundary.
	StateManifestCommitted State = "manifest_committed"
	StateInvalid           State = "invalid"
)

// CleanupReceipt records what cleanup actually changed without embedding a
// host path or treating a failed/partial removal as success.
type CleanupReceipt struct {
	Before      State `json:"before"`
	Removed     bool  `json:"removed"`
	ExistsAfter bool  `json:"existsAfter"`
}

// Workspace owns one exact run directory created by Create or explicitly
// recovered by OpenIncomplete. Its private identity pins recursive work to the
// opened inode and rejects observed path replacements.
type Workspace struct {
	root               string
	identity           os.FileInfo
	syncRoot           func(*os.File) error
	beforePublish      func() error
	beforeRemove       func() error
	afterMarkerRemoval func() error
}

// Create makes a new private run directory and durably records its incomplete
// marker. The target must be an absolute, previously absent path.
func Create(root string) (workspace *Workspace, err error) {
	if err := ensureSupportedPlatform(); err != nil {
		return nil, err
	}
	clean, err := validateRoot(root)
	if err != nil {
		return nil, err
	}
	if isRecoveryPath(clean) {
		return nil, errors.New("runfs: run root uses a reserved recovery name")
	}
	if _, err := os.Lstat(clean); err == nil {
		return nil, errors.New("runfs: run directory already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("runfs: stat run target: %w", err)
	}
	parent := filepath.Dir(clean)
	parentDirectory, err := openDirectoryNoFollow(parent)
	if err != nil {
		return nil, fmt.Errorf("runfs: open run parent without following its final component: %w", err)
	}
	defer parentDirectory.Close()
	staging, err := createStagingDirectory(parent, filepath.Base(clean))
	if err != nil {
		return nil, fmt.Errorf("runfs: create staging directory: %w", err)
	}
	ownedPath := staging
	identity, err := os.Lstat(staging)
	if err != nil {
		return nil, fmt.Errorf("runfs: stat staging directory: %w", err)
	}
	rollback := true
	defer func() {
		if rollback {
			cleanupErr := removeOwnedTree(ownedPath, identity)
			if cleanupErr != nil {
				err = errors.Join(err, fmt.Errorf("runfs: roll back failed run directory: %w", cleanupErr))
			}
		}
	}()
	if err := os.Chmod(staging, 0o700); err != nil {
		return nil, fmt.Errorf("runfs: secure run directory: %w", err)
	}
	if err := writeExclusive(filepath.Join(staging, IncompleteMarkerName), []byte(incompleteMarker), 0o600); err != nil {
		return nil, err
	}
	if err := syncDirectory(staging); err != nil {
		return nil, fmt.Errorf("runfs: sync run directory: %w", err)
	}
	if err := syncDirectory(parent); err != nil {
		return nil, fmt.Errorf("runfs: sync run parent: %w", err)
	}
	if err := renameNoReplaceAt(parentDirectory, filepath.Base(staging), parentDirectory, filepath.Base(clean)); err != nil {
		return nil, fmt.Errorf("runfs: publish incomplete run directory: %w", err)
	}
	ownedPath = clean
	if err := syncDirectory(parent); err != nil {
		return nil, fmt.Errorf("runfs: sync published run directory: %w", err)
	}
	rollback = false
	return &Workspace{root: clean, identity: identity, syncRoot: syncOpenDirectory}, nil
}

// OpenIncomplete recovers only a directory with a valid lifecycle sentinel.
// A generated name alone never grants deletion authority.
func OpenIncomplete(root string) (*Workspace, error) {
	if err := ensureSupportedPlatform(); err != nil {
		return nil, err
	}
	clean, err := validateRoot(root)
	if err != nil {
		return nil, err
	}
	identity, err := validateDirectoryModes(clean, 0o700, 0o500)
	if err != nil {
		return nil, err
	}
	state, err := Inspect(clean)
	if err != nil || state != StateIncomplete {
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("runfs: cannot recover workspace in %s state", state)
	}
	return &Workspace{root: clean, identity: identity, syncRoot: syncOpenDirectory}, nil
}

// Root returns the cleaned absolute path owned by the workspace.
func (workspace *Workspace) Root() string {
	return workspace.root
}

// WritePrivateFile writes one new owner-only regular file directly beneath the
// owned incomplete workspace. The name is deliberately a single path element:
// callers cannot traverse out of the workspace or create an ad-hoc directory
// tree. Publication is exclusive and handle-relative, so replacing the path to
// the workspace or a destination with a symlink cannot redirect the write.
func (workspace *Workspace) WritePrivateFile(name string, content []byte) error {
	if name == "" || filepath.Base(name) != name || name == "." || name == ".." ||
		name == IncompleteMarkerName || name == ManifestName || name == finalizeLockName ||
		name == cleanupMarkerName {
		return errors.New("runfs: invalid private file name")
	}
	if err := workspace.requireIdentity(); err != nil {
		return err
	}
	state, err := Inspect(workspace.root)
	if err != nil || state != StateIncomplete {
		if err != nil {
			return err
		}
		return fmt.Errorf("runfs: cannot write private file in %s state", state)
	}
	root, err := openOwnedRoot(workspace.root, workspace.identity)
	if err != nil {
		return fmt.Errorf("runfs: open owned workspace: %w", err)
	}
	defer root.Close()
	if err := writeExclusiveAt(root, name, content, 0o600); err != nil {
		return err
	}
	if err := workspace.syncRoot(root); err != nil {
		return fmt.Errorf("runfs: sync private file publication: %w", err)
	}
	return workspace.requireIdentity()
}

// WritePackObjects persists the exact content-addressed payloads assembled by
// artifact.AssemblePack. It never writes manifest.json; callers must use
// FinalizePack to bind the sealed on-disk object set to the assembled manifest.
// Neither operation implies schema-valid audit-pack evidence.
func (workspace *Workspace) WritePackObjects(pack *artifact.Pack) error {
	if pack == nil {
		return errors.New("runfs: nil assembled pack")
	}
	if err := workspace.requireIdentity(); err != nil {
		return err
	}
	state, err := Inspect(workspace.root)
	if err != nil || state != StateIncomplete {
		if err != nil {
			return err
		}
		return fmt.Errorf("runfs: cannot write pack objects in %s state", state)
	}
	objects, err := packObjectPayloads(pack)
	if err != nil {
		return err
	}
	root, err := openOwnedRoot(workspace.root, workspace.identity)
	if err != nil {
		return fmt.Errorf("runfs: open owned workspace: %w", err)
	}
	defer root.Close()
	if err := writePackObjectsAt(root, objects); err != nil {
		return fmt.Errorf("runfs: write pack objects: %w", err)
	}
	return workspace.requireIdentity()
}

// Inspect classifies an absent, incomplete, orphaned, filesystem-committed, or
// invalid run root. It does not validate an audit-pack schema, role map, or
// object CID. A manifest and incomplete marker together remain incomplete;
// readers must never upgrade an interrupted finalization.
func Inspect(root string) (State, error) {
	clean, err := validateRoot(root)
	if err != nil {
		return StateInvalid, err
	}
	info, err := os.Lstat(clean)
	if errors.Is(err, os.ErrNotExist) {
		return StateAbsent, nil
	}
	if err != nil {
		return StateInvalid, fmt.Errorf("runfs: stat run directory: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return StateInvalid, errors.New("runfs: run root is not a directory")
	}
	if orphan, orphanErr := isClosedOrphan(clean, info); orphanErr != nil {
		return StateInvalid, orphanErr
	} else if orphan {
		return StateOrphaned, nil
	}

	markerExists, markerErr := regularFileExists(filepath.Join(clean, IncompleteMarkerName))
	if markerErr != nil {
		return StateInvalid, markerErr
	}
	manifestExists, manifestErr := regularFileExists(filepath.Join(clean, ManifestName))
	if manifestErr != nil {
		return StateInvalid, manifestErr
	}
	lockExists, lockErr := regularFileExists(filepath.Join(clean, finalizeLockName))
	if lockErr != nil {
		return StateInvalid, lockErr
	}
	cleaningExists, cleaningErr := regularFileExists(filepath.Join(clean, cleanupMarkerName))
	if cleaningErr != nil {
		return StateInvalid, cleaningErr
	}
	if markerExists || lockExists || cleaningExists {
		if info.Mode().Perm() != 0o700 && info.Mode().Perm() != 0o500 {
			return StateInvalid, fmt.Errorf("runfs: interrupted root mode is %04o, want 0700 or 0500", info.Mode().Perm())
		}
		if err := validateMarker(clean); err != nil {
			if markerExists {
				return StateInvalid, err
			}
		}
		if lockExists {
			if err := validateSentinel(filepath.Join(clean, finalizeLockName), finalizeLock); err != nil {
				return StateInvalid, err
			}
		}
		if cleaningExists {
			if err := validateSentinel(filepath.Join(clean, cleanupMarkerName), cleanupMarker); err != nil {
				return StateInvalid, err
			}
		}
		return StateIncomplete, nil
	}
	if !manifestExists {
		return StateInvalid, errors.New("runfs: directory has neither incomplete marker nor manifest")
	}
	manifestInfo, err := os.Lstat(filepath.Join(clean, ManifestName))
	if err != nil {
		return StateInvalid, fmt.Errorf("runfs: stat manifest: %w", err)
	}
	if manifestInfo.Mode().Perm() != 0o400 {
		return StateInvalid, fmt.Errorf("runfs: manifest mode is %04o, want 0400", manifestInfo.Mode().Perm())
	}
	if info.Mode().Perm() == 0o700 {
		return StateIncomplete, nil
	}
	if info.Mode().Perm() != 0o500 {
		return StateInvalid, fmt.Errorf("runfs: finalized root mode is %04o, want 0500", info.Mode().Perm())
	}
	return StateManifestCommitted, nil
}

// Finalize commits caller-supplied canonical manifest bytes with an
// atomic no-replace rename, then seals the lifecycle sentinels and root. This
// low-level method proves only the filesystem transaction and does not bind an
// artifact.Pack; assembled packs must use FinalizePack. Audit-pack validity is
// a separate reader/validator boundary. Any returned failure after publication
// restores the incomplete marker so a partial pack cannot look committed.
func (workspace *Workspace) Finalize(manifest []byte) (digest artifact.ContentDigest, err error) {
	return workspace.finalize(manifest, nil, nil)
}

// FinalizePack verifies the sealed on-disk object set against the assembled
// pack immediately before publishing its manifest. This binds WritePackObjects
// to the manifest-last transaction without upgrading schema or semantic
// validity, which remains a separate reader boundary.
func (workspace *Workspace) FinalizePack(pack *artifact.Pack) (artifact.ContentDigest, error) {
	if pack == nil {
		return artifact.ContentDigest{}, errors.New("runfs: nil assembled pack")
	}
	objects, err := packObjectPayloads(pack)
	if err != nil {
		return artifact.ContentDigest{}, err
	}
	digest, err := workspace.finalize(pack.Manifest(), func(root *os.File) error {
		return verifyPackObjectsAt(root, objects)
	}, nil)
	if err != nil {
		return artifact.ContentDigest{}, err
	}
	if digest != pack.Digest() {
		return artifact.ContentDigest{}, errors.New("runfs: finalized manifest digest differs from assembled pack")
	}
	return digest, nil
}

func (workspace *Workspace) finalize(
	manifest []byte,
	verifyObjects func(*os.File) error,
	rootFiles map[string][]byte,
) (digest artifact.ContentDigest, err error) {
	if isRecoveryPath(workspace.root) {
		return digest, errors.New("runfs: recovery workspace cannot be finalized")
	}
	canonical, canonicalErr := artifact.CanonicalizeJSON(manifest)
	if canonicalErr != nil {
		return digest, fmt.Errorf("runfs: manifest is not valid canonical JSON: %w", canonicalErr)
	}
	if !bytes.Equal(canonical, manifest) {
		return digest, errors.New("runfs: manifest bytes are not canonical JSON")
	}
	if err := workspace.requireIdentity(); err != nil {
		return digest, err
	}
	state, err := Inspect(workspace.root)
	if err != nil || state != StateIncomplete {
		if err != nil {
			return digest, err
		}
		return digest, fmt.Errorf("runfs: cannot finalize workspace in %s state", state)
	}
	root, err := openOwnedRoot(workspace.root, workspace.identity)
	if err != nil {
		return digest, fmt.Errorf("runfs: open owned workspace: %w", err)
	}
	defer root.Close()
	if exists, statErr := entryExistsAt(root, ManifestName); statErr != nil {
		return digest, fmt.Errorf("runfs: stat manifest: %w", statErr)
	} else if exists {
		return digest, errors.New("runfs: manifest already exists")
	}
	if err := writeExclusiveAt(root, finalizeLockName, []byte(finalizeLock), 0o600); err != nil {
		return digest, fmt.Errorf("runfs: acquire finalization lock: %w", err)
	}
	lockHeld := true
	defer func() {
		if !lockHeld {
			return
		}
		removeErr := unlinkAt(root, finalizeLockName)
		if removeErr == nil {
			removeErr = syncOpenDirectory(root)
		}
		if removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			err = errors.Join(err, fmt.Errorf("runfs: release finalization lock: %w", removeErr))
		}
	}()
	if err := sealOwnedContents(root, workspace.identity, rootFiles); err != nil {
		return digest, err
	}
	if verifyObjects != nil {
		if err := verifyObjects(root); err != nil {
			return digest, fmt.Errorf("runfs: verify assembled pack objects: %w", err)
		}
	}

	temporaryName, err := createManifestTemporary(root, manifest)
	if err != nil {
		return digest, fmt.Errorf("runfs: create manifest staging file: %w", err)
	}
	defer func() {
		_ = unlinkAt(root, temporaryName)
	}()

	published := false
	defer func() {
		if err == nil || !published {
			return
		}
		if restoreErr := restoreIncompleteAt(root); restoreErr != nil {
			err = errors.Join(err, fmt.Errorf("runfs: restore incomplete marker: %w", restoreErr))
		}
	}()
	if workspace.beforePublish != nil {
		if err = workspace.beforePublish(); err != nil {
			return digest, fmt.Errorf("runfs: before manifest publication: %w", err)
		}
	}
	if err = renameNoReplaceAt(root, temporaryName, root, ManifestName); err != nil {
		return digest, fmt.Errorf("runfs: publish manifest atomically without replacement: %w", err)
	}
	published = true
	if err = workspace.syncRoot(root); err != nil {
		return digest, fmt.Errorf("runfs: sync published manifest: %w", err)
	}
	if err = unlinkAt(root, IncompleteMarkerName); err != nil {
		return digest, fmt.Errorf("runfs: remove incomplete marker: %w", err)
	}
	if err = unlinkAt(root, finalizeLockName); err != nil {
		return digest, fmt.Errorf("runfs: release finalization lock: %w", err)
	}
	lockHeld = false
	if err = root.Chmod(0o500); err != nil {
		return digest, fmt.Errorf("runfs: make finalized root read-only: %w", err)
	}
	if err = workspace.syncRoot(root); err != nil {
		return digest, fmt.Errorf("runfs: sync finalized root: %w", err)
	}
	if err = workspace.requireIdentity(); err != nil {
		return digest, err
	}
	return artifact.DigestBytes(manifest), nil
}

// WriteObservationPayloads writes the closed six-file observation payload set.
// FinalizeObservation must be called to verify the exact bytes and publish the
// manifest last. This path is separate from audit-pack objects.
func (workspace *Workspace) WriteObservationPayloads(payloads map[string][]byte) error {
	if !exactObservationPayloadNames(payloads) {
		return errors.New("runfs: observation payload set is not exact")
	}
	for _, name := range observationPayloadNames {
		if err := workspace.WritePrivateFile(name, payloads[name]); err != nil {
			return err
		}
	}
	return nil
}

// FinalizeObservation seals and verifies the exact root payload set before the
// canonical manifest-last commit.
func (workspace *Workspace) FinalizeObservation(payloads map[string][]byte, manifest []byte) (artifact.ContentDigest, error) {
	if !exactObservationPayloadNames(payloads) {
		return artifact.ContentDigest{}, errors.New("runfs: observation payload set is not exact")
	}
	return workspace.finalize(manifest, func(root *os.File) error {
		return verifyObservationFilesAt(root, payloads)
	}, payloads)
}

var observationPayloadNames = []string{
	"run.json", "backend.json", "openshell.jsonl", "effects.json", "behavior.json", "coverage.json",
}

func exactObservationPayloadNames(payloads map[string][]byte) bool {
	if len(payloads) != len(observationPayloadNames) {
		return false
	}
	for _, name := range observationPayloadNames {
		if _, ok := payloads[name]; !ok {
			return false
		}
	}
	return true
}

// PublishTo atomically moves an owned private sibling workspace to the
// previously absent requested destination without replacement.
func (workspace *Workspace) PublishTo(destination string) error {
	clean, err := validateRoot(destination)
	if err != nil {
		return err
	}
	if filepath.Dir(clean) != filepath.Dir(workspace.root) || clean == workspace.root {
		return errors.New("runfs: publication destination must be a distinct sibling")
	}
	if err := workspace.requireIdentity(); err != nil {
		return err
	}
	if _, err := os.Lstat(clean); err == nil {
		return errors.New("runfs: publication destination already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("runfs: stat publication destination: %w", err)
	}
	parent, err := openDirectoryNoFollow(filepath.Dir(clean))
	if err != nil {
		return fmt.Errorf("runfs: open publication parent: %w", err)
	}
	defer parent.Close()
	if err := renameNoReplaceAt(parent, filepath.Base(workspace.root), parent, filepath.Base(clean)); err != nil {
		return fmt.Errorf("runfs: publish owned workspace: %w", err)
	}
	workspace.root = clean
	return syncDirectory(filepath.Dir(clean))
}

func packObjectPayloads(pack *artifact.Pack) (map[string][]byte, error) {
	objects := make(map[string][]byte)
	for _, object := range pack.Objects() {
		content := object.Bytes()
		digest := object.Digest()
		if artifact.DigestBytes(content) != digest {
			return nil, fmt.Errorf("runfs: assembled object %q digest changed", object.Role())
		}
		name := strings.TrimPrefix(digest.String(), "sha256:")
		if existing, exists := objects[name]; exists {
			if !bytes.Equal(existing, content) {
				return nil, errors.New("runfs: conflicting payloads have the same content ID")
			}
			continue
		}
		objects[name] = content
	}
	return objects, nil
}

// Cleanup removes only a still-incomplete owned workspace. A finalized pack is
// evidence and is never deleted by this lifecycle cleanup path.
func (workspace *Workspace) Cleanup() (CleanupReceipt, error) {
	receipt := CleanupReceipt{ExistsAfter: true}
	state, err := Inspect(workspace.root)
	receipt.Before = state
	if err != nil {
		return receipt, err
	}
	if state == StateAbsent {
		receipt.ExistsAfter = false
		return receipt, nil
	}
	if state != StateIncomplete {
		return receipt, fmt.Errorf("runfs: cleanup refuses workspace in %s state", state)
	}
	if err := workspace.requireIdentity(); err != nil {
		return receipt, err
	}
	if err := cleanupOwnedTree(
		workspace.root,
		workspace.identity,
		workspace.beforeRemove,
		workspace.afterMarkerRemoval,
	); err != nil {
		receipt.ExistsAfter = pathExists(workspace.root)
		receipt.Removed = !receipt.ExistsAfter
		return receipt, fmt.Errorf("runfs: remove incomplete workspace: %w", err)
	}
	receipt.ExistsAfter = pathExists(workspace.root)
	receipt.Removed = !receipt.ExistsAfter
	if receipt.ExistsAfter {
		return receipt, errors.New("runfs: incomplete workspace still exists after cleanup")
	}
	return receipt, nil
}

// CleanupOrphan removes only the closed markerless residue left by a crash
// before marker publication or in the final empty-directory cleanup window.
// Calling it is separate explicit authority; OpenIncomplete never upgrades an
// orphan based on its name or mode.
func CleanupOrphan(root string) (CleanupReceipt, error) {
	receipt := CleanupReceipt{Before: StateOrphaned, ExistsAfter: true}
	if err := ensureSupportedPlatform(); err != nil {
		return receipt, err
	}
	clean, err := validateRoot(root)
	if err != nil {
		return receipt, err
	}
	info, err := validateDirectoryModes(clean, 0o700, 0o500)
	if err != nil {
		return receipt, err
	}
	orphan, err := isClosedOrphan(clean, info)
	if err != nil {
		return receipt, err
	}
	if !orphan {
		state, inspectErr := Inspect(clean)
		receipt.Before = state
		if inspectErr != nil {
			return receipt, inspectErr
		}
		return receipt, fmt.Errorf("runfs: orphan cleanup refuses workspace in %s state", state)
	}
	if err := removeOwnedTree(clean, info); err != nil {
		receipt.ExistsAfter = pathExists(clean)
		receipt.Removed = !receipt.ExistsAfter
		return receipt, fmt.Errorf("runfs: remove orphaned workspace: %w", err)
	}
	receipt.ExistsAfter = pathExists(clean)
	receipt.Removed = !receipt.ExistsAfter
	if receipt.ExistsAfter {
		return receipt, errors.New("runfs: orphaned workspace still exists after cleanup")
	}
	return receipt, nil
}

func (workspace *Workspace) requireIdentity() error {
	current, err := os.Lstat(workspace.root)
	if err != nil {
		return fmt.Errorf("runfs: stat owned workspace: %w", err)
	}
	if !os.SameFile(workspace.identity, current) {
		return errors.New("runfs: owned workspace identity changed")
	}
	return nil
}

func validateRoot(root string) (string, error) {
	if root == "" || !filepath.IsAbs(root) {
		return "", errors.New("runfs: run root must be an absolute path")
	}
	clean := filepath.Clean(root)
	volumeRoot := filepath.VolumeName(clean) + string(filepath.Separator)
	if clean == volumeRoot {
		return "", errors.New("runfs: filesystem root cannot be a run root")
	}
	if home, err := os.UserHomeDir(); err == nil && clean == filepath.Clean(home) {
		return "", errors.New("runfs: home directory cannot be a run root")
	}
	return clean, nil
}

func validateDirectoryModes(path string, modes ...os.FileMode) (os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("runfs: stat run directory: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("runfs: run root is not a directory")
	}
	for _, mode := range modes {
		if info.Mode().Perm() == mode {
			return info, nil
		}
	}
	return nil, fmt.Errorf("runfs: run root mode %04o is not recoverable", info.Mode().Perm())
}

func validateMarker(root string) error {
	return validateSentinel(filepath.Join(root, IncompleteMarkerName), incompleteMarker)
}

func validateSentinel(path, expected string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("runfs: stat %s: %w", filepath.Base(path), err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return fmt.Errorf("runfs: %s is not a private regular file", filepath.Base(path))
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("runfs: read %s: %w", filepath.Base(path), err)
	}
	if string(content) != expected {
		return fmt.Errorf("runfs: %s content is invalid", filepath.Base(path))
	}
	return nil
}

func regularFileExists(path string) (bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("runfs: stat %s: %w", filepath.Base(path), err)
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("runfs: %s is not a regular file", filepath.Base(path))
	}
	return true, nil
}

func writeExclusive(path string, content []byte, mode os.FileMode) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return fmt.Errorf("runfs: create %s: %w", filepath.Base(path), err)
	}
	remove := true
	defer func() {
		_ = file.Close()
		if remove {
			_ = os.Remove(path)
		}
	}()
	if err := file.Chmod(mode); err != nil {
		return fmt.Errorf("runfs: chmod %s: %w", filepath.Base(path), err)
	}
	if _, err := file.Write(content); err != nil {
		return fmt.Errorf("runfs: write %s: %w", filepath.Base(path), err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("runfs: sync %s: %w", filepath.Base(path), err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("runfs: close %s: %w", filepath.Base(path), err)
	}
	remove = false
	return nil
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		if runtime.GOOS == "windows" || errors.Is(err, os.ErrInvalid) {
			return nil
		}
		return err
	}
	return nil
}

func pathExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil || !errors.Is(err, os.ErrNotExist)
}

func randomEntryName(prefix, suffix string) (string, error) {
	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", fmt.Errorf("runfs: create random entry name: %w", err)
	}
	return prefix + hex.EncodeToString(random[:]) + suffix, nil
}

func createStagingDirectory(parent, base string) (string, error) {
	for attempts := 0; attempts < 128; attempts++ {
		name, err := randomEntryName("."+base+".creating-", "")
		if err != nil {
			return "", err
		}
		path := filepath.Join(parent, name)
		if err := os.Mkdir(path, 0o700); errors.Is(err, os.ErrExist) {
			continue
		} else if err != nil {
			return "", err
		}
		return path, nil
	}
	return "", errors.New("runfs: could not allocate creation staging directory")
}

func isRecoveryPath(root string) bool {
	base := filepath.Base(root)
	if !strings.HasPrefix(base, ".") {
		return false
	}
	separator := ".creating-"
	index := strings.LastIndex(base, separator)
	if index <= 1 {
		return false
	}
	token := base[index+len(separator):]
	return len(token) == 16 && strings.IndexFunc(token, func(character rune) bool {
		return !('0' <= character && character <= '9') && !('a' <= character && character <= 'f')
	}) == -1
}

func isClosedOrphan(root string, info os.FileInfo) (bool, error) {
	if info.Mode().Perm() != 0o700 && info.Mode().Perm() != 0o500 {
		return false, nil
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return false, err
	}
	if len(entries) == 0 {
		if isRecoveryPath(root) {
			return true, nil
		}
		return hasCleanupOrphanAttribute(root, info)
	}
	if !isRecoveryPath(root) {
		return false, nil
	}
	if len(entries) != 1 || entries[0].Name() != IncompleteMarkerName {
		return false, nil
	}
	markerInfo, err := os.Lstat(filepath.Join(root, IncompleteMarkerName))
	if err != nil || !markerInfo.Mode().IsRegular() || markerInfo.Mode().Perm() != 0o600 {
		return false, err
	}
	content, err := os.ReadFile(filepath.Join(root, IncompleteMarkerName))
	if err != nil {
		return false, err
	}
	return len(content) < len(incompleteMarker) && strings.HasPrefix(incompleteMarker, string(content)), nil
}
