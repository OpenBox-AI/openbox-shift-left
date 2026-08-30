package gatewayservice

import (
	"encoding/xml"
	"os"
	"strconv"
	"strings"
	"testing"
)

// TestUnitStopTimeoutMatchesTheGracePeriod is the coordination control.
// Http.Server.Shutdown never force-closes an active connection, so whatever is
// still streaming when the gateway's --shutdown-grace expires is cut when the
// process exits.
func TestUnitStopTimeoutMatchesTheGracePeriod(t *testing.T) {
	grace := strconv.Itoa(StopTimeout)

	plist := LaunchdPlist(t.TempDir(), "/usr/local/bin/openbox", "127.0.0.1:8788", "https://api.anthropic.com", false)
	if !strings.Contains(plist, "--shutdown-grace") || !strings.Contains(plist, grace+"s") {
		t.Errorf("plist does not pass --shutdown-grace %ss:\n%s", grace, plist)
	}
	if !strings.Contains(plist, "<key>ExitTimeOut</key>") || !strings.Contains(plist, "<integer>"+grace+"</integer>") {
		t.Errorf("plist does not raise ExitTimeOut to %s; launchd's 20s default would SIGKILL mid-drain:\n%s", grace, plist)
	}

	unit := SystemdUnit("/usr/local/bin/openbox", "127.0.0.1:8788", "https://api.anthropic.com", false)
	if !strings.Contains(unit, "TimeoutStopSec="+grace) {
		t.Errorf("systemd unit does not set TimeoutStopSec=%s:\n%s", grace, unit)
	}
	if !strings.Contains(unit, "--shutdown-grace "+grace+"s") {
		t.Errorf("systemd ExecStart does not pass the matching grace:\n%s", unit)
	}
}

// TestUnitsUseFlagsThatExist is the guard for the defect that would have
// broken every boot.
func TestUnitsUseFlagsThatExist(t *testing.T) {
	valid := map[string]bool{"--addr": true, "--upstream": true, "--shutdown-grace": true}

	for name, body := range map[string]string{
		"launchd": LaunchdPlist(t.TempDir(), "/bin/openbox", "127.0.0.1:8788", "https://api.anthropic.com", false),
		"systemd": SystemdUnit("/bin/openbox", "127.0.0.1:8788", "https://api.anthropic.com", false),
	} {
		for _, field := range strings.Fields(strings.NewReplacer("<string>", " ", "</string>", " ").Replace(body)) {
			if strings.HasPrefix(field, "--") && !valid[field] {
				t.Errorf("%s unit passes %q, which `openbox gateway` does not define — it would fail to start on every boot", name, field)
			}
		}
		if strings.Contains(body, "--config") {
			t.Errorf("%s unit uses --config, the flag that does not exist", name)
		}
	}
}

// TestLaunchdPlistIsWellFormedXML; an unescaped path produces a plist launchd
// silently refuses to load, which presents as "the gateway never starts" with
// no error anywhere.
func TestLaunchdPlistIsWellFormedXML(t *testing.T) {
	plist := LaunchdPlist(t.TempDir(), `/Users/a&b/<tools>/openbox`, "127.0.0.1:8788", "https://api.anthropic.com", false)

	var doc any
	if err := xml.Unmarshal([]byte(plist), &doc); err != nil {
		t.Fatalf("plist is not well-formed XML: %v\n%s", err, plist)
	}
	if strings.Contains(plist, "/Users/a&b/") {
		t.Error("'&' was not escaped; launchd would refuse this plist")
	}
	if !strings.Contains(plist, "&amp;") || !strings.Contains(plist, "&lt;tools&gt;") {
		t.Errorf("path was not escaped:\n%s", plist)
	}
}

