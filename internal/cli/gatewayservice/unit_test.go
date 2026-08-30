package gatewayservice

import (
	"encoding/xml"
	"os"
	"strconv"
	"strings"
	"testing"
)

// TestUnitStopTimeoutMatchesTheGracePeriod is the coordination control.
//
// http.Server.Shutdown never force-closes an ACTIVE connection, so whatever is
// still streaming when the gateway's --shutdown-grace expires is cut when the
// process exits. If the supervisor's stop timeout is SHORTER than that grace, the
// supervisor SIGKILLs first and the grace never gets used at all — the two numbers
// are only correct together, which is exactly the kind of pairing that drifts.
func TestUnitStopTimeoutMatchesTheGracePeriod(t *testing.T) {
	grace := strconv.Itoa(StopTimeout)

	plist := LaunchdPlist(t.TempDir(), "/usr/local/bin/openbox", "127.0.0.1:8788", "https://api.anthropic.com", false)
	if !strings.Contains(plist, "--shutdown-grace") || !strings.Contains(plist, grace+"s") {
		t.Errorf("plist does not pass --shutdown-grace %ss:\n%s", grace, plist)
	}
	// launchd defaults to 20s, BELOW the 30s grace, so it must be set explicitly
	// or a routine restart mid-stream is killed before it finishes draining.
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

// TestUnitsUseFlagsThatExist is the guard for the defect that would have broken
// every boot. Phase 07's plan documented the unit as running
// `openbox gateway --config <path>`; no such flag exists, so flag parsing would
// reject it and the supervised gateway would fail to start forever.
func TestUnitsUseFlagsThatExist(t *testing.T) {
	// The real flag set, as registered by cmd/openbox/gateway.go.
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

// TestLaunchdPlistIsWellFormedXML — an unescaped path produces a plist launchd
// silently refuses to load, which presents as "the gateway never starts" with no
// error anywhere.
func TestLaunchdPlistIsWellFormedXML(t *testing.T) {
	// A path with characters that MUST be escaped.
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

// TestSupervisorRestartsACrashedGateway pins the availability story. Supervision
// is the whole answer to "the gateway died"; without keep-alive the failure is
// permanent for the session.
func TestSupervisorRestartsACrashedGateway(t *testing.T) {
	plist := LaunchdPlist(t.TempDir(), "/bin/openbox", "127.0.0.1:8788", "https://x", false)
	if !strings.Contains(plist, "<key>KeepAlive</key>") || !strings.Contains(plist, "<key>RunAtLoad</key>") {
		t.Errorf("plist does not keep the gateway alive across crashes or logins:\n%s", plist)
	}
	unit := SystemdUnit("/bin/openbox", "127.0.0.1:8788", "https://x", false)
	if !strings.Contains(unit, "Restart=always") {
		t.Errorf("systemd unit does not restart on crash:\n%s", unit)
	}
	// A USER unit, not a system one: the base tier is user-owned by definition.
	if !strings.Contains(unit, "WantedBy=default.target") {
		t.Errorf("systemd unit is not a user unit:\n%s", unit)
	}
}

// TestWriteAndRemoveUnitRoundTrip covers the install/uninstall pair on both real
// targets, and the deferred one.
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
			// Removing twice must not error.
			if again, err := RemoveUnit(goos, home); err != nil || again != "" {
				t.Errorf("second RemoveUnit: %q, %v", again, err)
			}
		})
	}
}

// TestWindowsIsRefusedNotSilentlySkipped — a caller that believes it installed a
// service and did not is worse off than one told plainly. Windows daemon packaging
// is deferred (requirement 7), and the error says what to do instead.
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
//
// launchd.plist(5) defaults StandardOutPath and StandardErrorPath to /dev/null,
// and the systemd sibling gets the journal for free — so on macOS every line the
// gateway wrote to stderr was discarded. Those lines are not decoration: the
// emitter's two throttled warnings ("no developer DID configured, so relayed
// model calls are NOT being recorded"; no session header) are the only signal
// that a perfectly working relay is recording nothing, and doctor does not ask
// that question at all.
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

// TestBothUnitsCarryVerboseOnlyWhenAsked. The supervised daemon owns the port, so
// a platform whose unit cannot carry --verbose leaves a developer there with no
// way to see whether calls are flowing at all — and the two renderers are separate
// functions, so that gap would be invisible until someone tried to debug the one
// that does not log.
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
