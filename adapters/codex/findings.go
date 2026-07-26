package codex

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/openbox-ai/openbox-shift-left/client"
)

// Tier-3 findings loop for the Codex adapter (STORY-SL7-B, the port of E6-S11;
// design §7 T3). Byte-for-byte the Claude Code semantics; the ONLY delta is the
// surfacing channel, which probe P2 resolved.
//
// Governance findings (guardrail categories, goal-drift, risk, would-block) are
// computed by /evaluate and recorded — content-free — on the FLUSH path by the
// SL-9 Advisory sink (advisories.jsonl, shared with the CC adapter by design).
// This file is the CONSUMER that closes the loop: it tails advisories.jsonl from a
// byte-offset CURSOR and, on UserPromptSubmit + PostToolUse, surfaces a
// CONTENT-FREE summary back INTO the session.
//
// CHANNEL (OD-SL7-FINDINGS, resolved by probe P2 — NOT the degraded mode): the
// binary-embedded output schemas (pre/post-tool-use, session-start,
// user-prompt-submit .command.output) all define hookSpecificOutput.additionalContext
// (string), and discovery.rs confirms these events "can emit additionalContext";
// other events warn "this event cannot emit additionalContext". So Codex accepts
// the SAME additionalContext (→ model) + systemMessage (→ user) channel Claude
// Code uses on UserPromptSubmit + PostToolUse — full parity, no systemMessage-only
// degraded fallback needed.
//
// It adds NO new sink and NO new producer (reuse, don't rebuild) and is gated
// behind ResolveFindings (default OFF): with findings off it is never called.
//
// SAFETY:
//   - INV-2: the summary is built ONLY from content-free advisoryRecord fields
//     (verdict/would_block/risk/constraint COUNT, guardrail reason CATEGORIES,
//     drift boolean + COUNT). It NEVER emits free text, command, or patch body. The
//     cursor holds a byte offset only.
//   - INV-3: it emits ONLY additionalContext + systemMessage — never a decision/
//     permissionDecision/blocking field. Every fault is swallowed; the hook still
//     exits 0. It can never block, delay, or fail a tool call.
//   - NFR-2: the hot path (PostToolUse) is stat-guarded — with no new findings it
//     does one stat (+ a tiny cursor read) and no advisory-body read.

// surfaceFindings surfaces the content-free summary of any advisory findings
// recorded since the last surface, then advances the cursor. Best-effort; swallows
// every error (INV-3). hook is the current hook; stdout is where the summary JSON
// is written (nil ⇒ no-op).
func surfaceFindings(hook HookName, stdout io.Writer, logger interface{ Printf(string, ...any) }) {
	defer func() {
		if r := recover(); r != nil && logger != nil {
			logger.Printf("findings: recovered: %v", r)
		}
	}()
	if stdout == nil {
		return
	}

	advPath := DefaultAdvisoryPath()
	curPath := ResolveFindingsCursor()

	offset := readCursor(curPath)

	fi, err := os.Stat(advPath)
	if err != nil {
		return // no advisory sink yet → nothing to surface
	}
	size := fi.Size()
	if size < offset {
		offset = 0 // file shrank (rotation/truncation) → reset
	}
	if size <= offset {
		return // nothing new
	}

	data, err := readFrom(advPath, offset)
	if err != nil || len(data) == 0 {
		return
	}
	// Consume only COMPLETE lines: never advance past a trailing fragment.
	lastNL := bytes.LastIndexByte(data, '\n')
	if lastNL < 0 {
		return
	}
	complete := data[:lastNL+1]
	newOffset := offset + int64(len(complete))

	sum := summarizeFindings(complete)
	if sum == "" {
		advanceCursor(curPath, newOffset, logger)
		return
	}

	if err := writeFindingsOutput(stdout, hook, sum); err != nil {
		// A write failure must not lose the findings: leave the cursor so the next
		// hook re-attempts. Never blocks (INV-3).
		if logger != nil {
			logger.Printf("findings: surface write failed (will retry): %v", err)
		}
		return
	}
	advanceCursor(curPath, newOffset, logger)
	if logger != nil {
		logger.Printf("findings: surfaced summary at %s (cursor→%d)", hook, newOffset)
	}
}

// findingsSummary aggregates the content-free signals across a delta of advisory
// records (counts/sets) so the emitted message is bounded regardless of how many
// findings the delta holds.
type findingsSummary struct {
	total           int
	wouldBlock      int
	verdicts        map[string]bool
	guardrailCats   map[string]bool
	driftFindings   int
	driftViolations int
	constraints     int
	maxRisk         float64
}

// summarizeFindings parses the JSONL delta of advisoryRecords and renders ONE
// content-free summary line, or "" when the delta holds no notable record.
func summarizeFindings(delta []byte) string {
	s := findingsSummary{verdicts: map[string]bool{}, guardrailCats: map[string]bool{}}
	for _, line := range bytes.Split(delta, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var rec advisoryRecord
		if json.Unmarshal(line, &rec) != nil {
			continue // skip a corrupt line; never fail the whole summary
		}
		s.total++
		if rec.WouldBlock {
			s.wouldBlock++
		}
		if rec.Verdict != "" && rec.Verdict != string(client.VerdictAllow) {
			s.verdicts[rec.Verdict] = true
		}
		for _, c := range reasonTypeCategories(rec.GuardrailReasons) {
			s.guardrailCats[sanitizeCategory(c)] = true
		}
		if rec.DriftDetected {
			s.driftFindings++
			s.driftViolations += rec.DriftViolations
		}
		s.constraints += len(rec.Constraints)
		if rec.RiskScore > s.maxRisk {
			s.maxRisk = rec.RiskScore
		}
	}
	if s.total == 0 {
		return ""
	}
	return s.render()
}

