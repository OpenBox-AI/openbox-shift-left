package laneservice

import (
	"encoding/xml"
	"os"
	"strconv"
	"strings"
	"testing"
	"text/template"
)

// allLanes is every supervised daemon, with the flag set its command actually
// registers. The flag lists are hand-written on purpose: deriving them from
// the Spec under test would make this agree with any spelling.
var allLanes = []struct {
	name  string
	spec  Spec
	flags map[string]bool
}{
	{
		name:  "gateway",
		spec:  Gateway("127.0.0.1:8788", "https://api.anthropic.com", false),
		flags: map[string]bool{"--addr": true, "--upstream": true, "--shutdown-grace": true, "--verbose": true},
	},
	{
		name:  "telemetry",
		spec:  Telemetry("127.0.0.1:8789", false),
		flags: map[string]bool{"--addr": true, "--shutdown-grace": true, "--verbose": true, "--elected": true},
	},
	{
		name:  "transport",
		spec:  Transport("127.0.0.1:8790", false),
		flags: map[string]bool{"--addr": true, "--shutdown-grace": true, "--verbose": true},
	},
}

// TestSpecsUseFlagsThatExist is the guard for the defect that breaks every
// boot.
func TestSpecsUseFlagsThatExist(t *testing.T) {
	for _, lane := range allLanes {
		t.Run(lane.name, func(t *testing.T) {
			bodies := map[string]string{
				"launchd": lane.spec.LaunchdPlist(t.TempDir(), "/bin/openbox"),
				"systemd": lane.spec.SystemdUnit("/bin/openbox"),
			}
			for platform, body := range bodies {
				plain := strings.NewReplacer("<string>", " ", "</string>", " ").Replace(body)
				for _, field := range strings.Fields(plain) {
					if strings.HasPrefix(field, "--") && !lane.flags[field] {
						t.Errorf("%s %s unit passes %q, which `openbox %s` does not define; it would fail to start on every boot",
							lane.name, platform, field, lane.name)
					}
				}
			}
		})
	}
}

// TestEveryLaneIsAddressableAndSeparable. Two lanes sharing a label, a unit
// name or a log file would have one install silently replace another's unit,
// or two daemons interleave lines in one file.
func TestEveryLaneIsAddressableAndSeparable(t *testing.T) {
	home := t.TempDir()
	seen := map[string]string{}
	for _, lane := range allLanes {
		for field, value := range map[string]string{
			"label":     lane.spec.Label,
			"systemd":   lane.spec.SystemdUnitName(),
			"log":       lane.spec.LogPath(home),
			"launchd":   lane.spec.LaunchdPath(home),
			"unit path": lane.spec.SystemdPath(home),
		} {
			key := field + "=" + value
			if value == "" {
				t.Errorf("%s lane has an empty %s", lane.name, field)
			}
			if other, dup := seen[key]; dup {
				t.Errorf("%s and %s share a %s (%q); one install would silently replace the other",
					lane.name, other, field, value)
			}
			seen[key] = lane.name
		}
	}
}

// TestEveryLaneCapturesStdio is the visibility control for the platform that
// actually runs these daemons.
func TestEveryLaneCapturesStdio(t *testing.T) {
	home := t.TempDir()
	for _, lane := range allLanes {
		plist := lane.spec.LaunchdPlist(home, "/bin/openbox")
		for _, key := range []string{"StandardOutPath", "StandardErrorPath"} {
			if !strings.Contains(plist, "<key>"+key+"</key>") {
				t.Errorf("%s: the plist sets no %s, so launchd sends it to /dev/null", lane.name, key)
			}
		}
		if !strings.Contains(plist, lane.spec.LogPath(home)) {
			t.Errorf("%s: the plist does not name %s", lane.name, lane.spec.LogPath(home))
		}
	}
}

// TestEveryLaneStopTimeoutMatchesItsGrace. Launchd defaults to 20s, below the
// 30s grace, so a routine restart mid-stream is killed before it finishes
// draining unless the unit says otherwise.
func TestEveryLaneStopTimeoutMatchesItsGrace(t *testing.T) {
	grace := strconv.Itoa(StopTimeout)
	for _, lane := range allLanes {
		plist := lane.spec.LaunchdPlist(t.TempDir(), "/bin/openbox")
		if !strings.Contains(plist, "--shutdown-grace") || !strings.Contains(plist, grace+"s") {
			t.Errorf("%s: plist does not pass --shutdown-grace %ss", lane.name, grace)
		}
		if !strings.Contains(plist, "<integer>"+grace+"</integer>") {
			t.Errorf("%s: plist does not raise ExitTimeOut to %s", lane.name, grace)
		}
		unit := lane.spec.SystemdUnit("/bin/openbox")
		if !strings.Contains(unit, "TimeoutStopSec="+grace) {
			t.Errorf("%s: systemd unit does not set TimeoutStopSec=%s", lane.name, grace)
		}
		if !strings.Contains(unit, "--shutdown-grace "+grace+"s") {
			t.Errorf("%s: systemd ExecStart does not pass the matching grace", lane.name)
		}
	}
}

