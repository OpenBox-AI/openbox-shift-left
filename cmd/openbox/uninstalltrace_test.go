package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/trace"
)

// TestUninstallLeavesTraceDirByDefault: the trace directory documents the
// developer's own history and survives every OTHER surface uninstall
// reverses, so an ordinary run must not touch it at all.
func TestUninstallLeavesTraceDirByDefault(t *testing.T) {
	home := t.TempDir()
	traceDir := t.TempDir()
	restore := trace.SetDefault(&trace.Writer{Dir: traceDir})
	defer restore()
	if err := os.WriteFile(filepath.Join(traceDir, "trace-2026-09-27.jsonl"), []byte(`{"stage":"log"}`+"\n"), 0o600); err != nil {
		t.Fatalf("seeding trace file: %v", err)
	}

	a, out, _ := testApp(map[string]string{"HOME": home, devconfig.EnvHome: filepath.Join(home, ".openbox")})
	if code := a.runUninstall(nil); code != exitOK {
		t.Fatalf("runUninstall(nil) = %d:\n%s", code, out.String())
	}

	if _, err := os.Stat(traceDir); err != nil {
		t.Fatalf("trace directory did not survive an ordinary uninstall: %v", err)
	}
	if strings.Contains(out.String(), "purged") {
		t.Errorf("an ordinary uninstall reported purging the trace, want it untouched:\n%s", out.String())
	}
}

// TestUninstallPurgeTraceRemovesIt: --purge-trace is the one flag that
// destroys the trace directory, and it must actually be gone afterward.
func TestUninstallPurgeTraceRemovesIt(t *testing.T) {
	home := t.TempDir()
	traceDir := t.TempDir()
	restore := trace.SetDefault(&trace.Writer{Dir: traceDir})
	defer restore()
	if err := os.WriteFile(filepath.Join(traceDir, "trace-2026-09-27.jsonl"), []byte(`{"stage":"log"}`+"\n"), 0o600); err != nil {
		t.Fatalf("seeding trace file: %v", err)
	}

	a, out, _ := testApp(map[string]string{"HOME": home, devconfig.EnvHome: filepath.Join(home, ".openbox")})
	if code := a.runUninstall([]string{"--purge-trace"}); code != exitOK {
		t.Fatalf("runUninstall(--purge-trace) = %d:\n%s", code, out.String())
	}

	if _, err := os.Stat(traceDir); !os.IsNotExist(err) {
		t.Fatalf("trace directory survived --purge-trace: err=%v", err)
	}
	if !strings.Contains(out.String(), "purged") {
		t.Errorf("--purge-trace did not report purging the trace:\n%s", out.String())
	}
}
