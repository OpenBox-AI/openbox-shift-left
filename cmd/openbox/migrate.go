package main

import (
	"fmt"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
)

// migrateLegacyConfig carries dev.json / approver.json from the pre-that
// decision location into ~/.openbox/, and says so once when it does.
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
