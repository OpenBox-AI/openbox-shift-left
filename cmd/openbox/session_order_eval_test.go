package main

// Ordering and halt evals that need infrastructure beyond a plain hook-
// payload list: a lane record sharing a session's own spool with a real
// hook event, a new run after a halt, and the measured budget runs (the
// binary half of the session-start-ordering claim lives in main_test.go;
// the backlog-budget runs are here).
//
// Cited rather than duplicated (already proven at unit/integration level,
// so this file does not re-prove what a lower layer already pins):
//   - A failed WorkflowStarted, or a HALT verdict, found while a gate drains
//     denies that gated call without ever escalating: hookflow's own
//     TestGate_DrainedDeliveryFailureDeniesWithoutEscalating and
//     TestGate_DrainedHaltVerdictDeniesWithoutEscalating, and end to end
//     against a real fakecore, claude-code's own
//     TestGateDrain_ExplicitRejectionInsideTheWindowHaltsAndDeniesThatCall.
//   - A slow core never halts a run from inside a gate's own drain step,
//     and the eventual redelivery is deduped by core's own idempotency key:
//     claude-code's own TestGateDrain_SlowCoreNeverHaltsTheRun.
//   - A chat event core does not accept latches that conversation, and the
//     next completion of that SAME conversation is refused: already proven
//     end to end through the transport relay's own emitter and haltDecorator
//     by TestChatEventCoreDoesNotAcceptLatchesTheConversation
//     (transportchat_test.go) -- it drives gatewayemit.Emitter ->
//     routeLaneRecord -> the chat pool -> HaltOnDeliveryFailure, then reads
//     haltDecorator.Evaluate for the SAME conversation, exactly the path
//     this file would otherwise have to reproduce.
//
//   - The timeout failure class -- one attempt, and the run latched through
//     the SAME Engine.NewEngine wiring the real 30s flusher/lane-drain path
//     uses -- is proven at hookflow level without paying the real 30s bound:
//     hookflow's own TestNewEngine_TimeoutClassLatchesInExactlyOneAttempt
//     (a short AttemptTimeout stands in for DeliveryAttemptTimeout) and
//     TestSessionOrder_OneAttemptPerFailureClass's own "timeout" row (one
//     attempt, the next line still attempted). A gate's own LIVE escalation
//     treats an unanswered attempt as EscalationUnanswered instead (never a
//     halt, by design), so only the detached-flusher/lane-drain path can
//     ever halt on a timeout, and that is exactly the wiring these two
//     hookflow tests exercise.
//
// The other five failure classes this client can hit trying to reach
// /evaluate (network, 5xx, 401, token-exchange-failure, 4xx) are covered in
// governanceeval_scenarios_test.go's own TestGovernanceEvalOneAttemptPerFailureClass.

import (
	"context"
	"fmt"
	"io"
	"log"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/gatewayemit"
	"github.com/openbox-ai/openbox-shift-left/internal/client"
	"github.com/openbox-ai/openbox-shift-left/internal/client/fakecore"
	"github.com/openbox-ai/openbox-shift-left/internal/client/memhttptest"
	"github.com/openbox-ai/openbox-shift-left/internal/transport"
)