// render builds the content-free summary string. Sets are sorted for determinism.
func (s findingsSummary) render() string {
	var parts []string
	if s.wouldBlock > 0 {
		if v := sortedKeys(s.verdicts); len(v) > 0 {
			parts = append(parts, fmt.Sprintf("%d would-block [%s]", s.wouldBlock, strings.Join(v, ",")))
		} else {
			parts = append(parts, fmt.Sprintf("%d would-block", s.wouldBlock))
		}
	} else if v := sortedKeys(s.verdicts); len(v) > 0 {
		parts = append(parts, fmt.Sprintf("verdicts [%s]", strings.Join(v, ",")))
	}
	if g := sortedKeys(s.guardrailCats); len(g) > 0 {
		parts = append(parts, fmt.Sprintf("guardrails [%s]", joinCapped(g, maxCategoriesShown)))
	}
	if s.driftFindings > 0 {
		parts = append(parts, fmt.Sprintf("goal-drift on %d (%d violation(s))", s.driftFindings, s.driftViolations))
	}
	if s.constraints > 0 {
		parts = append(parts, fmt.Sprintf("%d constraint(s)", s.constraints))
	}
	if s.maxRisk > 0 {
		parts = append(parts, fmt.Sprintf("max risk %.2f", s.maxRisk))
	}
	detail := ""
	if len(parts) > 0 {
		detail = ": " + strings.Join(parts, "; ")
	}
	return fmt.Sprintf("OpenBox governance — %d finding(s) from recent activity%s. Review detail in the OpenBox dashboard.",
		s.total, detail)
}

// writeFindingsOutput emits the ONE hook JSON object that surfaces the summary:
// additionalContext (→ the model's context, probe P2) + systemMessage (→ the user).
// No blocking field is ever written (INV-3). One object per hook invocation.
func writeFindingsOutput(stdout io.Writer, hook HookName, summary string) error {
	out := map[string]any{
		"hookSpecificOutput": map[string]any{
			"hookEventName":     string(hook),
			"additionalContext": summary,
		},
		"systemMessage": summary,
	}
	line, err := json.Marshal(out)
	if err != nil {
		return err
	}
	_, err = stdout.Write(append(line, '\n'))
	return err
}

// readCursor reads the byte offset from the cursor state file. A missing/unparsable
// file yields 0 (surface from the start) — never an error (INV-3).
func readCursor(path string) int64 {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// advanceCursor writes the new offset atomically (temp file + rename). Best-effort:
// a failure is logged and swallowed (worst case the same findings surface once more).
func advanceCursor(path string, offset int64, logger interface{ Printf(string, ...any) }) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		if logger != nil {
			logger.Printf("findings: cursor mkdir failed: %v", err)
		}
		return
	}
	tmp := path + ".tmp." + randomID()
	if err := os.WriteFile(tmp, []byte(strconv.FormatInt(offset, 10)), 0o600); err != nil {
		if logger != nil {
			logger.Printf("findings: cursor write failed: %v", err)
		}
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		if logger != nil {
			logger.Printf("findings: cursor rename failed: %v", err)
		}
	}
}

// readFrom reads a file from byte offset to EOF, bounded, without loading the prefix.
func readFrom(path string, offset int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, err
	}
	return io.ReadAll(io.LimitReader(f, maxFindingsDelta))
}

// maxFindingsDelta bounds a single findings read so a pathological advisory backlog
// can't blow memory. Beyond it, the tail is read on the next hook.
const maxFindingsDelta = 4 << 20 // 4 MiB

// maxCategoryLen / maxCategoriesShown bound the guardrail categories rendered into
// the model-context summary. The guardrail `type` is a server-controlled vocabulary
// but remote-sourced free-form text, and this is the first place it is injected into
// the model's context — so it is defensively bounded in length and cardinality.
const (
	maxCategoryLen     = 40
	maxCategoriesShown = 12
)

// sanitizeCategory strips control characters (defense against context injection via
// a crafted category) and caps the length of one remote-sourced guardrail category.
func sanitizeCategory(c string) string {
	c = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, c)
	if len(c) > maxCategoryLen {
		c = c[:maxCategoryLen]
	}
	return c
}

// joinCapped joins up to n items with commas, appending "+K more" when the set is
// larger — so the rendered summary is bounded regardless of category cardinality.
func joinCapped(items []string, n int) string {
	if len(items) <= n {
		return strings.Join(items, ",")
	}
	return strings.Join(items[:n], ",") + fmt.Sprintf(",+%d more", len(items)-n)
}

// reasonTypeCategories returns the guardrail reason CATEGORY types (the `type`
// field, e.g. ["pii","secrets"]) — NEVER the reason free text or field name, which
// can describe detected content (INV-2). The slice form of advisory.go's
// reasonTypes, consumed by the enforcement audit and the findings summary.
func reasonTypeCategories(reasons []client.GuardrailReason) []string {
	if len(reasons) == 0 {
		return nil
	}
	types := make([]string, 0, len(reasons))
	for _, r := range reasons {
		t := r.Type
		if t == "" {
			t = "?"
		}
		types = append(types, t)
	}
	return types
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
