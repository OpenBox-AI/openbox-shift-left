package inspect

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/artifact"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/snapshot"
)

const (
	maxManifestBytes      = int64(1 << 20)
	maxManifestTotalBytes = int64(8 << 20)
	maxManifestCount      = 128
	maxManifestPathRunes  = 4096
	maxManifestPathDepth  = 64
	maxManifestJSONDepth  = 64
)

// ManifestKind identifies one closed declarative input understood by the MVP
// inspector. A kind identifies syntax only; behavioral interpretation belongs
// to later detector tasks.
type ManifestKind string

const (
	KindPackageJSON   ManifestKind = "package_json"
	KindPackageLock   ManifestKind = "package_lock_json"
	KindNPMShrinkwrap ManifestKind = "npm_shrinkwrap_json"
	KindYarnLock      ManifestKind = "yarn_lock"
	KindPNPMLock      ManifestKind = "pnpm_lock"
	KindPyprojectTOML ManifestKind = "pyproject_toml"
	KindRequirements  ManifestKind = "python_requirements"
	KindPoetryLock    ManifestKind = "poetry_lock"
	KindUVLock        ManifestKind = "uv_lock"
	KindPipfile       ManifestKind = "pipfile"
	KindPipfileLock   ManifestKind = "pipfile_lock_json"
	KindPDMLock       ManifestKind = "pdm_lock"
)

// ErrUnsupportedPlatform reports a platform without a proven descriptor-based,
// no-follow manifest read primitive.
var ErrUnsupportedPlatform = errors.New("inspect: safe manifest reads are not supported on this platform")

// Manifest is one exact, bounded file from the immutable project snapshot.
// JSON kinds have passed duplicate-safe syntax and root-object validation.
// TOML, YAML, requirements, and line-oriented lock kinds remain opaque,
// NUL-free UTF-8 evidence for later detectors; this reader does not claim that
// their format syntax or package semantics are valid. Bytes returns a copy so
// callers cannot mutate evidence retained by the reader.
type Manifest struct {
	path   string
	kind   ManifestKind
	digest artifact.ContentDigest
	bytes  []byte
}

func (manifest Manifest) Path() string                   { return manifest.path }
func (manifest Manifest) Kind() ManifestKind             { return manifest.kind }
func (manifest Manifest) Digest() artifact.ContentDigest { return manifest.digest }
func (manifest Manifest) Bytes() []byte                  { return append([]byte(nil), manifest.bytes...) }

// ReadManifests reads only the closed Python and Node/TypeScript manifest set
// from a Phase 01 immutable snapshot. Non-JSON kinds are encoding-checked but
// intentionally not parsed. It never imports code, invokes a package manager,
// evaluates configuration, or follows a symlink or mount transition.
func ReadManifests(copied *snapshot.Snapshot) ([]Manifest, error) {
	if copied == nil {
		return nil, errors.New("inspect: nil project snapshot")
	}
	type candidate struct {
		file snapshot.File
		kind ManifestKind
	}
	candidates := make([]candidate, 0)
	seen := make(map[string]struct{})
	var total int64
	for _, file := range copied.Files() {
		kind, matched := manifestKind(file.Path)
		if !matched {
			continue
		}
		if err := validateManifestPath(file.Path); err != nil {
			return nil, err
		}
		if _, duplicate := seen[file.Path]; duplicate {
			return nil, fmt.Errorf("inspect: duplicate manifest path %q", file.Path)
		}
		seen[file.Path] = struct{}{}
		if file.Size < 0 || file.Size > maxManifestBytes {
			return nil, fmt.Errorf("inspect: manifest %q exceeds %d bytes", file.Path, maxManifestBytes)
		}
		if file.Size > maxManifestTotalBytes-total {
			return nil, fmt.Errorf("inspect: manifest bytes exceed %d total", maxManifestTotalBytes)
		}
		total += file.Size
		candidates = append(candidates, candidate{file: file, kind: kind})
		if len(candidates) > maxManifestCount {
			return nil, fmt.Errorf("inspect: manifest count exceeds %d", maxManifestCount)
		}
	}
	sort.Slice(candidates, func(left, right int) bool { return candidates[left].file.Path < candidates[right].file.Path })
	result := make([]Manifest, 0, len(candidates))
	for _, current := range candidates {
		content, err := readManifestFile(copied.Root(), current.file.Path, current.file.Size)
		if err != nil {
			return nil, fmt.Errorf("inspect: read manifest %q: %w", current.file.Path, err)
		}
		if got := artifact.DigestBytes(content); got != current.file.Digest {
			return nil, fmt.Errorf("inspect: manifest %q changed after snapshot", current.file.Path)
		}
		if err := validateManifestContent(current.kind, content); err != nil {
			return nil, fmt.Errorf("inspect: invalid %s %q: %w", current.kind, current.file.Path, err)
		}
		result = append(result, Manifest{
			path: current.file.Path, kind: current.kind, digest: current.file.Digest,
			bytes: append([]byte(nil), content...),
		})
	}
	return result, nil
}

