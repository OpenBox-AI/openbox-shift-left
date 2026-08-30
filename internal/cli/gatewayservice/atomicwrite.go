package gatewayservice

import (
	"os"

	"github.com/openbox-ai/openbox-shift-left/internal/cli/atomicfile"
)

func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	return atomicfile.Write(path, data, perm)
}
