package main

import (
	"io"
	"os"
	"strings"
)

// enforcementTailBytes bounds a LOCAL read of the enforcement audit file
// (`enforcements.jsonl`) for `openbox doctor`. It is NOT an egress bound and
// must not be spelled with, or reused as, one (CLAUDE.md: bounds have
// owners) -- MaxCommandLen and MaxRedactBody/capBody bound different things
// for different reasons.
//
// 64 KiB is DECIDED, not measured, and does not need to be: the reader it
// replaces (`lastDecisionSummary`) read the whole file to look at one line,
// so any bound is strictly better than the status quo. One EnforcementRecord
// line is a few hundred bytes, so 64 KiB holds on the order of a hundred
// decisions, and both callers print at most a handful. If a reader ever needs
// more lines than the cap holds, raising the constant is a one-line change
// with no contract behind it.
const enforcementTailBytes = 64 << 10

// tailLines returns the last n COMPLETE lines of path, reading at most
// maxBytes from EOF.
//
// A window that did not start at byte 0 drops its first line, which is a
// fragment of a record, not a record. Absent, empty, or no-complete-line-in-
// the-window all return no lines and no error: every caller already prints
// "(none recorded)" for all three, and inventing a fourth outcome would make
// the two doctor readers disagree about what "nothing" means.
func tailLines(path string, maxBytes int64, n int) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil // absent: no lines
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil || fi.Size() == 0 {
		return nil // unreadable stat or empty: no lines
	}

	start := fi.Size() - maxBytes
	if start < 0 {
		start = 0
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return nil
	}
	buf := make([]byte, fi.Size()-start)
	if _, err := io.ReadFull(f, buf); err != nil {
		return nil
	}

	text := string(buf)
	if start > 0 {
		// The window's first line is a fragment (we seeked mid-file); drop
		// everything through the first newline instead of unmarshalling it.
		idx := strings.IndexByte(text, '\n')
		if idx < 0 {
			return nil // the whole window is one partial line: no complete line here
		}
		text = text[idx+1:]
	}
	text = strings.TrimRight(text, "\n")
	if text == "" {
		return nil
	}
	lines := strings.Split(text, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines
}
