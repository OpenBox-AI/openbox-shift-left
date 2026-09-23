package hookflow

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// DiscardLogName records every batch this spool gave up on, and why.
const DiscardLogName = ".discarded"

const maxDiscardLogBytes = 256 << 10

func (s Spool) DiscardPath() string {
	return filepath.Join(s.Dir, DiscardLogName)
}

func (s Spool) recordDiscard(basePath string, events int, reason string) {
	if events <= 0 {
		return
	}
	name := strings.TrimSuffix(filepath.Base(basePath), ".jsonl")
	line := fmt.Sprintf("%s\t%s\t%d\t%s\n", time.Now().UTC().Format(time.RFC3339), name, events, reason)

	s.logf("spool: DISCARDED %d event(s) for %s, and they are gone: %s. "+
		"Recorded in %s.", events, name, reason, s.DiscardPath())

	f := s.openCappedLog(s.DiscardPath(), maxDiscardLogBytes)
	if f == nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString(line)
}

// DiscardAll removes every event currently waiting in this spool -- session,
// recovery and in-flight rotated (`.flushing.`) files alike -- and records
// the total under reason in the discard ledger. It exists for D5: when
// `openbox init`/`adopt` writes a v3 identity over a legacy one, whatever
// that tool had queued under the old identity can never be delivered under
// the new one (the control plane ties every event to the credential that
// authenticated it), so it is discarded rather than left to churn forever
// unsent. It takes the spool lock, the same one Append/drain use, so this
// never races a concurrent flush into double-counting or a torn read.
//
// Best-effort past the first file: one unremovable file must not hide the
// rest, so every removable file is still removed and counted, and every
// removal failure is joined into the returned error.
func (s Spool) DiscardAll(reason string) (int, error) {
	unlock := s.lockSpool()
	defer unlock()

	entries, err := os.ReadDir(s.Dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("spool discard: readdir: %w", err)
	}

	total := 0
	var errs []error
	for _, e := range entries {
		if e.IsDir() || !IsBacklogFile(e.Name()) {
			continue
		}
		path := filepath.Join(s.Dir, e.Name())
		n := countLines(path)
		if rmErr := os.Remove(path); rmErr != nil {
			if !os.IsNotExist(rmErr) {
				errs = append(errs, fmt.Errorf("spool discard: remove %s: %w", e.Name(), rmErr))
			}
			continue
		}
		total += n
	}
	if total > 0 {
		s.recordDiscard(s.Dir, total, reason)
	}
	return total, errors.Join(errs...)
}

func (s Spool) DiscardedCount() int {
	data, err := os.ReadFile(s.DiscardPath())
	if err != nil {
		return 0
	}
	total := 0
	for _, line := range NonEmptyLines(data) {
		fields := bytes.Split(line, []byte{'\t'})
		if len(fields) < 3 {
			continue
		}
		if n, err := strconv.Atoi(string(fields[2])); err == nil && n > 0 {
			total += n
		}
	}
	return total
}
