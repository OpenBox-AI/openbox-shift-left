package activation

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// installLane activates one lane against a settings file, the way `openbox init`
// does, so the coverage check is compared against a real activation record.
func installLane(t *testing.T, home string, lane Lane, keys map[string]string) string {
	t.Helper()
	settings := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settings), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(settings, []byte(`{"hooks":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Activate(home, settings, lane, keys); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	return settings
}

// TestAVanishedEnvBlockIsDetected is the check that would have caught the
// observed wipe: `~/.claude/settings.json` carried OpenBox's full env block at
// 00:15 and by 00:28 held only `hooks`, `statusLine` and `switchModelsOnFlag`,
// with no removal run at all. The already-running CLI kept relaying, because
// environment routing binds at process start, so it was invisible from inside the
// session and the operator's only signal was absence.
func TestAVanishedEnvBlockIsDetected(t *testing.T) {
	home := t.TempDir()
	settings := installLane(t, home, LaneTransport, map[string]string{
		"HTTPS_PROXY": "http://127.0.0.1:8790",
		"HTTP_PROXY":  "http://127.0.0.1:8790",
	})

	// Intact first: without this control the assertion below could pass on a
	// coverage check that reports everything as missing.
	before, err := CoverageOf(home)
	if err != nil {
		t.Fatalf("CoverageOf: %v", err)
	}
	if len(before) != 1 || !before[0].Intact() {
		t.Fatalf("a freshly installed lane is not intact: %+v", before)
	}

	// The tool rewrites the file from a model that never held OpenBox's block.
	if err := os.WriteFile(settings, []byte(`{"hooks":{},"statusLine":{},"switchModelsOnFlag":true}`), 0o644); err != nil {
		t.Fatal(err)
	}

	after, err := CoverageOf(home)
	if err != nil {
		t.Fatalf("CoverageOf: %v", err)
	}
	if len(after) != 1 {
		t.Fatalf("coverage = %+v", after)
	}
	c := after[0]
	if c.Intact() {
		t.Fatal("the wipe was not detected")
	}
	if !c.Vanished() {
		t.Errorf("a wholly absent env block did not read as un-routed: %+v", c)
	}
	if !strings.Contains(c.Describe(), settings) {
		t.Errorf("the report does not name the file: %q", c.Describe())
	}
	if !strings.Contains(c.Describe(), "invisible from inside a session") {
		t.Errorf("the report does not explain why nobody noticed: %q", c.Describe())
	}
}

// TestAChangedValueIsDistinctFromAMissingOne someone else owning the routing is a
// different finding from the routing being gone, and collapsing them would make
// the report unactionable.
func TestAChangedValueIsDistinctFromAMissingOne(t *testing.T) {
	home := t.TempDir()
	settings := installLane(t, home, LaneTransport, map[string]string{
		"HTTPS_PROXY": "http://127.0.0.1:8790",
		"HTTP_PROXY":  "http://127.0.0.1:8790",
	})

	if err := os.WriteFile(settings, []byte(`{"env":{"HTTPS_PROXY":"http://corp-proxy:3128"}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	coverage, err := CoverageOf(home)
	if err != nil {
		t.Fatalf("CoverageOf: %v", err)
	}
	c := coverage[0]
	if len(c.Changed) != 1 || c.Changed[0] != "HTTPS_PROXY" {
		t.Errorf("changed = %v, want [HTTPS_PROXY]", c.Changed)
	}
	if len(c.Missing) != 1 || c.Missing[0] != "HTTP_PROXY" {
		t.Errorf("missing = %v, want [HTTP_PROXY]", c.Missing)
	}
	if c.Vanished() {
		t.Error("a partially edited block read as a wholesale un-routing")
	}
}

// TestCoverageIsSilentOnAMachineWithNothingInstalled a report that fires on a
// machine nobody configured trains its reader to ignore it.
func TestCoverageIsSilentOnAMachineWithNothingInstalled(t *testing.T) {
	coverage, err := CoverageOf(t.TempDir())
	if err != nil {
		t.Fatalf("CoverageOf: %v", err)
	}
	if len(coverage) != 0 {
		t.Errorf("coverage = %+v, want nothing", coverage)
	}
}

// TestCoverageRefusesToGuessFromAnUnreadableSettingsFile "the keys are missing"
// and "the file cannot be read" are different findings, and the producer election
// already made the mistake of conflating them once.
func TestCoverageRefusesToGuessFromAnUnreadableSettingsFile(t *testing.T) {
	home := t.TempDir()
	settings := installLane(t, home, LaneTransport, map[string]string{"HTTPS_PROXY": "http://127.0.0.1:8790"})
	if err := os.WriteFile(settings, []byte(`{"env": not json`), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := CoverageOf(home); err == nil {
		t.Error("an unreadable settings file was reported as every managed key being missing")
	}
}

// TestTheInstallWindowDoesNotFalsePositive is the interaction the plan flags:
// install ordering starts the daemon BEFORE the env is written, on purpose
// (writing the var first points the tool at a dead port), so "unit installed,
// keys absent" is legitimate for a few seconds. What distinguishes the two is the
// ACTIVATION RECORD -- it is written with the keys, not with the unit -- so a
// lane mid-install has no record and no coverage row at all.
func TestTheInstallWindowDoesNotFalsePositive(t *testing.T) {
	home := t.TempDir()
	// The unit is installed and listening; nothing has been activated yet.
	coverage, err := CoverageOf(home)
	if err != nil {
		t.Fatalf("CoverageOf: %v", err)
	}
	if len(coverage) != 0 {
		t.Errorf("a lane mid-install produced %d coverage finding(s): %+v", len(coverage), coverage)
	}
}

// --- desktop coverage ---

func fakeConns(remotes []string, err error) connLister {
	return func(context.Context, string) ([]string, error) { return remotes, err }
}

// fakePresence stands in for pgrep, so no test shells out to ask about a real
// process on the machine running it.
func fakePresence(running, known bool) presenceCheck {
	return func(context.Context, string) (bool, bool) { return running, known }
}

// TestDesktopCoverageClassification the classification is the whole of what can
// be implemented before discovery: the symptom is measurable, the routing
// mechanism is not yet known, and a detector written for a guessed mechanism
// would be worse than none.
func TestDesktopCoverageClassification(t *testing.T) {
	const relayPort = "8790"
	for name, tc := range map[string]struct {
		remotes    []string
		present    presenceCheck
		wantRouted bool
		wantSays   string
	}{
		"direct to the provider": {
			remotes:  []string{"[2607:bc0::10]:443", "17.253.1.1:443"},
			present:  fakePresence(true, true),
			wantSays: "NOT routed through the relay",
		},
		"through the relay": {
			remotes:    []string{"127.0.0.1:8790"},
			present:    fakePresence(true, true),
			wantRouted: true,
			wantSays:   "reaching the provider through",
		},
		"not running": {
			remotes:  nil,
			present:  fakePresence(false, true),
			wantSays: "not running",
		},
		// The one an ESTABLISHED-only listing cannot see: open, idle, and unrouted
		// reads exactly like absent, and calling that "not running" is an
		// all-clear for a governed surface nothing is governing.
		"open but holding no connections": {
			remotes:  nil,
			present:  fakePresence(true, true),
			wantSays: "holding no provider connections",
		},
		"presence undeterminable": {
			remotes:  nil,
			present:  fakePresence(false, false),
			wantSays: "could not be established",
		},
		"running but idle": {
			remotes:  []string{"127.0.0.1:5432"},
			present:  fakePresence(true, true),
			wantSays: "cannot be determined",
		},
	} {
		t.Run(name, func(t *testing.T) {
			d := inspectDesktopWith(context.Background(), relayPort, fakeConns(tc.remotes, nil), tc.present)
			if !d.Supported {
				t.Skip("desktop inspection is macOS-only; Windows is a discovery question")
			}
			if d.Routed() != tc.wantRouted {
				t.Errorf("Routed() = %v, want %v (%+v)", d.Routed(), tc.wantRouted, d)
			}
			if got := d.Describe("127.0.0.1:8790"); !strings.Contains(got, tc.wantSays) {
				t.Errorf("Describe() = %q, want it to mention %q", got, tc.wantSays)
			}
			// The wording is a requirement, not a preference: a legitimately
			// unconfigured machine looks identical to a bypassed one, so this must
			// report coverage and never accuse.
			if got := d.Describe("127.0.0.1:8790"); strings.Contains(strings.ToLower(got), "bypass") {
				t.Errorf("the report accuses rather than describes: %q", got)
			}
		})
	}
}

// TestDesktopInspectionFailureIsNotCoverage a check that cannot run must say so,
// never report the surface as fine.
func TestDesktopInspectionFailureIsNotCoverage(t *testing.T) {
	d := inspectDesktopWith(context.Background(), "8790", fakeConns(nil, os.ErrPermission), fakePresence(false, false))
	if !d.Supported {
		t.Skip("desktop inspection is macOS-only")
	}
	if d.Routed() {
		t.Error("a failed inspection reported the desktop app as routed")
	}
	if d.Note == "" {
		t.Error("a failed inspection recorded no reason")
	}
}

// TestParseLsofRemotes lsof is what produced the original observation, so its
// exact output shape is what this parses.
func TestParseLsofRemotes(t *testing.T) {
	out := `COMMAND   PID     USER   FD   TYPE             DEVICE SIZE/OFF NODE NAME
Claude  40321 dev        31u  IPv6 0xabc              0t0  TCP [2607:bc0::10]:52344->[2607:bc0::11]:443 (ESTABLISHED)
Claude  40321 dev        32u  IPv4 0xdef              0t0  TCP 127.0.0.1:52345->127.0.0.1:8790 (ESTABLISHED)
Claude  40321 dev        33u  IPv4 0x123              0t0  TCP 127.0.0.1:52346 (LISTEN)
`
	got := parseLsofRemotes(out)
	want := []string{"[2607:bc0::11]:443", "127.0.0.1:8790"}
	if len(got) != len(want) {
		t.Fatalf("parsed %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("remote %d = %q, want %q", i, got[i], want[i])
		}
	}
}
