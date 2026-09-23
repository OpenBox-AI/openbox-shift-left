package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/backend"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/devinit"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/providers"
	"github.com/openbox-ai/openbox-shift-left/internal/client"
)

// seedLegacyStore writes a pre-v3 (DID + Ed25519 seed) store for tool -- the
// shape LegacyStoreFor recognizes -- mirroring TestReuseGatesAgree's "legacy
// file" row.
func seedLegacyStore(t *testing.T, home, tool string) {
	t.Helper()
	envPath, err := devconfig.EnvFilePathFor(tool)
	if err != nil {
		t.Fatal(err)
	}
	if err := devconfig.WriteEnvFile(envPath, map[string]string{
		devconfig.EnvAPIKeyDirect:    "obx_legacy",
		devconfig.EnvAgentPrivateKey: testSeedB64,
	}); err != nil {
		t.Fatal(err)
	}
	if err := devconfig.SetLegacyDID(filepath.Join(home, tool, "dev.json"), "did:aip:legacy"); err != nil {
		t.Fatal(err)
	}
}

func legacySpoolEvent(session, id string) client.DevEvent {
	return client.DevEvent{
		SchemaVersion: client.SchemaVersion,
		EventID:       id,
		EventType:     client.EventToolCall,
		SessionID:     session,
		DeveloperDID:  "did:aip:legacy",
		Timestamp:     "2026-07-08T12:00:00Z",
		Tool:          client.Tool{Name: "Bash", Kind: client.ToolShell},
	}
}

// TestInitDiscardsEventsQueuedUnderLegacyIdentity: a tool with a legacy
// store and a spooled backlog gets that backlog discarded -- never delivered,
// since the write that follows replaces the very identity that authenticated
// it -- the moment init writes the v3 identity over it, counted and recorded
// in the discard ledger, and announced on stdout. A second init, now against
// the fresh v3 store, has nothing left to discard and says nothing.
func TestInitDiscardsEventsQueuedUnderLegacyIdentity(t *testing.T) {
	home := isolateHomeOnly(t)
	clearAgentEnv(t)
	seedLegacyStore(t, home, "claude-code")

	spoolDir := providers.SpoolDirFor("claude-code")
	sp := hookflow.Spool{Dir: spoolDir}
	for i, id := range []string{"e1", "e2", "e3"} {
		if err := sp.Append(legacySpoolEvent("sess", id)); err != nil {
			t.Fatalf("seed spool event %d: %v", i, err)
		}
	}
	if got := sp.BacklogCount(); got != 3 {
		t.Fatalf("backlog before init = %d, want 3", got)
	}

	t.Setenv(devconfig.EnvControlToken, testOrgToken)
	reg := &countingReg{byName: map[string]*backend.AgentSummary{}}
	a, out, errb := testApp(nil)
	a.newRegistrar = func(_, _, _ string) devinit.Registrar { return reg }
	a.newPrompt = declineAdopt(t)
	if code := a.run([]string{"init", "--provider", "claude-code"}); code != exitOK {
		t.Fatalf("init exit = %d; stderr=%q", code, errb.String())
	}
	if reg.creates != 1 {
		t.Fatalf("Create called %d times, want 1 (a legacy store must not reuse)", reg.creates)
	}
	if !strings.Contains(out.String(), "discarded 3 events queued under the previous identity") {
		t.Errorf("stdout missing the discard line:\n%s", out.String())
	}
	if got := sp.BacklogCount(); got != 0 {
		t.Errorf("backlog after init = %d, want 0", got)
	}
	if got := sp.DiscardedCount(); got != 3 {
		t.Errorf("discard ledger count = %d, want 3", got)
	}

	// Second init reuses the freshly written v3 store; the legacy identity is
	// already gone, so there is nothing left to discard and no line to print.
	b, out2, errb2 := testApp(nil)
	b.newRegistrar = func(_, _, _ string) devinit.Registrar { return panicReg{} }
	b.newPrompt = panicPrompt(t)
	if code := b.run([]string{"init", "--provider", "claude-code"}); code != exitOK {
		t.Fatalf("second init exit = %d; stderr=%q", code, errb2.String())
	}
	if strings.Contains(out2.String(), "discarded") {
		t.Errorf("second init printed a discard line:\n%s", out2.String())
	}
}

