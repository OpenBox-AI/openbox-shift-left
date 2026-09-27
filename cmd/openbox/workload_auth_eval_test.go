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

// TestKeycloakDownDeniesAndHaltsTheRun: with Keycloak's token endpoint
// unreachable, a token-acquisition failure is a transient (network) delivery
// failure; client.Emit never reaches /evaluate on it, so it is never mistaken
// for a judged refusal. The first call denies (always fail-closed), but its
// escalation failure only requeues the call's record. The next call's drain
// sends that record once and retries it once; both fail, so the run halts
// (HaltOnDeliveryFailure) and the record is ledgered and gone. Because the
// failure is caught before any bearer exists, the binary must never fall
// back to an unauthenticated or legacy request: zero evaluate attempts, and
// nothing beyond the bootstrap route ever reaches the fake at all (so no
// /api/v1 path could have been hit either).
func TestKeycloakDownDeniesAndHaltsTheRun(t *testing.T) {
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
	if n := countFiles(t, filepath.Join(dir, "halts")); n != 1 {
		t.Errorf("%d session halt latch(es), want exactly 1: an auth-plane failure that outlasts the retry halts the run", n)
	}
}

// TestCached401InvalidatesThenNextHookGoesCold: core answers a flat 401 for a
// cached-but-rejected token (an agent revoked after a token was issued),
// that 401 is never resent, and the cache file is deleted so the NEXT hook
// goes cold rather than replaying the same dead bearer forever. Delivery is
// always fail-closed now: the 401'd call denies and the run
// halts (HaltOnDeliveryFailure); single-attempt delivery has no carry-over,
// so the event is ledgered and gone, never held in the spool for a later
// drain -- superseding the old fail-open pin. SessionEnd is not a gated
// hook, so it is unaffected by the latch and still delivers its own
// (unrelated) queued event once the identity is good again, cold.
func TestCached401InvalidatesThenNextHookGoesCold(t *testing.T) {
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
	if got := fake.BootstrapHits() - bootstrapAfterWarm; got != 0 {
		t.Errorf("the 401'd hook re-bootstrapped (delta=%d); it should have used the warm cache and only discovered the rejection at /evaluate", got)
	}
	if got := fake.ExchangeHits() - exchangeAfterWarm; got != 0 {
		t.Errorf("the 401'd hook re-exchanged a token (delta=%d); it should have used the warm cache", got)
	}
	if _, err := os.Stat(cachePath); !os.IsNotExist(err) {
		t.Fatalf("the token cache file still exists after a 401 on a cached token; the next hook will replay the same dead bearer (stat err=%v)", err)
	}
	if spoolStillHolds(t, spool) {
		t.Error("single-attempt delivery has no carry-over: the 401'd call's one attempt failed, so it must be ledgered and gone, not held in the spool")
	}
	if n := countFiles(t, filepath.Join(dir, "halts")); n != 1 {
		t.Errorf("%d session halt latch(es), want exactly 1: a token-rejection 401 halts the run", n)
	}

	// The identity is good again. SessionEnd is not gated, so it never reads
	// the latch above: its own flush still goes cold -- bootstrap, exchange
	// and evaluate exactly once each -- to deliver its own queued lifecycle
	// event (the 401'd tool call left nothing behind to drain any more).
	fake.Unrevoke()
	bootstrapBeforeEnd := fake.BootstrapHits()
	exchangeBeforeEnd := fake.ExchangeHits()
	evaluateBeforeEnd := fake.V3EvaluateAttempts()

	driveHookOK(t, sessionEnd())

	if got := fake.BootstrapHits() - bootstrapBeforeEnd; got != 1 {
		t.Errorf("SessionEnd's flush made %d bootstrap call(s), want exactly 1 (cold, cache was deleted)", got)
	}
	if got := fake.ExchangeHits() - exchangeBeforeEnd; got != 1 {
		t.Errorf("SessionEnd's flush made %d token-exchange call(s), want exactly 1", got)
	}
	if got := fake.V3EvaluateAttempts() - evaluateBeforeEnd; got < 1 {
		t.Errorf("SessionEnd's flush made %d evaluate call(s), want at least 1", got)
	}
	if spoolStillHolds(t, spool) {
		t.Error("SessionEnd's own queued event did not drain once the identity was good again")
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
