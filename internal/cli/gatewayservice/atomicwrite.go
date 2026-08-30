package gatewayservice

import (
	"os"

	"github.com/openbox-ai/openbox-shift-left/internal/cli/atomicfile"
)

// writeFileAtomic delegates to the one atomic writer in this module.
//
// It was a per-platform pair in this package until three lanes needed the same
// guarantee. The pair moved to cli/internal/atomicfile unchanged; this alias
// stays so the call sites and their reasoning — the settings file must never be
// left as an arbitrary prefix, because readSettings then refuses to repair it —
// read exactly as before.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	return atomicfile.Write(path, data, perm)
}
