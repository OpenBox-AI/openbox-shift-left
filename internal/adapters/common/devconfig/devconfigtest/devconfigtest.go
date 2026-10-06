// Package devconfigtest holds fixtures for tests that need a store shape the
// product no longer writes. It is test-only: TestDevconfigtestStaysTestOnly
// fails if a non-test file anywhere in the repo imports it.
package devconfigtest

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
)

// SetLegacyDID plants a developer_did on the dev config at path, simulating a
// pre-IAMv3 store. Nothing in the product writes one any more (a v3 identity
// never has one, and WriteWorkloadIdentity clears it), so exercising legacy
// detection needs a writer of its own.
func SetLegacyDID(path, did string) error {
	cfg, err := devconfig.Load(path)
	if err != nil {
		return err
	}
	cfg.DID = did
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("devconfigtest: marshal: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("devconfigtest: create dir: %w", err)
	}
	return os.WriteFile(path, append(raw, '\n'), 0o600)
}
