package devconfig_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAdaptersNeverReadApproverConfig the hook path must never be able to
// reach the approver's config.
func TestAdaptersNeverReadApproverConfig(t *testing.T) {
	root := filepath.Join("..", "..") // adapters/
	forbidden := []string{"ApproverConfig", "DefaultApproverConfigPath", "approver.json", "RoleApprover"}

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") {
			return err
		}
		if strings.Contains(filepath.ToSlash(path), "common/devconfig/") {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, name := range forbidden {
			if strings.Contains(string(raw), name) {
				t.Errorf("%s references %q — the approver config must stay out of the hook path; "+
					"if a hook needs this value, it belongs in dev.json", path, name)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk adapters/: %v", err)
	}
}