// TestLaneRecordAndAGatedHookOrderThroughTheSameSpool: a SessionStart's
// own WorkflowStarted event -- seeded as if an earlier, still-in-flight hook
// process had already queued it, core holding its own answer for 300ms
// (DelayFor) -- and a relayed model call through a LaneQueue-backed
// gatewayemit.Emitter, and a real PreToolUse hook, are driven CONCURRENTLY
// against the SAME session's own cc-spool. Whichever of the two drainers
// (the PreToolUse hook's own gate drain, or the lane's own Kick-triggered
// drain) actually wins the session's stripe, WorkflowStarted is always
// delivered first: it was appended to the spool before either concurrent
// producer's own action began, and delivery order is append order,
// regardless of which drainer performs it. The lane's own Started row still
// precedes its Completed row, the ordinary in-lane pairing ordering never
// changes.
func TestLaneRecordAndAGatedHookOrderThroughTheSameSpool(t *testing.T) {
	memhttptest.RequireBind(t)

	t.Setenv(devconfig.EnvSpoolRoot, t.TempDir())
	t.Setenv(devconfig.EnvHome, t.TempDir())
	t.Setenv(devconfig.EnvHaltDir, t.TempDir())
	t.Setenv(devconfig.EnvEnforcementFile, filepath.Join(t.TempDir(), "enforcements.jsonl"))
	t.Setenv(devconfig.EnvPendingApprovalDir, t.TempDir())
	t.Setenv("OPENBOX_ADVISORY_FILE", filepath.Join(t.TempDir(), "advisories.jsonl"))
	t.Setenv("OPENBOX_SESSION_DIR", t.TempDir())
	t.Setenv(devconfig.EnvRealtime, "0")
	t.Setenv(devconfig.EnvContentCapture, "0")

	fake := fakecore.New(t, fakecore.Script{
		DelayFor: map[string]time.Duration{fakecore.WireWorkflowStarted: 300 * time.Millisecond},
	})
	t.Setenv(devconfig.EnvBaseURL, fake.URL())
	seedV3EnvIdentity(t)

	const sessionID = "sess-lane-and-hook-order"
	hookSpool := hookflow.Spool{Dir: devconfig.SpoolDir("cc-spool")}
	if err := hookSpool.Append(client.DevEvent{
		SchemaVersion: client.SchemaVersion,
		EventID:       "racing-hook-session-started",
		EventType:     client.EventSessionStarted,
		SessionID:     sessionID,
		DeveloperDID:  devconfig.ResolveDIDOrEmpty(),
		Tool:          client.Tool{Name: "claude-code", Kind: client.ToolShell},
		Timestamp:     "2026-09-24T00:00:00Z",
	}); err != nil {
		t.Fatalf("seeding the racing SessionStart's own event: %v", err)
	}

	logger := log.New(io.Discard, "", 0)
	identities := resolveProviderIdentities(logger.Printf)
	queues := laneQueues(identities, logger)

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

	done := make(chan struct{}, 2)
	go func() {
		defer func() { done <- struct{}{} }()
		a, _, errb := testApp(nil)
		a.stdin = strings.NewReader(`{"hook_event_name":"PreToolUse","session_id":"` + sessionID + `","cwd":"/repo","tool_name":"Bash","tool_use_id":"racing-tool-call","tool_input":{"command":"ls"}}`)
		if code := a.run([]string{"hook", "claude-code", "PreToolUse"}); code != exitOK {
			t.Errorf("PreToolUse exit = %d; stderr=%q", code, errb.String())
		}
	}()
	go func() {
		defer func() { done <- struct{}{} }()
		serveOneCall(t, p, ca, sessionID)
	}()
	<-done
	<-done

	deadline := time.Now().Add(10 * time.Second)
	var inbox []fakecore.Received
	for time.Now().Before(deadline) {
		inbox = fake.Inbox()
		if len(inbox) >= 4 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(inbox) < 4 {
		t.Fatalf("fakecore received %d event(s), want at least 4 (the seeded WorkflowStarted, the PreToolUse call's own ActivityStarted, the lane's own Started/Completed pair); rejections=%v", len(inbox), fake.Rejections())
	}
	if got := inbox[0].EventType(); got != fakecore.WireWorkflowStarted {
		t.Fatalf("first row = %q, want WorkflowStarted: it was queued before either concurrent producer acted, and delivery order is append order", got)
	}

	var laneStarted, laneCompleted int = -1, -1
	for i, r := range inbox {
		if at, _ := r.Body["activity_type"].(string); at == "llm_completion" {
			switch r.EventType() {
			case fakecore.WireActivityStarted:
				if laneStarted == -1 {
					laneStarted = i
				}
			case fakecore.WireActivityCompleted:
				if laneCompleted == -1 {
					laneCompleted = i
				}
			}
		}
	}
	if laneStarted == -1 || laneCompleted == -1 {
		t.Fatalf("the lane's own model-call pair never reached core (rows: %d)", len(inbox))
	}
	if laneStarted >= laneCompleted {
		t.Errorf("lane Started arrived at index %d, Completed at %d; Started must precede Completed", laneStarted, laneCompleted)
	}
}

// TestGovernanceEvalNewRunAfterAHaltIsUnhaltedButCodexResumeStaysHalted:
// Claude Code's own /clear mints an entirely new session id, so a run halted
// under the old one is trivially unhalted under the new one; --resume keeps
// the SAME session id but bumps the run's own generation (RunStore.Bump),
// and the halt latch is keyed on the RUN id (WriteSessionHalt/SessionHalted),
// so the bumped run is unhalted too. Codex has no bump mechanism at all --
// every hook keys on the raw session id (its own gate sets no RunID) -- so a
// Codex session that fires SessionStart again under the SAME session id
// stays halted.
func TestGovernanceEvalNewRunAfterAHaltIsUnhaltedButCodexResumeStaysHalted(t *testing.T) {
	t.Run("claude-code-clear-mints-a-new-session-unhalted", func(t *testing.T) {
		const oldSession, newSession = "clear-old-session", "clear-new-session"
		sc := fakecore.Scenario{
			Name: "cc-clear-after-a-halt-is-unhalted-because-it-mints-a-new-session-id",
			Payloads: []fakecore.HookPayload{
				hook("SessionStart", `{"hook_event_name":"SessionStart","session_id":"`+oldSession+`","cwd":"/repo","source":"startup"}`),
				hook("PreToolUse", `{"hook_event_name":"PreToolUse","session_id":"`+oldSession+`","cwd":"/repo","tool_name":"Bash","tool_use_id":"toolu_before_clear","tool_input":{"command":"ls"}}`),
				hook("SessionEnd", `{"hook_event_name":"SessionEnd","session_id":"`+oldSession+`","cwd":"/repo","reason":"clear"}`),
				hook("SessionStart", `{"hook_event_name":"SessionStart","session_id":"`+newSession+`","cwd":"/repo","source":"clear"}`),
				hook("PreToolUse", `{"hook_event_name":"PreToolUse","session_id":"`+newSession+`","cwd":"/repo","tool_name":"Bash","tool_use_id":"toolu_after_clear","tool_input":{"command":"ls"}}`),
				hook("SessionEnd", `{"hook_event_name":"SessionEnd","session_id":"`+newSession+`","cwd":"/repo","reason":"other"}`),
			},
			Denied:       map[string]bool{"toolu_before_clear": true},
			OutageDuring: func(i int, _ string) bool { return i == 0 },
			Provenance:   authoredProvenance,
		}
		run := runScenario(t, sc)
		old, ok := run.DecisionFor("toolu_before_clear")
		if !ok || old.Verb != "deny" {
			t.Fatalf("the old session's gated call = %+v, want a deny (its own session start failed)", old)
		}
		fresh, ok := run.DecisionFor("toolu_after_clear")
		if !ok || fresh.Verb != "" {
			t.Fatalf("the NEW session's gated call = %+v, want a silent allow: /clear mints a new session id, unrelated to the old halt", fresh)
		}
	})

	t.Run("claude-code-resume-bumps-the-run-unhalted", func(t *testing.T) {
		const sessionID = "resume-bumped-run-session"
		sc := fakecore.Scenario{
			Name: "cc-resume-after-a-halt-is-unhalted-because-the-latch-is-keyed-on-the-bumped-run",
			Payloads: []fakecore.HookPayload{
				hook("SessionStart", `{"hook_event_name":"SessionStart","session_id":"`+sessionID+`","cwd":"/repo","source":"startup"}`),
				hook("PreToolUse", `{"hook_event_name":"PreToolUse","session_id":"`+sessionID+`","cwd":"/repo","tool_name":"Bash","tool_use_id":"toolu_before_resume","tool_input":{"command":"ls"}}`),
				hook("SessionEnd", `{"hook_event_name":"SessionEnd","session_id":"`+sessionID+`","cwd":"/repo","reason":"other"}`),
				hook("SessionStart", `{"hook_event_name":"SessionStart","session_id":"`+sessionID+`","cwd":"/repo","source":"resume"}`),
				hook("PreToolUse", `{"hook_event_name":"PreToolUse","session_id":"`+sessionID+`","cwd":"/repo","tool_name":"Bash","tool_use_id":"toolu_after_resume","tool_input":{"command":"ls"}}`),
				hook("SessionEnd", `{"hook_event_name":"SessionEnd","session_id":"`+sessionID+`","cwd":"/repo","reason":"other"}`),
			},
			Denied:       map[string]bool{"toolu_before_resume": true},
			OutageDuring: func(i int, _ string) bool { return i == 0 },
			Provenance:   authoredProvenance,
		}
		run := runScenario(t, sc)
		before, ok := run.DecisionFor("toolu_before_resume")
		if !ok || before.Verb != "deny" {
			t.Fatalf("before resume = %+v, want a deny", before)
		}
		after, ok := run.DecisionFor("toolu_after_resume")
		if !ok || after.Verb != "" {
			t.Fatalf("after --resume = %+v, want a silent allow: the bumped run is a fresh, unlatched generation", after)
		}
	})

	t.Run("codex-resume-of-a-halted-session-stays-halted", func(t *testing.T) {
		const sessionID = "codex-resume-session"
		sc := fakecore.Scenario{
			Name:     "codex-resuming-a-halted-session-stays-halted-no-bump-mechanism-exists",
			Provider: "codex",
			Payloads: []fakecore.HookPayload{
				hook("SessionStart", `{"hook_event_name":"SessionStart","session_id":"`+sessionID+`","cwd":"/repo","model":"gpt-5.3-codex","permission_mode":"default","source":"startup"}`),
				hook("PreToolUse", `{"hook_event_name":"PreToolUse","session_id":"`+sessionID+`","turn_id":"turn-1","cwd":"/repo","model":"gpt-5.3-codex","permission_mode":"default","tool_name":"Bash","tool_use_id":"toolu_codex_before_resume","tool_input":{"command":"ls"}}`),
				hook("SessionEnd", `{"hook_event_name":"SessionEnd","session_id":"`+sessionID+`","cwd":"/repo","reason":"other"}`),
				// Codex's own "resume": SessionStart fires again for the SAME
				// session id. No bump mechanism exists for it at all.
				hook("SessionStart", `{"hook_event_name":"SessionStart","session_id":"`+sessionID+`","cwd":"/repo","model":"gpt-5.3-codex","permission_mode":"default","source":"startup"}`),
				hook("PreToolUse", `{"hook_event_name":"PreToolUse","session_id":"`+sessionID+`","turn_id":"turn-2","cwd":"/repo","model":"gpt-5.3-codex","permission_mode":"default","tool_name":"Bash","tool_use_id":"toolu_codex_after_resume","tool_input":{"command":"pwd"}}`),
				hook("SessionEnd", `{"hook_event_name":"SessionEnd","session_id":"`+sessionID+`","cwd":"/repo","reason":"other"}`),
			},
			Denied:       map[string]bool{"toolu_codex_before_resume": true, "toolu_codex_after_resume": true},
			OutageDuring: func(i int, _ string) bool { return i == 0 },
			Provenance:   authoredProvenance,
		}
		run := runScenario(t, sc)
		before, ok := run.DecisionFor("toolu_codex_before_resume")
		if !ok || before.Verb != "deny" {
			t.Fatalf("before resume = %+v, want a deny", before)
		}
		after, ok := run.DecisionFor("toolu_codex_after_resume")
		if !ok || after.Verb != "deny" {
			t.Fatalf("after resume = %+v, want STILL a deny: Codex has no run-bump mechanism", after)
		}
		grade(t, fakecore.HaltedAfterFailure(), sc, run.Run)
	})
}

// TestGatedVerdictWithinBudgetUnderBacklog: a 50-record backlog (~64KB)
// already queued for a session, a cold (uncached) workload token, and a 20ms
// per-request hold, still let that SAME session's next gated call render a
// verdict comfortably inside its own hook budget: the gate's own drain step
// clears the backlog (within its slack) before ever reaching its own
// escalation. Measured, not merely asserted: p50/max wall time logged over
// 10 runs, with a hard per-run ceiling well under a 29s bound.
func TestGatedVerdictWithinBudgetUnderBacklog(t *testing.T) {
	if testing.Short() {
		t.Skip("10 runs x a 50-record backlog; skipped in -short")
	}
	const runs = 10
	const backlogSize = 50
	// ~1.3KB/record x 50 ~= 64KB aggregate, the mission's own sizing.
	padding := strings.Repeat("x", 1200)

	wallTimes := make([]time.Duration, 0, runs)
	for run := 0; run < runs; run++ {
		fake := fakecore.New(t, fakecore.Script{Delay: 20 * time.Millisecond})
		dir := t.TempDir()
		spool := filepath.Join(dir, "spool")
		evalEnv(t, fake, dir, spool, fakecore.Posture{})

		const sessionID = "backlog-session"
		seedSpool := hookflow.Spool{Dir: spool}
		for i := 0; i < backlogSize; i++ {
			ev := client.DevEvent{
				SchemaVersion: client.SchemaVersion,
				EventID:       fmt.Sprintf("backlog-event-%d-%d", run, i),
				EventType:     client.EventToolCall,
				SessionID:     sessionID,
				DeveloperDID:  devconfig.ResolveDIDOrEmpty(),
				Tool:          client.Tool{Name: "Bash", Kind: client.ToolShell},
				Timestamp:     "2026-09-24T00:00:00Z",
				Content:       &client.Content{ToolInput: padding},
			}
			if err := seedSpool.Append(ev); err != nil {
				t.Fatalf("run %d: seeding backlog event %d: %v", run, i, err)
			}
		}

		a, out, errb := testApp(nil)
		a.stdin = strings.NewReader(`{"hook_event_name":"PreToolUse","session_id":"` + sessionID + `","cwd":"/repo","tool_name":"Bash","tool_use_id":"toolu_gated_call","tool_input":{"command":"ls"}}`)
		start := time.Now()
		if code := a.run([]string{"hook", "claude-code", "PreToolUse"}); code != exitOK {
			t.Fatalf("run %d: PreToolUse exit = %d; stderr=%q", run, code, errb.String())
		}
		elapsed := time.Since(start)
		if panicked(errb.String()) {
			t.Fatalf("run %d: panicked: %s", run, errb.String())
		}

		d := decodeDecision(t, 0, fakecore.HookPayload{Event: "PreToolUse"}, out.String())
		if d.Verb != "" {
			t.Errorf("run %d: gated call rendered %q, want a silent allow", run, d.Verb)
		}
		const ceiling = 29 * time.Second
		if elapsed >= ceiling {
			t.Errorf("run %d: gated verdict took %v, want < %v", run, elapsed, ceiling)
		}

		inbox := fake.Inbox()
		if len(inbox) < backlogSize+1 {
			t.Fatalf("run %d: fakecore received %d event(s), want at least %d (the backlog plus the gated call's own row)", run, len(inbox), backlogSize+1)
		}
		last := inbox[len(inbox)-1]
		if last.ToolUseID() != "toolu_gated_call" {
			t.Errorf("run %d: the last row delivered is not the gated call's own; the backlog did not precede it", run)
		}
		for _, r := range inbox[:backlogSize] {
			if r.ToolUseID() == "toolu_gated_call" {
				t.Errorf("run %d: the gated call's own row arrived inside the backlog's own span, not after it", run)
			}
		}

		wallTimes = append(wallTimes, elapsed)
	}

	p50, _, maxT := wallPercentiles(wallTimes)
	t.Logf("gated-verdict-under-backlog: wall times over %d runs: %v; p50=%v max=%v", runs, wallTimes, p50, maxT)
}

// wallPercentiles returns p50/p95/max over samples (sorted on an internal
// copy; the caller's own slice is never reordered), shared by both budget
// tests in this file. At n=5 (the stripe-held variant's own runs constant),
// p95 and max land on the SAME sample -- index 4 either way -- which is
// expected, not a bug: a genuinely distinct p95 needs more runs than 5.
func wallPercentiles(samples []time.Duration) (p50, p95, max time.Duration) {
	sorted := append([]time.Duration(nil), samples...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	p50 = sorted[len(sorted)/2]
	p95 = sorted[int(float64(len(sorted))*0.95)]
	max = sorted[len(sorted)-1]
	return
}

// TestGatedVerdictWhileAFlusherHoldsTheStripeOnASlowEvent (the stripe-held
// variant): a background drainer (standing in for a live detached flusher)
// holds the SAME session's own stripe on a single slow (10s-held) event
// while a gated call for that session is made. The gate's own drain step
// (Block mode) waits to ACQUIRE that held stripe, but never past
// hookflow.MaxStripeWait (5s): a measured p95 above 5s here is exactly the
// case that cap exists for, so the gated
// call proceeds to its own escalation once the stripe frees OR the wait
// bound elapses, whichever comes first -- never blocked anywhere near the
// full 10s hold. Measured: gated-call wall p50/p95/max over several runs.
func TestGatedVerdictWhileAFlusherHoldsTheStripeOnASlowEvent(t *testing.T) {
	if testing.Short() {
		t.Skip("holds a session's stripe for 10s per run; skipped in -short")
	}
	const runs = 5
	const holdSeconds = 10

	wallTimes := make([]time.Duration, 0, runs)
	for run := 0; run < runs; run++ {
		fake := fakecore.New(t, fakecore.Script{
			DelayFor: map[string]time.Duration{fakecore.WireWorkflowStarted: holdSeconds * time.Second},
		})
		dir := t.TempDir()
		spool := filepath.Join(dir, "spool")
		evalEnv(t, fake, dir, spool, fakecore.Posture{})

		sessionID := fmt.Sprintf("stripe-held-session-%d", run)
		seedSpool := hookflow.Spool{Dir: spool}
		if err := seedSpool.Append(client.DevEvent{
			SchemaVersion: client.SchemaVersion,
			EventID:       fmt.Sprintf("stripe-held-slow-event-%d", run),
			EventType:     client.EventSessionStarted,
			SessionID:     sessionID,
			DeveloperDID:  devconfig.ResolveDIDOrEmpty(),
			Tool:          client.Tool{Name: "claude-code", Kind: client.ToolShell},
			Timestamp:     "2026-09-24T00:00:00Z",
		}); err != nil {
			t.Fatalf("run %d: seeding the slow event: %v", run, err)
		}

		// Take the stripe first, in-process, standing in for a live detached
		// flusher's own held drain: the SAME Spool.DrainSession (Block mode,
		// the 30s attempt bound every non-inline drainer uses) that a real
		// flush subcommand would run.
		engine := hookflow.NewEngine(spool)
		creds, err := devconfig.ResolveCredentialsFor("claude-code")
		if err != nil {
			t.Fatalf("run %d: resolve credentials: %v", run, err)
		}
		clientCfg := client.Config{
			BaseURL:               creds.BaseURL,
			WorkloadPrivateKey:    creds.WorkloadPrivateKey,
			TokenCachePath:        creds.TokenCachePath,
			ContentCaptureEnabled: creds.ContentCaptureEnabled,
			MaxRetries:            &zeroLaneRetries,
		}
		key := creds.APIKey
		clientCfg.APIKey = key
		client0, err := client.New(clientCfg)
		if err != nil {
			t.Fatalf("run %d: build client: %v", run, err)
		}
		drainStarted := make(chan struct{})
		drainDone := make(chan struct{})
		go func() {
			close(drainStarted)
			ctx, cancel := context.WithTimeout(context.Background(), hookflow.DeliveryAttemptTimeout+5*time.Second)
			defer cancel()
			_, _ = engine.DrainSession(ctx, sessionID, client0, hookflow.DrainOptions{
				Mode: hookflow.Block, AttemptTimeout: hookflow.DeliveryAttemptTimeout,
			})
			close(drainDone)
		}()
		<-drainStarted // only proves the goroutine itself has started, not the lock
		// Real synchronization on the stripe actually being held: DrainSession
		// takes the session's own stripe BEFORE it ever calls Emit and holds
		// it across the whole attempt (including the delayed response), so
		// fakecore recording the attempt (V3EvaluateAttempts, incremented at
		// the top of its own handler, before the scripted hold is applied)
		// can only happen once the lock is already taken. No fixed sleep
		// guessing at timing.
		attemptDeadline := time.Now().Add(5 * time.Second)
		for fake.V3EvaluateAttempts() == 0 {
			if time.Now().After(attemptDeadline) {
				t.Fatalf("run %d: the background drain never reached /evaluate at all within 5s", run)
			}
			time.Sleep(5 * time.Millisecond)
		}

		a, out, errb := testApp(nil)
		a.stdin = strings.NewReader(`{"hook_event_name":"PreToolUse","session_id":"` + sessionID + `","cwd":"/repo","tool_name":"Bash","tool_use_id":"toolu_stripe_gated_call","tool_input":{"command":"ls"}}`)
		start := time.Now()
		if code := a.run([]string{"hook", "claude-code", "PreToolUse"}); code != exitOK {
			t.Fatalf("run %d: PreToolUse exit = %d; stderr=%q", run, code, errb.String())
		}
		elapsed := time.Since(start)
		if panicked(errb.String()) {
			t.Fatalf("run %d: panicked: %s", run, errb.String())
		}
		d := decodeDecision(t, 0, fakecore.HookPayload{Event: "PreToolUse"}, out.String())
		if d.Verb != "" {
			t.Errorf("run %d: gated call rendered %q, want a silent allow: a busy stripe must never halt or deny", run, d.Verb)
		}
		// The gate's own wait for a busy stripe is capped (hookflow.MaxStripeWait):
		// a busy stripe on a held event must never make a gated call wait
		// anywhere near the event's own full hold (here 10s) -- the cap is
		// the response to a stripe-held p95 measured above 5s.
		const ceiling = 5500 * time.Millisecond
		if elapsed >= ceiling {
			t.Errorf("run %d: gated call took %v while the stripe was held, want < %v (the drain slack cap)", run, elapsed, ceiling)
		}
		wallTimes = append(wallTimes, elapsed)
		<-drainDone
	}

	p50, p95, maxT := wallPercentiles(wallTimes)
	t.Logf("gated-verdict-while-stripe-held: wall times over %d runs: %v; p50=%v p95=%v max=%v", runs, wallTimes, p50, p95, maxT)
}
