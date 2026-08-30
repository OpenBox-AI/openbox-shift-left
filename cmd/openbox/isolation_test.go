package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
)

// TestIsolateHomeRedirectsEveryPathInitWritesTo isolateHome is the only thing
// standing between `go test` and the developer's real machine, and each axis
// it covers was added because the previous version leaked through it:
//   - HOME: the installer resolves ~/.claude/plugins/openbox-observe from it
//     and copies the running engine into that bundle's bin/.
//   - The working directory: `init` defaults to project scope and takes the
//     project from cwd, which under `go test` is this package's own directory,
//     so a test run registered real hooks into the checked-out source tree.
func TestIsolateHomeRedirectsEveryPathInitWritesTo(t *testing.T) {
	realHome := os.Getenv("HOME")
	realWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("resolve working directory: %v", err)
	}

	t.Run("inside the isolation", func(t *testing.T) {
		obxHome := isolateHome(t)

		if got := os.Getenv("HOME"); got == realHome {
			t.Errorf("HOME was not redirected (%s): the installer would write the plugin bundle, "+
				"engine binary included, into the developer's real ~/.claude", got)
		}
		if got := os.Getenv(devconfig.EnvHome); got != obxHome {
			t.Errorf("OPENBOX_HOME = %q, want the returned dir %q", got, obxHome)
		}

		wd, err := os.Getwd()
		if err != nil {
			t.Fatalf("resolve working directory: %v", err)
		}
		if wd == realWD {
			t.Errorf("working directory was not redirected (%s): a project-scoped `init` "+
				"would register hooks into the source tree", wd)
		}

		if wd == obxHome || wd == os.Getenv("HOME") || os.Getenv("HOME") == obxHome {
			t.Errorf("isolation dirs overlap: cwd=%s HOME=%s OPENBOX_HOME=%s", wd, os.Getenv("HOME"), obxHome)
		}
	})

	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("resolve working directory: %v", err)
	}
	if wd != realWD {
		t.Errorf("working directory not restored: %s, want %s", wd, realWD)
	}
	if got := os.Getenv("HOME"); got != realHome {
		t.Errorf("HOME not restored: %s, want %s", got, realHome)
	}
}

// TestProjectScopedInitWritesNoSettingsIntoTheSourceTree a project-scoped
// `init` under isolateHome must leave its hook registrations in the temp cwd,
// not in the package directory this test binary runs from.
func TestProjectScopedInitWritesNoSettingsIntoTheSourceTree(t *testing.T) {
	pkgDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("resolve working directory: %v", err)
	}
	sourceSettings := filepath.Join(pkgDir, ".claude", "settings.local.json")

	isolateHome(t)
	seedCredentials(t)
	a, _, errb := testApp(nil)
	if code := a.run([]string{"init", "--provider", "claude-code"}); code != exitOK {
		t.Fatalf("init exit = %d; stderr=%q", code, errb.String())
	}

	isolatedWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("resolve working directory: %v", err)
	}
	if _, err := os.Stat(filepath.Join(isolatedWD, ".claude", "settings.local.json")); err != nil {
		t.Fatalf("init wrote no project settings anywhere: %v", err)
	}
	if _, err := os.Stat(sourceSettings); !os.IsNotExist(err) {
		t.Errorf("init wrote hook registrations into the source tree at %s (err=%v)", sourceSettings, err)
	}
}