// TestInitWithoutLegacyStoreDiscardsNothing a fresh registration (no store at
// all, so LegacyStoreFor is false because there is nothing to be legacy, not
// because there is a v3 identity) must leave an unrelated spool backlog
// completely untouched: the discard is gated on replacing a legacy identity,
// never fired just because a spool happens to have something in it.
func TestInitWithoutLegacyStoreDiscardsNothing(t *testing.T) {
	isolateHomeOnly(t)
	clearAgentEnv(t)

	spoolDir := providers.SpoolDirFor("claude-code")
	sp := hookflow.Spool{Dir: spoolDir}
	if err := sp.Append(legacySpoolEvent("sess", "e1")); err != nil {
		t.Fatal(err)
	}

	t.Setenv(devconfig.EnvControlToken, testOrgToken)
	reg := &countingReg{byName: map[string]*backend.AgentSummary{}}
	a, out, errb := testApp(nil)
	a.newRegistrar = func(_, _, _ string) devinit.Registrar { return reg }
	a.newPrompt = declineAdopt(t)
	if code := a.run([]string{"init", "--provider", "claude-code"}); code != exitOK {
		t.Fatalf("init exit = %d; stderr=%q", code, errb.String())
	}
	if reg.creates != 1 {
		t.Fatalf("Create called %d times, want 1 (fresh registration)", reg.creates)
	}
	if strings.Contains(out.String(), "discarded") {
		t.Errorf("a fresh registration (no legacy store) printed a discard line:\n%s", out.String())
	}
	if got := sp.BacklogCount(); got != 1 {
		t.Errorf("backlog after a non-legacy init = %d, want 1 (untouched)", got)
	}
}

// TestDiscardIsSkippedWhenTheWriteFails proves the gate is write SUCCESS, not
// merely having seen a legacy store: wasLegacy is captured before
// WriteWorkloadIdentity runs, so if that write then fails, the discard must
// never fire, and whatever was spooled stays exactly as it was.
func TestDiscardIsSkippedWhenTheWriteFails(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mode bits do not deny writes on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("running as root: mode bits do not deny writes")
	}
	home := isolateHomeOnly(t)
	clearAgentEnv(t)
	seedLegacyStore(t, home, "claude-code")

	// WriteWorkloadIdentity writes dev.json first (os.WriteFile, O_TRUNC): a
	// read-only dev.json fails that write outright, before the credential file
	// (which uses a temp-then-rename, needing the directory) is ever touched.
	cfgPath := filepath.Join(home, "claude-code", "dev.json")
	if err := os.Chmod(cfgPath, 0o400); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(cfgPath, 0o600) })

	spoolDir := providers.SpoolDirFor("claude-code")
	sp := hookflow.Spool{Dir: spoolDir}
	if err := sp.Append(legacySpoolEvent("sess", "e1")); err != nil {
		t.Fatal(err)
	}

	t.Setenv(devconfig.EnvControlToken, testOrgToken)
	reg := &countingReg{byName: map[string]*backend.AgentSummary{}}
	a, out, errb := testApp(nil)
	a.newRegistrar = func(_, _, _ string) devinit.Registrar { return reg }
	a.newPrompt = declineAdopt(t)
	if code := a.run([]string{"init", "--provider", "claude-code"}); code == exitOK {
		t.Fatalf("init succeeded despite an unwritable dev.json; stdout=%q stderr=%q", out.String(), errb.String())
	}
	if strings.Contains(out.String(), "discarded") {
		t.Errorf("a failed identity write still printed a discard line:\n%s", out.String())
	}
	if got := sp.BacklogCount(); got != 1 {
		t.Errorf("backlog after a failed write = %d, want 1 (untouched)", got)
	}
}
