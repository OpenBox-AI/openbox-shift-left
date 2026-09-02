package runfs

import (
	"errors"
	"fmt"
	"os"

	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/artifact"
)

const (
	maxVerifiedManifestBytes = 1 << 20
	maxVerifiedObjectBytes   = 16 << 20
	maxVerifiedPackBytes     = 128 << 20
)

// VerifiedPack is an immutable in-memory view of a structurally valid,
// content-addressed pack. Public-schema validation is a separate boundary.
type VerifiedPack struct {
	manifest []byte
	digest   artifact.ContentDigest
	objects  map[artifact.Role][]byte
}

func (pack *VerifiedPack) Digest() artifact.ContentDigest {
	if pack == nil {
		return artifact.ContentDigest{}
	}
	return pack.digest
}

func (pack *VerifiedPack) Manifest() []byte {
	if pack == nil {
		return nil
	}
	return append([]byte(nil), pack.manifest...)
}

func (pack *VerifiedPack) Object(role artifact.Role) ([]byte, bool) {
	if pack == nil {
		return nil, false
	}
	content, ok := pack.objects[role]
	return append([]byte(nil), content...), ok
}

func (pack *VerifiedPack) RoleCount() int {
	if pack == nil {
		return 0
	}
	return len(pack.objects)
}

// VerifyPack reopens a filesystem-committed root without following its final
// component, requires the exact v1 directory/object set, and rechecks every
// addressed byte length, content ID, and role encoding. It never repairs data.
func VerifyPack(root string) (*VerifiedPack, error) {
	if err := ensureSupportedPlatform(); err != nil {
		return nil, err
	}
	clean, err := validateRoot(root)
	if err != nil {
		return nil, err
	}
	state, err := Inspect(clean)
	if err != nil {
		return nil, err
	}
	if state != StateManifestCommitted {
		return nil, fmt.Errorf("runfs: cannot verify pack in %s state", state)
	}
	identity, err := validateDirectoryModes(clean, 0o500)
	if err != nil {
		return nil, err
	}
	opened, err := openOwnedRoot(clean, identity)
	if err != nil {
		return nil, fmt.Errorf("runfs: open committed pack: %w", err)
	}
	defer opened.Close()
	index, objects, err := readCommittedPackAt(opened)
	if err != nil {
		return nil, fmt.Errorf("runfs: verify committed pack: %w", err)
	}
	if index == nil || len(objects) == 0 {
		return nil, errors.New("runfs: verified pack is empty")
	}
	current, err := opened.Stat()
	if err != nil || !os.SameFile(identity, current) {
		if err != nil {
			return nil, err
		}
		return nil, errors.New("runfs: committed pack identity changed while verifying")
	}
	return &VerifiedPack{manifest: index.Manifest(), digest: index.Digest(), objects: objects}, nil
}
