package main

import (
	"fmt"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
)

// migrateLegacyConfig carries dev.json / approver.json from the pre-that decision
// location into ~/.openbox/, and says so once when it does.
//
// Every command that WRITES config calls this first. The order matters: the
// config writers merge over whatever is already at the target path, so writing to
// a fresh ~/.openbox/dev.json while the user's real posture still sat in the
// legacy file would reset enforce, capture and the signing pins to defaults — a
// silent posture downgrade performed by a repair command.
//
// Read paths do not need it: they fall back to the legacy location on their own
// (devconfig.DefaultConfigPath), so an upgraded binary keeps governing a machine
// before anyone runs `auth` or `init`.
//
// Failure is reported and not fatal. A migration that cannot run leaves the
// legacy file readable, so the worst case is the command writing a fresh config —
// which is exactly what would happen if the user had never had one. Refusing to
// install over it would be a worse trade.
func (a *app) migrateLegacyConfig() {
	migrated, err := devconfig.MigrateLegacyConfig()
	if err != nil {
		fmt.Fprintf(a.stderr, "warning: could not migrate the previous config location: %v\n", err)
		return
	}
	if len(migrated) == 0 {
		return
	}
	devJSON, approverJSON, _ := devconfig.LegacyConfigPaths()
	home, _ := devconfig.Home()
	for _, name := range migrated {
		from := devJSON
		if name == "approver.json" {
			from = approverJSON
		}
		fmt.Fprintf(a.stdout, "Migrated %s → %s (the original is left in place).\n", from, home)
	}
}
