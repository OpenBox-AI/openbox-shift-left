//go:build !darwin && !linux

package runfs

func ReadCommittedManifestDiscriminator(string) (ManifestDiscriminator, error) {
	return ManifestDiscriminator{}, ensureSupportedPlatform()
}
