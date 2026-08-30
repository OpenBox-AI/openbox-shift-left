package hookflow

import (
	"path/filepath"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
)

// isolateConfig points config resolution at a nonexistent file under a temp
// dir, so a test reads defaults rather than the developer's real dev.json.
//
// It lived in staleness_test.go until ADR-0017 deleted the staleness check; it
// is a general helper that happened to be defined beside its first caller, so
// it moved here rather than disappearing with that file.
func isolateConfig(t *testing.T) {
	t.Helper()
	t.Setenv(devconfig.EnvConfigPath, filepath.Join(t.TempDir(), "none.json"))
}