func readExactManifest(file io.Reader, expected int64) ([]byte, error) {
	content, err := io.ReadAll(io.LimitReader(file, expected+1))
	if err != nil {
		return nil, err
	}
	if int64(len(content)) != expected {
		return nil, fmt.Errorf("file length changed: got %d, want %d", len(content), expected)
	}
	return content, nil
}

func validateManifestContent(kind ManifestKind, content []byte) error {
	if !utf8.Valid(content) || bytes.IndexByte(content, 0) >= 0 {
		return errors.New("content is not valid NUL-free UTF-8")
	}
	switch kind {
	case KindPackageJSON, KindPackageLock, KindNPMShrinkwrap, KindPipfileLock:
		if jsonNestingDepth(content) > maxManifestJSONDepth {
			return fmt.Errorf("JSON manifest depth exceeds %d", maxManifestJSONDepth)
		}
		if _, err := artifact.CanonicalizeJSON(content); err != nil {
			return err
		}
		decoder := json.NewDecoder(bytes.NewReader(content))
		decoder.UseNumber()
		var object map[string]any
		if err := decoder.Decode(&object); err != nil {
			return err
		}
		if object == nil {
			return errors.New("JSON manifest root must be an object")
		}
	}
	return nil
}

func jsonNestingDepth(content []byte) int {
	depth := 0
	maximum := 0
	inString := false
	escaped := false
	for _, character := range content {
		if inString {
			if escaped {
				escaped = false
			} else if character == '\\' {
				escaped = true
			} else if character == '"' {
				inString = false
			}
			continue
		}
		switch character {
		case '"':
			inString = true
		case '{', '[':
			depth++
			if depth > maximum {
				maximum = depth
			}
		case '}', ']':
			depth--
		}
	}
	return maximum
}

func validateManifestPath(relative string) error {
	if !utf8.ValidString(relative) || utf8.RuneCountInString(relative) > maxManifestPathRunes ||
		relative == "" || relative == "." || path.IsAbs(relative) || path.Clean(relative) != relative ||
		strings.Contains(relative, "\\") || hasDrivePrefix(relative) {
		return fmt.Errorf("inspect: manifest path %q is outside the v1 path boundary", relative)
	}
	components := strings.Split(relative, "/")
	if len(components) > maxManifestPathDepth {
		return fmt.Errorf("inspect: manifest path %q exceeds depth %d", relative, maxManifestPathDepth)
	}
	for _, component := range components {
		if component == "" || component == "." || component == ".." {
			return fmt.Errorf("inspect: manifest path %q has an unsafe component", relative)
		}
		for _, character := range component {
			if character <= 0x1f || character == 0x7f {
				return fmt.Errorf("inspect: manifest path %q contains a control character", relative)
			}
		}
	}
	return nil
}

func hasDrivePrefix(value string) bool {
	return len(value) >= 2 && ((value[0] >= 'A' && value[0] <= 'Z') || (value[0] >= 'a' && value[0] <= 'z')) && value[1] == ':'
}

func manifestKind(relative string) (ManifestKind, bool) {
	base := path.Base(relative)
	switch base {
	case "package.json":
		return KindPackageJSON, true
	case "package-lock.json":
		return KindPackageLock, true
	case "npm-shrinkwrap.json":
		return KindNPMShrinkwrap, true
	case "yarn.lock":
		return KindYarnLock, true
	case "pnpm-lock.yaml":
		return KindPNPMLock, true
	case "pyproject.toml":
		return KindPyprojectTOML, true
	case "poetry.lock":
		return KindPoetryLock, true
	case "uv.lock":
		return KindUVLock, true
	case "Pipfile":
		return KindPipfile, true
	case "Pipfile.lock":
		return KindPipfileLock, true
	case "pdm.lock":
		return KindPDMLock, true
	}
	if base == "requirements.txt" || (strings.HasPrefix(base, "requirements-") && strings.HasSuffix(base, ".txt")) {
		return KindRequirements, true
	}
	for parent := path.Dir(relative); parent != "." && parent != "/"; parent = path.Dir(parent) {
		if path.Base(parent) == "requirements" && strings.HasSuffix(base, ".txt") {
			return KindRequirements, true
		}
	}
	return "", false
}
