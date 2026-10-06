package activation

import (
	"os"
	"testing"
)

// TestMain pins currentGOOS to darwin for the whole package. Most tests here
// drive the macOS sequence through a fake Runner and are plain Go, so they
// must exercise that arm on every host, Linux CI included; the unsupported-OS
// tests override it per test through withGOOS.
func TestMain(m *testing.M) {
	currentGOOS = "darwin"
	os.Exit(m.Run())
}
