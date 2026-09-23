package devconfig

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// setLegacyDID plants a developer_did on the config at path, simulating a
// pre-IAMv3 store. Tests outside this package use devconfigtest.SetLegacyDID,
// which cannot be imported here without a cycle.
func setLegacyDID(path, did string) error {
	cfg, err := Load(path)
	if err != nil {
		return err
	}
	cfg.DID = did
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, append(raw, '\n'), 0o600)
}
