package trace

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// positioned pairs a decoded Record with where it was found, so a stable
// sort by TS can still fall back to file/line order for same-timestamp
// records instead of leaving their relative order to sort's whim.
type positioned struct {
	rec     Record
	fileIdx int
	lineIdx int
}

// Read loads every record from dir's .jsonl and .jsonl.gz files that match
// (match==nil keeps everything), sorted by TS and, for ties, by the file/line
// order records were found in. A line that fails to decode as a Record is
// skipped and counted rather than failing the whole read -- a single
// corrupt line (a torn write during a crash, mid-write during a concurrent
// read) must not hide every other record in the trace.
func Read(dir string, match func(Record) bool) (recs []Record, skipped int, err error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, 0, err
	}

	var names []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		n := e.Name()
		if strings.HasSuffix(n, ".jsonl") || strings.HasSuffix(n, ".jsonl.gz") {
			names = append(names, n)
		}
	}
	// Filenames are trace-YYYY-MM-DD.jsonl[.gz]; lexical order is date order,
	// which is the file order the sort's tiebreak means.
	sort.Strings(names)

	var all []positioned
	for fi, name := range names {
		n, s := readFile(filepath.Join(dir, name), fi, match, &all)
		skipped += s
		if n != nil {
			err = n
		}
	}

	sort.SliceStable(all, func(i, j int) bool {
		if !all[i].rec.TS.Equal(all[j].rec.TS) {
			return all[i].rec.TS.Before(all[j].rec.TS)
		}
		if all[i].fileIdx != all[j].fileIdx {
			return all[i].fileIdx < all[j].fileIdx
		}
		return all[i].lineIdx < all[j].lineIdx
	})

	recs = make([]Record, len(all))
	for i, p := range all {
		recs[i] = p.rec
	}
	return recs, skipped, nil
}

// readFile decodes one trace file's lines into out, returning any I/O error
// encountered opening/decompressing it (an unreadable file is not the same
// failure mode as a corrupt line: the former means the whole file's worth of
// records is missing, the latter is counted as skipped and moves on).
func readFile(path string, fileIdx int, match func(Record) bool, out *[]positioned) (error, int) {
	f, err := os.Open(path)
	if err != nil {
		return err, 0
	}
	defer f.Close()

	var r io.Reader = f
	if strings.HasSuffix(path, ".gz") {
		gr, err := gzip.NewReader(f)
		if err != nil {
			// The whole file is unreadable as gzip -- count it as one skipped
			// record rather than surfacing an error that would hide every
			// other file's records too.
			return nil, 1
		}
		defer gr.Close()
		r = gr
	}

	skipped := 0
	br := bufio.NewReader(r)
	lineIdx := 0
	for {
		line, rerr := br.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			var rec Record
			if jerr := json.Unmarshal(line, &rec); jerr != nil {
				skipped++
			} else if match == nil || match(rec) {
				*out = append(*out, positioned{rec: rec, fileIdx: fileIdx, lineIdx: lineIdx})
			}
			lineIdx++
		}
		if rerr != nil {
			break
		}
	}
	return nil, skipped
}
