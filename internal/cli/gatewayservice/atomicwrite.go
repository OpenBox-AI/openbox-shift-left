package gatewayservice

import (
	"os"

	"github.com/openbox-ai/openbox-shift-left/internal/cli/atomicfile"
)

// writeFileAtomic delegates to the one atomic writer in this module.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	return atomicfile.Write(path, data, perm)
}
