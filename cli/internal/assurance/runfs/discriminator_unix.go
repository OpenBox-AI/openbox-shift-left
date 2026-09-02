//go:build darwin || linux

package runfs

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/artifact"
)

const (
	observationManifestSchema    = "ai.openbox.project-observation.manifest/v1"
	securityReportManifestSchema = "ai.openbox.project-security-report.manifest/v1"
)

// ReadCommittedManifestDiscriminator reads only the private committed
// manifest through a bounded no-follow descriptor. It does not trust this read
// as pack verification and never selects from filenames or payload contents.
func ReadCommittedManifestDiscriminator(path string) (ManifestDiscriminator, error) {
	var result ManifestDiscriminator
	if err := ensureSupportedPlatform(); err != nil {
		return result, err
	}
	clean, err := validateRoot(path)
	if err != nil {
		return result, err
	}
	identity, err := validateDirectoryModes(clean, 0o500)
	if err != nil {
		return result, err
	}
	state, err := Inspect(clean)
	if err != nil {
		return result, err
	}
	if state != StateManifestCommitted {
		return result, fmt.Errorf("runfs: cannot inspect manifest in %s state", state)
	}
	root, err := openOwnedRoot(clean, identity)
	if err != nil {
		return result, fmt.Errorf("runfs: open committed manifest: %w", err)
	}
	defer root.Close()
	manifest, _, err := readRegularAt(root, ManifestName, 0o400, maxVerifiedManifestBytes)
	if err != nil {
		return result, fmt.Errorf("runfs: read committed manifest: %w", err)
	}
	if _, err := artifact.CanonicalizeJSON(manifest); err != nil {
		return result, fmt.Errorf("runfs: decode committed manifest discriminator: %w", err)
	}
	var fields struct {
		APIVersion *string `json:"apiVersion"`
		Kind       *string `json:"kind"`
		Schema     *string `json:"schema"`
		PackSchema *string `json:"pack_schema"`
	}
	if err := json.Unmarshal(manifest, &fields); err != nil {
		return result, fmt.Errorf("runfs: decode committed manifest discriminator: %w", err)
	}
	switch {
	case fields.APIVersion != nil && *fields.APIVersion == AuditPackSchema && fields.Kind != nil && *fields.Kind == "AuditPack" && fields.Schema == nil && fields.PackSchema == nil:
		result.packSchema = AuditPackSchema
	case fields.APIVersion == nil && fields.Kind == nil && fields.Schema != nil && *fields.Schema == observationManifestSchema && fields.PackSchema != nil && *fields.PackSchema == ObservationPackSchema:
		result.packSchema = ObservationPackSchema
	case fields.APIVersion == nil && fields.Kind == nil && fields.Schema != nil && *fields.Schema == securityReportManifestSchema && fields.PackSchema != nil && *fields.PackSchema == SecurityReportPackSchema:
		result.packSchema = SecurityReportPackSchema
	default:
		return result, errors.New("runfs: committed manifest has an unknown or ambiguous pack discriminator")
	}
	current, err := os.Lstat(clean)
	if err != nil || !os.SameFile(identity, current) {
		if err != nil {
			return result, err
		}
		return result, errors.New("runfs: committed pack root changed while reading discriminator")
	}
	result.manifestDigest = artifact.DigestBytes(manifest)
	result.identity = current
	return result, nil
}
