package gatewayservice

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"text/template"
)

// TestSuppliedTemplatesSurviveRendering the library renders a supplied
// template through text/template before writing it (service_darwin.go,
// service_systemd_linux.go both call `t.render(data, …)`).
func TestSuppliedTemplatesSurviveRendering(t *testing.T) {
	home := t.TempDir()
	for name, body := range map[string]string{
		"launchd": LaunchdPlist(home, "/usr/local/bin/openbox", "127.0.0.1:8788", "https://api.anthropic.com", false),
		"launchd verbose": LaunchdPlist(home, "/usr/local/bin/openbox", "127.0.0.1:8788",
			"https://api.anthropic.com", true),
		"systemd": SystemdUnit("/usr/local/bin/openbox", "127.0.0.1:8788", "https://api.anthropic.com", false),
	} {
		t.Run(name, func(t *testing.T) {
			if strings.Contains(body, "{{") {
				t.Fatalf("the %s body contains a template action, so the library's render "+
					"would rewrite it: %q", name, body)
			}
			tmpl, err := template.New(name).Parse(body)
			if err != nil {
				t.Fatalf("the library could not parse the %s body as a template: %v", name, err)
			}
			var out strings.Builder
			if err := tmpl.Execute(&out, map[string]any{"Name": "ignored"}); err != nil {
				t.Fatalf("rendering the %s body failed: %v", name, err)
			}
			if out.String() != body {
				t.Errorf("the %s body is not render-stable.\n got: %q\nwant: %q", name, out.String(), body)
			}
		})
	}
}

// TestServiceNameMatchesTheRepoPaths the library's Name must produce exactly
// the paths this repo already uses, on both platforms.
func TestServiceNameMatchesTheRepoPaths(t *testing.T) {
	home := t.TempDir()

	darwinName, err := serviceName("darwin")
	if err != nil {
		t.Fatalf("darwin: %v", err)
	}
	if want := filepath.Join(home, "Library", "LaunchAgents", darwinName+".plist"); want != LaunchdPath(home) {
		t.Errorf("library would write %q, LaunchdPath says %q", want, LaunchdPath(home))
	}

	linuxName, err := serviceName("linux")
	if err != nil {
		t.Fatalf("linux: %v", err)
	}
	if want := filepath.Join(home, ".config", "systemd", "user", linuxName+".service"); want != SystemdPath(home) {
		t.Errorf("library would write %q, SystemdPath says %q", want, SystemdPath(home))
	}
	if linuxName+".service" != SystemdUnitName {
		t.Errorf("unit name %q does not match SystemdUnitName %q — `systemctl --user enable` "+
			"names the latter", linuxName+".service", SystemdUnitName)
	}
}

// TestUnsupportedPlatformIsAnError an unsupported platform is an error, not a
// silent no-op: a developer who believes a service was installed and finds
// none later is worse off than one told plainly.
func TestUnsupportedPlatformIsAnError(t *testing.T) {
	if _, err := New("windows", t.TempDir(), "openbox.exe", "127.0.0.1:8788", "https://x", false); err == nil {
		t.Fatal("expected an error for windows")
	}
	if err := Install("plan9", t.TempDir(), "openbox", "127.0.0.1:8788", "https://x", false); err == nil {
		t.Fatal("expected an error for plan9")
	}
}

// TestUninstallIsIdempotent uninstalling when nothing is installed is success;
// `--remove-gateway` has to be safe on a machine that never had a gateway, and
// it runs before the credential gate precisely so a wiped ~/.openbox can still
// be backed out.
func TestUninstallIsIdempotent(t *testing.T) {
	if !IsNotInstalled(nil) {
		t.Error("nil must read as not-installed")
	}
	for _, msg := range []string{
		"open /x/y.plist: no such file or directory",
		"Init already exists: /x/y",
	} {
		got := IsNotInstalled(&stringError{msg})
		want := strings.Contains(msg, "no such file")
		if got != want {
			t.Errorf("IsNotInstalled(%q) = %v, want %v", msg, got, want)
		}
	}
}

type stringError struct{ s string }

func (e *stringError) Error() string { return e.s }

// TestRealInstallWritesTheExpectedArtifact requirement 2, the artifact
// assertion; OPT-IN, and here is why. Run it deliberately:
func TestRealInstallWritesTheExpectedArtifact(t *testing.T) {
	if os.Getenv("OPENBOX_TEST_REAL_SERVICE_INSTALL") != "1" {
		t.Skip("opt-in: writes a real unit into your home directory. " +
			"Set OPENBOX_TEST_REAL_SERVICE_INSTALL=1 to run.")
	}
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skipf("no daemon packaging for %s", runtime.GOOS)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("resolving home: %v", err)
	}
	unitPath := UnitPath(runtime.GOOS, home)
	if _, err := os.Stat(unitPath); err == nil {
		t.Skipf("%s already exists — refusing to disturb a real install", unitPath)
	}

	if err := Reinstall(runtime.GOOS, home, "/usr/local/bin/openbox",
		"127.0.0.1:8788", "https://api.anthropic.com", true); err != nil {
		t.Fatalf("Reinstall: %v", err)
	}
	t.Cleanup(func() { _ = Uninstall(runtime.GOOS, home) })

	raw, err := os.ReadFile(unitPath)
	if err != nil {
		t.Fatalf("the library reported success but %s is not readable: %v", unitPath, err)
	}
	got := string(raw)

	if runtime.GOOS == "darwin" {
		logPath := LogPath(home)
		for _, key := range []string{"StandardOutPath", "StandardErrorPath"} {
			if !strings.Contains(got, "<key>"+key+"</key>") {
				t.Errorf("%s missing from the written plist", key)
			}
		}
		// This is the requirement the library's own LogDirectory option cannot
		// express: it hardcodes <Name>.out.log and <Name>.err.log.
		if strings.Count(got, logPath) != 2 {
			t.Errorf("expected both log keys to name %s; plist:\n%s", logPath, got)
		}
		for _, want := range []string{
			"<string>" + LaunchdLabel + "</string>",
			"<key>KeepAlive</key>", "<key>RunAtLoad</key>", "<key>ExitTimeOut</key>",
			"<string>gateway</string>", "<string>" + verboseFlag + "</string>",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("written plist missing %q", want)
			}
		}
	} else {
		for _, want := range []string{"ExecStart=", "Restart=always", "TimeoutStopSec="} {
			if !strings.Contains(got, want) {
				t.Errorf("written unit missing %q", want)
			}
		}
	}

	if err := Uninstall(runtime.GOOS, home); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if _, err := os.Stat(unitPath); !os.IsNotExist(err) {
		t.Errorf("%s survived Uninstall (err=%v)", unitPath, err)
	}
}
