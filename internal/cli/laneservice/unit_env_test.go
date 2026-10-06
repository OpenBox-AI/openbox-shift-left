package laneservice

import (
	"encoding/xml"
	"strings"
	"testing"
	"text/template"
)

// envLanes is every lane, each carrying the two coordinates an installer can
// hold. Built per call because WithEnv returns a copy and a shared fixture
// would let one subtest's mutation reach another's assertion.
func envLanes(env map[string]string) map[string]Spec {
	return map[string]Spec{
		"gateway":   Gateway("127.0.0.1:8788", "https://api.anthropic.com", "", false).WithEnv(env),
		"telemetry": Telemetry("127.0.0.1:8789", "", false).WithEnv(env),
		"transport": Transport("127.0.0.1:8790", "", false).WithEnv(env),
	}
}

// TestEveryLaneUnitCarriesTheInstallersCoordinates is the fix for a shipped
// defect asserted where it lands: in the unit.
//
// A daemon has no environment of its own. launchd and systemd start it with
// neither $HOME nor $OPENBOX_HOME, so devconfig.Home() inside the daemon
// resolves the real ~/.openbox while the installing process validated the path
// its own $OPENBOX_HOME named. setupTransport then refuses to point
// NODE_EXTRA_CA_CERTS at a CA the daemon minted somewhere else, and the whole
// install rolls back -- so any developer who set OPENBOX_HOME, which README.md
// documents, could not bring up transport at all. Same reasoning as
// --settings: the install path is the only place that knows the answer for
// certain, so it puts the answer in the unit.
func TestEveryLaneUnitCarriesTheInstallersCoordinates(t *testing.T) {
	const (
		obxHome = "/Users/dev/obx-home"
		spool   = "/Users/dev/obx-spool"
	)
	env := map[string]string{"OPENBOX_HOME": obxHome, "OPENBOX_SPOOL_DIR": spool}
	for name, spec := range envLanes(env) {
		plist := spec.LaunchdPlist(t.TempDir(), "/bin/openbox")
		if !strings.Contains(plist, "<key>EnvironmentVariables</key>") {
			t.Errorf("%s: the plist declares no EnvironmentVariables, so the daemon resolves the real ~/.openbox:\n%s", name, plist)
		}
		for key, value := range env {
			if !strings.Contains(plist, "<key>"+key+"</key>") {
				t.Errorf("%s: the plist does not carry %s", name, key)
			}
			if !strings.Contains(plist, "<string>"+value+"</string>") {
				t.Errorf("%s: the plist does not carry %s=%s", name, key, value)
			}
		}
		unit := spec.SystemdUnit("/bin/openbox")
		for key, value := range env {
			if want := `Environment="` + key + "=" + value + `"`; !strings.Contains(unit, want) {
				t.Errorf("%s: the systemd unit is missing %s:\n%s", name, want, unit)
			}
		}
	}
}

// TestALaneUnitOmitsEnvWhenTheInstallerHadNone. A machine with neither
// variable set must get the unit it gets today, byte for byte: a re-install
// that rewrites every unit file looks like a change the developer did not make,
// and an empty OPENBOX_HOME is worse than an absent one -- devconfig.Home
// rejects a relative path but treats "" as unset, so a blank key would send the
// daemon somewhere different again.
func TestALaneUnitOmitsEnvWhenTheInstallerHadNone(t *testing.T) {
	home := t.TempDir()
	for name, bare := range envLanes(nil) {
		for platform, body := range map[string]string{
			"launchd": bare.LaunchdPlist(home, "/bin/openbox"),
			"systemd": bare.SystemdUnit("/bin/openbox"),
		} {
			if strings.Contains(body, "EnvironmentVariables") || strings.Contains(body, "Environment=") {
				t.Errorf("%s %s renders an environment block for an installer that had none:\n%s", name, platform, body)
			}
		}
	}
	// A key present with an empty value is dropped, not rendered blank.
	for name, spec := range envLanes(map[string]string{"OPENBOX_HOME": "", "OPENBOX_SPOOL_DIR": "/s"}) {
		plist := spec.LaunchdPlist(home, "/bin/openbox")
		if strings.Contains(plist, "OPENBOX_HOME") {
			t.Errorf("%s: an empty OPENBOX_HOME was rendered into the unit:\n%s", name, plist)
		}
		if !strings.Contains(plist, "OPENBOX_SPOOL_DIR") {
			t.Errorf("%s: dropping the empty key also dropped the set one", name)
		}
	}
}

