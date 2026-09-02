package runfs

import (
	"errors"
	"os"

	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/artifact"
)

const (
	AuditPackSchema          = "openbox.audit-pack/v1"
	ObservationPackSchema    = "ai.openbox.project-observation/v1"
	SecurityReportPackSchema = "ai.openbox.project-security-report/v1"
)

// ManifestDiscriminator is a point-in-time, bounded selection token. It is not
// verification evidence: the selected verifier must independently reopen and
// validate the target before RecheckCommittedManifest compares this identity.
type ManifestDiscriminator struct {
	packSchema     string
	manifestDigest artifact.ContentDigest
	identity       os.FileInfo
}

func (discriminator ManifestDiscriminator) PackSchema() string {
	return discriminator.packSchema
}

// RecheckCommittedManifest rejects a root replacement or format change across
// verifier dispatch. It deliberately performs a fresh no-follow read.
func RecheckCommittedManifest(path string, selected ManifestDiscriminator) error {
	current, err := ReadCommittedManifestDiscriminator(path)
	if err != nil {
		return err
	}
	if selected.identity == nil || current.identity == nil ||
		!os.SameFile(selected.identity, current.identity) ||
		selected.packSchema != current.packSchema ||
		selected.manifestDigest != current.manifestDigest {
		return errors.New("runfs: committed pack changed during verifier dispatch")
	}
	return nil
}
