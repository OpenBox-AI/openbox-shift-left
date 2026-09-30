package main

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/cli/laneservice"
	"github.com/openbox-ai/openbox-shift-left/internal/client/memhttptest"
	"github.com/openbox-ai/openbox-shift-left/internal/telemetry"
	"github.com/openbox-ai/openbox-shift-left/internal/transport"
)

// TestEveryLaneUnitArgvIsAcceptedByItsOwnCommand closes a loophole in the two
// tests that already guard this: both check a unit body against a HAND-WRITTEN
// list of flags "the command defines", so adding a flag to the unit and the list
// stays green whether or not the command defines it. A unit passing an unknown
// flag fails to start on EVERY boot.
//
// So this runs the REAL command over the exact argv the installer writes, with
// the addresses swapped for free ones.
func TestEveryLaneUnitArgvIsAcceptedByItsOwnCommand(t *testing.T) {
	memhttptest.RequireBind(t)
	// Deliberately UNREADABLE rather than merely absent. An absent settings file is
	// the normal state of a machine with nothing installed and is reported quietly;
	// a file that exists and cannot be parsed is the loud case, and it is the only
	// one that proves the command actually READ the path rather than accepting the
	// flag and ignoring it.
	settings := unreadableSettings(t)
	// Codex's two paths ride both units the same way: an unparsable
	// config.toml is the loud case that proves the daemon READ --codex-settings
	// rather than accepting it, and the record path is one it must accept.
	codexConfig := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(codexConfig, []byte("not [ valid"), 0o644); err != nil {
		t.Fatal(err)
	}
	pacRecord := filepath.Join(t.TempDir(), "activation.json")
	// Muse's settings path rides the telemetry unit only; an unparsable file is
	// the loud case that proves the daemon READ --muse-settings.
	museConfig := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(museConfig, []byte(`{"telemetry":`), 0o644); err != nil {
		t.Fatal(err)
	}

	for name, spec := range map[string]laneservice.Spec{
		"telemetry": laneservice.Telemetry(telemetry.DefaultAddr, settings, true).
			WithCodexSettings(codexConfig).WithPACRecord(pacRecord).WithMuseSettings(museConfig),
		"transport": laneservice.Transport(transport.DefaultAddr, settings, true).
			WithProviders([]string{"claude-code", "codex"}).
			WithCodexSettings(codexConfig).WithPACRecord(pacRecord),
	} {
		t.Run(name, func(t *testing.T) {
			argv := spec.Argv("/bin/openbox")
			args := replaceAddr(t, argv[2:])

			t.Setenv("OPENBOX_SPOOL_DIR", t.TempDir())
			t.Setenv("OPENBOX_HOME", t.TempDir())
			t.Setenv("OPENBOX_REALTIME", "0")

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ready := make(chan struct{}, 1)
			a, _, errb := testApp(nil)

			var code int
			done := make(chan struct{})
			switch name {
			case "telemetry":
				a.telemetryCtx = ctx
				a.telemetryReady = func(string) { ready <- struct{}{} }
				go func() { code = a.runTelemetry(args); close(done) }()
			case "transport":
				a.transportCtx = ctx
				a.transportReady = func(string) { ready <- struct{}{} }
				go func() { code = a.runTransport(args); close(done) }()
			}

			select {
			case <-ready:
			case <-done:
				t.Fatalf("`openbox %s %s` exited %d before listening; stderr: %s",
					name, strings.Join(args, " "), code, errb.String())
			case <-time.After(15 * time.Second):
				t.Fatalf("%s never reported ready; stderr: %s", name, errb.String())
			}
			cancel()
			select {
			case <-done:
			case <-time.After(20 * time.Second):
				t.Fatalf("%s did not return after cancellation", name)
			}
			if code != exitOK {
				t.Errorf("`openbox %s` exited %d for its own unit's argv; stderr: %s",
					name, code, errb.String())
			}
			// The --settings flag the daemon's election rests on has to be one
			// the command reads, not merely one it tolerates: an unreadable path
			// is reported loudly, and a path it never looked at would report
			// nothing.
			if !strings.Contains(errb.String(), "CANNOT DECIDE") {
				t.Errorf("%s did not report the unreadable settings path it was handed, so it is "+
					"not reading --settings at all; stderr: %s", name, errb.String())
			}
			if !strings.Contains(errb.String(), "CANNOT DECIDE whether to emit Codex model-call turns") {
				t.Errorf("%s did not report the unreadable Codex config it was handed, so it is "+
					"not reading --codex-settings; stderr: %s", name, errb.String())
			}
			if name == "telemetry" && !strings.Contains(errb.String(), "CANNOT DECIDE whether to emit Muse model-call turns") {
				t.Errorf("telemetry did not report the unreadable Muse settings it was handed, so it is "+
					"not reading --muse-settings; stderr: %s", errb.String())
			}
		})
	}
}

// TestTheGatewayCommandAcceptsItsOwnUnitArgv the same check for the lane whose
// unit is rendered by gatewayservice rather than by laneservice.
func TestTheGatewayCommandAcceptsItsOwnUnitArgv(t *testing.T) {
	upstream := memhttptest.NewServer(t, nil)
	defer upstream.Close()
	settings := unreadableSettings(t)

	t.Setenv("OPENBOX_SPOOL_DIR", t.TempDir())
	t.Setenv("OPENBOX_HOME", t.TempDir())
	t.Setenv("OPENBOX_REALTIME", "0")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan struct{}, 1)
	a, _, errb := testApp(nil)
	a.gatewayCtx = ctx
	a.gatewayReady = func(net.Addr) { ready <- struct{}{} }

	var code int
	done := make(chan struct{})
	go func() {
		code = a.runGateway([]string{
			"--addr", "127.0.0.1:0",
			"--upstream", upstream.URL,
			"--shutdown-grace", "30s",
			"--settings", settings,
			"--verbose",
		})
		close(done)
	}()
	select {
	case <-ready:
	case <-done:
		t.Fatalf("`openbox gateway` exited %d before listening; stderr: %s", code, errb.String())
	case <-time.After(15 * time.Second):
		t.Fatalf("the gateway never reported ready; stderr: %s", errb.String())
	}
	cancel()
	<-done
	if code != exitOK {
		t.Errorf("`openbox gateway` exited %d for its own unit's argv; stderr: %s", code, errb.String())
	}
	if !strings.Contains(errb.String(), "CANNOT DECIDE") {
		t.Errorf("the gateway did not report the unreadable settings path it was handed: %s", errb.String())
	}
}

// replaceAddr swaps the unit's fixed --addr for a free one. The unit's real value
// is the point of the argv, but binding it here would collide with the
// developer's own running lane, which is exactly the machine this suite runs on.
func replaceAddr(t *testing.T, args []string) []string {
	t.Helper()
	out := append([]string(nil), args...)
	for i := 0; i < len(out)-1; i++ {
		if out[i] == "--addr" {
			out[i+1] = freeLoopbackAddr(t)
			return out
		}
	}
	t.Fatalf("the unit argv carries no --addr: %v", args)
	return nil
}

// unreadableSettings writes a settings file that exists and cannot be parsed.
func unreadableSettings(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"env": this is not json`), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}
