package claudecode

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

// Tier-3 findings loop.
//
// Governance findings (guardrail categories, goal-drift, risk, would-block)
// are computed by /evaluate and recorded — content-free — on the flush
// path by the Advisory sink (advisories.jsonl). They land in the dashboard
// today but never in the developer's session. This file is the consumer
// that closes that loop: it tails advisories.jsonl from a byte-offset
// cursor and, on UserPromptSubmit + PostToolUse, surfaces a content-free
// summary of the new findings back into the session via
// hookSpecificOutput.additionalContext (→ the model) + systemMessage (→
// the user).
//
// It adds no new sink and no new producer — it reuses the existing
// advisory record (reuse, don't rebuild). It is gated behind
// ResolveFindings (default off): with findings off it is never called and
// the hooks write nothing.
//
// Safety:
//   - INV-2: the summary is built only from content-free advisoryRecord
//     fields (verdict/would_block/risk/constraint count, guardrail reason
//     categories via reasonTypeCategories, drift boolean + count). It
//     never emits guardrail/drift free text, tool content, command, or
//     file body. The cursor holds a byte offset only (structural).
//   - INV-3: it emits only additionalContext + systemMessage — never a
//     decision / permissionDecision / blocking field. Every fault
//     (missing/corrupt file, unwritable cursor, bad record) is swallowed;
//     the hook still exits 0. It can never block, delay, or fail a tool
//     call.
//   - The hot path (PostToolUse) is stat-guarded — with no new findings it
//     does one stat (+ a tiny cursor read) and no advisory-body read.

// surfaceFindings surfaces the content-free summary of any advisory findings recorded
// since the last surface, then advances the cursor. It is best-effort and swallows
// every error (INV-3). hook is the current hook (its name goes on hookSpecificOutput);
// stdout is where the summary JSON is written (nil ⇒ no-op).
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

	// Hot-path guard (NFR-2): if the advisory sink has grown no further than the
	// cursor, there is nothing new — do not read its body. A file that SHRANK below
	// the offset (truncation/rotation — not done today, defensive) resets to 0.
	fi, err := os.Stat(advPath)
	if err != nil {
		return // no advisory sink yet → nothing to surface
	}
	size := fi.Size()
	if size < offset {
		offset = 0
	}
	if size <= offset {
		return // nothing new
	}

	data, err := readFrom(advPath, offset)
	if err != nil || len(data) == 0 {
		return
	}
	// Consume only COMPLETE lines: never advance the cursor past a trailing fragment
	// (advisory appends are atomic whole-line O_APPEND writes, so a fragment is only
	// possible mid-write; leaving it re-reads it next time — at-most-once, never lost).
	lastNL := bytes.LastIndexByte(data, '\n')
	if lastNL < 0 {
		return
	}
	complete := data[:lastNL+1]
	newOffset := offset + int64(len(complete))

	sum := summarizeFindings(complete)
	if sum == "" {
		// No parseable/notable record in the delta — still advance so we don't
		// re-scan the same bytes forever.
		advanceCursor(curPath, newOffset, logger)
		return
	}

	if err := writeFindingsOutput(stdout, hook, sum); err != nil {
		// A write failure must not lose the findings: leave the cursor where it was so
		// the next hook re-attempts. Never blocks (INV-3).
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
// records. Aggregate (counts/sets) so the emitted message is bounded regardless of
// how many findings the delta holds.
type findingsSummary struct {
	total          int
	wouldBlock     int
	verdicts       map[string]bool
	guardrailCats  map[string]bool
	driftFindings  int
	driftViolations int
	constraints    int
	maxRisk        float64
}

// summarizeFindings parses the JSONL delta of advisoryRecords and renders ONE
// content-free summary line, or "" when the delta holds no notable record. Every
// field it reads is a category/count/label — never free text or content (INV-2).
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
// additionalContext (→ the model's context) + systemMessage (→ the user). No
// blocking field is ever written (INV-3). One object per hook invocation.
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

// advanceCursor writes the new offset atomically (temp file + rename) so a concurrent
// reader never sees a half-written offset. Best-effort: a failure is logged and
// swallowed (worst case the same findings surface once more — harmless).
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

// maxFindingsDelta bounds a single findings read so a pathologically large advisory
// backlog can't blow memory. Beyond it, the tail is read on the next hook (the cursor
// advances by what was read). advisoryRecords are tiny, so this holds many thousands.
const maxFindingsDelta = 4 << 20 // 4 MiB

// maxCategoryLen / maxCategoriesShown bound the guardrail categories
// rendered into the model-context summary. The guardrail `type` is a
// server-controlled vocabulary, but it is remote-sourced free-form text
// and this is the first place it is injected into Claude's context — so
// it is defensively bounded in both length (per category) and cardinality
// (per summary) before it lands there.
const (
	maxCategoryLen     = 40
	maxCategoriesShown = 12
)

// sanitizeCategory strips control characters (defense against context injection via a
// crafted category) and caps the length of one remote-sourced guardrail category.
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

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
