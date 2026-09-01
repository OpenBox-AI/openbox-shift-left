package hookflow

import (
	"bytes"
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
