package hookflow

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/client"
)

// They land in the dashboard today but never in the developer's session.
//   - INV-2: the summary is built only from content-free AdvisoryRecord fields
//     (verdict/would_block/risk/constraint count, guardrail reason categories
//     via reasonTypeCategories, drift boolean + count).
//   - INV-3: it emits only additionalContext + systemMessage; never a decision
//     / permissionDecision / blocking field.

// SurfaceFindings surfaces the content-free summary of any advisory findings
// recorded since the last surface, then advances the cursor.
func SurfaceFindings(provider, hook string, stdout io.Writer, logger interface{ Printf(string, ...any) }) {
	defer func() {
		if r := recover(); r != nil && logger != nil {
			logger.Printf("findings: recovered: %v", r)
		}
	}()
	if stdout == nil {
		return
	}

	advPath := DefaultAdvisoryPath()
	curPath := devconfig.ResolveFindingsCursor(provider)

	offset := readCursor(curPath)

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
		// Never blocks (INV-3).
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

// summarizeFindings every field it reads is a category/count/label; never free
// text or content (INV-2).
func summarizeFindings(delta []byte) string {
	s := findingsSummary{verdicts: map[string]bool{}, guardrailCats: map[string]bool{}}
	for line := range bytes.SplitSeq(delta, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var rec AdvisoryRecord
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
		for _, c := range ReasonTypeCategories(rec.GuardrailReasons) {
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

func (s findingsSummary) render() string {
	var parts []string
	if s.wouldBlock > 0 {
		if v := slices.Sorted(maps.Keys(s.verdicts)); len(v) > 0 {
			parts = append(parts, fmt.Sprintf("%d would-block [%s]", s.wouldBlock, strings.Join(v, ",")))
		} else {
			parts = append(parts, fmt.Sprintf("%d would-block", s.wouldBlock))
		}
	} else if v := slices.Sorted(maps.Keys(s.verdicts)); len(v) > 0 {
		parts = append(parts, fmt.Sprintf("verdicts [%s]", strings.Join(v, ",")))
	}
	if g := slices.Sorted(maps.Keys(s.guardrailCats)); len(g) > 0 {
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
	return fmt.Sprintf("OpenBox governance; %d finding(s) from recent activity%s. Review detail in the OpenBox dashboard.",
		s.total, detail)
}

func writeFindingsOutput(stdout io.Writer, hook string, summary string) error {
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

// readCursor a missing/unparsable file yields 0 (surface from the start);
// never an error (INV-3).
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

func advanceCursor(path string, offset int64, logger interface{ Printf(string, ...any) }) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		if logger != nil {
			logger.Printf("findings: cursor mkdir failed: %v", err)
		}
		return
	}
	if err := atomicWriteFile(path, []byte(strconv.FormatInt(offset, 10)), 0o600); err != nil {
		if logger != nil {
			logger.Printf("findings: cursor write failed: %v", err)
		}
	}
}

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

const maxFindingsDelta = 4 << 20 // 4 MiB

const (
	maxCategoryLen     = 40
	maxCategoriesShown = 12
)

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

func joinCapped(items []string, n int) string {
	if len(items) <= n {
		return strings.Join(items, ",")
	}
	return strings.Join(items[:n], ",") + fmt.Sprintf(",+%d more", len(items)-n)
}
