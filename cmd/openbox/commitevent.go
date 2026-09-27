package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"slices"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	obgit "github.com/openbox-ai/openbox-shift-left/internal/adapters/common/git"
	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/providers"
	"github.com/openbox-ai/openbox-shift-left/internal/client"
	"github.com/openbox-ai/openbox-shift-left/internal/provider"
)

// newCommitSink builds the post-commit hook's CommitCreated producer:
// for each session the git package resolved, it
// decides whether an agent tool's own marker actually produced this commit
// (R2), resolves that tool's identity (R3), reads its current run and skips a
// halted one (R4), and appends one event into that tool's own spool before
// nudging the realtime flusher (R5). getenv is the same source
// attestProvider reads its marker from; logger is the git-hook engine's own
// logger, so a skip reason lands on the same stderr stream as every other
// git-hook diagnostic.
//
// This never touches the network (R8) and never fails a commit (R7): every
// branch below is a log line and a return, the same fail-open shape the
// git-hook engine uses everywhere else.
func newCommitSink(getenv func(string) string, logger *log.Logger) obgit.CommitSink {
	return func(facts obgit.CommitFacts, sessions []obgit.ResolvedSession) {
		marker := attestProvider(getenv)
		for _, rs := range sessions {
			emitCommitEvent(marker, facts, rs, logger)
		}
	}
}

// emitCommitEvent handles exactly one resolved session. A session id that
// fails ValidateSessionID is silently skipped: the trailer's own validation
// already treats that the same way, and a commit event must never be more
// permissive than the artifact it exists to corroborate.
func emitCommitEvent(marker string, facts obgit.CommitFacts, rs obgit.ResolvedSession, logger *log.Logger) {
	if err := obgit.ValidateSessionID(rs.ID); err != nil {
		return
	}

	tool, reason := routeCommitTool(marker, rs.Tool)
	if tool == "" {
		logger.Printf("commit event skipped for session %s: %s", rs.ID, reason)
		return
	}

	release, err := devconfig.BindProvider(tool)
	if err != nil {
		logger.Printf("commit event skipped for session %s: identity store unavailable: %v", rs.ID, err)
		return
	}
	defer release()

	did, err := devconfig.ResolveDID()
	if err != nil {
		logger.Printf("commit event skipped for session %s: no credentials for %s: %v", rs.ID, tool, err)
		return
	}

	regDir := obgit.DefaultSessionDir()
	runStore := obgit.RunStore{Dir: obgit.RunDir(regDir)}
	run, err := runStore.Read(rs.ID)
	if err != nil {
		logger.Printf("commit event: run identity unreadable for session %s, continuing at generation 0: %v", rs.ID, err)
		run = obgit.RunRecord{}
	}
	// The same selection client.runIDFor makes: the minted run id when one
	// exists, else the bare session id (generation 0).
	runKey := rs.ID
	if run.RunID != "" {
		runKey = run.RunID
	}

	if _, halted := hookflow.SessionHalted(runKey); halted {
		logger.Printf("commit event skipped for session %s: run is halted", rs.ID)
		return
	}

	ev := client.DevEvent{
		SchemaVersion: client.SchemaVersion,
		EventID:       commitEventID(rs.ID, runKey, facts.SHA),
		EventType:     client.EventCommitCreated,
		SessionID:     rs.ID,
		RunID:         run.RunID,
		RunGeneration: run.Generation,
		DeveloperDID:  did,
		Timestamp:     time.Now().UTC().Format(time.RFC3339),
		Tool:          client.Tool{Name: "git", Kind: client.ToolShell},
		Metadata:      commitEventMetadata(facts, rs.ID),
	}

	spool := hookflow.Spool{Dir: providers.SpoolDirFor(tool)}
	if err := spool.Append(ev); err != nil {
		logger.Printf("commit event skipped for session %s: spool append failed: %v", rs.ID, err)
		return
	}
	hookflow.RealtimeTrigger{Spool: spool, Provider: tool}.Maybe(logger, rs.ID)
}

// routeCommitTool is R2, "agent commits only": an event is emitted only when
// the tool marker attestProvider found agrees with the tool the session
// itself resolved from (git.ResolvedSession.Tool) -- never derived from the
// marker alone, and never from the session alone.
//
//   - marker == "" -> no marker at all: a hand commit in a governed worktree.
//   - sessionTool == "" -> a session-env override (not attributable to a
//     tool) or a registry record written before the Tool field existed.
//   - marker != sessionTool -> the two disagree; a wrong guess here would
//     drain the event under another agent's client (see the plan's risk
//     table), so this is a skip, never a best-effort pick of either side.
//   - anything else not in provider.Supported() -> an unrecognized tool name,
//     defensive against a future marker/record value this binary does not
//     know how to bind.
func routeCommitTool(marker, sessionTool string) (tool, reason string) {
	if marker == "" {
		return "", "no tool marker present (hand commit)"
	}
	if sessionTool == "" {
		return "", "session has no recorded tool"
	}
	if marker != sessionTool {
		return "", fmt.Sprintf("tool marker %q disagrees with the session's own tool %q", marker, sessionTool)
	}
	if !slices.Contains(provider.Supported(), marker) {
		return "", fmt.Sprintf("unknown tool %q", marker)
	}
	return marker, ""
}

// commitEventMetadata is R6's metadata contract: structural commit identity
// only, no message/diff/patch body, plus the trailer's own session id so core
// can bind this leaf to a resumed run's claim (a resumed run's wire run_id is
// a minted UUID that never appears in the trailer).
func commitEventMetadata(facts obgit.CommitFacts, sessionID string) map[string]any {
	parents := facts.Parents
	if parents == nil {
		parents = []string{}
	}
	meta := map[string]any{
		"commit_sha":         facts.SHA,
		"parent_shas":        parents,
		"repo":               facts.Repo,
		"openbox_session_id": sessionID,
	}
	if facts.Tree != "" {
		meta["tree_sha"] = facts.Tree
	}
	if facts.Branch != "" {
		meta["branch"] = facts.Branch
	}
	if facts.PatchID != "" {
		meta["patch_id"] = facts.PatchID
	}
	return meta
}

// commitEventID is R1: deterministic over (session id, run id, commit sha) so
// a re-fired hook (an amend, a re-run of the same post-commit) dedupes at
// core rather than double-counting. runKey is whatever the caller used to
// check the halt latch (the minted run id, or the session id at generation
// 0) -- the same value, so two events for the same commit under a bumped run
// are deliberately distinct ids.
func commitEventID(sessionID, runKey, sha string) string {
	sum := sha256.Sum256([]byte("commit_created\x00" + sessionID + "\x00" + runKey + "\x00" + sha))
	return hex.EncodeToString(sum[:])[:32]
}
