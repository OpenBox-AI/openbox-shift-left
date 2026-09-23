package main

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"

	"github.com/openbox-ai/openbox-shift-left/internal/client/memhttptest"
)

// TestProvidersFromFlagDistinguishesAbsentFromEmpty is the whole point of
// carrying `seen` alongside the raw flag value: transport.Config.Validate
// treats a nil Providers (its own claude-code default) and an explicitly
// empty, non-nil one (intercept nothing) as different values, and *string
// alone cannot tell "never passed" from "passed empty" -- both read "".
func TestProvidersFromFlagDistinguishesAbsentFromEmpty(t *testing.T) {
	for _, tc := range []struct {
		name string
		seen bool
		raw  string
		want []string
	}{
		{"absent", false, "", nil},
		{"absent with stray raw value", false, "claude-code", nil}, // seen=false always wins
		{"present and empty", true, "", []string{}},
		{"present, one provider", true, "claude-code", []string{"claude-code"}},
		{"present, two providers", true, "claude-code,codex", []string{"claude-code", "codex"}},
		{"present with spaces and a trailing comma", true, " claude-code , codex ,", []string{"claude-code", "codex"}},
		{"present but all blank entries", true, " , ,", []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := providersFromFlag(tc.seen, tc.raw)
			if tc.want == nil {
				if got != nil {
					t.Errorf("got %#v, want nil", got)
				}
				return
			}
			if got == nil {
				t.Fatalf("got nil, want %#v", tc.want)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %#v, want %#v", got, tc.want)
			}
		})
	}
}

// TestTransportCommandProvidersEmptyInterceptsNothing is the end-to-end half
// of providersFromFlag: `--providers ""` on the real command must reach
// transport.Config.Providers as a non-nil empty slice, which Validate turns
// into an empty host union rather than its own claude-code default -- the
// log line this daemon prints on startup names exactly what it intercepts.
func TestTransportCommandProvidersEmptyInterceptsNothing(t *testing.T) {
	memhttptest.RequireBind(t)

	t.Setenv("OPENBOX_SPOOL_DIR", t.TempDir())
	t.Setenv("OPENBOX_HOME", t.TempDir())
	t.Setenv(devconfig.EnvAgentID, "7f3c9b2e-0000-5000-a000-00000000feed")
	t.Setenv("OPENBOX_REALTIME", "0")

	addr := freeLoopbackAddr(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ready := make(chan string, 1)
	a, _, errb := testApp(nil)
	a.transportCtx = ctx
	a.transportReady = func(bound string) { ready <- bound }

	done := make(chan int, 1)
	go func() { done <- a.runTransport([]string{"--addr", addr, "--providers", ""}) }()

	select {
	case <-ready:
	case <-time.After(15 * time.Second):
		t.Fatalf("the transport never reported ready; stderr: %s", errb.String())
	}
	cancel()
	select {
	case code := <-done:
		if code != exitOK {
			t.Fatalf("runTransport exited %d; stderr: %s", code, errb.String())
		}
	case <-time.After(20 * time.Second):
		t.Fatal("runTransport did not return after cancellation")
	}

	if !strings.Contains(errb.String(), "intercepting ;") {
		t.Errorf("--providers \"\" did not produce an empty intercept set; stderr: %s", errb.String())
	}
}
