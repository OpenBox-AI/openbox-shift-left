package codex

import (
	"context"
	"io"
	"log"
	"os"
	"time"

	obgit "github.com/openbox-ai/openbox-shift-left/internal/adapters/common/git"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
	"github.com/openbox-ai/openbox-shift-left/internal/client"
	"github.com/openbox-ai/openbox-shift-left/internal/decision"
)

const flushBudget = 12 * time.Second

// RunHook executes the path for one Codex hook invocation; the engine behind
// the unified `openbox hook codex <event>` subcommand. Safety contract (INV-3;
// observe-only, never block; the default whole-product posture): - In observe
// mode (the default) it writes nothing to stdout.
func RunHook(sub string, stdin io.Reader, stdout io.Writer, logger *log.Logger) {
	defer devconfig.Pin()()
	defer func() {
		if r := recover(); r != nil {
			logger.Printf("recovered: %v", r)
		}
	}()

	if sub == "" {
		logger.Printf("usage: openbox hook codex <HookName|flush>")
		return
	}
	if sub == "flush" {
		runFlush(logger, os.Getenv(hookflow.EnvFlushSession))
		return
	}

	hook, err := ParseHookName(sub)
	if err != nil {
		logger.Printf("%v", err)
		return
	}

	id, err := ResolveIdentity()
	if err != nil {
		logger.Printf("no identity, dropping %s event: %v", hook, err)
		return
	}
	ev, err := ParseHookEvent(stdin)
	if err != nil {
		logger.Printf("dropping %s event: %v", hook, err)
		return
	}

	ad := New(id, DefaultSpoolDir())
	ad.Mapper.CaptureContent = ResolveContentCapture()
	ad.Mapper.ThreadID = os.Getenv(obgit.EnvCodexThreadID)
	pinnedNow := time.Now()
	ad.Mapper.Now = func() time.Time { return pinnedNow }

	// This is the only place transcript_path is opened, and only when
	// ResolveFinops() is set; with finops off it is never dereferenced.
	if hook == HookSessionEnd && ResolveFinops() {
		if tokens, model, err := readRolloutUsage(ev.TranscriptPath); err != nil {
			logger.Printf("finops: rollout usage skipped: %v", err)
		} else if tokens != nil {
			ad.Mapper.Finops = &FinopsUsage{Tokens: tokens, Model: model}
		}
	}

	if hook == HookSessionEnd {
		ad.Mapper.Evidence = &EvidenceState{Undelivered: ad.Spool.UndeliveredCount()}
	}

	if hook == HookSessionStart {
		posture := effectivePosture()
		ad.Mapper.Posture = &posture
	}

	nudgeFlush := func() {
		hookflow.RealtimeTrigger{Spool: ad.Spool, Provider: "codex"}.Maybe(logger, ev.SessionID)
	}

	// The two must agree: deciding to defer the observe copy here and then not
	// running the gate would drop the event.
	gated := hook == HookPreToolUse && ResolveEnforce()

	var spoolObserve func()
	if gated {
		if devEv, ok := ad.Mapper.Map(hook, ev); ok {
			appendObserve := ad.RecordDeferred(devEv)
			spoolObserve = func() {
				if err := appendObserve(); err != nil {
					logger.Printf("spool %s event: %v", hook, err)
				}
				nudgeFlush()
			}
		}
	} else {
		if _, err := ad.Observe(hook, ev); err != nil {
			logger.Printf("spool %s event: %v", hook, err)
		}
		if hook != HookSessionEnd {
			nudgeFlush()
		}
	}

	// Default off: with enforce off the decider is never invoked and this is
	// inert, so the observe path stays byte-identical to observe-only.
	if gated {
		g := hookflow.EnforceGate{
			Contract:     contract,
			Evaluator:    evaluator,
			Record:       func(dec decision.Decision, res hookflow.ApplyResult) { recordEnforcement(logger, ev, dec, res) },
			SpoolObserve: spoolObserve,
		}
		g.Run(context.Background(), logger, stdout, enforceTarget{id: id, mapper: ad.Mapper, ev: ev})
		return
	}

	// Never a blocking field (INV-3); categories/counts only (INV-2); PostToolUse
	// is stat-guarded.
	if hook == HookPostToolUse || hook == HookUserPromptSubmit {
		if ResolveFindings() {
			hookflow.SurfaceFindings("codex", string(hook), stdout, logger)
		}
	}

	if hook == HookSessionStart {
		maybeInstallGitHook(logger, ev.Cwd)
	}

	if hook == HookSessionEnd {
		if started, completed, ok := ad.Mapper.MapUsageRollup(ev); ok {
			for _, usageEv := range []client.DevEvent{started, completed} {
				if err := ad.Record(usageEv); err != nil {
					logger.Printf("finops: spool %s event: %v", usageEv.EventType, err)
					break
				}
			}
		}
	}

	if hook == HookSessionEnd {
		runFlush(logger, ev.SessionID)
	}
}

func maybeInstallGitHook(logger *log.Logger, cwd string) {
	if !ResolveInstallGitHook() {
		return
	}
	self, err := os.Executable()
	if err != nil || self == "" {
		return
	}
	hooksDir, err := obgit.Git{Dir: cwd}.HooksDirDefault()
	if err != nil {
		return // not a git repo / detached worktree — nothing to install into
	}
	cfg := obgit.HookConfig{Command: self, Args: []string{"hook", "git", "prepare-commit-msg"}}
	if err := obgit.InstallPostCommitHook(hooksDir, cfg); err != nil {
		logger.Printf("post-commit hook not installed (trailer still works): %v", err)
	}
	if err := obgit.InstallHook(hooksDir, cfg); err != nil {
		logger.Printf("git-hook install skipped: %v", err)
	}
}

func runFlush(logger *log.Logger, sessionID string) {
	creds, err := ResolveCredentials()
	if err != nil {
		logger.Printf("flush skipped (events remain spooled): %v", err)
		return
	}
	cl, err := creds.NewClient(logger)
	if err != nil {
		logger.Printf("flush skipped (client init): %v", err)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), flushBudget)
	defer cancel()

	ad := New(creds.Identity(), DefaultSpoolDir())
	// Diagnostics only; stderr, never stdout.
	ad.Advisory.Log = logger
	var n int
	if sessionID == "" {
		n, err = ad.FlushAll(ctx, cl)
	} else {
		ad.Spool.TouchFlushLock(sessionID)
		defer ad.Spool.ReleaseFlushLock(sessionID)
		n, err = ad.Flush(ctx, sessionID, cl)
	}
	if err != nil {
		logger.Printf("flush ended early after %d event(s): %v", n, err)
	}
}
