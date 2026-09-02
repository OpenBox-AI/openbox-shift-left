//go:build darwin || linux

package runfs

import (
	"errors"
	"fmt"
	"os"
)

// ReadObservation reads only a finalized exact-file observation transaction.
// It rejects widened modes, links, replacement, omissions, and extras.
func ReadObservation(path string) (map[string][]byte, []byte, error) {
	clean, err := validateRoot(path)
	if err != nil {
		return nil, nil, err
	}
	identity, err := validateDirectoryModes(clean, 0o500)
	if err != nil {
		return nil, nil, err
	}
	state, err := Inspect(clean)
	if err != nil || state != StateManifestCommitted {
		if err != nil {
			return nil, nil, err
		}
		return nil, nil, fmt.Errorf("runfs: observation reader refuses %s state", state)
	}
	root, err := openOwnedRoot(clean, identity)
	if err != nil {
		return nil, nil, err
	}
	defer root.Close()
	entries, err := readDirectoryBounded(root, len(observationPayloadNames)+1)
	if err != nil {
		return nil, nil, err
	}
	if len(entries) != len(observationPayloadNames)+1 {
		return nil, nil, errors.New("runfs: observation file set is not exact")
	}
	want := map[string]bool{ManifestName: true}
	for _, name := range observationPayloadNames {
		want[name] = true
	}
	for _, entry := range entries {
		if !want[entry.Name()] {
			return nil, nil, fmt.Errorf("runfs: unexpected observation entry %s", entry.Name())
		}
	}
	payloads := make(map[string][]byte, len(observationPayloadNames))
	for _, name := range observationPayloadNames {
		content, _, err := readRegularAt(root, name, 0o400, MaxObservationFileBytes+1)
		if err != nil {
			return nil, nil, err
		}
		payloads[name] = content
	}
	manifest, _, err := readRegularAt(root, ManifestName, 0o400, MaxObservationFileBytes+1)
	if err != nil {
		return nil, nil, err
	}
	current, err := os.Lstat(clean)
	if err != nil || !os.SameFile(identity, current) {
		return nil, nil, errors.New("runfs: observation root changed while reading")
	}
	return payloads, manifest, nil
}

const MaxObservationFileBytes int64 = 72 << 20