// TestEveryLaneCarriesVerboseOnlyWhenAsked. The supervised daemon owns the
// port, so a platform whose unit cannot carry --verbose leaves a developer
// there with no way to see whether anything is flowing at all.
func TestEveryLaneCarriesVerboseOnlyWhenAsked(t *testing.T) {
	for _, tc := range []struct {
		name    string
		off, on Spec
	}{
		{"gateway", Gateway("127.0.0.1:8788", "https://x", false), Gateway("127.0.0.1:8788", "https://x", true)},
		{"telemetry", Telemetry("127.0.0.1:8789", false), Telemetry("127.0.0.1:8789", true)},
		{"transport", Transport("127.0.0.1:8790", false), Transport("127.0.0.1:8790", true)},
	} {
		home := t.TempDir()
		for platform, pair := range map[string][2]string{
			"launchd": {tc.off.LaunchdPlist(home, "/bin/openbox"), tc.on.LaunchdPlist(home, "/bin/openbox")},
			"systemd": {tc.off.SystemdUnit("/bin/openbox"), tc.on.SystemdUnit("/bin/openbox")},
		} {
			if strings.Contains(pair[0], VerboseFlag) {
				t.Errorf("%s %s carries %s when it was not asked for", tc.name, platform, VerboseFlag)
			}
			if !strings.Contains(pair[1], VerboseFlag) {
				t.Errorf("%s %s cannot carry %s, so a supervised daemon there can never report whether calls arrive",
					tc.name, platform, VerboseFlag)
			}
		}
	}
}

// TestPlistsAreWellFormedXML; an unescaped path produces a plist launchd
// silently refuses to load, which presents as "the daemon never starts" with
// no error anywhere.
func TestPlistsAreWellFormedXML(t *testing.T) {
	for _, lane := range allLanes {
		plist := lane.spec.LaunchdPlist(t.TempDir(), `/Users/a&b/<tools>/openbox`)
		var doc any
		if err := xml.Unmarshal([]byte(plist), &doc); err != nil {
			t.Errorf("%s: plist is not well-formed XML: %v\n%s", lane.name, err, plist)
		}
	}
}

// TestSystemdQuotingIsExplicitPerArgument.
func TestSystemdQuotingIsExplicitPerArgument(t *testing.T) {
	unit := Gateway("127.0.0.1:8788", "https://relay.example/%s/v1", false).
		SystemdUnit(`/Users/a b/bin/openbox`)
	if !strings.Contains(unit, `"/Users/a b/bin/openbox"`) {
		t.Errorf("a binary path with a space was not quoted:\n%s", unit)
	}
	if !strings.Contains(unit, "%%s") {
		t.Errorf("a '%%' in a value was not doubled, so systemd would expand it as a specifier:\n%s", unit)
	}
	if !strings.Contains(unit, " gateway --addr ") {
		t.Errorf("the subcommand or a flag name was quoted:\n%s", unit)
	}
}

// TestWriteAndRemoveUnitRoundTrip covers the install/uninstall pair on both
// real targets, for every lane.
func TestWriteAndRemoveUnitRoundTrip(t *testing.T) {
	for _, lane := range allLanes {
		for _, goos := range []string{"darwin", "linux"} {
			t.Run(lane.name+"/"+goos, func(t *testing.T) {
				home := t.TempDir()
				path, err := lane.spec.WriteUnit(goos, home, "/bin/openbox")
				if err != nil {
					t.Fatalf("WriteUnit: %v", err)
				}
				if _, err := os.Stat(path); err != nil {
					t.Fatalf("unit not written: %v", err)
				}
				removed, err := lane.spec.RemoveUnit(goos, home)
				if err != nil || removed != path {
					t.Fatalf("RemoveUnit = %q, %v; want %q", removed, err, path)
				}
				if again, err := lane.spec.RemoveUnit(goos, home); err != nil || again != "" {
					t.Errorf("second RemoveUnit: %q, %v", again, err)
				}
			})
		}
	}
}

// TestWindowsIsRefusedNotSilentlySkipped; a caller that believes it installed
// a service and did not is worse off than one told plainly, and the error has
// to name the foreground command to run instead.
func TestWindowsIsRefusedNotSilentlySkipped(t *testing.T) {
	for _, lane := range allLanes {
		_, err := lane.spec.WriteUnit("windows", t.TempDir(), "openbox.exe")
		if err == nil {
			t.Fatalf("%s: WriteUnit claimed success on windows", lane.name)
		}
		if !strings.Contains(err.Error(), "openbox "+lane.name) {
			t.Errorf("%s: the error does not say what to run instead: %v", lane.name, err)
		}
	}
}

// TestSuppliedTemplatesSurviveRendering. Our bodies contain no template
// actions, so that render must be an identity transform; and the whole test
// strategy rests on it: because the bytes are the same either way, asserting
// WriteUnit's artifact also asserts the library's.
func TestSuppliedTemplatesSurviveRendering(t *testing.T) {
	home := t.TempDir()
	for _, lane := range allLanes {
		for platform, body := range map[string]string{
			"launchd": lane.spec.LaunchdPlist(home, "/usr/local/bin/openbox"),
			"systemd": lane.spec.SystemdUnit("/usr/local/bin/openbox"),
		} {
			name := lane.name + "/" + platform
			if strings.Contains(body, "{{") {
				t.Fatalf("%s: the body contains a template action, so the library's render would rewrite it", name)
			}
			tmpl, err := template.New(name).Parse(body)
			if err != nil {
				t.Fatalf("%s: the library could not parse the body as a template: %v", name, err)
			}
			var out strings.Builder
			if err := tmpl.Execute(&out, map[string]any{"Name": "ignored"}); err != nil {
				t.Fatalf("%s: rendering failed: %v", name, err)
			}
			if out.String() != body {
				t.Errorf("%s: body is not render-stable", name)
			}
		}
	}
}
