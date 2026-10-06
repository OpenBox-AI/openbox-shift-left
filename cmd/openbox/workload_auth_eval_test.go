package main

// This file pins the workload-identity acceptance claims -- a warm hook
// reusing its cached bearer, a cold hook bootstrapping and exchanging exactly
// once, Keycloak-outage fail-open behaviour, a cached token's 401 forcing the
// next hook cold, and a legacy store governing nothing -- as evals over the
// real `openbox hook claude-code <Event>` entrypoint against fakecore's v3
// control plane, exactly the architecture governanceeval_test.go documents:
// a session is a frozen list of native hook payloads, and a grader reads what
// the binary put on the wire, rendered, and left on disk, never what it
// merely intended.
//
// Each test below drives testApp/a.run directly rather than through
// runScenario, because each one needs a hand between two hook invocations
// (a counter snapshot, a fakecore knob such as Revoke, a legacy dev.json
// written to the same OPENBOX_HOME) that runScenario's single pass over
// Scenario.Payloads has no seam for.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig/devconfigtest"
	"github.com/openbox-ai/openbox-shift-left/internal/client/fakecore"
)

// driveHookOK runs one native hook payload through the real entrypoint and
// requires it to exit 0 without panicking -- the same two checks
// runScenario's own loop makes, factored out because these tests interleave
// fakecore-knob changes between invocations that runScenario has no seam for.
func driveHookOK(t *testing.T, p fakecore.HookPayload) (stdout, stderr string) {
	t.Helper()
	a, out, errb := testApp(nil)
	a.stdin = strings.NewReader(p.JSON)
	if code := a.run([]string{"hook", "claude-code", p.Event}); code != exitOK {
		t.Fatalf("%s exit = %d; stderr=%q", p.Event, code, errb.String())
	}
	if panicked(errb.String()) {
		t.Fatalf("%s panicked: %s", p.Event, errb.String())
	}
	return out.String(), errb.String()
}

