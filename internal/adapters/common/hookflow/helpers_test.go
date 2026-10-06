package hookflow

import (
	"path/filepath"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
)

func isolateConfig(t *testing.T) {
	t.Helper()
	t.Setenv(devconfig.EnvConfigPath, filepath.Join(t.TempDir(), "none.json"))
}
