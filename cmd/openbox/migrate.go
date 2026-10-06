package main

import (
	"fmt"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
)

// migrateLegacyConfig read paths do not need it: they fall back to the legacy
// location on their own (devconfig.DefaultConfigPath), so an upgraded binary
// keeps governing a machine before anyone runs `auth` or `init`.
func (a *app) migrateLegacyConfig() {
	migrated, err := devconfig.MigrateLegacyConfig()
	if err != nil {
		fmt.Fprintf(a.stderr, "warning: could not migrate the previous config location: %v\n", err)
		return
	}
	if len(migrated) == 0 {
		return
	}
	// One migratable file, so there is nothing to select between.
	devJSON, _ := devconfig.LegacyConfigPaths()
	home, _ := devconfig.Home()
	fmt.Fprintf(a.stdout, "Migrated %s → %s (the original is left in place).\n", devJSON, home)
}