// permissionVerb reads the coding-agent-facing verb off one hook's stdout, ""
// for an allow (nothing rendered) -- the same decoding decodeDecision uses,
// trimmed to the one field these tests need.
func permissionVerb(t *testing.T, stdout string) string {
	t.Helper()
	out := strings.TrimSpace(stdout)
	if out == "" {
		return ""
	}
	var got struct {
		HookSpecificOutput struct {
			PermissionDecision string `json:"permissionDecision"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("stdout is not valid hook JSON: %v (%q)", err, out)
	}
	return got.HookSpecificOutput.PermissionDecision
}

// TestColdHookBootstrapsAndExchangesOnce: a hook with no warm cache on disk
// bootstraps once, exchanges once, and evaluates once -- never more (a retry
// loop that re-bootstrapped per attempt would pass a warm-path test but
// multiply every cold call by its retry count).
func TestColdHookBootstrapsAndExchangesOnce(t *testing.T) {
	fake := fakecore.New(t, fakecore.Script{})
	dir := t.TempDir()
	spool := filepath.Join(dir, "spool")
	evalEnv(t, fake, dir, spool, fakecore.Posture{})

	stdout, _ := driveHookOK(t, preBash("toolu_cold", "ls -la"))
	if v := permissionVerb(t, stdout); v != "" && v != "allow" {
		t.Errorf("a scripted ALLOW rendered %q", v)
	}

	if got := fake.BootstrapHits(); got != 1 {
		t.Errorf("bootstrap hits = %d, want exactly 1 for a cold hook", got)
	}
	if got := fake.ExchangeHits(); got != 1 {
		t.Errorf("token-exchange hits = %d, want exactly 1 for a cold hook", got)
	}
	if got := fake.V3EvaluateAttempts(); got != 1 {
		t.Errorf("evaluate attempts = %d, want exactly 1", got)
	}
	if n := len(fake.Inbox()); n != 1 {
		t.Errorf("inbox has %d entries, want exactly 1 (the tool call itself)", n)
	}
}

// TestWarmHookMakesOneEvaluateAndNoAuthCalls: once a hook has already
// populated the on-disk token cache, the next hook -- a separate process in
// production, a separate app/Client here, sharing the same OPENBOX_HOME --
// must reuse the cached bearer rather than re-running Keycloak's
// bootstrap/exchange dance.
func TestWarmHookMakesOneEvaluateAndNoAuthCalls(t *testing.T) {
	fake := fakecore.New(t, fakecore.Script{})
	dir := t.TempDir()
	spool := filepath.Join(dir, "spool")
	evalEnv(t, fake, dir, spool, fakecore.Posture{})

	// Pre-warm: one full cold hook populates the on-disk cache.
	driveHookOK(t, preBash("toolu_warm1", "ls -la"))
	bootstrapBefore := fake.BootstrapHits()
	exchangeBefore := fake.ExchangeHits()
	evaluateBefore := fake.V3EvaluateAttempts()
	if bootstrapBefore == 0 || exchangeBefore == 0 {
		t.Fatalf("the warm-up hook never went cold (bootstrap=%d exchange=%d); nothing is warmed yet", bootstrapBefore, exchangeBefore)
	}

	stdout, _ := driveHookOK(t, preBash("toolu_warm2", "git status"))
	if v := permissionVerb(t, stdout); v != "" && v != "allow" {
		t.Errorf("a scripted ALLOW rendered %q", v)
	}

	if got := fake.BootstrapHits() - bootstrapBefore; got != 0 {
		t.Errorf("a warm hook made %d bootstrap call(s), want 0", got)
	}
	if got := fake.ExchangeHits() - exchangeBefore; got != 0 {
		t.Errorf("a warm hook made %d token-exchange call(s), want 0", got)
	}
	if got := fake.V3EvaluateAttempts() - evaluateBefore; got != 1 {
		t.Errorf("a warm hook made %d evaluate call(s), want exactly 1", got)
	}
}

// TestKeycloakDownDeniesButDoesNotLatch: with Keycloak's token endpoint
// unreachable, a token-acquisition failure is a transient (network) delivery
// failure; client.Emit never reaches /evaluate on it, so it is never mistaken
// for a judged refusal. The first call denies (always fail-closed), but its
// escalation failure only requeues the call's record. The next call's drain
// sends that record once and retries it once; both fail, so the record is
// recorded (hookflow.RecordDeliveryFailure) and ledgered and gone -- but the
// run is NOT latched: a delivery failure no longer halts the run, only the
// gated call it belongs to is denied, and the NEXT call tries again. Because
// the failure is caught before any bearer exists, the binary must never fall
// back to an unauthenticated or legacy request: zero evaluate attempts, and
// nothing beyond the bootstrap route ever reaches the fake at all (so no
// /api/v1 path could have been hit either).
func TestKeycloakDownDeniesButDoesNotLatch(t *testing.T) {
	fake := fakecore.New(t, fakecore.Script{})
	fake.TokenEndpointDown()

	dir := t.TempDir()
	spool := filepath.Join(dir, "spool")
	evalEnv(t, fake, dir, spool, fakecore.Posture{})

	stdout, _ := driveHookOK(t, preBash("toolu_kcdown", "ls -la"))
	if v := permissionVerb(t, stdout); v != "deny" {
		t.Errorf("Keycloak being unreachable must deny now (delivery is always fail-closed), got permissionDecision=%q", v)
	}

	if got := fake.BootstrapHits(); got == 0 {
		t.Fatal("bootstrap was never even attempted; the scripted outage never happened")
	}
	if !spoolStillHolds(t, spool) {
		t.Error("a transient escalation failure must requeue the call's record for its one retry")
	}
	if n := countFiles(t, filepath.Join(dir, "halts")); n != 0 {
		t.Errorf("%d session halt latch(es) after the first call, want 0: its record has not had its retry yet", n)
	}

	stdout, _ = driveHookOK(t, preBash("toolu_kcdown2", "pwd"))
	if v := permissionVerb(t, stdout); v != "deny" {
		t.Errorf("the next call while Keycloak is still down must deny, got permissionDecision=%q", v)
	}
	if got := fake.V3EvaluateAttempts(); got != 0 {
		t.Errorf("evaluate attempts = %d, want 0: a token-acquisition failure must never reach /evaluate at all", got)
	}
	if got, want := fake.Hits(), fake.BootstrapHits(); got != want {
		t.Errorf("the fake saw %d total request(s) but only %d were bootstrap; something besides bootstrap reached the control plane while Keycloak was down", got, want)
	}
	if n := len(fake.Rejections()); n != 0 {
		t.Errorf("the fake refused %d request(s) (want 0, including zero \"v1 route\" rejections): %v", n, fake.Rejections())
	}
	// The first call's record failed its attempt and its one retry: it is
	// ledgered and gone. Whatever the spool still holds is the second call's
	// own record, queued when the halt denied it.
	ledger, _ := os.ReadFile(filepath.Join(spool, ".discarded"))
	if n := strings.Count(strings.TrimSpace(string(ledger)), "\n") + 1; len(strings.TrimSpace(string(ledger))) == 0 || n != 1 {
		t.Errorf("discard ledger has %q, want exactly one line for the first call's record", ledger)
	}
	if !spoolHoldsText(t, spool, `"toolu_kcdown2"`) {
		t.Fatal("the second call's record is not in the spool, so the check below could not see the first one either")
	}
	if spoolHoldsText(t, spool, `"toolu_kcdown"`) {
		t.Error("the first call's record is still queued; after its attempt and one retry it must be ledgered and gone")
	}
	if n := countFiles(t, filepath.Join(dir, "halts")); n != 0 {
		t.Errorf("%d session halt latch(es), want 0: an auth-plane failure that outlasts the retry no longer halts the run", n)
	}
}

// TestCached401InvalidatesThenRetriesColdButStillDenies: core answers a flat
// 401 for a cached-but-rejected token (an agent revoked after a token was
// issued). The client now retries a 401 exactly once with a force-refreshed
// token (client.Client.post): the cached token's 401 invalidates it and
// fetches a fresh one cold (one more bootstrap and exchange), but the agent
// is durably revoked, not merely holding a stale token, so the retry's own
// fresh token is rejected too and the call still denies (always fail-closed).
// That retry's own fetch succeeds and is cached, so -- unlike an
// unrecoverable rejection -- the cache file is NOT deleted; the next hook
// will cold-start from that freshly cached (but still-rejected) token rather
// than the truly dead one the warm-up call left behind. A 401 is classified
// EscalationUnanswered, not a proven refusal (RecordDeliveryFailure is never
// called for it), so the gate's own escalation requeues the observe copy
// (SpoolObserveHead) rather than ledgering it -- it stays in the spool for a
// later drain to retry -- and the run is NOT latched either way: nothing
// here denies more than the one call it belongs to. SessionEnd is not a
// gated hook, so it delivers its own (unrelated) queued event once the
// identity is good again, cold.
func TestCached401InvalidatesThenRetriesColdButStillDenies(t *testing.T) {
	fake := fakecore.New(t, fakecore.Script{})
	dir := t.TempDir()
	spool := filepath.Join(dir, "spool")
	evalEnv(t, fake, dir, spool, fakecore.Posture{})

	// Warm the cache.
	driveHookOK(t, preBash("toolu_pre-revoke", "ls -la"))
	bootstrapAfterWarm := fake.BootstrapHits()
	exchangeAfterWarm := fake.ExchangeHits()

	cachePath, err := devconfig.WorkloadTokenCachePathFor("claude-code")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cachePath); err != nil {
		t.Fatalf("no token cache file after the warm-up hook: %v", err)
	}

	fake.Revoke()
	stdout, _ := driveHookOK(t, preBash("toolu_revoked", "git status"))
	if v := permissionVerb(t, stdout); v != "deny" {
		t.Errorf("a token-rejection 401 must deny now (delivery is always fail-closed), got permissionDecision=%q", v)
	}
	if got := fake.BootstrapHits() - bootstrapAfterWarm; got != 1 {
		t.Errorf("the 401'd hook made %d extra bootstrap call(s), want exactly 1 (the client's own one cold retry on 401)", got)
	}
	if got := fake.ExchangeHits() - exchangeAfterWarm; got != 1 {
		t.Errorf("the 401'd hook made %d extra token-exchange call(s), want exactly 1 (the client's own one cold retry on 401)", got)
	}
	if _, err := os.Stat(cachePath); err != nil {
		t.Fatalf("the token cache file must still exist: the 401 retry's own fresh fetch succeeded and was cached, "+
			"even though the agent's durable revocation still rejected it at /evaluate (stat err=%v)", err)
	}
	if !spoolStillHolds(t, spool) {
		t.Error("a 401 is classified EscalationUnanswered and requeues its observe copy (SpoolObserveHead); it must still be in the spool, not ledgered and gone")
	}
	if n := countFiles(t, filepath.Join(dir, "halts")); n != 0 {
		t.Errorf("%d session halt latch(es), want 0: a delivery failure no longer halts the run", n)
	}

	// The identity is good again. SessionEnd is not gated, so it never reads
	// any latch: its own flush reuses the still-fresh cache the 401 retry's
	// own successful fetch left behind (unlike the old fail-open pin, that
	// cache was never deleted -- only a call that ends with NO usable cache
	// entry forces the next hook cold), now actually valid since the agent is
	// no longer revoked, to deliver its own queued lifecycle event (the
	// 401'd tool call left nothing behind to drain any more).
	fake.Unrevoke()
	bootstrapBeforeEnd := fake.BootstrapHits()
	exchangeBeforeEnd := fake.ExchangeHits()
	evaluateBeforeEnd := fake.V3EvaluateAttempts()

	driveHookOK(t, sessionEnd())

	if got := fake.BootstrapHits() - bootstrapBeforeEnd; got != 0 {
		t.Errorf("SessionEnd's flush made %d bootstrap call(s), want 0 (it reused the still-fresh cache)", got)
	}
	if got := fake.ExchangeHits() - exchangeBeforeEnd; got != 0 {
		t.Errorf("SessionEnd's flush made %d token-exchange call(s), want 0 (it reused the still-fresh cache)", got)
	}
	if got := fake.V3EvaluateAttempts() - evaluateBeforeEnd; got < 1 {
		t.Errorf("SessionEnd's flush made %d evaluate call(s), want at least 1", got)
	}
	if spoolStillHolds(t, spool) {
		t.Error("SessionEnd's own queued event did not drain once the identity was good again")
	}
}

// TestSpurious401IsRetriedColdAndStillSucceeds pins the owner-decided client
// behavior a genuinely revoked agent's 401 (above) must be told apart from: a
// single SPURIOUS 401 (core mapping a transient datastore error during agent
// lookup to 401 on an agent that was never actually revoked) followed by a
// 200 on the client's own one further cold retry must now succeed end to end
// -- the call allows, nothing denies, and no run is ever latched.
func TestSpurious401IsRetriedColdAndStillSucceeds(t *testing.T) {
	fake := fakecore.New(t, fakecore.Script{})
	dir := t.TempDir()
	spool := filepath.Join(dir, "spool")
	evalEnv(t, fake, dir, spool, fakecore.Posture{})

	// Warm the cache first, same as every other cached-token scenario here.
	driveHookOK(t, preBash("toolu_pre-spurious", "ls -la"))
	bootstrapAfterWarm := fake.BootstrapHits()
	exchangeAfterWarm := fake.ExchangeHits()

	fake.SpuriousUnauthorizedOnce()
	stdout, _ := driveHookOK(t, preBash("toolu_spurious", "git status"))
	if v := permissionVerb(t, stdout); v != "" && v != "allow" {
		t.Errorf("a spurious 401 absorbed by the client's own one cold retry must still allow, got permissionDecision=%q", v)
	}
	if got := fake.BootstrapHits() - bootstrapAfterWarm; got != 1 {
		t.Errorf("the retry made %d extra bootstrap call(s), want exactly 1 (the client's own one cold retry on 401)", got)
	}
	if got := fake.ExchangeHits() - exchangeAfterWarm; got != 1 {
		t.Errorf("the retry made %d extra token-exchange call(s), want exactly 1", got)
	}
	if spoolStillHolds(t, spool) {
		t.Error("a call that ultimately succeeded must not still be queued in the spool")
	}
	if n := countFiles(t, filepath.Join(dir, "halts")); n != 0 {
		t.Errorf("%d session halt latch(es), want 0: nothing failed here", n)
	}
}

// TestLegacyStoreGovernsNothingAndSaysSo pins devconfig's ordering: a legacy
// marker on disk (LegacyStoreFor's own detection) refuses BEFORE the healthy
// v3 secrets this test's own env carries are ever read, so the hook makes
// zero requests to the control plane and names the reason on stderr rather
// than rendering a decision.
func TestLegacyStoreGovernsNothingAndSaysSo(t *testing.T) {
	fake := fakecore.New(t, fakecore.Script{})
	dir := t.TempDir()
	spool := filepath.Join(dir, "spool")
	evalEnv(t, fake, dir, spool, fakecore.Posture{})
	// evalEnv points OPENBOX_CONFIG at a private org-level file so scenarios
	// never collide; clear it here so the bound hook reads this tool's own
	// ~/.openbox/claude-code/dev.json -- the file the legacy marker below is
	// written to, and the one production reads when no operator override is
	// set.
	t.Setenv(devconfig.EnvConfigPath, "")

	homeDir := filepath.Join(dir, "home")
	if err := devconfigtest.SetLegacyDID(filepath.Join(homeDir, "claude-code", "dev.json"), "did:aip:legacy-store"); err != nil {
		t.Fatal(err)
	}

	stdout, stderr := driveHookOK(t, preBash("toolu_legacy", "ls -la"))
	if stdout != "" {
		t.Fatalf("a legacy store's hook must render nothing, got %q", stdout)
	}
	if !strings.Contains(stderr, "no identity") {
		t.Errorf("expected a no-identity drop notice, got %q", stderr)
	}
	if n := fake.Hits(); n != 0 {
		t.Errorf("a legacy store made %d request(s) to the control plane; hooks send nothing for it, valid-looking env credentials notwithstanding", n)
	}
	entries, _ := os.ReadDir(spool)
	if len(entries) != 0 {
		t.Errorf("a legacy store's event reached the spool: %v", entries)
	}
}

// The git-action deploy-reaches-/api/v3 eval lives in
// cmd/openbox-git-action/workload_e2e_test.go, not here: it execs a real,
// separately-built openbox-git-action binary as a child process, which needs
// fakecore.NewReal (a real OS socket). This package already runs
// TestHookRealtimeDelivery, which also uses NewReal from a detached
// subprocess of its own -- adding a second real-socket, real-subprocess test
// here produced a reproducible data race (fakecore.Server.URL read/written
// unlocked) when a stray request from one test's child landed on the other
// test's freshly-bound, coincidentally-reused ephemeral port. The two tests
// never share a test binary or a port timeline once this one moves to
// cmd/openbox-git-action's own `go test` process, so the collision cannot
// occur there.

// spoolHoldsText reports whether any spooled .jsonl file under spool contains
// needle.
func spoolHoldsText(t *testing.T, spool, needle string) bool {
	t.Helper()
	entries, err := os.ReadDir(spool)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		b, _ := os.ReadFile(filepath.Join(spool, e.Name()))
		if strings.Contains(string(b), needle) {
			return true
		}
	}
	return false
}