// TestSupervisorRestartsACrashedGateway pins the availability story.
func TestSupervisorRestartsACrashedGateway(t *testing.T) {
	plist := LaunchdPlist(t.TempDir(), "/bin/openbox", "127.0.0.1:8788", "https://x", false)
	if !strings.Contains(plist, "<key>KeepAlive</key>") || !strings.Contains(plist, "<key>RunAtLoad</key>") {
		t.Errorf("plist does not keep the gateway alive across crashes or logins:\n%s", plist)
	}
	unit := SystemdUnit("/bin/openbox", "127.0.0.1:8788", "https://x", false)
	if !strings.Contains(unit, "Restart=always") {
		t.Errorf("systemd unit does not restart on crash:\n%s", unit)
	}
	if !strings.Contains(unit, "WantedBy=default.target") {
		t.Errorf("systemd unit is not a user unit:\n%s", unit)
	}
}

// TestWriteAndRemoveUnitRoundTrip covers the install/uninstall pair on both
// real targets, and the deferred one.
func TestWriteAndRemoveUnitRoundTrip(t *testing.T) {
	for _, goos := range []string{"darwin", "linux"} {
		t.Run(goos, func(t *testing.T) {
			home := t.TempDir()
			path, err := WriteUnit(goos, home, "/bin/openbox", "127.0.0.1:8788", "https://api.anthropic.com", false)
			if err != nil {
				t.Fatalf("WriteUnit: %v", err)
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("unit not written: %v", err)
			}
			removed, err := RemoveUnit(goos, home)
			if err != nil {
				t.Fatalf("RemoveUnit: %v", err)
			}
			if removed != path {
				t.Errorf("RemoveUnit reported %q want %q", removed, path)
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Error("unit survived removal")
			}
			if again, err := RemoveUnit(goos, home); err != nil || again != "" {
				t.Errorf("second RemoveUnit: %q, %v", again, err)
			}
		})
	}
}

// TestWindowsIsRefusedNotSilentlySkipped; a caller that believes it installed
// a service and did not is worse off than one told plainly.
func TestWindowsIsRefusedNotSilentlySkipped(t *testing.T) {
	_, err := WriteUnit("windows", t.TempDir(), "openbox.exe", "127.0.0.1:8788", "https://x", false)
	if err == nil {
		t.Fatal("WriteUnit claimed success on windows, where packaging is deferred")
	}
	if !strings.Contains(err.Error(), "openbox gateway") {
		t.Errorf("the error does not say what to do instead: %v", err)
	}
}

// TestLaunchdUnitCapturesStdio is the visibility control for the platform that
// actually runs the daemon.
func TestLaunchdUnitCapturesStdio(t *testing.T) {
	home := t.TempDir()
	plist := LaunchdPlist(home, "/bin/openbox", "127.0.0.1:8788", "https://api.anthropic.com", false)

	for _, key := range []string{"StandardOutPath", "StandardErrorPath"} {
		if !strings.Contains(plist, "<key>"+key+"</key>") {
			t.Errorf("the plist sets no %s, so launchd sends it to /dev/null and the "+
				"gateway's only 'I am recording nothing' warning is lost", key)
		}
	}
	if !strings.Contains(plist, LogPath(home)) {
		t.Errorf("the plist does not name %s:\n%s", LogPath(home), plist)
	}
}

// TestBothUnitsCarryVerboseOnlyWhenAsked.
func TestBothUnitsCarryVerboseOnlyWhenAsked(t *testing.T) {
	for _, tc := range []struct {
		name string
		off  string
		on   string
	}{
		{
			name: "launchd",
			off:  LaunchdPlist(t.TempDir(), "/bin/openbox", "127.0.0.1:8788", "https://api.anthropic.com", false),
			on:   LaunchdPlist(t.TempDir(), "/bin/openbox", "127.0.0.1:8788", "https://api.anthropic.com", true),
		},
		{
			name: "systemd",
			off:  SystemdUnit("/bin/openbox", "127.0.0.1:8788", "https://api.anthropic.com", false),
			on:   SystemdUnit("/bin/openbox", "127.0.0.1:8788", "https://api.anthropic.com", true),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if strings.Contains(tc.off, verboseFlag) {
				t.Errorf("%s unit carries %s when it was not asked for:\n%s", tc.name, verboseFlag, tc.off)
			}
			if !strings.Contains(tc.on, verboseFlag) {
				t.Errorf("%s unit cannot carry %s, so a supervised gateway there can never report whether calls arrive:\n%s",
					tc.name, verboseFlag, tc.on)
			}
		})
	}
}