// TestUnitEnvRenderingIsDeterministic. Go randomizes map iteration, so an
// unsorted render would produce a different unit on every install and make a
// no-op re-install look like a change.
func TestUnitEnvRenderingIsDeterministic(t *testing.T) {
	env := map[string]string{"OPENBOX_HOME": "/h", "OPENBOX_SPOOL_DIR": "/s", "OPENBOX_EXTRA": "/e"}
	home := t.TempDir()
	for name, spec := range envLanes(env) {
		plist, unit := spec.LaunchdPlist(home, "/bin/openbox"), spec.SystemdUnit("/bin/openbox")
		// Enough repeats that a randomized order would have to be unlucky many
		// times over to pass.
		for i := 0; i < 20; i++ {
			again := envLanes(env)[name]
			if got := again.LaunchdPlist(home, "/bin/openbox"); got != plist {
				t.Fatalf("%s: plist render is not deterministic:\n%s\n---\n%s", name, plist, got)
			}
			if got := again.SystemdUnit("/bin/openbox"); got != unit {
				t.Fatalf("%s: systemd render is not deterministic:\n%s\n---\n%s", name, unit, got)
			}
		}
	}
}

// TestUnitEnvIsEscapedForItsPlatform. A path can legitimately contain '&' or a
// space, and an unescaped one produces a plist launchd silently refuses to
// load -- which presents as "the daemon never starts" with no error anywhere.
// A '%' in a systemd Environment= line is a specifier systemd would expand.
func TestUnitEnvIsEscapedForItsPlatform(t *testing.T) {
	env := map[string]string{"OPENBOX_HOME": `/Users/a&b/<obx>`, "OPENBOX_SPOOL_DIR": `/Users/a b/100%/spool`}
	for name, spec := range envLanes(env) {
		plist := spec.LaunchdPlist(t.TempDir(), "/bin/openbox")
		var doc any
		if err := xml.Unmarshal([]byte(plist), &doc); err != nil {
			t.Errorf("%s: an environment value made the plist malformed: %v\n%s", name, err, plist)
		}
		if strings.Contains(plist, "<obx>") {
			t.Errorf("%s: '<' in an environment value was not escaped:\n%s", name, plist)
		}
		unit := spec.SystemdUnit("/bin/openbox")
		if !strings.Contains(unit, "100%%") {
			t.Errorf("%s: '%%' in an environment value was not doubled, so systemd would expand it:\n%s", name, unit)
		}
		if !strings.Contains(unit, `Environment="OPENBOX_SPOOL_DIR=/Users/a b/`) {
			t.Errorf("%s: a value with a space was not quoted as one argument:\n%s", name, unit)
		}
	}
}

// TestUnitEnvSurvivesTheLibrarysRender. kardianos/service takes our bodies as
// its own template overrides and runs them through text/template, so a body
// containing a template action would be rewritten on the way to disk. This is
// what lets a test assert WriteUnit's artifact and mean the library's too.
func TestUnitEnvSurvivesTheLibrarysRender(t *testing.T) {
	env := map[string]string{"OPENBOX_HOME": "/h", "OPENBOX_SPOOL_DIR": "/s"}
	home := t.TempDir()
	for name, spec := range envLanes(env) {
		for platform, body := range map[string]string{
			"launchd": spec.LaunchdPlist(home, "/bin/openbox"),
			"systemd": spec.SystemdUnit("/bin/openbox"),
		} {
			label := name + "/" + platform
			if strings.Contains(body, "{{") {
				t.Fatalf("%s: the body contains a template action, so the library's render would rewrite it", label)
			}
			tmpl, err := template.New(label).Parse(body)
			if err != nil {
				t.Fatalf("%s: the library could not parse the body as a template: %v", label, err)
			}
			var out strings.Builder
			if err := tmpl.Execute(&out, map[string]any{"Name": "ignored"}); err != nil {
				t.Fatalf("%s: rendering failed: %v", label, err)
			}
			if out.String() != body {
				t.Errorf("%s: body is not render-stable", label)
			}
		}
	}
}

// TestWithEnvDoesNotAliasItsCallersMap. The installer builds one map and hands
// it to three lanes; a shared reference would let a later mutation rewrite a
// unit that was already rendered.
func TestWithEnvDoesNotAliasItsCallersMap(t *testing.T) {
	env := map[string]string{"OPENBOX_HOME": "/h"}
	spec := Telemetry("127.0.0.1:8789", "", false).WithEnv(env)
	env["OPENBOX_HOME"] = "/mutated"
	env["OPENBOX_SPOOL_DIR"] = "/added"
	plist := spec.LaunchdPlist(t.TempDir(), "/bin/openbox")
	if strings.Contains(plist, "/mutated") || strings.Contains(plist, "OPENBOX_SPOOL_DIR") {
		t.Errorf("WithEnv kept a reference to its caller's map:\n%s", plist)
	}
}
