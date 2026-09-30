package main

import (
	"context"
	"io"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/gatewayemit"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/gatewayservice"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/providers"
	"github.com/openbox-ai/openbox-shift-left/internal/client"
	"github.com/openbox-ai/openbox-shift-left/internal/client/fakecore"
	"github.com/openbox-ai/openbox-shift-left/internal/client/memhttptest"
	"github.com/openbox-ai/openbox-shift-left/internal/provider"
	"github.com/openbox-ai/openbox-shift-left/internal/transport"
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

// TestLaneRecordInterleavesWithHookEventsInAppendOrder pins: a claude-code
// lane record appends into and drains through the SAME cc-spool file a hook
// process already queued its own event into (seeded here to stand in for
// one), so core receives them in append order regardless of producer, and
// the session's spool ends up EMPTY -- everything queued got its one
// attempt (lane records queue through the session spool).
func TestLaneRecordInterleavesWithHookEventsInAppendOrder(t *testing.T) {
	memhttptest.RequireBind(t)

	t.Setenv(devconfig.EnvSpoolRoot, t.TempDir())
	t.Setenv(devconfig.EnvHome, t.TempDir())
	// Belt and braces: nothing in this test SHOULD fail delivery, but an
	// isolated halt dir keeps any accidental failure off the real machine
	// instead of the hermeticity guard's own real-write scan being the first
	// thing to notice it.
	t.Setenv(devconfig.EnvHaltDir, t.TempDir())

	fake := fakecore.New(t, fakecore.Script{})
	t.Setenv(devconfig.EnvBaseURL, fake.URL())
	seedV3EnvIdentity(t)

	logger := log.New(io.Discard, "", 0)
	identities := resolveProviderIdentities(logger.Printf)
	queues := laneQueues(identities, logger)

	const sessionID = "sess-lane-order-1"
	// Seeded directly into the exact spool file laneQueues' own claude-code
	// Engine will drain: standing in for a hook process that queued its own
	// event before any relayed call happened.
	hookSpool := hookflow.Spool{Dir: devconfig.SpoolDir("cc-spool")}
	if err := hookSpool.Append(client.DevEvent{
		SchemaVersion: client.SchemaVersion,
		EventID:       "hook-session-started",
		EventType:     client.EventSessionStarted,
		SessionID:     sessionID,
		DeveloperDID:  devconfig.ResolveDIDOrEmpty(),
		Tool:          client.Tool{Name: "claude-code", Kind: client.ToolShell},
		Timestamp:     "2026-09-24T00:00:00Z",
	}); err != nil {
		t.Fatalf("seeding the hook's own event: %v", err)
	}

	ca, err := transport.LoadOrCreateCA(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreateCA: %v", err)
	}
	em := &gatewayemit.Emitter{
		Lane:    gatewayemit.LaneProxy,
		Elected: func() bool { return true },
		Deliver: routeLaneRecord(queues, nil, logger, "transport"),
		DID:     devconfig.ResolveDIDOrEmpty,
		Warn:    logger.Printf,
	}
	p, err := transport.New(transport.Config{Upstream: refusedUpstream}, ca, em)
	if err != nil {
		t.Fatalf("transport.New: %v", err)
	}

	serveOneCall(t, p, ca, sessionID)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && len(fake.Inbox()) < 3 {
		time.Sleep(10 * time.Millisecond)
	}
	inbox := fake.Inbox()
	if len(inbox) != 3 {
		t.Fatalf("fake core received %d event(s), want 3 (the seeded hook event plus the relayed "+
			"call's Started/Completed pair); rejections=%v", len(inbox), fake.Rejections())
	}
	if got := inbox[0].EventType(); got != "WorkflowStarted" {
		t.Errorf("first event reaching core = %q, want WorkflowStarted (the hook's own, seeded first "+
			"and drained first): append order was not preserved", got)
	}

	if n := hookSpool.PendingCount(sessionID); n != 0 {
		t.Errorf("PendingCount(%s) = %d after both producers' events were drained, want 0 (spool empty)",
			sessionID, n)
	}
}

// TestDerivedTransportProviders is the derived set per installed-provider
// fixture: which providers' model calls can reach the relay on this machine.
func TestDerivedTransportProviders(t *testing.T) {
	for _, tc := range []struct {
		name       string
		installing provider.Name
		ccRouted   bool // settings.json routes Claude Code at the relay
		codexOtel  bool // Codex has an OpenBox-owned [otel] block
		pacOS      bool // this OS has a system PAC
		want       []string
	}{
		{"claude-code alone", provider.ClaudeCode, false, false, true, []string{"claude-code"}},
		{"claude-code alone without a PAC", provider.ClaudeCode, false, false, false, []string{"claude-code"}},
		{"codex alone on a PAC platform", provider.Codex, false, false, true, []string{"codex"}},
		{"codex joins an installed claude-code", provider.Codex, true, false, true, []string{"claude-code", "codex"}},
		{"claude-code joins an installed codex", provider.ClaudeCode, false, true, true, []string{"claude-code", "codex"}},
		{"both already installed", provider.ClaudeCode, true, true, true, []string{"claude-code", "codex"}},
		{"linux/windows: codex is never in the set", provider.ClaudeCode, true, true, false, []string{"claude-code"}},
		{"linux/windows: codex alone has no relay at all", provider.Codex, false, false, false, []string{}},
		{"muse has no proxy lane", provider.Muse, false, false, true, []string{}},
		{"muse does not displace an installed codex", provider.Muse, false, true, true, []string{"codex"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := isolateHome(t)
			t.Setenv("CODEX_HOME", filepath.Join(home, "codex-home"))
			withSystemPACSupport(t, tc.pacOS)
			claudeHome := t.TempDir()
			if tc.ccRouted {
				settings := gatewayservice.SettingsPath(claudeHome)
				if err := os.MkdirAll(filepath.Dir(settings), 0o755); err != nil {
					t.Fatal(err)
				}
				body := `{"env":{"HTTPS_PROXY":"http://127.0.0.1:8790"}}`
				if err := os.WriteFile(settings, []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if tc.codexOtel {
				if err := providers.WriteCodexOtel(providers.CodexConfigTOMLPath(), "http://127.0.0.1:4318/v1/logs"); err != nil {
					t.Fatal(err)
				}
			}
			got := derivedTransportProviders(claudeHome, tc.installing)
			if !slices.Equal(got, tc.want) {
				t.Errorf("derived set = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestHasTransportArm: Claude Code always, Codex only with a system PAC,
// nothing else.
func TestHasTransportArm(t *testing.T) {
	withSystemPACSupport(t, true)
	if !hasTransportArm(provider.ClaudeCode) || !hasTransportArm(provider.Codex) || hasTransportArm(provider.Muse) {
		t.Error("with a system PAC: claude-code and codex have a transport arm, muse does not")
	}
	withSystemPACSupport(t, false)
	if !hasTransportArm(provider.ClaudeCode) || hasTransportArm(provider.Codex) || hasTransportArm(provider.Muse) {
		t.Error("without a system PAC: only claude-code has a transport arm")
	}
}
